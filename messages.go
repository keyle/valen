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

const (
	messagesMaxTokens      = 8192
	messagesThinkingBudget = 4096
	anthropicVersion       = "2023-06-01"
)

// transportError carries an HTTP failure from a non-chat transport so
// friendlyError can map status codes the same way it maps OpenAI errors.
type transportError struct {
	Transport string
	Status    int
	Message   string
}

func (e *transportError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("api: %s HTTP %d: %s", e.Transport, e.Status, e.Message)
	}
	return fmt.Sprintf("api: %s: %s", e.Transport, e.Message)
}

func messagesEndpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/messages"
}

type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type messagesRequest struct {
	Model       string            `json:"model"`
	MaxTokens   int               `json:"max_tokens"`
	System      string            `json:"system,omitempty"`
	Messages    []messagesMessage `json:"messages"`
	Tools       []messagesTool    `json:"tools,omitempty"`
	ToolChoice  map[string]any    `json:"tool_choice,omitempty"`
	Temperature *float32          `json:"temperature,omitempty"`
	Thinking    map[string]any    `json:"thinking,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

type messagesResponse struct {
	ID         string          `json:"id"`
	Content    []messagesBlock `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// MessagesTools exposes the registry in Anthropic tool format.
func (r *Registry) MessagesTools() []messagesTool {
	out := make([]messagesTool, 0, len(r.names))
	for _, n := range r.names {
		t := r.tools[n]
		schema := t.Schema
		if len(bytes.TrimSpace(schema)) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, messagesTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}

// messagesThinking maps our toggle levels onto Anthropic thinking. Empty
// omits the field (provider default). "none" disables; "high" enables with a
// budget below max_tokens. Unknown levels omit rather than risk a 400.
func messagesThinking(level string) map[string]any {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return map[string]any{"type": "disabled"}
	case "high":
		return map[string]any{"type": "enabled", "budget_tokens": messagesThinkingBudget}
	default:
		return nil
	}
}

// splitChatHistory converts OpenAI-shaped history into an Anthropic system
// prompt plus user/assistant turns. Tool results become user tool_result
// blocks; assistant tool calls become tool_use blocks.
func splitChatHistory(history []openai.ChatCompletionMessage) (string, []messagesMessage) {
	var system []string
	msgs := make([]messagesMessage, 0, len(history))
	for _, m := range history {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
		case "user":
			msgs = append(msgs, messagesMessage{Role: "user", Content: m.Content})
		case "assistant":
			if len(m.ToolCalls) == 0 {
				msgs = append(msgs, messagesMessage{Role: "assistant", Content: m.Content})
				continue
			}
			blocks := make([]any, 0, len(m.ToolCalls)+1)
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
			}
			msgs = append(msgs, messagesMessage{Role: "assistant", Content: blocks})
		case "tool":
			msgs = append(msgs, messagesMessage{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content},
			}})
		}
	}
	return strings.Join(system, "\n\n"), msgs
}

func parseMessagesError(status int, body []byte) *transportError {
	var anth struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &anth); err == nil && anth.Error.Message != "" {
		return &transportError{Transport: "messages", Status: status, Message: anth.Error.Message}
	}
	var oa struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &oa); err == nil && oa.Error.Message != "" {
		return &transportError{Transport: "messages", Status: status, Message: oa.Error.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &transportError{Transport: "messages", Status: status, Message: msg}
}

func callMessagesOnce(ctx context.Context, hc *http.Client, cfg Config, apiKey string, req messagesRequest) (messagesResponse, error) {
	var out messagesResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesEndpoint(cfg.BaseURL), bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("x-api-key", apiKey)
	}
	if cfg.SessionID != "" {
		httpReq.Header.Set("x-opencode-session", cfg.SessionID)
	}
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
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
		return out, parseMessagesError(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("api: messages: decode: %w", err)
	}
	return out, nil
}

// messagesReplyToOpenAI folds Anthropic content blocks back into an
// OpenAI-shaped assistant message for storage. Thinking blocks are wrapped
// as <think> so the existing UI collapse keeps working; redacted thinking
// (opaque) is dropped.
func messagesReplyToOpenAI(resp messagesResponse) (openai.ChatCompletionMessage, error) {
	var text, thinking []string
	var calls []openai.ToolCall
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "thinking":
			if b.Thinking != "" {
				thinking = append(thinking, b.Thinking)
			} else if b.Text != "" {
				thinking = append(thinking, b.Text)
			}
		case "redacted_thinking":
			continue
		case "tool_use":
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			calls = append(calls, openai.ToolCall{
				ID:   b.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      b.Name,
					Arguments: args,
				},
			})
		}
	}
	content := strings.Join(text, "")
	if t := strings.TrimSpace(strings.Join(thinking, "\n")); t != "" {
		content = "<think>" + t + "</think>" + content
	}
	if strings.TrimSpace(content) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, errors.New("api: empty choices in messages response")
	}
	return openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   content,
		ToolCalls: calls,
	}, nil
}

// runMessagesTurn drives the tool loop against POST {base}/messages. History
// stays OpenAI-shaped (so the TUI/session layers are untouched); each round
// converts to Anthropic wire format and folds the reply back. Rounds stream
// SSE when the provider accepts it, else fall back to one unary call.
func runMessagesTurn(ctx context.Context, cfg Config, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	tools := registry.MessagesTools()
	thinking := messagesThinking(cfg.Thinking)
	for round := 0; round < cfg.MaxRounds; round++ {
		system, msgs := splitChatHistory(*history)
		req := messagesRequest{
			Model:     cfg.Model,
			MaxTokens: messagesMaxTokens,
			System:    system,
			Messages:  msgs,
			Thinking:  thinking,
		}
		if len(tools) > 0 {
			req.Tools = tools
			req.ToolChoice = map[string]any{"type": "auto"}
		}
		if cfg.Temperature != nil {
			req.Temperature = cfg.Temperature
		}

		sreq := req
		sreq.Stream = true
		msg, roundUsage, err := messagesRoundStream(ctx, cfg, apiKey, hc, sreq, onEvent)
		if err != nil {
			if !isStreamRejected(err) {
				return openai.ChatCompletionMessage{}, usage, err
			}
			resp, err := callMessagesOnce(ctx, hc, cfg, apiKey, req)
			if err != nil {
				return openai.ChatCompletionMessage{}, usage, err
			}
			usage.PromptTokens += resp.Usage.InputTokens
			usage.CompletionTokens += resp.Usage.OutputTokens
			usage.TotalTokens += resp.Usage.InputTokens + resp.Usage.OutputTokens

			msg, err = messagesReplyToOpenAI(resp)
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

type messagesBlockAcc struct {
	kind     string // "text", "thinking", "tool_use"
	id       string
	name     string
	text     strings.Builder
	thinking strings.Builder
	input    strings.Builder
}

// messagesRoundStream runs one round as Anthropic SSE, emitting text/thinking
// deltas live. A unary JSON body folds through messagesReplyToOpenAI.
func messagesRoundStream(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req messagesRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
		headers["x-api-key"] = apiKey
	}
	headers["anthropic-version"] = anthropicVersion
	resp, err := postAPI(ctx, hc, cfg, messagesEndpoint(cfg.BaseURL), req, headers)
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
		return openai.ChatCompletionMessage{}, usage, parseMessagesError(resp.StatusCode, raw)
	}
	sse, raw, err := splitBodyMode(br)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	if !sse {
		var out messagesResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openai.ChatCompletionMessage{}, usage, fmt.Errorf("api: messages: decode: %w", err)
		}
		msg, err := messagesReplyToOpenAI(out)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		usage.PromptTokens += out.Usage.InputTokens
		usage.CompletionTokens += out.Usage.OutputTokens
		usage.TotalTokens += out.Usage.InputTokens + out.Usage.OutputTokens
		return msg, usage, nil
	}

	emit := func(kind, text string) {
		if onEvent != nil && text != "" {
			onEvent(Event{Kind: kind, Text: text})
		}
	}
	accs := map[int]*messagesBlockAcc{}
	var order []int
	at := func(idx int) *messagesBlockAcc {
		a := accs[idx]
		if a == nil {
			a = &messagesBlockAcc{}
			accs[idx] = a
			order = append(order, idx)
		}
		return a
	}
	var scanErr error
	scanErr = scanSSE(ctx, br, func(ev sseEvent) bool {
		if strings.TrimSpace(ev.data) == "[DONE]" {
			return false
		}
		var payload struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message *struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Err *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			return true
		}
		switch payload.Type {
		case "error":
			if payload.Err != nil && payload.Err.Message != "" {
				scanErr = &transportError{Transport: "messages", Status: 0, Message: payload.Err.Message}
				return false
			}
			return true
		case "message_start":
			if payload.Message != nil && payload.Message.Usage != nil {
				usage.PromptTokens += payload.Message.Usage.InputTokens
				usage.TotalTokens += payload.Message.Usage.InputTokens
			}
		case "content_block_start":
			if payload.ContentBlock == nil {
				return true
			}
			a := at(payload.Index)
			a.kind = payload.ContentBlock.Type
			a.id = payload.ContentBlock.ID
			a.name = payload.ContentBlock.Name
		case "content_block_delta":
			if payload.Delta == nil {
				return true
			}
			a := at(payload.Index)
			switch payload.Delta.Type {
			case "text_delta":
				a.kind = "text"
				a.text.WriteString(payload.Delta.Text)
				emit("text_delta", payload.Delta.Text)
			case "thinking_delta":
				a.kind = "thinking"
				a.thinking.WriteString(payload.Delta.Thinking)
				emit("thinking_delta", payload.Delta.Thinking)
			case "input_json_delta":
				a.input.WriteString(payload.Delta.PartialJSON)
			}
		case "message_delta":
			if payload.Usage != nil {
				usage.CompletionTokens += payload.Usage.OutputTokens
				usage.TotalTokens += payload.Usage.OutputTokens
			}
		case "message_stop":
			return false
		}
		return true
	})
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, scanErr
	}
	var text, thinking []string
	var calls []openai.ToolCall
	for _, idx := range order {
		a := accs[idx]
		switch a.kind {
		case "text":
			text = append(text, a.text.String())
		case "thinking":
			thinking = append(thinking, a.thinking.String())
		case "tool_use":
			args := strings.TrimSpace(a.input.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			calls = append(calls, openai.ToolCall{
				ID:   a.id,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      a.name,
					Arguments: args,
				},
			})
		}
	}
	content := strings.Join(text, "")
	if t := strings.TrimSpace(strings.Join(thinking, "\n")); t != "" {
		content = "<think>" + t + "</think>" + content
	}
	if strings.TrimSpace(content) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: empty choices in messages response")
	}
	return openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   content,
		ToolCalls: calls,
	}, usage, nil
}
