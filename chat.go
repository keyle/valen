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
	"github.com/keyle/valen/internal/prompts"
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
	switch modelProvider(cfg.Model) {
	case ProviderOpenRouter:
		sendCfg := cfg
		sendCfg.SessionID = ""
		return openRouterBaseURL + "/chat/completions", openRouterHeaders(cfg.OpenRouterKey), sendCfg, openRouterHTTPClient
	case ProviderOpenAI:
		sendCfg := cfg
		sendCfg.SessionID = ""
		return chatEndpoint(openAIBaseURL), chatHeaders(cfg.OpenAIKey), sendCfg, upstreamHTTPClient
	}
	return chatEndpoint(providerBaseURL(cfg)), chatHeaders(apiKey), cfg, hc
}

// chatTransportLabel names the failing side for error mapping: OpenRouter
// failures must not be reported as Go ("chat") ones, or the wrong env var
// gets blamed.
func chatTransportLabel(cfg Config) string {
	switch modelProvider(cfg.Model) {
	case ProviderOpenRouter:
		return "openrouter"
	case ProviderOpenAI:
		return "openai"
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

// orProvider is the OpenRouter `provider` routing object valen sends: only
// the fields we model. ZDR and DataCollection are always present because
// valen's defaults are privacy-first (zero retention, no data collection).
type orProvider struct {
	Ignore         []string `json:"ignore,omitempty"`
	Only           []string `json:"only,omitempty"`
	ZDR            bool     `json:"zdr"`
	DataCollection string   `json:"data_collection"`
}

// openRouterProvider builds the `provider` object for cfg, nil on Go/Zen (the
// object is OpenRouter-only). provider_only wins; otherwise provider_ignore,
// defaulting to [baseten] when unset. An explicit empty ignore disables it.
// The privacy filters follow zdr/data_collection, with a session-wide
// relaxation applied when the user allowed one after a refusal.
func openRouterProvider(cfg Config) *orProvider {
	if modelProvider(cfg.Model) != ProviderOpenRouter {
		return nil
	}
	relaxed := orPrivacyRelaxed()
	p := orProvider{ZDR: zdrEnabled(cfg) && !relaxed, DataCollection: "deny"}
	if dataCollectionAllowed(cfg) || relaxed {
		p.DataCollection = "allow"
	}
	p.Only = cfg.ProviderOnly
	if len(p.Only) == 0 {
		p.Ignore = cfg.ProviderIgnore
		if p.Ignore == nil {
			p.Ignore = defaultProviderIgnore
		}
	}
	return &p
}

// chatNative is provider state go-openai does not model: OpenRouter's
// reasoning_details array and each tool call's extra_content (Gemini 3 thought
// signatures). It is captured per tool call and replayed on the next request of
// the same active tool loop only; ordinary history keeps the text form.
type chatNative struct {
	ReasoningDetails json.RawMessage
	ExtraContent     json.RawMessage
}

func cleanRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// chatWireBody patches the serialized request. OpenRouter gets the provider
// routing object (ignore/only) plus each assistant reasoning_content mirrored
// as `reasoning` (its documented field). DeepSeek thinking mode 400s unless
// every assistant tool-call message carries reasoning_content, and go-openai
// omits empty strings, so those get an explicit "". Captured native state
// (reasoning_details, tool-call extra_content) is reattached by call ID.
// extra_body keys are merged first, so valen's own fields win. Everything else
// is sent untouched.
func chatWireBody(cfg Config, req openai.ChatCompletionRequest, native map[string]chatNative) any {
	mirror := modelProvider(cfg.Model) == ProviderOpenRouter
	pad := strings.HasPrefix(modelBaseID(cfg.Model), "deepseek") && strings.ToLower(strings.TrimSpace(cfg.Thinking)) != "none"
	provider := openRouterProvider(cfg)
	need := provider != nil || len(cfg.ExtraBody) > 0 || len(native) > 0
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
	if json.Unmarshal(raw, &body) != nil {
		return req
	}
	for k, v := range cfg.ExtraBody {
		if enc, err := json.Marshal(v); err == nil {
			body[k] = enc
		}
	}
	if provider != nil {
		if pb, err := json.Marshal(provider); err == nil {
			body["provider"] = pb
		}
	}
	if mirror || pad || len(native) > 0 {
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(body["messages"], &msgs) == nil {
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
				if len(native) == 0 {
					continue
				}
				var calls []map[string]json.RawMessage
				if json.Unmarshal(m["tool_calls"], &calls) != nil {
					continue
				}
				var details json.RawMessage
				patched := false
				for _, call := range calls {
					n, ok := native[jsonString(call["id"])]
					if !ok {
						continue
					}
					if details == nil {
						details = n.ReasoningDetails
					}
					if len(n.ExtraContent) > 0 {
						call["extra_content"] = n.ExtraContent
						patched = true
					}
				}
				if details != nil {
					m["reasoning_details"] = details
				}
				if patched {
					m["tool_calls"], _ = json.Marshal(calls)
				}
			}
			body["messages"], _ = json.Marshal(msgs)
		}
	}
	return body
}

// chatRoundOnce is the non-streaming round, kept as the fallback when a
// provider rejects `stream: true`.
func chatRoundOnce(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest) (openai.ChatCompletionMessage, openai.Usage, error) {
	msg, usage, _, err := chatRoundOnceDetailed(ctx, cfg, apiKey, hc, req, nil)
	return msg, usage, err
}

func chatRoundOnceDetailed(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest, native map[string]chatNative) (openai.ChatCompletionMessage, openai.Usage, map[string]chatNative, error) {
	var usage openai.Usage
	endpoint, headers, sendCfg, httpClient := chatTarget(cfg, apiKey, hc)
	resp, err := postAPI(ctx, httpClient, sendCfg, endpoint, chatWireBody(cfg, req, native), headers)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return openai.ChatCompletionMessage{}, usage, nil, parseTransportError(chatTransportLabel(cfg), resp.StatusCode, raw)
	}
	if te := chatBodyError(chatTransportLabel(cfg), raw); te != nil {
		return openai.ChatCompletionMessage{}, usage, nil, te
	}
	var out openai.ChatCompletionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, fmt.Errorf("api: chat: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return openai.ChatCompletionMessage{}, usage, nil, errors.New("api: empty choices in response")
	}
	msg := out.Choices[0].Message
	if err := chatFinishError(string(out.Choices[0].FinishReason), chatNativeFinish(raw, 0), ""); err != nil {
		return openai.ChatCompletionMessage{}, out.Usage, nil, err
	}
	fillReasoning(&msg, raw)
	return msg, out.Usage, chatNativeFromBody(raw), validateModelReply(msg)
}

// errContentFilter marks a round the provider cut off with a content filter.
var errContentFilter = errors.New("content filter")

// errOutputLimit marks a round cut off at the output cap (max_tokens, which
// reasoning also spends). Resending the same request hits the same cap.
var errOutputLimit = errors.New("output limit")

// chatStopError is a provider stop that was not a normal completion. Reason is
// the normalized finish_reason; Native is the provider's precise reason
// (OpenRouter's native_finish_reason, e.g. MALFORMED_FUNCTION_CALL); Detail is
// the provider message when the stop carried one. Cause carries the output-limit
// and content-filter sentinels so errors.Is keeps working everywhere.
type chatStopError struct {
	Reason    string
	Native    string
	Detail    string
	Raw       string
	Transport string
	Cause     error
}

func (e *chatStopError) Error() string {
	label := e.Reason
	if label == "" {
		label = "error"
	}
	if e.Native != "" && !strings.EqualFold(e.Native, label) {
		label += "/" + e.Native
	}
	msg := "api: chat: incomplete response (" + label + ")"
	if d := singleLine(e.Detail, 300); d != "" {
		msg += ": " + d
	}
	return msg
}

func (e *chatStopError) Unwrap() error { return e.Cause }

func chatFinishError(reason, native, detail string) error {
	switch reason {
	case "", "stop", "tool_calls", "function_call":
		return nil
	case "length":
		return &chatStopError{Reason: reason, Native: native, Detail: detail, Cause: errOutputLimit}
	case "content_filter":
		return &chatStopError{Reason: reason, Native: native, Detail: detail, Cause: errContentFilter}
	default:
		return &chatStopError{Reason: reason, Native: native, Detail: detail}
	}
}

// chatNativeFinish extracts native_finish_reason for one choice. OpenRouter
// sends it alongside finish_reason, including on the usage chunk that repeats
// the stop; providers without it leave it empty.
func chatNativeFinish(raw []byte, index int) string {
	if !bytes.Contains(raw, []byte(`"native_finish_reason"`)) {
		return ""
	}
	var c struct {
		Choices []struct {
			Index              int    `json:"index"`
			NativeFinishReason string `json:"native_finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	for _, ch := range c.Choices {
		if ch.Index == index {
			return ch.NativeFinishReason
		}
	}
	return ""
}

// chatBodyError returns the provider error carried inside a 200 body, if any.
// A 200 with an error object is not an empty-choices bug; it is the provider's
// own failure and its message is the actionable part.
func chatBodyError(transport string, raw []byte) *transportError {
	if !bytes.Contains(raw, []byte(`"error"`)) {
		return nil
	}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || cleanRaw(env.Error) == nil {
		return nil
	}
	return parseTransportError(transport, 0, raw)
}

// chatNativeFromBody extracts the native tool state from a unary chat body,
// keyed by tool call ID. A round without tool calls has nothing to replay.
func chatNativeFromBody(raw []byte) map[string]chatNative {
	if !bytes.Contains(raw, []byte(`"reasoning_details"`)) && !bytes.Contains(raw, []byte(`"extra_content"`)) {
		return nil
	}
	var body struct {
		Choices []struct {
			Message struct {
				ReasoningDetails json.RawMessage `json:"reasoning_details"`
				ToolCalls        []struct {
					ID           string          `json:"id"`
					ExtraContent json.RawMessage `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Choices) == 0 {
		return nil
	}
	m := body.Choices[0].Message
	details := cleanRaw(m.ReasoningDetails)
	native := make(map[string]chatNative, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		if tc.ID == "" {
			continue
		}
		extra := cleanRaw(tc.ExtraContent)
		if details != nil || extra != nil {
			native[tc.ID] = chatNative{ReasoningDetails: details, ExtraContent: extra}
		}
	}
	if len(native) == 0 {
		return nil
	}
	return native
}

// streamReasoningDetails returns a chunk's reasoning_details delta for one
// choice, raw so unknown detail types survive the round trip.
func streamReasoningDetails(data string, index int) json.RawMessage {
	if !strings.Contains(data, `"reasoning_details"`) {
		return nil
	}
	var c struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				ReasoningDetails json.RawMessage `json:"reasoning_details"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return nil
	}
	for _, ch := range c.Choices {
		if ch.Index == index {
			return cleanRaw(ch.Delta.ReasoningDetails)
		}
	}
	return nil
}

// streamToolExtras returns a chunk's per-tool-call extra_content keyed by tool
// call index. Gemini 3 sends its thought signature there.
func streamToolExtras(data string, index int) map[int]json.RawMessage {
	if !strings.Contains(data, `"extra_content"`) {
		return nil
	}
	var c struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				ToolCalls []struct {
					Index        *int            `json:"index"`
					ExtraContent json.RawMessage `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return nil
	}
	out := map[int]json.RawMessage{}
	for _, ch := range c.Choices {
		if ch.Index != index {
			continue
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if extra := cleanRaw(tc.ExtraContent); extra != nil {
				out[idx] = extra
			}
		}
	}
	return out
}

// mergeJSONString appends fragment to a JSON string field in place.
func mergeJSONString(obj map[string]json.RawMessage, key, fragment string) {
	enc, err := json.Marshal(jsonString(obj[key]) + fragment)
	if err == nil {
		obj[key] = enc
	}
}

// chatReasoningDetails accumulates streamed reasoning_details. OpenRouter
// reuses index 0 for every logical block while streaming, so entries merge by
// consecutive type: text and summary fragments concatenate, a signature-only
// text delta folds into the preceding entry, and encrypted blobs stay discrete.
// The replayed array must match the model's original output element for element.
type chatReasoningDetails struct {
	entries []map[string]json.RawMessage
}

func (a *chatReasoningDetails) add(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return false
	}
	added := false
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		added = true
		typ := jsonString(item["type"])
		last := len(a.entries) - 1
		switch typ {
		case "reasoning.text", "reasoning.summary":
			key := "text"
			if typ == "reasoning.summary" {
				key = "summary"
			}
			fragment := jsonString(item[key])
			if last >= 0 && jsonString(a.entries[last]["type"]) == typ {
				mergeJSONString(a.entries[last], key, fragment)
				// signature/format/id may arrive in a later, text-less delta.
				for k, v := range item {
					if k != "type" && k != key {
						a.entries[last][k] = v
					}
				}
				continue
			}
			a.entries = append(a.entries, item)
		default:
			a.entries = append(a.entries, item)
		}
	}
	return added
}

func (a *chatReasoningDetails) raw() json.RawMessage {
	if len(a.entries) == 0 {
		return nil
	}
	raw, err := json.Marshal(a.entries)
	if err != nil {
		return nil
	}
	return raw
}

type chatToolAcc struct {
	id    string
	name  string
	args  strings.Builder
	extra json.RawMessage
}

// minTPSWindow is the shortest first-to-last delta span that yields a TPS
// figure: shorter windows are timing noise from coalesced chunks, not decode.
const minTPSWindow = 250 * time.Millisecond

// decodeTPS measures one round's decode rate from our own stream. usage counts
// hidden reasoning tokens that never crossed the stream, so subtract them when
// no reasoning was streamed; tool-call rounds measure argument assembly, not
// decode, and are skipped.
func decodeTPS(completion, reasoning int, streamedReasoning, toolRound bool, span time.Duration) float64 {
	if toolRound || span < minTPSWindow {
		return 0
	}
	if !streamedReasoning {
		completion -= reasoning
	}
	if completion <= 0 {
		return 0
	}
	return float64(completion) / span.Seconds()
}

// chatRoundStream runs one round as SSE, emitting text/thinking deltas live.
// A unary JSON body (provider ignored streaming) folds through the same
// validation as chatRoundOnce without emitting deltas.
func chatRoundStream(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	msg, usage, _, err := chatRoundStreamDetailed(ctx, cfg, apiKey, hc, req, nil, onEvent)
	return msg, usage, err
}

// chatRoundStreamDetailed also returns the round's native tool state for the
// next request of the same active loop: reasoning_details and per-call
// extra_content. It is never emitted to the UI or stored in ordinary history.
func chatRoundStreamDetailed(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest, native map[string]chatNative, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, map[string]chatNative, error) {
	var usage openai.Usage
	req.Stream = true
	req.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	endpoint, headers, sendCfg, httpClient := chatTarget(cfg, apiKey, hc)
	start := time.Now()
	resp, err := postAPI(ctx, httpClient, sendCfg, endpoint, chatWireBody(cfg, req, native), headers)
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
		return openai.ChatCompletionMessage{}, usage, nil, parseTransportError(chatTransportLabel(cfg), resp.StatusCode, raw)
	}
	sse, raw, err := splitBodyMode(br)
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, nil, err
	}
	emit := func(kind, text string) {
		if onEvent != nil && text != "" {
			onEvent(Event{Kind: kind, Text: text})
		}
	}
	if !sse {
		if te := chatBodyError(chatTransportLabel(cfg), raw); te != nil {
			return openai.ChatCompletionMessage{}, usage, nil, te
		}
		var out openai.ChatCompletionResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openai.ChatCompletionMessage{}, usage, nil, fmt.Errorf("api: chat: decode: %w", err)
		}
		if len(out.Choices) == 0 {
			return openai.ChatCompletionMessage{}, usage, nil, errors.New("api: empty choices in response")
		}
		if err := chatFinishError(string(out.Choices[0].FinishReason), chatNativeFinish(raw, 0), ""); err != nil {
			return openai.ChatCompletionMessage{}, out.Usage, nil, err
		}
		msg := out.Choices[0].Message
		fillReasoning(&msg, raw)
		if modelProvider(cfg.Model) == ProviderOpenRouter {
			var meta ORMeta
			meta.ID = out.ID
			parseOpenRouterMeta(raw, &meta.Provider, &meta.Cost, &meta.HasCost)
			if onEvent != nil && (meta.Provider != "" || meta.HasCost) {
				onEvent(Event{Kind: "or_meta", ORMeta: &meta})
			}
		}
		return msg, out.Usage, chatNativeFromBody(raw), validateModelReply(msg)
	}

	var content, thinking strings.Builder
	var reasoning chatReasoningDetails
	accs := map[int]*chatToolAcc{}
	var order []int
	var eventErr, finishErr, stopErr error
	var terminal bool
	orMeta := ORMeta{}
	sentProvider, sentCost := false, false
	var firstDelta, lastDelta time.Time
	streamedReasoning := false
	markDelta := func() {
		now := time.Now()
		if firstDelta.IsZero() {
			firstDelta = now
			// TTFT is known at the first token: report it while the model is
			// still reasoning instead of waiting for the round to end.
			if onEvent != nil && modelProvider(cfg.Model) == ProviderOpenRouter {
				m := ORMeta{Provider: orMeta.Provider, TTFT: now.Sub(start).Seconds()}
				onEvent(Event{Kind: "or_meta", ORMeta: &m})
			}
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
			te := parseTransportError(chatTransportLabel(cfg), 0, []byte(ev.data))
			if ce, ok := stopErr.(*chatStopError); ok {
				// The stop reason names the failure; the error body names the
				// provider's message. Keep both for the errors view.
				ce.Detail, ce.Raw, ce.Transport = te.Message, te.Raw, te.Transport
				eventErr = ce
			} else {
				eventErr = te
			}
			return false
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if modelProvider(cfg.Model) == ProviderOpenRouter {
			parseOpenRouterMeta([]byte(ev.data), &orMeta.Provider, &orMeta.Cost, &orMeta.HasCost)
			if orMeta.ID == "" {
				orMeta.ID = chunk.ID
			}
			if onEvent != nil {
				if !sentProvider && orMeta.Provider != "" {
					sentProvider = true
					m := ORMeta{ID: orMeta.ID, Provider: orMeta.Provider}
					onEvent(Event{Kind: "or_meta", ORMeta: &m})
				}
				if !sentCost && orMeta.HasCost {
					sentCost = true
					m := ORMeta{Provider: orMeta.Provider, Cost: orMeta.Cost, HasCost: true}
					onEvent(Event{Kind: "or_meta", ORMeta: &m})
				}
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if err := chatFinishError(string(choice.FinishReason), chatNativeFinish([]byte(ev.data), choice.Index), ""); err != nil {
				switch {
				case errors.Is(err, errOutputLimit):
					// Keep reading: the usage chunk follows the finish_reason
					// chunk, and a truncated round's tokens still count.
					finishErr = err
				case strings.EqualFold(string(choice.FinishReason), "error"):
					// OpenRouter reports the precise native reason on this
					// chunk and may send the provider error body after it.
					stopErr = err
				default:
					eventErr = err
					return false
				}
			}
			if choice.FinishReason != "" {
				terminal = true
			}
			d := choice.Delta
			details := streamReasoningDetails(ev.data, choice.Index)
			if reasoning.add(details) {
				// Reasoning crossed the stream: it marks the generation clock
				// and the usage chunk bills its tokens to this round's window.
				streamedReasoning = true
				markDelta()
			}
			extras := streamToolExtras(ev.data, choice.Index)
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
				streamedReasoning = true
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
				if extra := extras[idx]; extra != nil {
					a.extra = extra
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
		return true
	})
	if eventErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, eventErr
	}
	if finishErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, finishErr
	}
	if stopErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, stopErr
	}
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, nil, scanErr
	}
	if !terminal {
		return openai.ChatCompletionMessage{}, usage, nil, errors.New("api: chat: incomplete stream (missing [DONE] or finish_reason)")
	}
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		if !firstDelta.IsZero() {
			reasoningTokens := 0
			if usage.CompletionTokensDetails != nil {
				reasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
			}
			span := lastDelta.Sub(firstDelta)
			orMeta.TTFT = firstDelta.Sub(start).Seconds()
			orMeta.TPS = decodeTPS(usage.CompletionTokens, reasoningTokens, streamedReasoning, len(order) > 0, span)
			dvlog.Logf("or tps out=%d reasoning=%d streamed=%v tools=%d span=%s tps=%.0f", usage.CompletionTokens, reasoningTokens, streamedReasoning, len(order), dvlog.Dur(span), orMeta.TPS)
		}
		if onEvent != nil {
			m := orMeta
			// The cost event already reached the UI (and the caller's
			// accumulator) as soon as the usage chunk arrived; repeating it
			// here would bill the round twice.
			if sentCost {
				m.Cost, m.HasCost = 0, false
			}
			if m.Provider != "" || m.HasCost || m.TTFT > 0 || m.TPS > 0 {
				onEvent(Event{Kind: "or_meta", ORMeta: &m})
			}
		}
	}
	var calls []openai.ToolCall
	details := reasoning.raw()
	var nativeOut map[string]chatNative
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
		if a.id == "" || (details == nil && a.extra == nil) {
			continue
		}
		if nativeOut == nil {
			nativeOut = map[string]chatNative{}
		}
		nativeOut[a.id] = chatNative{ReasoningDetails: details, ExtraContent: a.extra}
	}
	msg := openai.ChatCompletionMessage{
		Role:             openai.ChatMessageRoleAssistant,
		Content:          content.String(),
		ReasoningContent: thinking.String(),
		ToolCalls:        calls,
	}
	return msg, usage, nativeOut, validateModelReply(msg)
}

// withNoQuestionRule appends the no-question policy to the system message.
// A resumed session may already carry it; the marker check keeps repeated
// resumes from stacking copies.
func withNoQuestionRule(history []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	rule := prompts.NoQuestionRule
	for i := range history {
		if history[i].Role != openai.ChatMessageRoleSystem {
			continue
		}
		if !strings.Contains(history[i].Content, rule) {
			history[i].Content = strings.TrimRight(history[i].Content, "\n") + "\n\n" + rule
		}
		return history
	}
	return append([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: rule}}, history...)
}

// headlessOptions are the one-shot (-p) switches beyond the prompt itself.
type headlessOptions struct {
	files   []string                       // -f/--file attachments
	verbose int                            // -v/-vv: 0 tools + live answer, 1 adds rounds/thinking, 2 adds raw/results
	stream  bool                           // --stream: key events as NDJSON on stdout, a done line last
	history []openai.ChatCompletionMessage // --history: prior messages, before the prompt
	// resume continues a saved session: its turns seed history and the turn
	// appends to the same JSONL instead of a fresh one.
	resume *Session
	// noQuestion means nobody can answer: out-of-scope file access and shell
	// approvals are denied instead of read from stdin or /dev/tty.
	noQuestion bool
}

// chat is the headless one-shot path: one prompt, one answer, stdout.
func chat(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, prompt string, stdout, stderr io.Writer) error {
	return chatWithAttachments(ctx, cfg, client, apiKey, hc, registry, prompt, headlessOptions{}, stdout, stderr)
}

// chatWithAttachments is chat plus -f/--file paths (and @path mentions, also
// expanded headless): text rides inlined in the prompt, images on the wire only.
func chatWithAttachments(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, prompt string, opts headlessOptions, stdout, stderr io.Writer) error {
	resetTodos()
	askPrompter = nil
	if !opts.noQuestion {
		askPrompter = func(ctx context.Context, qs []AskQuestion) ([]string, error) {
			in, closeIn, err := headlessAskInput()
			if err != nil {
				return nil, err
			}
			defer closeIn()
			return headlessAsk(ctx, qs, in, os.Stderr)
		}
	}
	displayPrompt := prompt
	paths := append(append([]string(nil), opts.files...), atPaths(prompt)...)
	refs, notices := resolveAttachments(ctx, paths, cfg)
	for _, n := range notices {
		fmt.Fprintln(stderr, "valen: "+n)
	}
	ctx = withTurnAttachments(ctx, refs)
	prompt = promptWithAttachments(prompt, refs)
	// Headless runs are conversations too: record them like TUI turns so /sessions
	// can resume them. A save failure must never cost the answer. With -r the
	// saved session is reopened (never renamed) and the turn appends to it.
	session := opts.resume
	if session == nil {
		var err error
		if session, err = newSession(cfg); err != nil {
			session = nil
			fmt.Fprintln(stderr, "valen: session: "+err.Error())
		} else {
			session.nameSession(displayPrompt)
		}
	}
	started := time.Now()
	var history []openai.ChatCompletionMessage
	if opts.resume != nil {
		history = historyFromTurns(cfg, opts.resume.Turns)
	} else {
		history = seedHistory(cfg)
	}
	history = append(history, opts.history...)
	if opts.noQuestion {
		history = withNoQuestionRule(history)
	}
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})
	dvlog.Logf("start model=%s transport=%s thinking=%s maxrounds=%d budget~%dtok schemastate~%dtok tools=%d headless prompt=%dB %q",
		cfg.Model, modelTransport(cfg.Model), cfg.Thinking, cfg.MaxRounds, contextBudget(cfg), requestToolsTokens(registry), len(registry.Names()), len(prompt), dvlog.Preview(prompt, 200))

	var tools []ToolRecord
	var orMeta *ORMeta
	var turnCost float64
	var hasTurnCost bool
	vt := newHeadlessVerbose(stderr, opts.verbose)
	var enc *json.Encoder
	var streamErr error
	if opts.stream {
		enc = json.NewEncoder(stdout)
	}
	onEvent := func(ev Event) {
		vt.event(ev)
		if enc != nil && streamErr == nil {
			switch ev.Kind {
			case "thinking_delta", "text_delta":
				// Live reasoning and answer text are not part of the stream
				// contract: done.reply carries the final answer, and the live
				// answer goes to stderr.
			default:
				if err := enc.Encode(ev); err != nil {
					streamErr = err
				}
			}
		}
		switch ev.Kind {
		case "round_start", "retry":
			orMeta = clearRoundPerf(orMeta)
		case "tool_start":
			tools = append(tools, ToolRecord{ID: ev.ID, Name: ev.Text, Args: ev.Args})
		case "tool_result":
			for i := len(tools) - 1; i >= 0; i-- {
				if tools[i].ID == ev.ID && tools[i].Name == ev.Text {
					tools[i].Output = ev.Output
					tools[i].DurMs = ev.DurMs
					break
				}
			}
		case "or_meta":
			orMeta = mergeORMeta(orMeta, ev.ORMeta)
			if ev.ORMeta != nil && ev.ORMeta.HasCost {
				turnCost += ev.ORMeta.Cost
				hasTurnCost = true
			}
		}
	}
	reply, usage, err := runTurn(ctx, cfg, client, apiKey, hc, registry, &history, onEvent)
	thinking, answer := splitThinking(reply.Content)
	if reply.ReasoningContent != "" {
		thinking = reply.ReasoningContent
	}
	vt.fallbackThinking(thinking)
	vt.closeLine()
	turn := Turn{
		At: started, Prompt: displayPrompt, Thinking: thinking, Reply: answer, Tools: tools,
		Usage: Usage{Prompt: usage.PromptTokens, Completion: usage.CompletionTokens, Total: usage.TotalTokens},
		Model: cfg.Model, Effort: cfg.Thinking, Plan: cfg.Plan, History: history, ORMeta: orMeta,
		DurMs: time.Since(started).Milliseconds(),
	}
	if turn.DurMs < 1 {
		turn.DurMs = 1
	}
	if hasTurnCost {
		turn.Cost = turnCost
	}
	if err != nil {
		fe := friendlyError(err)
		turn.Error = &fe
	}
	sessionPath := ""
	if session != nil {
		if aerr := session.append(turn); aerr == nil {
			session.ensureMeta(cfg.Model, cfg.Thinking)
			sessionPath = session.Path
		} else {
			fmt.Fprintln(stderr, "valen: session: "+aerr.Error())
		}
	}
	if opts.stream {
		return writeStreamDone(enc, usage, answer, err, streamErr, sessionPath)
	}
	if sessionPath != "" {
		fmt.Fprintf(stderr, "[session %s]\n", sessionPath)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, answer)
	fmt.Fprintf(stderr, "[tokens in=%d out=%d]\n", usage.PromptTokens, usage.CompletionTokens)
	return nil
}

// streamDone is the last NDJSON line of a --stream run. It is written even
// when the turn failed, so a reader always sees how the run ended.
type streamDone struct {
	Kind    string         `json:"kind"`
	Reply   string         `json:"reply,omitempty"`
	Usage   Usage          `json:"usage"`
	Session string         `json:"session,omitempty"`
	Error   *FriendlyError `json:"error,omitempty"`
}

// writeStreamDone writes the done line and returns the first error that
// stopped the run: the turn's, then a failed event write.
func writeStreamDone(enc *json.Encoder, usage openai.Usage, reply string, turnErr, streamErr error, sessionPath string) error {
	done := streamDone{Kind: "done", Reply: reply, Usage: Usage{Prompt: usage.PromptTokens, Completion: usage.CompletionTokens, Total: usage.TotalTokens}, Session: sessionPath}
	if turnErr != nil {
		fe := friendlyError(turnErr)
		done.Error = &fe
	}
	if err := enc.Encode(done); err != nil && turnErr == nil {
		return err
	}
	if turnErr != nil {
		return turnErr
	}
	return streamErr
}

// maxHistoryBytes caps a --history file, like the other input caps.
const maxHistoryBytes = 8 << 20

// loadHistoryFile reads --history: a JSON array of {"role","content"} objects.
// Only user and assistant roles are accepted, so the file cannot inject a
// system prompt or fake tool results.
func loadHistoryFile(path string) ([]openai.ChatCompletionMessage, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	if fi.Size() > maxHistoryBytes {
		return nil, fmt.Errorf("history %s: larger than %d bytes", path, maxHistoryBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	var in []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("history %s: %w", path, err)
	}
	// Only role and content are kept: tool calls in a file would have no
	// matching tool results.
	msgs := make([]openai.ChatCompletionMessage, 0, len(in))
	for i, m := range in {
		if m.Role != openai.ChatMessageRoleUser && m.Role != openai.ChatMessageRoleAssistant {
			return nil, fmt.Errorf("history %s: message %d has role %q; only user and assistant are allowed", path, i, m.Role)
		}
		msgs = append(msgs, openai.ChatCompletionMessage{Role: m.Role, Content: m.Content})
	}
	return msgs, nil
}
