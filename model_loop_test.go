package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func TestModelLoopFreshReadsAndNativeReplayOnWire(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "test-key")
	for _, transport := range []string{"chat", "messages", "responses"} {
		for _, mode := range []string{"stream", "unary", "rejected"} {
			t.Run(transport+"/"+mode, func(t *testing.T) {
				calls := make([]openai.ToolCall, 4)
				for i := range calls {
					calls[i] = openaiToolCall(fmt.Sprintf("call-%d", i), "read")
					calls[i].Function.Arguments = fmt.Sprintf(`{"path":"file-%d"}`, i)
				}
				msg := openai.ChatCompletionMessage{Role: "assistant", Content: "checking", ReasoningContent: "native plan", ToolCalls: calls}
				var first, final []byte
				var firstStream, finalStream string
				model, capField, path := "m", "max_tokens", "/chat/completions"
				switch transport {
				case "chat":
					first, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"message": msg, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
					final = completion(nil, "done")
					deltaCalls := make([]map[string]any, len(calls))
					for i, call := range calls {
						deltaCalls[i] = map[string]any{"index": i, "id": call.ID, "type": "function", "function": call.Function}
					}
					chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "checking", "reasoning_content": "native plan", "tool_calls": deltaCalls}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
					firstStream, finalStream = "data: "+string(chunk)+"\n\ndata: [DONE]\n\n", chatTextStream("done")
				case "messages":
					model, path = "minimax-m2.7", "/messages"
					blocks := []messagesBlock{{Type: "thinking", Thinking: "native plan", Signature: "signed-plan"}, {Type: "redacted_thinking", Data: "opaque-plan"}, {Type: "text", Text: "checking"}}
					for _, call := range calls {
						blocks = append(blocks, messagesBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments)})
					}
					first, _ = json.Marshal(map[string]any{"content": blocks, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 10, "output_tokens": 5}})
					final = messagesTextResp("done")
					for i, block := range blocks {
						start, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": i, "content_block": block})
						firstStream += "data: " + string(start) + "\n\n" + fmt.Sprintf("data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
					}
					firstStream = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" + firstStream + "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
					// A provider may ignore the streaming flag on a later round.
					finalStream = string(final)
				case "responses":
					model, capField, path = "gpt-6-luna", "max_output_tokens", "/responses"
					items := []responsesOutputItem{{Type: "reasoning", ID: "rs-1", EncryptedContent: "opaque-plan", Summary: []responsesSummaryPart{{Type: "summary_text", Text: "native plan"}}}, {Type: "message", Role: "assistant", Phase: "commentary", Content: []responsesContent{{Type: "output_text", Text: "checking"}}}}
					for _, call := range calls {
						items = append(items, responsesOutputItem{Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
					}
					first, _ = json.Marshal(map[string]any{"status": "completed", "output": items, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}})
					final = responsesTextResp("done")
					firstStream = "data: {\"type\":\"response.completed\",\"response\":" + string(first) + "}\n\n"
					finalStream = "data: {\"type\":\"response.completed\",\"response\":" + string(final) + "}\n\n"
				}
				var requests []map[string]any
				accepted := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != path {
						t.Errorf("path=%s, want %s", r.URL.Path, path)
					}
					var req map[string]any
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					requests = append(requests, req)
					if mode == "rejected" && req["stream"] == true {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":{"message":"stream not supported"}}`))
						return
					}
					accepted++
					body := final
					if accepted == 1 {
						body = first
					}
					if mode == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						body = []byte(finalStream)
						if accepted == 1 {
							body = []byte(firstStream)
						}
					}
					_, _ = w.Write(body)
				}))
				defer srv.Close()
				output := "File large.go (500 lines, showing 1-500):\n" + strings.Repeat("x", maxToolOutput-200) + "\nFRESH_READ_TAIL"
				r := NewRegistry()
				var executed atomic.Int32
				_ = r.Add(Tool{Name: "read", Fn: func(context.Context, string) (string, error) { executed.Add(1); return output, nil }})
				cfg := newTestConfig(srv.URL)
				cfg.Model, cfg.MaxRounds, cfg.MaxStagnantRounds, cfg.MaxTokens = model, 2, 1, 64
				if transport == "responses" {
					cfg.Thinking = "none"
				}
				history := guardTestHistory()
				msg, usage, err := runTurn(context.Background(), cfg, nil, "", srv.Client(), r, &history, nil)
				if err != nil || msg.Content != "done" || executed.Load() != 4 || usage.PromptTokens != 20 || usage.CompletionTokens != 10 {
					t.Fatalf("reply=%+v usage=%+v executed=%d err=%v", msg, usage, executed.Load(), err)
				}
				wantRequests := 2
				if mode == "rejected" {
					wantRequests = 3
				}
				if len(requests) != wantRequests {
					t.Fatalf("requests=%d, want %d", len(requests), wantRequests)
				}
				for _, req := range requests {
					if req[capField] != float64(64) {
						t.Fatalf("configured output cap ignored: %v", req[capField])
					}
					if transport == "responses" && req["reasoning"].(map[string]any)["effort"] != "none" {
						t.Fatalf("GPT none omitted: %v", req["reasoning"])
					}
				}
				wire, _ := json.Marshal(requests[len(requests)-1])
				if strings.Count(string(wire), "FRESH_READ_TAIL") != 4 || strings.Count(string(wire), strings.Repeat("x", maxToolOutput-200)) != 4 {
					t.Fatal("four fresh reads were truncated or pruned on wire")
				}
				if transport == "chat" {
					if history[2].ReasoningContent != "native plan" || strings.Contains(history[2].Content, "<think>") || !strings.Contains(string(wire), `"reasoning_content":"native plan"`) {
						t.Fatalf("chat reasoning was flattened or lost: %+v", history[2])
					}
				} else if !strings.Contains(string(wire), "opaque-plan") || (transport == "messages" && !strings.Contains(string(wire), "signed-plan")) || (transport == "responses" && !strings.Contains(string(wire), `"phase":"commentary"`)) {
					t.Fatalf("native continuation lost: %s", wire)
				}
				// A fresh execution closure must not retain native metadata.
				history = append(history, openai.ChatCompletionMessage{Role: "user", Content: "next"})
				if _, _, err := runTurn(context.Background(), cfg, nil, "", srv.Client(), r, &history, nil); err != nil {
					t.Fatal(err)
				}
				wire, _ = json.Marshal(requests[len(requests)-1])
				if transport != "chat" && (strings.Contains(string(wire), "opaque-plan") || strings.Contains(string(wire), "signed-plan")) {
					t.Fatal("native continuation leaked across turns")
				}
			})
		}
	}
}

func TestChatRoundRejectsInvalidCompletion(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"truncated_stream", `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n", "incomplete stream"},
		{"malformed_stream", "data: {bad}\n\n", "decode stream event"},
		{"provider_error", "data: {\"error\":{\"message\":\"provider overloaded\"}}\n\n", "provider overloaded"},
		{"length_stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "length"},
		{"filter_stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n", "content_filter"},
		{"thinking_stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"plan\"},\"finish_reason\":\"stop\"}]}\n\n", "empty choices"},
		{"length_unary", `{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`, "length"},
		{"filter_unary", `{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"content_filter"}]}`, "content_filter"},
		{"thinking_unary", `{"choices":[{"message":{"role":"assistant","reasoning_content":"plan"},"finish_reason":"stop"}]}`, "empty choices"},
		{"missing_call_id", `{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, "incomplete tool call"},
		{"bad_call_args", `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"read","arguments":"[]"}}]},"finish_reason":"tool_calls"}]}`, "invalid tool arguments"},
		{"partial_call_stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", "invalid tool arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer srv.Close()
			cfg := newTestConfig(srv.URL)
			_, _, err := chatRoundStream(context.Background(), cfg, "", srv.Client(), openai.ChatCompletionRequest{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if !strings.HasPrefix(tc.body, "data:") {
				_, _, err = chatRoundOnce(context.Background(), cfg, "", srv.Client(), openai.ChatCompletionRequest{})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("unary err=%v, want %q", err, tc.want)
				}
			}
		})
	}
}

func TestChatStreamFinishReasonWithoutDone(t *testing.T) {
	var bodies []string
	srv := sseServer(t, &bodies, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
	msg, _, err := chatRoundStream(context.Background(), newTestConfig(srv.URL), "", srv.Client(), openai.ChatCompletionRequest{}, nil)
	if err != nil || msg.Content != "done" {
		t.Fatalf("reply=%+v err=%v", msg, err)
	}
}

func TestToolBatchCancellationWhileWaiting(t *testing.T) {
	r := NewRegistry()
	started := make(chan struct{}, maxParallelTools)
	var executed atomic.Int32
	_ = r.Add(Tool{Name: "read", Fn: func(ctx context.Context, _ string) (string, error) {
		executed.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}})
	calls := make([]openai.ToolCall, maxParallelTools*2)
	for i := range calls {
		calls[i] = openaiToolCall(fmt.Sprint(i), "read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []openai.ChatCompletionMessage, 1)
	go func() { done <- runToolCalls(ctx, r, calls, nil) }()
	for range maxParallelTools {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("parallel workers did not start")
		}
	}
	cancel()
	select {
	case out := <-done:
		if len(out) != len(calls) || executed.Load() != maxParallelTools {
			t.Fatalf("out=%d executed=%d", len(out), executed.Load())
		}
		for i, msg := range out {
			if msg.ToolCallID != calls[i].ID || !strings.Contains(msg.Content, "context canceled") {
				t.Fatalf("result[%d]=%+v", i, msg)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("canceled semaphore waiters stalled")
	}
}

func TestToolBatchPotentialWritesStaySerial(t *testing.T) {
	for _, name := range []string{"write", "edit", "shell", "todowrite", "todoread", "task", "unknown", "server__tool"} {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry()
			step := 0
			_ = r.Add(Tool{Name: name, Fn: func(context.Context, string) (string, error) {
				previous := step
				time.Sleep(time.Millisecond)
				step = previous + 1
				return fmt.Sprint(step), nil
			}})
			out := runToolCalls(context.Background(), r, []openai.ToolCall{openaiToolCall("1", name), openaiToolCall("2", name)}, nil)
			if step != 2 || out[0].Content != "1" || out[1].Content != "2" {
				t.Fatalf("dependent updates lost or reordered: step=%d out=%+v", step, out)
			}
		})
	}
}

func TestRunTurnCarriesCurrentConfig(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "test-key")
	srv, _ := fakeServer(t, completion(toolCall("task", `{}`), ""), completion(nil, "done"))
	cfg := newTestConfig(srv.URL)
	cfg.Thinking, cfg.MaxTokens = "high", 512
	r := NewRegistry()
	var got Config
	_ = r.Add(Tool{Name: "task", Fn: func(ctx context.Context, _ string) (string, error) {
		got = currentTurnConfig(ctx, Config{Model: "old"})
		return "ok", nil
	}})
	history := guardTestHistory()
	if _, _, err := runTurn(context.Background(), cfg, nil, "", srv.Client(), r, &history, nil); err != nil {
		t.Fatal(err)
	}
	if got.Model != cfg.Model || got.Thinking != cfg.Thinking || got.MaxTokens != cfg.MaxTokens {
		t.Fatalf("task saw stale config: %+v", got)
	}
	if currentTurnConfig(context.Background(), cfg).Model != cfg.Model || requestTimeout(Config{}) != 300*time.Second {
		t.Fatal("config/timeout fallback changed")
	}
}
