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
	"os"
	"strings"
	"time"

	"github.com/keyle/valen/internal/dvlog"
	"github.com/sashabaranov/go-openai"
)

func chatEndpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

// openRouterHTTPClient is the sessionless client for OpenRouter traffic:
// the shared keep-alive transport without the Go identity headers.
var openRouterHTTPClient = &http.Client{Transport: sharedTransport}

// chatTarget resolves where a chat turn is sent. Go models ride cfg.BaseURL
// with the Go key and session affinity; OpenRouter models ride OpenRouter's
// OpenAI-compatible chat endpoint with the OpenRouter key and no Go session
// header. It returns the endpoint URL, headers, send config and client.
func chatTarget(cfg Config, apiKey string, hc *http.Client) (string, map[string]string, Config, *http.Client) {
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		sendCfg := cfg
		sendCfg.SessionID = ""
		return openRouterBaseURL + "/chat/completions", openRouterHeaders(cfg.OpenRouterKey), sendCfg, openRouterHTTPClient
	}
	return chatEndpoint(providerBaseURL(cfg)), chatHeaders(apiKey), cfg, hc
}

// chatTransportLabel names the failing side for error mapping: OpenRouter
// failures must not be reported as Go ("chat") ones, or the wrong env var
// gets blamed.
func chatTransportLabel(cfg Config) string {
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		return "openrouter"
	}
	return "chat"
}

func chatHeaders(apiKey string) map[string]string {
	if apiKey == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + apiKey}
}

// OpenRouter names reasoning `reasoning` (not DeepSeek's `reasoning_content`)
// both ways. go-openai only knows the latter, so without these helpers the
// reasoning is dropped and thinking-mode upstreams 400 on the next round
// ("reasoning_content must be passed back").
func altStreamReasoning(data string, index int) string {
	if !strings.Contains(data, `"reasoning"`) {
		return ""
	}
	var c struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Reasoning string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return ""
	}
	for _, ch := range c.Choices {
		if ch.Index == index {
			return ch.Delta.Reasoning
		}
	}
	return ""
}

// fillReasoning recovers `reasoning` from a unary body when reasoning_content
// was absent.
func fillReasoning(msg *openai.ChatCompletionMessage, raw []byte) {
	if msg.ReasoningContent != "" || !bytes.Contains(raw, []byte(`"reasoning"`)) {
		return
	}
	var c struct {
		Choices []struct {
			Message struct {
				Reasoning string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &c) == nil && len(c.Choices) > 0 {
		msg.ReasoningContent = c.Choices[0].Message.Reasoning
	}
}

// chatWireBody patches reasoning on the serialized request. OpenRouter gets
// each assistant reasoning_content mirrored as `reasoning` (its documented
// field). DeepSeek thinking mode 400s unless every assistant tool-call
// message carries reasoning_content, and go-openai omits empty strings, so
// those get an explicit "". Everything else is sent untouched.
func chatWireBody(cfg Config, req openai.ChatCompletionRequest) any {
	mirror := modelProvider(cfg.Model) == ProviderOpenRouter
	pad := strings.HasPrefix(modelBaseID(cfg.Model), "deepseek") && strings.ToLower(strings.TrimSpace(cfg.Thinking)) != "none"
	need := false
	for _, m := range req.Messages {
		if m.Role == openai.ChatMessageRoleAssistant && ((mirror && m.ReasoningContent != "") || (pad && len(m.ToolCalls) > 0 && m.ReasoningContent == "")) {
			need = true
			break
		}
	}
	if !need {
		return req
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return req
	}
	var body map[string]json.RawMessage
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || json.Unmarshal(body["messages"], &msgs) != nil {
		return req
	}
	for _, m := range msgs {
		if string(m["role"]) != `"assistant"` {
			continue
		}
		rc, ok := m["reasoning_content"]
		if mirror && ok {
			m["reasoning"] = rc
		}
		if pad && !ok && len(m["tool_calls"]) > 0 {
			m["reasoning_content"] = json.RawMessage(`""`)
		}
	}
	body["messages"], _ = json.Marshal(msgs)
	return body
}

// chatRoundOnce is the non-streaming round, kept as the fallback when a
// provider rejects `stream: true`.
func chatRoundOnce(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	endpoint, headers, sendCfg, httpClient := chatTarget(cfg, apiKey, hc)
	resp, err := postAPI(ctx, httpClient, sendCfg, endpoint, chatWireBody(cfg, req), headers)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	if resp.StatusCode != http.StatusOK {
		return openai.ChatCompletionMessage{}, usage, parseTransportError(chatTransportLabel(cfg), resp.StatusCode, raw)
	}
	var out openai.ChatCompletionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return openai.ChatCompletionMessage{}, usage, fmt.Errorf("api: chat: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: empty choices in response")
	}
	msg := out.Choices[0].Message
	if err := chatFinishError(string(out.Choices[0].FinishReason)); err != nil {
		return openai.ChatCompletionMessage{}, out.Usage, err
	}
	fillReasoning(&msg, raw)
	return msg, out.Usage, validateModelReply(msg)
}

// errContentFilter marks a round the provider cut off with a content filter.
var errContentFilter = errors.New("content filter")

func chatFinishError(reason string) error {
	switch reason {
	case "", "stop", "tool_calls", "function_call":
		return nil
	case "content_filter":
		return fmt.Errorf("api: chat: incomplete response (%s): %w", reason, errContentFilter)
	default:
		return fmt.Errorf("api: chat: incomplete response (%s)", reason)
	}
}

type chatToolAcc struct {
	id   string
	name string
	args strings.Builder
}

// chatRoundStream runs one round as SSE, emitting text/thinking deltas live.
// A unary JSON body (provider ignored streaming) folds through the same
// validation as chatRoundOnce without emitting deltas.
func chatRoundStream(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	req.Stream = true
	req.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	endpoint, headers, sendCfg, httpClient := chatTarget(cfg, apiKey, hc)
	start := time.Now()
	resp, err := postAPI(ctx, httpClient, sendCfg, endpoint, chatWireBody(cfg, req), headers)
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
		return openai.ChatCompletionMessage{}, usage, parseTransportError(chatTransportLabel(cfg), resp.StatusCode, raw)
	}
	sse, raw, err := splitBodyMode(br)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	emit := func(kind, text string) {
		if onEvent != nil && text != "" {
			onEvent(Event{Kind: kind, Text: text})
		}
	}
	if !sse {
		var out openai.ChatCompletionResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openai.ChatCompletionMessage{}, usage, fmt.Errorf("api: chat: decode: %w", err)
		}
		if len(out.Choices) == 0 {
			return openai.ChatCompletionMessage{}, usage, errors.New("api: empty choices in response")
		}
		if err := chatFinishError(string(out.Choices[0].FinishReason)); err != nil {
			return openai.ChatCompletionMessage{}, out.Usage, err
		}
		msg := out.Choices[0].Message
		fillReasoning(&msg, raw)
		if modelProvider(cfg.Model) == ProviderOpenRouter {
			var meta ORMeta
			parseOpenRouterMeta(raw, &meta.Provider, &meta.Cost, &meta.HasCost)
			if onEvent != nil && (meta.Provider != "" || meta.HasCost) {
				onEvent(Event{Kind: "or_meta", ORMeta: &meta})
			}
		}
		return msg, out.Usage, validateModelReply(msg)
	}

	var content, thinking strings.Builder
	accs := map[int]*chatToolAcc{}
	var order []int
	var eventErr error
	var terminal bool
	orMeta := ORMeta{}
	var firstDelta, lastDelta time.Time
	markDelta := func() {
		now := time.Now()
		if firstDelta.IsZero() {
			firstDelta = now
		}
		lastDelta = now
	}
	scanErr := scanSSE(ctx, br, func(ev sseEvent) bool {
		if strings.TrimSpace(ev.data) == "[DONE]" {
			terminal = true
			return false
		}
		var chunk struct {
			openai.ChatCompletionStreamResponse
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
			eventErr = fmt.Errorf("api: chat: decode stream event: %w", err)
			return false
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			eventErr = parseTransportError(chatTransportLabel(cfg), 0, []byte(ev.data))
			return false
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if modelProvider(cfg.Model) == ProviderOpenRouter {
			parseOpenRouterMeta([]byte(ev.data), &orMeta.Provider, &orMeta.Cost, &orMeta.HasCost)
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if err := chatFinishError(string(choice.FinishReason)); err != nil {
				eventErr = err
				return false
			}
			if choice.FinishReason != "" {
				terminal = true
			}
			d := choice.Delta
			altReasoning := altStreamReasoning(ev.data, choice.Index)
			if d.Content != "" {
				markDelta()
				content.WriteString(d.Content)
				emit("text_delta", d.Content)
			}
			if r := d.ReasoningContent; r != "" || altReasoning != "" {
				if r == "" {
					r = altReasoning
				}
				markDelta()
				thinking.WriteString(r)
				emit("thinking_delta", r)
			}
			for _, tc := range d.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				if idx < 0 || (tc.Type != "" && tc.Type != openai.ToolTypeFunction) {
					eventErr = errors.New("api: chat: invalid streamed tool call")
					return false
				}
				markDelta()
				a := accs[idx]
				if a == nil {
					a = &chatToolAcc{}
					accs[idx] = a
					order = append(order, idx)
				}
				if tc.ID != "" {
					a.id = tc.ID
				}
				if tc.Function.Name != "" {
					a.name = tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
		return true
	})
	if eventErr != nil {
		return openai.ChatCompletionMessage{}, usage, eventErr
	}
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, scanErr
	}
	if !terminal {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: chat: incomplete stream (missing [DONE] or finish_reason)")
	}
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		if !firstDelta.IsZero() {
			orMeta.TTFT = firstDelta.Sub(start).Seconds()
			if lastDelta.After(firstDelta) && usage.CompletionTokens > 0 {
				orMeta.TPS = float64(usage.CompletionTokens) / lastDelta.Sub(firstDelta).Seconds()
			}
		}
		if onEvent != nil && (orMeta.Provider != "" || orMeta.HasCost || orMeta.TTFT > 0 || orMeta.TPS > 0) {
			m := orMeta
			onEvent(Event{Kind: "or_meta", ORMeta: &m})
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
	msg := openai.ChatCompletionMessage{
		Role:             openai.ChatMessageRoleAssistant,
		Content:          content.String(),
		ReasoningContent: thinking.String(),
		ToolCalls:        calls,
	}
	return msg, usage, validateModelReply(msg)
}

// chat is the headless one-shot path: one prompt, one answer, stdout.
func chat(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, prompt string, stdout, stderr io.Writer) error {
	resetTodos()
	history := make([]openai.ChatCompletionMessage, 0, 8)
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)})
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})
	dvlog.Logf("start model=%s transport=%s thinking=%s maxrounds=%d budget~%dtok schemastate~%dtok tools=%d headless prompt=%dB %q",
		cfg.Model, modelTransport(cfg.Model), cfg.Thinking, cfg.MaxRounds, contextBudget(cfg), requestToolsTokens(registry), len(registry.Names()), len(prompt), dvlog.Preview(prompt, 200))

	onEvent := func(ev Event) {
		if ev.Kind == "tool_start" {
			fmt.Fprintf(stderr, "[tool %s]\n", ev.Text)
		}
	}
	askPrompter = func(ctx context.Context, qs []AskQuestion) ([]string, error) {
		in, closeIn, err := headlessAskInput()
		if err != nil {
			return nil, err
		}
		defer closeIn()
		return headlessAsk(ctx, qs, in, os.Stderr)
	}
	reply, usage, err := runTurn(ctx, cfg, client, apiKey, hc, registry, &history, onEvent)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, reply.Content)
	fmt.Fprintf(stderr, "[tokens in=%d out=%d]\n", usage.PromptTokens, usage.CompletionTokens)
	return nil
}
