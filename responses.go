package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sashabaranov/go-openai"
)

func responsesEndpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/responses"
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesRequest struct {
	Model        string          `json:"model"`
	Input        []any           `json:"input"`
	Instructions string          `json:"instructions,omitempty"`
	Tools        []responsesTool `json:"tools,omitempty"`
	ToolChoice   any             `json:"tool_choice,omitempty"`
	Temperature  *float32        `json:"temperature,omitempty"`
	Reasoning    map[string]any  `json:"reasoning,omitempty"`
	Store        bool            `json:"store"`
	Stream       bool            `json:"stream,omitempty"`
}

type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesOutputItem struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"id,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
	Role      string                 `json:"role,omitempty"`
	Phase     string                 `json:"phase,omitempty"`
	Content   []responsesContent     `json:"content,omitempty"`
	Summary   []responsesSummaryPart `json:"summary,omitempty"`
	Status    string                 `json:"status,omitempty"`
}

type responsesResponse struct {
	ID     string                `json:"id"`
	Model  string                `json:"model"`
	Status string                `json:"status,omitempty"`
	Output []responsesOutputItem `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// ResponsesTools exposes the registry in Responses function-tool format.
func (r *Registry) ResponsesTools() []responsesTool {
	out := make([]responsesTool, 0, len(r.names))
	for _, n := range r.names {
		t := r.tools[n]
		params := t.Schema
		if len(bytes.TrimSpace(params)) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, responsesTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: params})
	}
	return out
}

// responsesReasoning maps our levels onto Responses reasoning effort. Empty
// omits the field (provider default). "none" also omits: Muse Spark rejects
// effort "none" with 400, and grok/gpt never offer it, so omitting is the
// safe no-reasoning request. Other levels pass through verbatim.
func responsesReasoning(level string) map[string]any {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" || level == "none" {
		return nil
	}
	return map[string]any{"effort": level}
}

// splitResponsesInput converts OpenAI-shaped history into Responses
// instructions plus a stateless input array. Assistant text that precedes a
// function call is tagged phase "commentary" (Meta requires it); reasoning
// items are omitted in v1 (chain-of-thought continuity is a follow-up).
func splitResponsesInput(history []openai.ChatCompletionMessage) (string, []any) {
	var system []string
	input := make([]any, 0, len(history))
	for _, m := range history {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
		case "user":
			input = append(input, map[string]any{"role": "user", "content": m.Content})
		case "assistant":
			if len(m.ToolCalls) == 0 {
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": m.Content}},
				})
				continue
			}
			if strings.TrimSpace(m.Content) != "" {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"phase":   "commentary",
					"content": []any{map[string]any{"type": "output_text", "text": m.Content}},
				})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		}
	}
	return strings.Join(system, "\n\n"), input
}

func parseResponsesError(status int, body []byte) *transportError {
	var oa struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &oa); err == nil && oa.Error.Message != "" {
		return &transportError{Transport: "responses", Status: status, Message: oa.Error.Message}
	}
	var flat struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &flat); err == nil {
		if flat.Message != "" {
			return &transportError{Transport: "responses", Status: status, Message: flat.Message}
		}
		if flat.Error != "" {
			return &transportError{Transport: "responses", Status: status, Message: flat.Error}
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &transportError{Transport: "responses", Status: status, Message: msg}
}

func callResponsesOnce(ctx context.Context, hc *http.Client, cfg Config, apiKey string, req responsesRequest) (responsesResponse, error) {
	var out responsesResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesEndpoint(cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if cfg.SessionID != "" {
		httpReq.Header.Set("x-opencode-session", cfg.SessionID)
	}
	httpReq.Header.Set("User-Agent", userAgent)
	client := hc
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, parseResponsesError(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("api: responses: decode: %w", err)
	}
	return out, nil
}

// responsesReplyToOpenAI folds Responses output items back into an
// OpenAI-shaped assistant message. Reasoning summaries become <think> blocks
// so the existing UI collapse keeps working.
func responsesReplyToOpenAI(resp responsesResponse) (openai.ChatCompletionMessage, error) {
	var text, thinking []string
	var calls []openai.ToolCall
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text = append(text, c.Text)
				}
			}
		case "function_call":
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			args := strings.TrimSpace(item.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			calls = append(calls, openai.ToolCall{
				ID:   id,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      item.Name,
					Arguments: args,
				},
			})
		case "reasoning":
			for _, s := range item.Summary {
				if strings.TrimSpace(s.Text) != "" {
					thinking = append(thinking, s.Text)
				}
			}
		}
	}
	content := strings.Join(text, "")
	if t := strings.TrimSpace(strings.Join(thinking, "\n")); t != "" {
		content = "<think>" + t + "</think>" + content
	}
	if strings.TrimSpace(content) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, errors.New("api: empty choices in responses response")
	}
	return openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   content,
		ToolCalls: calls,
	}, nil
}

// runResponsesTurn drives the tool loop against POST {base}/responses with
// store:false (stateless). History stays OpenAI-shaped like the messages
// transport. Rounds stream SSE when the provider accepts it, else fall back
// to one unary call.
func runResponsesTurn(ctx context.Context, cfg Config, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	tools := registry.ResponsesTools()
	reasoning := responsesReasoning(cfg.Thinking)
	for round := 0; round < cfg.MaxRounds; round++ {
		instructions, input := splitResponsesInput(*history)
		req := responsesRequest{
			Model:        cfg.Model,
			Input:        input,
			Instructions: instructions,
			Reasoning:    reasoning,
			Store:        false,
		}
		if len(tools) > 0 {
			req.Tools = tools
			req.ToolChoice = "auto"
		}
		if cfg.Temperature != nil {
			req.Temperature = cfg.Temperature
		}

		sreq := req
		sreq.Stream = true
		msg, roundUsage, err := responsesRoundStream(ctx, cfg, apiKey, hc, sreq, onEvent)
		if err != nil {
			if !isStreamRejected(err) {
				return openai.ChatCompletionMessage{}, usage, err
			}
			resp, err := callResponsesOnce(ctx, hc, cfg, apiKey, req)
			if err != nil {
				return openai.ChatCompletionMessage{}, usage, err
			}
			usage.PromptTokens += resp.Usage.InputTokens
			usage.CompletionTokens += resp.Usage.OutputTokens
			if resp.Usage.TotalTokens != 0 {
				usage.TotalTokens += resp.Usage.TotalTokens
			} else {
				usage.TotalTokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
			}

			msg, err = responsesReplyToOpenAI(resp)
			if err != nil {
				return openai.ChatCompletionMessage{}, usage, err
			}
			roundUsage = openai.Usage{}
		} else {
			usage.PromptTokens += roundUsage.PromptTokens
			usage.CompletionTokens += roundUsage.CompletionTokens
			usage.TotalTokens += roundUsage.TotalTokens
		}

		*history = append(*history, msg)
		if len(msg.ToolCalls) == 0 {
			return msg, usage, nil
		}
		*history = append(*history, runToolCalls(ctx, registry, msg.ToolCalls, onEvent)...)
	}
	return openai.ChatCompletionMessage{}, usage, fmt.Errorf("stopped after %d rounds without a final answer", cfg.MaxRounds)
}

type responsesCallAcc struct {
	id   string
	name string
	args strings.Builder
}

// responsesRoundStream runs one round as Responses SSE, emitting text and
// reasoning deltas live. A unary JSON body folds through
// responsesReplyToOpenAI. The completed event carries usage; when deltas
// produced nothing usable, the completed response object is folded instead.
func responsesRoundStream(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req responsesRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	resp, err := postAPI(ctx, hc, cfg, responsesEndpoint(cfg.BaseURL), req, headers)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	defer resp.Body.Close()
	br := bufio.NewReader(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(br)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		return openai.ChatCompletionMessage{}, usage, parseResponsesError(resp.StatusCode, raw)
	}
	sse, raw, err := splitBodyMode(br)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	if !sse {
		var out responsesResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openai.ChatCompletionMessage{}, usage, fmt.Errorf("api: responses: decode: %w", err)
		}
		msg, err := responsesReplyToOpenAI(out)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		usage.PromptTokens += out.Usage.InputTokens
		usage.CompletionTokens += out.Usage.OutputTokens
		if out.Usage.TotalTokens != 0 {
			usage.TotalTokens += out.Usage.TotalTokens
		} else {
			usage.TotalTokens += out.Usage.InputTokens + out.Usage.OutputTokens
		}
		return msg, usage, nil
	}

	emit := func(kind, text string) {
		if onEvent != nil && text != "" {
			onEvent(Event{Kind: kind, Text: text})
		}
	}
	var text, thinking strings.Builder
	accs := map[int]*responsesCallAcc{}
	var order []int
	var completed *responsesResponse
	var scanErr error
	scanErr = scanSSE(ctx, br, func(ev sseEvent) bool {
		if strings.TrimSpace(ev.data) == "[DONE]" {
			return false
		}
		var payload struct {
			Type        string `json:"type"`
			OutputIndex *int   `json:"output_index"`
			Delta       string `json:"delta"`
			Item        *struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *responsesResponse `json:"response"`
			Message  string             `json:"message"`
			Error    string             `json:"error"`
		}
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			return true
		}
		switch payload.Type {
		case "error", "response.failed", "response.incomplete":
			msg := payload.Message
			if msg == "" {
				msg = payload.Error
			}
			if msg == "" {
				msg = payload.Type
			}
			scanErr = &transportError{Transport: "responses", Status: 0, Message: msg}
			return false
		case "response.output_text.delta":
			text.WriteString(payload.Delta)
			emit("text_delta", payload.Delta)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			thinking.WriteString(payload.Delta)
			emit("thinking_delta", payload.Delta)
		case "response.output_item.added":
			if payload.Item == nil || payload.Item.Type != "function_call" || payload.OutputIndex == nil {
				return true
			}
			idx := *payload.OutputIndex
			if _, ok := accs[idx]; !ok {
				order = append(order, idx)
			}
			accs[idx] = &responsesCallAcc{id: payload.Item.CallID, name: payload.Item.Name}
			if accs[idx].id == "" {
				accs[idx].id = payload.Item.ID
			}
		case "response.function_call_arguments.delta":
			idx := 0
			if payload.OutputIndex != nil {
				idx = *payload.OutputIndex
			}
			a := accs[idx]
			if a == nil {
				a = &responsesCallAcc{}
				accs[idx] = a
				order = append(order, idx)
			}
			a.args.WriteString(payload.Delta)
		case "response.completed":
			if payload.Response != nil {
				completed = payload.Response
			}
			return false
		}
		return true
	})
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, scanErr
	}
	if completed != nil {
		usage.PromptTokens += completed.Usage.InputTokens
		usage.CompletionTokens += completed.Usage.OutputTokens
		if completed.Usage.TotalTokens != 0 {
			usage.TotalTokens += completed.Usage.TotalTokens
		} else {
			usage.TotalTokens += completed.Usage.InputTokens + completed.Usage.OutputTokens
		}
		if strings.TrimSpace(text.String()) == "" && len(order) == 0 {
			msg, err := responsesReplyToOpenAI(*completed)
			if err != nil {
				return openai.ChatCompletionMessage{}, usage, err
			}
			return msg, usage, nil
		}
	}
	var calls []openai.ToolCall
	for _, idx := range order {
		a := accs[idx]
		calls = append(calls, openai.ToolCall{
			ID:   a.id,
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      a.name,
				Arguments: a.args.String(),
			},
		})
	}
	content := text.String()
	if t := strings.TrimSpace(thinking.String()); t != "" {
		content = "<think>" + t + "</think>" + content
	}
	if strings.TrimSpace(content) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: empty choices in responses response")
	}
	return openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   content,
		ToolCalls: calls,
	}, usage, nil
}
