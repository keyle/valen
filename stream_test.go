package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func sseServer(t *testing.T, bodies *[]string, rounds ...string) *httptest.Server {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(raw))
		i := calls
		if i >= len(rounds) {
			i = len(rounds) - 1
		}
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(rounds[i]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatTextStream(text string) string {
	return "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":" + jsonQuote(text) + "},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestChatStreamsTextDeltas(t *testing.T) {
	var bodies []string
	srv := sseServer(t, &bodies,
		"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n",
	)
	registry := NewRegistry()
	cfg := newTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	var deltas []string
	onEvent := func(ev Event) {
		if ev.Kind == "text_delta" {
			deltas = append(deltas, ev.Text)
		}
	}
	reply, usage, err := runChatTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, onEvent)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "Hello" {
		t.Fatalf("reply=%q", reply.Content)
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas=%q", deltas)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Fatalf("usage=%+v", usage)
	}
	if !strings.Contains(bodies[0], `"stream":true`) {
		t.Fatalf("stream not requested: %s", bodies[0])
	}
}

func TestChatStreamsToolCalls(t *testing.T) {
	toolRound := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":" + jsonQuote(`{"command":"echo hi"}`) + "}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	var bodies []string
	srv := sseServer(t, &bodies, toolRound, chatTextStream("done"))
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	var starts, results []Event
	onEvent := func(ev Event) {
		switch ev.Kind {
		case "tool_start":
			starts = append(starts, ev)
		case "tool_result":
			results = append(results, ev)
		}
	}
	reply, _, err := runChatTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, onEvent)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "done" {
		t.Fatalf("reply=%q", reply.Content)
	}
	if len(starts) != 1 || !strings.Contains(starts[0].Args, "echo hi") {
		t.Fatalf("starts=%+v", starts)
	}
	if len(results) != 1 || !strings.Contains(results[0].Output, "hi") {
		t.Fatalf("results=%+v", results)
	}
}

func TestChatFallsBackWhenStreamRejected(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if stream, _ := body["stream"].(bool); stream {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"streaming is not supported for this model","type":"invalid_request"}}`))
			return
		}
		if calls == 2 {
			w.Write(completion(toolCall("shell", `{"command":"echo hi"}`), ""))
		} else {
			w.Write(completion(nil, "all done"))
		}
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	reply, _, err := runChatTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "all done" {
		t.Fatalf("reply=%q", reply.Content)
	}
	if calls != 4 {
		t.Fatalf("calls=%d, want stream attempt + unary tool round + stream attempt + final", calls)
	}
}

func TestMessagesStreamsTextAndUsage(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"all \"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var bodies []string
	srv := sseServer(t, &bodies, stream)
	registry := NewRegistry()
	cfg := messagesTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	var deltas []string
	onEvent := func(ev Event) {
		if ev.Kind == "text_delta" {
			deltas = append(deltas, ev.Text)
		}
	}
	reply, usage, err := runMessagesTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, onEvent)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "all done" {
		t.Fatalf("reply=%q", reply.Content)
	}
	if strings.Join(deltas, "") != "all done" {
		t.Fatalf("deltas=%q", deltas)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Fatalf("usage=%+v", usage)
	}
	if !strings.Contains(bodies[0], `"stream":true`) {
		t.Fatalf("stream not requested: %s", bodies[0])
	}
}

func TestMessagesStreamsToolUse(t *testing.T) {
	toolRound := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"shell\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + jsonQuote(`{"command":`) + "}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + jsonQuote(`"echo hi"}`) + "}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	final := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var bodies []string
	srv := sseServer(t, &bodies, toolRound, final)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := messagesTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	reply, _, err := runMessagesTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "done" {
		t.Fatalf("reply=%q", reply.Content)
	}
	found := false
	for _, m := range history {
		if m.Role == openai.ChatMessageRoleTool && strings.Contains(m.Content, "hi") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result missing from history: %+v", history)
	}
}

func TestResponsesStreamsTextAndUsage(t *testing.T) {
	completed := `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"all done"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"all \"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"done\"}\n\nevent: response.completed\ndata: " + completed + "\n\n"
	var bodies []string
	srv := sseServer(t, &bodies, stream)
	registry := NewRegistry()
	cfg := responsesTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	var deltas []string
	onEvent := func(ev Event) {
		if ev.Kind == "text_delta" {
			deltas = append(deltas, ev.Text)
		}
	}
	reply, usage, err := runResponsesTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, onEvent)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "all done" {
		t.Fatalf("reply=%q", reply.Content)
	}
	if strings.Join(deltas, "") != "all done" {
		t.Fatalf("deltas=%q", deltas)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage=%+v", usage)
	}
	if !strings.Contains(bodies[0], `"stream":true`) {
		t.Fatalf("stream not requested: %s", bodies[0])
	}
}

func TestResponsesStreamsFunctionCall(t *testing.T) {
	callRound := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_call-1\",\"call_id\":\"call-1\",\"name\":\"shell\",\"arguments\":\"\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":" + jsonQuote(`{"command":"echo hi"}`) + "}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"
	final := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"done\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	var bodies []string
	srv := sseServer(t, &bodies, callRound, final)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := responsesTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	reply, _, err := runResponsesTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "done" {
		t.Fatalf("reply=%q", reply.Content)
	}
	found := false
	for _, m := range history {
		if m.Role == openai.ChatMessageRoleTool && m.ToolCallID == "call-1" && strings.Contains(m.Content, "hi") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result missing from history: %+v", history)
	}
}

func TestTUIPendingShowsSequentialToolsAndStream(t *testing.T) {
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	m.busy = true
	m.pendingPrompt = "search please"
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_search", args: `{"query":"go"}`})
	_, _ = m.Update(toolEventMsg{kind: "text_delta", text: "Checking "})
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_search", output: "1. release notes"})
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_fetch", args: `{"url":"https://x"}`})

	out := m.renderPending(80)
	searchIdx := strings.Index(out, "web_search")
	fetchIdx := strings.Index(out, "web_fetch")
	if searchIdx < 0 || fetchIdx < 0 || searchIdx > fetchIdx {
		t.Fatalf("tools not sequential:\n%s", out)
	}
	for _, want := range []string{`{"query":"go"}`, "1. release notes", "Checking ", "web_fetch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pending missing %q:\n%s", want, out)
		}
	}

	_, _ = m.Update(turnDoneMsg{prompt: "search please", answer: "ok", history: []openai.ChatCompletionMessage{}})
	if len(m.pendingTools) != 0 || m.pendingText != "" {
		t.Fatalf("pending state not cleared: %+v %q", m.pendingTools, m.pendingText)
	}
}

func TestTUIPendingTextDeltasRenderOnOwnLines(t *testing.T) {
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	m.busy = true
	m.pendingPrompt = "tell me more"
	_, _ = m.Update(toolEventMsg{kind: "text_delta", text: "Checking "})
	_, _ = m.Update(toolEventMsg{kind: "text_delta", text: "release notes"})
	_, _ = m.Update(toolEventMsg{kind: "text_delta", text: "done"})

	if !strings.Contains(m.pendingText, "Checking \nrelease notes\ndone") {
		t.Fatalf("each text delta should start its own line, got %q", m.pendingText)
	}
	out := m.renderPending(80)
	checkIdx := strings.Index(out, "Checking ")
	notesIdx := strings.Index(out, "release notes")
	doneIdx := strings.Index(out, "done")
	if checkIdx < 0 || notesIdx < 0 || doneIdx < 0 || !(checkIdx < notesIdx && notesIdx < doneIdx) {
		t.Fatalf("pending deltas not in order:\n%s", out)
	}
	if tail := out[checkIdx:doneIdx]; !strings.Contains(tail, "\n") {
		t.Fatalf("pending deltas should render on separate lines:\n%s", out)
	}
}

// TestPostAPIRetriesTransientThenSucceeds: a 429 followed by 200 returns the
// 200 without surfacing the transient failure, proving backoff retry.
func TestPostAPIRetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	resp, err := postAPI(context.Background(), srv.Client(), cfg, srv.URL+"/chat/completions", map[string]any{"model": "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 1 retry then success", calls)
	}
}

// TestPostAPINoRetryOn400: client errors other than 429/5xx return
// immediately so stream-rejection fallback still sees a single 400.
func TestPostAPINoRetryOn400(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"streaming is not supported"}}`))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	resp, err := postAPI(context.Background(), srv.Client(), cfg, srv.URL+"/chat/completions", map[string]any{"model": "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, 400 must not retry", calls)
	}
}
