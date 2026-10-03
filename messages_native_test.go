package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func messagesSSE(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func TestMessagesNativeReplay(t *testing.T) {
	content := `[
		{"type":"thinking","thinking":"plan more","signature":"sig-one-two","provider_field":{"keep":true}},
		{"type":"redacted_thinking","data":"opaque-data"},
		{"type":"text","text":"preface"},
		{"type":"tool_use","id":"call-1","name":"shell","input":{"command":"ls"}},
		{"type":"text","text":"between"},
		{"type":"tool_use","id":"call-2","name":"shell","input":{"command":"pwd"}},
		{"type":"thinking","thinking":"","signature":"empty-signed"}
	]`
	stream := messagesSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"plan","signature":"sig-","provider_field":{"keep":true}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" more"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"one"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-two"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque-data"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":"pre"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"face"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call-1","name":"shell","input":{}}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"content_block_start","index":4,"content_block":{"type":"text","text":"between"}}`,
		`{"type":"content_block_stop","index":4}`,
		`{"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"call-2","name":"shell","input":{"command":"pwd"}}}`,
		`{"type":"content_block_stop","index":5}`,
		`{"type":"content_block_start","index":6,"content_block":{"type":"thinking","thinking":"","signature":"empty-signed"}}`,
		`{"type":"content_block_stop","index":6}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	)
	unary := `{"content":` + content + `,"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`
	var want any
	if err := json.Unmarshal([]byte(content), &want); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct{ name, body string }{{"stream", stream}, {"unary", unary}} {
		t.Run(mode.name, func(t *testing.T) {
			calls := 0
			var replay any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					_, _ = w.Write([]byte(mode.body))
					return
				}
				var req struct {
					Messages []struct {
						Role    string `json:"role"`
						Content any    `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				} else {
					for _, m := range req.Messages {
						if m.Role == "assistant" {
							replay = m.Content
						}
					}
				}
				_, _ = w.Write(messagesTextResp("done"))
			}))
			defer srv.Close()
			cfg := messagesTestConfig(srv.URL)
			var text, thinking strings.Builder
			msg, usage, native, err := messagesRoundStreamDetailed(context.Background(), cfg, "key", srv.Client(), messagesRequest{Model: cfg.Model, Stream: true}, func(ev Event) {
				switch ev.Kind {
				case "text_delta":
					text.WriteString(ev.Text)
				case "thinking_delta":
					thinking.WriteString(ev.Text)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if msg.Content != "<think>plan more</think>prefacebetween" || len(msg.ToolCalls) != 2 {
				t.Fatalf("message=%+v", msg)
			}
			if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
				t.Fatalf("usage=%+v", usage)
			}
			if mode.name == "stream" && (text.String() != "prefacebetween" || thinking.String() != "plan more") {
				t.Fatalf("text=%q thinking=%q", text.String(), thinking.String())
			}
			history := []openai.ChatCompletionMessage{
				{Role: "system", Content: "system"},
				{Role: "user", Content: "start"}, msg,
				{Role: "tool", ToolCallID: "call-1", Content: "first result"},
				{Role: "tool", ToolCallID: "call-2", Content: "second result"},
			}
			system, msgs := splitMessagesHistoryNative(history, map[string][]messagesBlock{msg.ToolCalls[0].ID: native})
			if system != "system" {
				t.Fatalf("system=%q", system)
			}
			if _, err := callMessagesOnce(context.Background(), srv.Client(), cfg, "key", messagesRequest{Model: cfg.Model, Messages: msgs}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replay, want) {
				t.Fatalf("native replay=%v, want %v", replay, want)
			}
			// Even a trailing assistant must not gain a cache field or mutate
			// its native signed blocks, and the second call ID is not a key.
			_, trailing := splitMessagesHistoryNative([]openai.ChatCompletionMessage{msg}, map[string][]messagesBlock{"call-1": native})
			data, err := json.Marshal(trailing[0].Content)
			if err != nil {
				t.Fatal(err)
			}
			var got any
			if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("trailing replay=%s err=%v", data, err)
			}
			_, fallback := splitMessagesHistoryNative([]openai.ChatCompletionMessage{msg}, map[string][]messagesBlock{"call-2": native})
			data, err = json.Marshal(fallback)
			if err != nil || strings.Contains(string(data), "signature") || strings.Contains(string(data), "<think>") {
				t.Fatalf("fallback=%s err=%v", data, err)
			}
		})
	}
}

func TestMessagesStreamRejectsIncompleteAndProviderErrors(t *testing.T) {
	text := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial answer"}}`,
		`{"type":"content_block_stop","index":0}`,
	}
	tool := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"shell","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_stop","index":0}`,
	}
	for _, tc := range []struct {
		name     string
		events   []string
		want     string
		provider bool
	}{
		{"provider before content", []string{`{"type":"error","error":{"message":"overloaded"}}`}, "overloaded", true},
		{"provider after content", append(append([]string{}, text...), `{"type":"error","error":{"message":"provider failed"}}`), "provider failed", true},
		{"typed provider error", []string{`{"type":"error","error":{"type":"overloaded_error"}}`}, "overloaded_error", true},
		{"empty provider error", []string{`{"type":"error"}`}, "provider stream error", true},
		{"EOF", text, "message_stop", false},
		{"DONE is not terminal", append(append([]string{}, text...), `[DONE]`), "message_stop", false},
		{"unfinished block", []string{text[0], `{"type":"message_stop"}`}, "unfinished content block", false},
		{"unfinished tool block", []string{tool[0], `{"type":"message_stop"}`}, "unfinished content block", false},
		{"malformed arguments", append(append([]string{}, tool...), `{"type":"message_stop"}`), "invalid tool arguments", false},
		{"max tokens", append(append([]string{}, text...), `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`, `{"type":"message_stop"}`), "max_tokens", false},
		{"max tokens tool", []string{tool[0], tool[2], `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`, `{"type":"message_stop"}`}, "max_tokens", false},
		{"thinking only", []string{`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"plan","signature":"sig"}}`, text[1], `{"type":"message_stop"}`}, "empty choices", false},
		{"redacted only", []string{`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}`, text[1], `{"type":"message_stop"}`}, "empty choices", false},
		{"malformed event", append(append([]string{}, text...), `{broken`), "decode stream event", false},
		{"missing delta", []string{tool[0], `{"type":"content_block_delta","index":0}`, tool[2], `{"type":"message_stop"}`}, "missing content block delta", false},
		{"delta without start", []string{`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, `{"type":"message_stop"}`}, "active content block", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(messagesSSE(tc.events...)))
			}))
			defer srv.Close()
			msg, _, native, err := messagesRoundStreamDetailed(context.Background(), messagesTestConfig(srv.URL), "", srv.Client(), messagesRequest{Stream: true}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if msg.Content != "" || len(msg.ToolCalls) != 0 || native != nil {
				t.Fatalf("failed response must not be usable: message=%+v native=%+v", msg, native)
			}
			var transport *transportError
			if tc.provider && (!errors.As(err, &transport) || transport.Transport != "messages") {
				t.Fatalf("provider error type=%T", err)
			}
		})
	}
}

func TestMessagesReplyRejectsInvalidCallsAndIncompleteFinal(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp messagesResponse
		want string
	}{
		{"missing id", messagesResponse{Content: []messagesBlock{{Type: "tool_use", Name: "shell", Input: json.RawMessage(`{}`)}}}, "missing id or name"},
		{"missing name", messagesResponse{Content: []messagesBlock{{Type: "tool_use", ID: "call-1", Input: json.RawMessage(`{}`)}}}, "missing id or name"},
		{"missing input", messagesResponse{Content: []messagesBlock{{Type: "tool_use", ID: "call-1", Name: "shell"}}}, "invalid tool arguments"},
		{"broken input", messagesResponse{Content: []messagesBlock{{Type: "tool_use", ID: "call-1", Name: "shell", Input: json.RawMessage(`{"x":`)}}}, "invalid tool arguments"},
		{"array input", messagesResponse{Content: []messagesBlock{{Type: "tool_use", ID: "call-1", Name: "shell", Input: json.RawMessage(`[]`)}}}, "invalid tool arguments"},
		{"null input", messagesResponse{Content: []messagesBlock{{Type: "tool_use", ID: "call-1", Name: "shell", Input: json.RawMessage(`null`)}}}, "invalid tool arguments"},
		{"thinking only", messagesResponse{Content: []messagesBlock{{Type: "thinking", Thinking: "plan", Signature: "signed"}}}, "empty choices"},
		{"tagged thinking only", messagesResponse{Content: []messagesBlock{{Type: "text", Text: "<think>plan</think>"}}}, "empty choices"},
		{"redacted only", messagesResponse{Content: []messagesBlock{{Type: "redacted_thinking", Data: "opaque"}}}, "empty choices"},
		{"max tokens", messagesResponse{StopReason: "max_tokens", Content: []messagesBlock{{Type: "text", Text: "partial answer"}}}, "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := messagesReplyToOpenAI(tc.resp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestMessagesFallbackStripsThinking(t *testing.T) {
	history := []openai.ChatCompletionMessage{
		{Role: "user", Content: "literal <think>user text</think>"},
		{Role: "assistant", Content: "<think>private</think>answer<think>more private</think>"},
		{Role: "assistant", Content: "<think>only thinking</think>"},
		{Role: "assistant", Content: "<think>private tool plan</think>calling", ToolCalls: []openai.ToolCall{
			{ID: "call-1", Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "call-1", Content: "result"},
	}
	_, msgs := splitMessagesHistory(history)
	if len(msgs) != 4 || msgs[0].Content != history[0].Content || msgs[1].Content != "answer" {
		t.Fatalf("messages=%+v", msgs)
	}
	blocks := msgs[2].Content.([]any)
	if blocks[0].(map[string]any)["text"] != "calling" {
		t.Fatalf("tool text=%+v", blocks)
	}
	history[3].ToolCalls[0].Function.Arguments = `{"broken":`
	_, msgs = splitMessagesHistory(history)
	if _, err := json.Marshal(msgs); err == nil {
		t.Fatal("malformed history arguments must not silently become {}")
	}
}
