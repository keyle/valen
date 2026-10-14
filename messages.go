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
	"regexp"
	"strconv"
	"strings"

	"github.com/sashabaranov/go-openai"
)

const (
	messagesThinkingBudget = 4096
	anthropicVersion       = "2023-06-01"
)

// messagesMaxTokens respects explicit output caps, including small ones.
func messagesMaxTokens(cfg Config) int {
	return outputTokens(cfg)
}

// transportError carries an HTTP failure from a non-chat transport so
// friendlyError can map status codes the same way it maps OpenAI errors.
// Raw keeps the capped provider body so ctrl+x details can show the full
// JSON, not just the extracted message.
type transportError struct {
	Transport string
	Status    int
	Message   string
	Raw       string
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
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl map[string]any  `json:"cache_control,omitempty"`
}

type messagesMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type messagesRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	System       any               `json:"system,omitempty"`
	Messages     []messagesMessage `json:"messages"`
	Tools        []messagesTool    `json:"tools,omitempty"`
	ToolChoice   map[string]any    `json:"tool_choice,omitempty"`
	Temperature  *float32          `json:"temperature,omitempty"`
	Thinking     map[string]any    `json:"thinking,omitempty"`
	OutputConfig map[string]any    `json:"output_config,omitempty"`
	Stream       bool              `json:"stream,omitempty"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	raw       map[string]json.RawMessage
}

// Keep native fields (including empty and provider-specific fields) intact
// when replaying signed thinking and its accompanying content blocks.
func (b *messagesBlock) UnmarshalJSON(data []byte) error {
	type block messagesBlock
	var decoded block
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &decoded.raw); err != nil {
		return err
	}
	*b = messagesBlock(decoded)
	return nil
}

func (b messagesBlock) MarshalJSON() ([]byte, error) {
	type block messagesBlock
	data, err := json.Marshal(block(b))
	if err != nil {
		return data, err
	}
	fields := make(map[string]json.RawMessage, len(b.raw))
	for key, value := range b.raw {
		fields[key] = value
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(data, &current); err != nil {
		return nil, err
	}
	for key, value := range current {
		fields[key] = value
	}
	// omitempty drops empty strings, but these fields are required by the API
	// on their block types; always send them as strings.
	switch b.Type {
	case "text":
		if _, ok := fields["text"]; !ok {
			fields["text"] = json.RawMessage(`""`)
		}
	case "thinking":
		if _, ok := fields["thinking"]; !ok {
			fields["thinking"] = json.RawMessage(`""`)
		}
	}
	return json.Marshal(fields)
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

// MessagesTools exposes the registry in Anthropic tool format. The last
// tool carries the single tools cache breakpoint (one breakpoint: caching
// every tool would bill a cache write per tool per round).
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
	if len(out) > 0 {
		out[len(out)-1].CacheControl = ephemeralCache()
	}
	return out
}

// ephemeralCache marks one Anthropic block as a prompt-cache breakpoint.
// Prefixes above the last breakpoint hit cache; the tail re-fills. Three
// breakpoints per request (system, last tool, last message block) mirror the
// documented pattern — stable prefixes (system+tools) are where the win is,
// since history tails move every round.
func ephemeralCache() map[string]any {
	return map[string]any{"type": "ephemeral"}
}

// messagesSystemParam renders the system prompt as a single cached text
// block; empty omits the field (provider default).
func messagesSystemParam(system string) any {
	if strings.TrimSpace(system) == "" {
		return nil
	}
	return []any{map[string]any{"type": "text", "text": system, "cache_control": ephemeralCache()}}
}

// messagesThinking maps our levels onto Anthropic thinking. Empty omits the
// field (provider default). "none" disables; any other known effort level
// enables with a budget scaled low to high, staying below max_tokens.
// Unknown levels omit rather than risk a 400.
func messagesThinking(level string) map[string]any {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return map[string]any{"type": "disabled"}
	case "minimal":
		return map[string]any{"type": "enabled", "budget_tokens": 1024}
	case "low":
		return map[string]any{"type": "enabled", "budget_tokens": 2048}
	case "medium":
		return map[string]any{"type": "enabled", "budget_tokens": 3072}
	case "high":
		return map[string]any{"type": "enabled", "budget_tokens": messagesThinkingBudget}
	case "xhigh", "max":
		return map[string]any{"type": "enabled", "budget_tokens": 6144}
	default:
		return nil
	}
}

var claudeVersionRe = regexp.MustCompile(`^claude-(?:opus|sonnet|haiku)-(\d+)(?:-(\d{1,2}))?(?:$|-)`)

// messagesAdaptiveModel reports whether a Claude model (wire ID) rejects
// thinking.type=enabled in favour of adaptive thinking plus
// output_config.effort: Claude 4.6 and newer.
func messagesAdaptiveModel(model string) bool {
	m := claudeVersionRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major > 4 || (major == 4 && minor >= 6)
}

// messagesAdaptiveThinking is the adaptive-model counterpart of
// messagesThinking: it returns the thinking param and output_config. Empty or
// unknown levels omit both; "none" disables thinking.
func messagesAdaptiveThinking(level string) (map[string]any, map[string]any) {
	switch l := strings.ToLower(strings.TrimSpace(level)); l {
	case "none":
		return map[string]any{"type": "disabled"}, nil
	case "minimal", "low":
		return map[string]any{"type": "adaptive"}, map[string]any{"effort": "low"}
	case "medium", "high", "xhigh", "max":
		return map[string]any{"type": "adaptive"}, map[string]any{"effort": l}
	default:
		return nil, nil
	}
}

// isAdaptiveThinkingRequired detects the provider 400 telling us to use
// adaptive thinking, so unknown model names still self-correct.
func isAdaptiveThinkingRequired(err error) bool {
	var te *transportError
	if !errors.As(err, &te) || te.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(te.Message), "thinking.type.adaptive")
}

// splitChatHistory converts OpenAI-shaped history into an Anthropic system
// prompt plus user/assistant turns. Tool results become user tool_result
// blocks; assistant tool calls become tool_use blocks.
func splitChatHistory(history []openai.ChatCompletionMessage) (string, []messagesMessage) {
	return splitMessagesHistory(history)
}

func splitMessagesHistory(history []openai.ChatCompletionMessage) (string, []messagesMessage) {
	return splitMessagesHistoryNative(history, nil)
}

// Native blocks are scoped to the active turn and keyed by its first tool
// call ID. Settled history falls back to ordinary text without UI thinking.
func splitMessagesHistoryNative(history []openai.ChatCompletionMessage, native map[string][]messagesBlock) (string, []messagesMessage) {
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
			if len(m.ToolCalls) > 0 {
				if blocks, ok := native[m.ToolCalls[0].ID]; ok {
					// Empty text blocks (e.g. a streamed text block with no
					// deltas) are rejected upstream; drop them on replay.
					kept := make([]messagesBlock, 0, len(blocks))
					for _, b := range blocks {
						if b.Type == "text" && b.Text == "" {
							continue
						}
						kept = append(kept, b)
					}
					msgs = append(msgs, messagesMessage{Role: "assistant", Content: kept})
					continue
				}
			}
			_, content := splitThinking(m.Content)
			if len(m.ToolCalls) == 0 {
				if content != "" {
					msgs = append(msgs, messagesMessage{Role: "assistant", Content: content})
				}
				continue
			}
			blocks := make([]any, 0, len(m.ToolCalls)+1)
			if content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": content})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": json.RawMessage(args)})
			}
			msgs = append(msgs, messagesMessage{Role: "assistant", Content: blocks})
		case "tool":
			msgs = append(msgs, messagesMessage{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": fitSendBudget(m.Content)},
			}})
		}
	}
	stampLastBlockCache(msgs)
	return strings.Join(system, "\n\n"), msgs
}

// stampLastBlockCache puts the trailing prompt-cache breakpoint on the last
// message's final content block, so everything above it (system, tools,
// settled history) is prefix-cacheable. String content becomes one cached
// text block; block slices gain cache_control on their last map.
// Native block slices stay untouched for exact continuation replay.
func stampLastBlockCache(msgs []messagesMessage) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	switch c := last.Content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return
		}
		last.Content = []any{map[string]any{"type": "text", "text": c, "cache_control": ephemeralCache()}}
	case []any:
		if len(c) == 0 {
			return
		}
		if blk, ok := c[len(c)-1].(map[string]any); ok {
			blk["cache_control"] = ephemeralCache()
		}
	}
}

func parseMessagesError(status int, body []byte) *transportError {
	raw := capRawBody(body)
	var anth struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &anth); err == nil && anth.Error.Message != "" {
		return &transportError{Transport: "messages", Status: status, Message: anth.Error.Message, Raw: rawDiff(raw, anth.Error.Message)}
	}
	var oa struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &oa); err == nil && oa.Error.Message != "" {
		return &transportError{Transport: "messages", Status: status, Message: oa.Error.Message, Raw: rawDiff(raw, oa.Error.Message)}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &transportError{Transport: "messages", Status: status, Message: msg, Raw: rawDiff(raw, msg)}
}

func callMessagesOnce(ctx context.Context, hc *http.Client, cfg Config, apiKey string, req messagesRequest) (messagesResponse, error) {
	var out messagesResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesEndpoint(providerBaseURL(cfg)), bytes.NewReader(body))
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
	if resp.StopReason == "max_tokens" {
		return openai.ChatCompletionMessage{}, errors.New("api: messages: incomplete response (max_tokens)")
	}
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
			if strings.TrimSpace(b.ID) == "" || strings.TrimSpace(b.Name) == "" {
				return openai.ChatCompletionMessage{}, errors.New("api: messages: incomplete tool call (missing id or name)")
			}
			if !json.Valid([]byte(args)) || !strings.HasPrefix(args, "{") {
				return openai.ChatCompletionMessage{}, fmt.Errorf("api: messages: invalid tool arguments for %s", b.ID)
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
	_, answer := splitThinking(content)
	if strings.TrimSpace(answer) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, errors.New("api: empty choices in messages response")
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

// runMessagesTurn drives the tool loop against POST {base}/messages. History
// stays OpenAI-shaped (so the TUI/session layers are untouched); each round
// converts to Anthropic wire format and folds the reply back. Rounds stream
// SSE when the provider accepts it, else fall back to one unary call.
func runMessagesTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	tools := registry.MessagesTools()
	adaptive := messagesAdaptiveModel(wireModelID(cfg.Model))
	native := make(map[string][]messagesBlock)
	stream := true
	return runModelTurn(ctx, cfg, registry, history, onEvent, func(ctx context.Context, history []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		system, msgs := splitMessagesHistoryNative(history, native)
		req := messagesRequest{
			Model:     wireModelID(cfg.Model, cfg.Routing),
			MaxTokens: messagesMaxTokens(cfg),
			System:    messagesSystemParam(system),
			Messages:  msgs,
			Stream:    stream,
		}
		if adaptive {
			req.Thinking, req.OutputConfig = messagesAdaptiveThinking(cfg.Thinking)
		} else {
			req.Thinking = messagesThinking(cfg.Thinking)
		}
		if len(tools) > 0 {
			req.Tools = tools
			req.ToolChoice = map[string]any{"type": "auto"}
		}
		if cfg.Temperature != nil {
			req.Temperature = cfg.Temperature
		}
		msg, usage, blocks, err := messagesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
		if !adaptive && isAdaptiveThinkingRequired(err) {
			adaptive = true
			req.Thinking, req.OutputConfig = messagesAdaptiveThinking(cfg.Thinking)
			msg, usage, blocks, err = messagesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
		}
		if stream && isStreamRejected(err) {
			stream, req.Stream = false, false
			msg, usage, blocks, err = messagesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
		}
		if err == nil && len(msg.ToolCalls) > 0 {
			native[msg.ToolCalls[0].ID] = blocks
		}
		return msg, usage, err
	})
}

type messagesBlockAcc struct {
	block      messagesBlock
	stopped    bool
	inputDelta bool
	text       strings.Builder
	thinking   strings.Builder
	signature  strings.Builder
	input      strings.Builder
}

func messagesRoundStream(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req messagesRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	msg, usage, _, err := messagesRoundStreamDetailed(ctx, cfg, apiKey, hc, req, onEvent)
	return msg, usage, err
}

// messagesRoundStreamDetailed also returns native content for active tool
// continuation. Unary responses use Content directly, without reconstruction.
func messagesRoundStreamDetailed(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req messagesRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, []messagesBlock, error) {
	var usage openai.Usage
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
		headers["x-api-key"] = apiKey
	}
	headers["anthropic-version"] = anthropicVersion
	resp, err := postAPI(ctx, hc, cfg, messagesEndpoint(providerBaseURL(cfg)), req, headers)
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
		return openai.ChatCompletionMessage{}, usage, nil, parseMessagesError(resp.StatusCode, raw)
	}
	sse, raw, err := splitBodyMode(br)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	if !sse {
		var out messagesResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openai.ChatCompletionMessage{}, usage, nil, fmt.Errorf("api: messages: decode: %w", err)
		}
		usage.PromptTokens += out.Usage.InputTokens
		usage.CompletionTokens += out.Usage.OutputTokens
		usage.TotalTokens += out.Usage.InputTokens + out.Usage.OutputTokens
		msg, err := messagesReplyToOpenAI(out)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, nil, err
		}
		return msg, usage, out.Content, nil
	}

	emit := func(kind, text string, idx int) {
		if onEvent != nil && text != "" {
			onEvent(Event{Kind: kind, Text: text, Index: idx})
		}
	}
	accs := map[int]*messagesBlockAcc{}
	var order []int
	var eventErr error
	var stopReason string
	var terminal bool
	scanErr := scanSSE(ctx, br, func(ev sseEvent) bool {
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
			ContentBlock *messagesBlock `json:"content_block"`
			Delta        *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Err *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			eventErr = fmt.Errorf("api: messages: decode stream event: %w", err)
			return false
		}
		switch payload.Type {
		case "error":
			message := "provider stream error"
			if payload.Err != nil {
				if payload.Err.Message != "" {
					message = payload.Err.Message
				} else if payload.Err.Type != "" {
					message = payload.Err.Type
				}
			}
			eventErr = &transportError{Transport: "messages", Message: message}
			return false
		case "message_start":
			if payload.Message != nil && payload.Message.Usage != nil {
				usage.PromptTokens = payload.Message.Usage.InputTokens
				usage.CompletionTokens = payload.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if payload.ContentBlock == nil {
				eventErr = errors.New("api: messages: missing content block")
				return false
			}
			if accs[payload.Index] != nil {
				eventErr = errors.New("api: messages: duplicate content block")
				return false
			}
			a := &messagesBlockAcc{block: *payload.ContentBlock}
			a.text.WriteString(a.block.Text)
			a.thinking.WriteString(a.block.Thinking)
			a.signature.WriteString(a.block.Signature)
			accs[payload.Index] = a
			order = append(order, payload.Index)
			if a.block.Type == "text" {
				emit("text_delta", a.block.Text, 0)
			} else if a.block.Type == "thinking" {
				emit("thinking_delta", a.block.Thinking, payload.Index)
			}
		case "content_block_delta":
			if payload.Delta == nil {
				eventErr = errors.New("api: messages: missing content block delta")
				return false
			}
			a := accs[payload.Index]
			if a == nil || a.stopped {
				eventErr = errors.New("api: messages: delta outside an active content block")
				return false
			}
			switch payload.Delta.Type {
			case "text_delta":
				if a.block.Type != "text" {
					eventErr = errors.New("api: messages: text delta in non-text block")
					return false
				}
				a.text.WriteString(payload.Delta.Text)
				emit("text_delta", payload.Delta.Text, 0)
			case "thinking_delta":
				if a.block.Type != "thinking" {
					eventErr = errors.New("api: messages: thinking delta in non-thinking block")
					return false
				}
				a.thinking.WriteString(payload.Delta.Thinking)
				emit("thinking_delta", payload.Delta.Thinking, payload.Index)
			case "signature_delta":
				if a.block.Type != "thinking" {
					eventErr = errors.New("api: messages: signature delta in non-thinking block")
					return false
				}
				a.signature.WriteString(payload.Delta.Signature)
			case "input_json_delta":
				if a.block.Type != "tool_use" {
					eventErr = errors.New("api: messages: input delta in non-tool block")
					return false
				}
				// The initial tool input is a placeholder; deltas carry the
				// complete replacement JSON, not a suffix of that object.
				a.inputDelta = true
				a.input.WriteString(payload.Delta.PartialJSON)
			}
		case "content_block_stop":
			a := accs[payload.Index]
			if a == nil || a.stopped {
				eventErr = errors.New("api: messages: stop outside an active content block")
				return false
			}
			a.stopped = true
		case "message_delta":
			if payload.Delta != nil && payload.Delta.StopReason != "" {
				stopReason = payload.Delta.StopReason
			}
			if payload.Usage != nil {
				usage.CompletionTokens = payload.Usage.OutputTokens
			}
		case "message_stop":
			terminal = true
			return false
		}
		return true
	})
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if eventErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, eventErr
	}
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, scanErr
	}
	if !terminal {
		return openai.ChatCompletionMessage{}, usage, nil, errors.New("api: messages: incomplete stream (missing message_stop)")
	}
	native := make([]messagesBlock, 0, len(order))
	for _, idx := range order {
		a := accs[idx]
		if !a.stopped {
			return openai.ChatCompletionMessage{}, usage, nil, fmt.Errorf("api: messages: unfinished content block %d", idx)
		}
		a.block.Text = a.text.String()
		a.block.Thinking = a.thinking.String()
		a.block.Signature = a.signature.String()
		if a.inputDelta {
			a.block.Input = json.RawMessage(a.input.String())
		}
		native = append(native, a.block)
	}
	msg, err := messagesReplyToOpenAI(messagesResponse{Content: native, StopReason: stopReason})
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	return msg, usage, native, nil
}
