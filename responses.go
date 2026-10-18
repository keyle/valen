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
	"sort"
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
	Model           string          `json:"model"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Input           []any           `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Tools           []responsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	Temperature     *float32        `json:"temperature,omitempty"`
	Reasoning       map[string]any  `json:"reasoning,omitempty"`
	Store           bool            `json:"store"`
	Stream          bool            `json:"stream,omitempty"`
	Include         []string        `json:"include,omitempty"`
}

type responsesContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesOutputItem struct {
	Type             string                 `json:"type"`
	ID               string                 `json:"id,omitempty"`
	CallID           string                 `json:"call_id,omitempty"`
	Name             string                 `json:"name,omitempty"`
	Arguments        string                 `json:"arguments,omitempty"`
	Role             string                 `json:"role,omitempty"`
	Phase            string                 `json:"phase,omitempty"`
	Content          []responsesContent     `json:"content,omitempty"`
	Summary          []responsesSummaryPart `json:"summary,omitempty"`
	Status           string                 `json:"status,omitempty"`
	EncryptedContent string                 `json:"encrypted_content,omitempty"`
}

// MarshalJSON ensures Responses reasoning items always carry `summary`.
// The provider returns `"summary":[]` for empty reasoning and requires it on
// replay (`input[1] missing required field summary` otherwise). Go's
// `omitempty` drops empty slices, so without this the second round of every
// tool-using turn 400s. Non-reasoning items keep omitempty behavior.
func (it responsesOutputItem) MarshalJSON() ([]byte, error) {
	type plain responsesOutputItem
	if it.Type != "reasoning" {
		return json.Marshal(plain(it))
	}
	// Build the object manually so an empty summary encodes as [] not null
	// and is never omitted. Nil becomes [].
	summary := it.Summary
	if summary == nil {
		summary = []responsesSummaryPart{}
	}
	m := map[string]any{
		"type":    it.Type,
		"summary": summary,
	}
	if it.ID != "" {
		m["id"] = it.ID
	}
	if it.CallID != "" {
		m["call_id"] = it.CallID
	}
	if it.Name != "" {
		m["name"] = it.Name
	}
	if it.Arguments != "" {
		m["arguments"] = it.Arguments
	}
	if it.Role != "" {
		m["role"] = it.Role
	}
	if it.Phase != "" {
		m["phase"] = it.Phase
	}
	if len(it.Content) > 0 {
		m["content"] = it.Content
	}
	if it.Status != "" {
		m["status"] = it.Status
	}
	if it.EncryptedContent != "" {
		m["encrypted_content"] = it.EncryptedContent
	}
	return json.Marshal(m)
}

type responsesResponse struct {
	ID                string                `json:"id"`
	Model             string                `json:"model"`
	Status            string                `json:"status,omitempty"`
	Output            []responsesOutputItem `json:"output"`
	Error             json.RawMessage       `json:"error,omitempty"`
	IncompleteDetails json.RawMessage       `json:"incomplete_details,omitempty"`
	Usage             struct {
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

// Without model information, omit "none" for providers that reject it.
// runResponsesTurn explicitly sends it for GPT models only.
func responsesReasoning(level string) map[string]any {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" || level == "none" {
		return nil
	}
	return map[string]any{"effort": level}
}

// splitResponsesInput converts OpenAI-shaped history into Responses
// instructions plus a stateless input array. Assistant text that precedes a
// function call is tagged phase "commentary" (Meta requires it).
func splitResponsesInput(history []openai.ChatCompletionMessage) (string, []any) {
	return splitResponsesInputNative(history, nil)
}

// Native output is scoped to the active turn, keyed by the assistant's first
// tool call ID. Replay it verbatim rather than flattening reasoning or phases.
func splitResponsesInputNative(history []openai.ChatCompletionMessage, native map[string][]responsesOutputItem) (string, []any) {
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
			if len(m.ToolCalls) > 0 {
				if items := native[m.ToolCalls[0].ID]; len(items) > 0 {
					for _, item := range items {
						input = append(input, item)
					}
					continue
				}
			}
			content := m.Content
			if strings.Contains(content, "<think>") {
				_, content = splitThinking(content)
			}
			if len(m.ToolCalls) == 0 {
				if strings.TrimSpace(content) != "" {
					input = append(input, map[string]any{
						"role":    "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": content}},
					})
				}
				continue
			}
			if strings.TrimSpace(content) != "" {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"phase":   "commentary",
					"content": []any{map[string]any{"type": "output_text", "text": content}},
				})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
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
				"output":  fitSendBudget(m.Content),
			})
		}
	}
	return strings.Join(system, "\n\n"), input
}

func parseResponsesError(status int, body []byte) *transportError {
	var detail func(any) string
	detail = func(v any) string {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		if obj, ok := v.(map[string]any); ok {
			for _, key := range []string{"message", "error", "response", "incomplete_details", "reason", "code", "status", "type"} {
				if s := detail(obj[key]); s != "" {
					return s
				}
			}
		}
		return ""
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err == nil {
		if msg := detail(decoded); msg != "" {
			raw := capRawBody(body)
			return &transportError{Transport: "responses", Status: status, Message: msg, Raw: rawDiff(raw, msg)}
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &transportError{Transport: "responses", Status: status, Message: msg, Raw: rawDiff(capRawBody(body), msg)}
}

func responsesResponseError(resp responsesResponse) error {
	if (len(resp.Error) > 0 && string(resp.Error) != "null") || (resp.Status != "" && resp.Status != "completed") {
		raw, _ := json.Marshal(resp)
		return parseResponsesError(0, raw)
	}
	return nil
}

func callResponsesOnce(ctx context.Context, hc *http.Client, cfg Config, apiKey string, req responsesRequest) (responsesResponse, error) {
	var out responsesResponse
	if !req.Store && len(req.Include) == 0 {
		req.Include = []string{"reasoning.encrypted_content"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesEndpoint(providerBaseURL(cfg)), bytes.NewReader(body))
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
	resp, err := clientDoWithRetry(ctx, client, httpReq)
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
	return out, responsesResponseError(out)
}

// responsesReplyToOpenAI folds Responses output items back into an
// OpenAI-shaped assistant message. Reasoning summaries become <think> blocks
// so the existing UI collapse keeps working.
func responsesReplyToOpenAI(resp responsesResponse) (openai.ChatCompletionMessage, error) {
	if err := responsesResponseError(resp); err != nil {
		return openai.ChatCompletionMessage{}, err
	}
	var text, thinking []string
	var calls []openai.ToolCall
	callIDs := make(map[string]bool)
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
			var object map[string]json.RawMessage
			if strings.TrimSpace(id) == "" || strings.TrimSpace(item.Name) == "" {
				return openai.ChatCompletionMessage{}, errors.New("api: responses: function call missing ID or name")
			}
			if callIDs[id] {
				return openai.ChatCompletionMessage{}, fmt.Errorf("api: responses: duplicate function call ID %q", id)
			}
			callIDs[id] = true
			if err := json.Unmarshal([]byte(args), &object); err != nil || object == nil {
				return openai.ChatCompletionMessage{}, fmt.Errorf("api: responses: invalid arguments for function call %q", id)
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
	_, answer := splitThinking(content)
	if strings.TrimSpace(answer) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, errors.New("api: empty choices in responses response")
	}
	if t := strings.TrimSpace(strings.Join(thinking, "\n")); t != "" {
		content = "<think>" + t + "</think>" + content
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
func runResponsesTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	tools := registry.ResponsesTools()
	reasoning := responsesReasoning(cfg.Thinking)
	if strings.HasPrefix(modelBaseID(cfg.Model), "gpt-") && cfg.Thinking == "none" {
		reasoning = map[string]any{"effort": "none"}
	}
	native := make(map[string][]responsesOutputItem)
	stream := true
	imgs := imageAttachments(turnAttachments(ctx))
	return runModelTurn(ctx, cfg, registry, history, onEvent, func(ctx context.Context, history []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		instructions, input := splitResponsesInputNative(history, native)
		responsesWithImages(input, imgs)
		req := responsesRequest{
			Model:           wireModelID(cfg.Model, cfg.Routing),
			MaxOutputTokens: outputTokens(cfg),
			Input:           input,
			Instructions:    instructions,
			Reasoning:       reasoning,
			Store:           false,
			Stream:          stream,
		}
		if len(tools) > 0 {
			req.Tools = tools
			req.ToolChoice = "auto"
		}
		if cfg.Temperature != nil {
			req.Temperature = cfg.Temperature
		}
		msg, usage, items, err := responsesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
		if stream && isStreamRejected(err) {
			stream, req.Stream = false, false
			msg, usage, items, err = responsesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
		}
		if err == nil && len(msg.ToolCalls) > 0 {
			native[msg.ToolCalls[0].ID] = items
		}
		return msg, usage, err
	})
}

// responsesRoundStream runs one round as Responses SSE, emitting text and
// reasoning deltas live. Native output is available through the detailed API.
func responsesRoundStream(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req responsesRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	msg, usage, _, err := responsesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
	return msg, usage, err
}

// responsesRoundStreamDetailed returns native output separately from display
// content. The transport closure can retain it for active tool continuations;
// no native metadata is emitted to the UI or stored in ordinary history.
func responsesRoundStreamDetailed(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req responsesRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, []responsesOutputItem, error) {
	var usage openai.Usage
	if !req.Store && len(req.Include) == 0 {
		req.Include = []string{"reasoning.encrypted_content"}
	}
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	resp, err := postAPI(ctx, hc, cfg, responsesEndpoint(providerBaseURL(cfg)), req, headers)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	defer resp.Body.Close()
	br := bufio.NewReader(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(br)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, nil, err
		}
		return openai.ChatCompletionMessage{}, usage, nil, parseResponsesError(resp.StatusCode, raw)
	}
	sse, raw, err := splitBodyMode(br)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	fold := func(out responsesResponse) (openai.ChatCompletionMessage, openai.Usage, []responsesOutputItem, error) {
		usage := openai.Usage{PromptTokens: out.Usage.InputTokens, CompletionTokens: out.Usage.OutputTokens, TotalTokens: out.Usage.TotalTokens}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		msg, err := responsesReplyToOpenAI(out)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, nil, err
		}
		return msg, usage, out.Output, nil
	}
	if !sse {
		var out responsesResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openai.ChatCompletionMessage{}, usage, nil, fmt.Errorf("api: responses: decode: %w", err)
		}
		return fold(out)
	}

	emit := func(kind, text string, idx int) {
		if onEvent != nil && text != "" {
			onEvent(Event{Kind: kind, Text: text, Index: idx})
		}
	}
	// Builders keep many small deltas linear in total response size. Complete
	// item/argument events replace these accumulators rather than appending.
	type streamedItem struct {
		item    responsesOutputItem
		text    map[int]*strings.Builder
		summary map[int]*strings.Builder
		args    *strings.Builder
	}
	items := map[int]*streamedItem{}
	var completed *responsesResponse
	var eventErr error
	scanErr := scanSSE(ctx, br, func(ev sseEvent) bool {
		if strings.TrimSpace(ev.data) == "[DONE]" {
			return false
		}
		var payload struct {
			Type         string               `json:"type"`
			OutputIndex  *int                 `json:"output_index"`
			ContentIndex int                  `json:"content_index"`
			SummaryIndex int                  `json:"summary_index"`
			ItemID       string               `json:"item_id"`
			Delta        string               `json:"delta"`
			Text         string               `json:"text"`
			Arguments    string               `json:"arguments"`
			Item         *responsesOutputItem `json:"item"`
			Response     *responsesResponse   `json:"response"`
		}
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			eventErr = fmt.Errorf("api: responses: decode event: %w", err)
			return false
		}
		if payload.Type == "" {
			payload.Type = ev.name
		}
		idx := 0
		if payload.OutputIndex != nil {
			idx = *payload.OutputIndex
		} else if payload.ItemID != "" {
			for i, item := range items {
				if item.item.ID == payload.ItemID {
					idx = i
					break
				}
			}
		}
		// Bound indexes before allocating content/summary slots on untrusted SSE.
		if idx < 0 || payload.ContentIndex < 0 || payload.ContentIndex > 1024 || payload.SummaryIndex < 0 || payload.SummaryIndex > 1024 {
			eventErr = errors.New("api: responses: invalid event index")
			return false
		}
		itemAt := func() *streamedItem {
			if items[idx] == nil {
				items[idx] = &streamedItem{item: responsesOutputItem{ID: payload.ItemID}}
			}
			return items[idx]
		}
		switch payload.Type {
		case "error", "response.failed", "response.incomplete":
			eventErr = parseResponsesError(0, []byte(ev.data))
			return false
		case "response.output_text.delta", "response.output_text.done":
			acc := itemAt()
			item := &acc.item
			item.Type, item.Role = "message", "assistant"
			for len(item.Content) <= payload.ContentIndex {
				item.Content = append(item.Content, responsesContent{})
			}
			part := &item.Content[payload.ContentIndex]
			part.Type = "output_text"
			if payload.Type == "response.output_text.done" {
				part.Text = payload.Text
				delete(acc.text, payload.ContentIndex)
			} else {
				if acc.text == nil {
					acc.text = make(map[int]*strings.Builder)
				}
				if acc.text[payload.ContentIndex] == nil {
					acc.text[payload.ContentIndex] = &strings.Builder{}
					acc.text[payload.ContentIndex].WriteString(part.Text)
				}
				acc.text[payload.ContentIndex].WriteString(payload.Delta)
				emit("text_delta", payload.Delta, 0)
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.done":
			acc := itemAt()
			item := &acc.item
			item.Type = "reasoning"
			for len(item.Summary) <= payload.SummaryIndex {
				item.Summary = append(item.Summary, responsesSummaryPart{})
			}
			part := &item.Summary[payload.SummaryIndex]
			part.Type = "summary_text"
			if strings.HasSuffix(payload.Type, ".done") {
				part.Text = payload.Text
				// Done-only parts never streamed deltas, so the live view
				// has nothing yet: emit the authoritative text. Parts that
				// streamed stay as-is; the committed turn takes the
				// authoritative text and the live view keeps its preview.
				if _, seen := acc.summary[payload.SummaryIndex]; !seen {
					emit("thinking_delta", payload.Text, payload.SummaryIndex)
				}
				delete(acc.summary, payload.SummaryIndex)
			} else {
				if acc.summary == nil {
					acc.summary = make(map[int]*strings.Builder)
				}
				if acc.summary[payload.SummaryIndex] == nil {
					acc.summary[payload.SummaryIndex] = &strings.Builder{}
					// An added item may already carry summary text that the
					// live view never saw; emit the seed before its deltas.
					if part.Text != "" {
						acc.summary[payload.SummaryIndex].WriteString(part.Text)
						emit("thinking_delta", part.Text, payload.SummaryIndex)
					}
				}
				acc.summary[payload.SummaryIndex].WriteString(payload.Delta)
				emit("thinking_delta", payload.Delta, payload.SummaryIndex)
			}
		case "response.output_item.added", "response.output_item.done":
			if payload.Item != nil {
				items[idx] = &streamedItem{item: *payload.Item}
			}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			acc := itemAt()
			acc.item.Type = "function_call"
			if payload.Type == "response.function_call_arguments.done" {
				acc.item.Arguments = payload.Arguments
				acc.args = nil
			} else {
				if acc.args == nil {
					acc.args = &strings.Builder{}
					acc.args.WriteString(acc.item.Arguments)
				}
				acc.args.WriteString(payload.Delta)
			}
		case "response.completed":
			completed = payload.Response
			if completed == nil {
				eventErr = errors.New("api: responses: response.completed missing response")
			}
			return false
		}
		return true
	})
	// scanSSE returns nil when the callback stops; keep its error separate.
	if eventErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, eventErr
	}
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, scanErr
	}
	if completed == nil {
		return openai.ChatCompletionMessage{}, usage, nil, errors.New("api: responses: stream ended without response.completed")
	}
	// Gateways sometimes send an empty final output. Only then use accumulated
	// items; a nonempty final output is authoritative, even after text deltas.
	if len(completed.Output) == 0 {
		order := make([]int, 0, len(items))
		for idx := range items {
			order = append(order, idx)
		}
		sort.Ints(order)
		for _, idx := range order {
			acc := items[idx]
			for i, text := range acc.text {
				acc.item.Content[i].Text = text.String()
			}
			for i, summary := range acc.summary {
				acc.item.Summary[i].Text = summary.String()
			}
			if acc.args != nil {
				acc.item.Arguments = acc.args.String()
			}
			completed.Output = append(completed.Output, acc.item)
		}
	}
	return fold(*completed)
}
