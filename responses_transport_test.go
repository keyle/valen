package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func responsesSSE(data ...string) string {
	var out strings.Builder
	for _, item := range data {
		fmt.Fprintf(&out, "data: %s\n\n", item)
	}
	return out.String()
}

func responsesDetailedTestRound(t *testing.T, body string, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, []responsesOutputItem, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(req.Include, []string{"reasoning.encrypted_content"}) {
			t.Errorf("include=%v", req.Include)
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	return responsesRoundStreamDetailed(context.Background(), Config{BaseURL: srv.URL}, "key", srv.Client(), responsesRequest{Stream: true}, onEvent)
}

func TestResponsesStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, terminal, want string
	}{
		{"structured", `{"type":"error","error":{"message":"structured failure","code":"bad_request"}}`, "structured failure"},
		{"numeric code", `{"type":"error","error":{"message":"numeric code failure","code":400}}`, "numeric code failure"},
		{"nested failed", `{"type":"response.failed","response":{"status":"failed","error":{"message":"nested failure"}}}`, "nested failure"},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "max_output_tokens"},
		{"flat", `{"type":"error","error":"flat failure"}`, "flat failure"},
		{"message", `{"type":"error","message":"message failure"}`, "message failure"},
		{"failed no details", `{"type":"response.failed"}`, "response.failed"},
		{"EOF", "", "without response.completed"},
		{"DONE", "[DONE]", "without response.completed"},
		{"missing response", `{"type":"response.completed"}`, "missing response"},
		{"malformed event", `{"type":`, "decode event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := responsesSSE(`{"type":"response.output_text.delta","delta":"partial"}`, tc.terminal)
			msg, _, native, err := responsesDetailedTestRound(t, stream, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if msg.Content != "" || len(msg.ToolCalls) != 0 || native != nil {
				t.Fatalf("failed stream returned a usable reply: %+v native=%+v", msg, native)
			}
			if strings.Contains(tc.name, "failed") || tc.name == "structured" {
				var transport *transportError
				if !errors.As(err, &transport) {
					t.Fatalf("not a transport error: %T", err)
				}
			}
		})
	}
}

func TestResponsesUnaryStatusFailures(t *testing.T) {
	for _, body := range []string{
		`{"status":"failed","error":{"message":"failed response"}}`,
		`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`,
		`{"status":"in_progress"}`,
		`{"error":{"message":"error without status"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			// A plausible answer must not hide a failed/incomplete unary response.
			var payload map[string]any
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatal(err)
			}
			payload["output"] = []responsesOutputItem{{Type: "message", Content: []responsesContent{{Type: "output_text", Text: "partial"}}}}
			raw, _ := json.Marshal(payload)
			if _, _, _, err := responsesDetailedTestRound(t, string(raw), nil); err == nil {
				t.Fatal("stream's unary path accepted failure")
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req responsesRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				if !reflect.DeepEqual(req.Include, []string{"reasoning.encrypted_content"}) {
					t.Errorf("include=%v", req.Include)
				}
				w.Write(raw)
			}))
			defer srv.Close()
			if _, err := callResponsesOnce(context.Background(), srv.Client(), Config{BaseURL: srv.URL}, "key", responsesRequest{}); err == nil {
				t.Fatal("unary call accepted failure")
			}
		})
	}
}

func TestResponsesFinalOutputAuthoritative(t *testing.T) {
	native := []responsesOutputItem{
		{Type: "reasoning", ID: "rs_1", EncryptedContent: "opaque+/==", Summary: []responsesSummaryPart{{Type: "summary_text", Text: "final plan"}}},
		{Type: "message", ID: "msg_1", Role: "assistant", Phase: "commentary", Content: []responsesContent{{Type: "output_text", Text: "final commentary"}}},
		{Type: "function_call", ID: "fc_1", CallID: "call-1", Name: "shell", Arguments: `{"command":"final"}`},
		{Type: "function_call", ID: "fc_2", CallID: "call-2", Name: "shell", Arguments: `{"command":"second"}`},
	}
	resp := responsesResponse{Status: "completed", Output: native}
	resp.Usage.InputTokens, resp.Usage.OutputTokens = 7, 3
	final, _ := json.Marshal(map[string]any{"type": "response.completed", "response": resp})
	stream := responsesSSE(
		`{"type":"response.output_text.delta","output_index":1,"delta":"stale text"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"stale plan"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"stale","name":"shell"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"malformed"}`,
		string(final),
	)
	var events []Event
	msg, usage, gotNative, err := responsesDetailedTestRound(t, stream, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "<think>final plan</think>final commentary" || len(msg.ToolCalls) != 2 || msg.ToolCalls[0].Function.Arguments != native[2].Arguments {
		t.Fatalf("reply=%+v", msg)
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 || usage.TotalTokens != 10 {
		t.Fatalf("usage=%+v", usage)
	}
	if !reflect.DeepEqual(gotNative, native) {
		t.Fatalf("native=%+v, want %+v", gotNative, native)
	}
	if strings.Contains(msg.Content, "opaque") || len(events) != 2 || events[0].Kind != "text_delta" || events[1].Kind != "thinking_delta" {
		t.Fatalf("native metadata leaked: msg=%+v events=%+v", msg, events)
	}
	// The next input retains reasoning, phase and item order before tool outputs.
	history := []openai.ChatCompletionMessage{msg, {Role: "tool", ToolCallID: "call-1", Content: "result"}}
	_, input := splitResponsesInputNative(history, map[string][]responsesOutputItem{msg.ToolCalls[0].ID: gotNative})
	if len(input) != len(native)+1 {
		t.Fatalf("input=%+v", input)
	}
	for i, item := range native {
		if !reflect.DeepEqual(input[i], item) {
			t.Fatalf("native input[%d]=%+v, want %+v", i, input[i], item)
		}
	}
	if out := input[len(native)].(map[string]any); out["call_id"] != "call-1" || out["type"] != "function_call_output" {
		t.Fatalf("tool output=%v", out)
	}
	raw, _ := json.Marshal(resp)
	unaryMsg, unaryUsage, unaryNative, err := responsesDetailedTestRound(t, string(raw), nil)
	if err != nil || !reflect.DeepEqual(unaryNative, native) || !reflect.DeepEqual(unaryMsg, msg) || unaryUsage != usage {
		t.Fatalf("unary native capture: msg=%+v usage=%+v native=%+v err=%v", unaryMsg, unaryUsage, unaryNative, err)
	}
}

func TestResponsesCompletedEmptyOutputFallback(t *testing.T) {
	for _, done := range []string{
		`{"type":"response.function_call_arguments.done","output_index":2,"arguments":"{\"command\":\"done\"}"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call-2","name":"shell","arguments":"{\"command\":\"done\"}"}}`,
	} {
		t.Run(done, func(t *testing.T) {
			stream := responsesSSE(
				`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call-2","name":"shell","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"partial"}`,
				done,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"secret","summary":[{"type":"summary_text","text":"plan"}]}}`,
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"checking"}]}}`,
				`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
			)
			msg, _, native, err := responsesDetailedTestRound(t, stream, nil)
			if err != nil {
				t.Fatal(err)
			}
			if msg.Content != "<think>plan</think>checking" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"command":"done"}` {
				t.Fatalf("msg=%+v", msg)
			}
			if len(native) != 3 || native[0].EncryptedContent != "secret" || native[1].Phase != "commentary" || native[2].CallID != "call-2" {
				t.Fatalf("native order/metadata=%+v", native)
			}
		})
	}
}

func TestResponsesValidatesFunctionCalls(t *testing.T) {
	for _, args := range []string{"", "{", "null", "[]", `"string"`, "1", "true", `{"x":1} trailing`} {
		t.Run(args, func(t *testing.T) {
			resp := responsesResponse{Output: []responsesOutputItem{{Type: "function_call", CallID: "call-1", Name: "shell", Arguments: args}}}
			if _, err := responsesReplyToOpenAI(resp); err == nil {
				t.Fatalf("accepted malformed arguments %q", args)
			}
			item, _ := json.Marshal(resp.Output[0])
			stream := responsesSSE(`{"type":"response.output_item.done","output_index":0,"item":`+string(item)+`}`, `{"type":"response.completed","response":{"status":"completed","output":[]}}`)
			if _, _, _, err := responsesDetailedTestRound(t, stream, nil); err == nil {
				t.Fatalf("stream accepted malformed arguments %q", args)
			}
		})
	}
	for _, item := range []responsesOutputItem{
		{Type: "function_call", Name: "shell", Arguments: "{}"},
		{Type: "function_call", CallID: "call-1", Arguments: "{}"},
	} {
		if _, err := responsesReplyToOpenAI(responsesResponse{Output: []responsesOutputItem{item}}); err == nil {
			t.Fatalf("accepted missing call metadata: %+v", item)
		}
	}
	duplicate := responsesOutputItem{Type: "function_call", CallID: "call-1", Name: "shell", Arguments: "{}"}
	if _, err := responsesReplyToOpenAI(responsesResponse{Output: []responsesOutputItem{duplicate, duplicate}}); err == nil {
		t.Fatal("accepted duplicate call IDs")
	}
	msg, err := responsesReplyToOpenAI(responsesResponse{Output: []responsesOutputItem{{Type: "function_call", ID: "fc_1", Name: "shell", Arguments: " {} "}}})
	if err != nil || msg.ToolCalls[0].ID != "fc_1" || msg.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("valid empty object/ID fallback rejected: msg=%+v err=%v", msg, err)
	}
}

func TestResponsesStreamCompleteParts(t *testing.T) {
	stream := responsesSSE(
		`{"type":"response.output_text.delta","output_index":1,"content_index":1,"delta":"stale"}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"hello "}`,
		`{"type":"response.output_text.done","output_index":1,"content_index":1,"text":"world"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"stale"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"text":"plan"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call-2","name":"shell","arguments":"{"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","delta":"\"x\":1}"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	)
	msg, _, native, err := responsesDetailedTestRound(t, stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "<think>plan</think>hello world" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"x":1}` || len(native) != 3 {
		t.Fatalf("msg=%+v native=%+v", msg, native)
	}
}

func TestResponsesThinkingOnlyRejected(t *testing.T) {
	for _, output := range [][]responsesOutputItem{
		{{Type: "reasoning", Summary: []responsesSummaryPart{{Type: "summary_text", Text: "plan"}}}},
		{{Type: "reasoning", EncryptedContent: "secret"}},
		{{Type: "message", Content: []responsesContent{{Type: "output_text", Text: "<think>plan</think>"}}}},
	} {
		resp := responsesResponse{Status: "completed", Output: output}
		if _, err := responsesReplyToOpenAI(resp); err == nil {
			t.Fatalf("thinking-only response accepted: %+v", resp)
		}
		final, _ := json.Marshal(map[string]any{"type": "response.completed", "response": resp})
		if _, _, _, err := responsesDetailedTestRound(t, responsesSSE(`{"type":"response.output_text.delta","delta":"stale answer"}`, string(final)), nil); err == nil {
			t.Fatal("streamed text hid authoritative thinking-only output")
		}
	}
	stream := responsesSSE(`{"type":"response.reasoning_summary_text.delta","delta":"plan"}`, `{"type":"response.completed","response":{"status":"completed","output":[]}}`)
	if _, _, _, err := responsesDetailedTestRound(t, stream, nil); err == nil {
		t.Fatal("thinking-only fallback accepted")
	}
}

func TestSplitResponsesInputStripsThinking(t *testing.T) {
	history := []openai.ChatCompletionMessage{
		{Role: "assistant", Content: "<think>secret</think>answer"},
		{Role: "assistant", Content: "<think>secret</think>"},
		{Role: "assistant", Content: "<think>secret</think>checking", ToolCalls: []openai.ToolCall{{ID: "call-1", Function: openai.FunctionCall{Name: "shell", Arguments: "{}"}}}},
	}
	_, input := splitResponsesInputNative(history, map[string][]responsesOutputItem{"unrelated": {{Type: "reasoning", EncryptedContent: "secret"}}})
	raw, _ := json.Marshal(input)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "think") || len(input) != 3 {
		t.Fatalf("thinking replayed: %s", raw)
	}
	if !strings.Contains(string(raw), "answer") || !strings.Contains(string(raw), "checking") {
		t.Fatalf("answer dropped: %s", raw)
	}
}
