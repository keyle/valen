package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"kimi-k3\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != defaultBaseURL || cfg.APIKeyEnv != defaultAPIKeyEnv || cfg.MaxRounds != defaultMaxRounds || cfg.MaxContextTokens != defaultMaxContextTokens || cfg.MaxTokens != defaultMaxTokens || cfg.MaxStagnantRounds != defaultMaxStagnantRounds {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if !strings.HasPrefix(cfg.SessionID, "ses-") {
		t.Fatalf("bad session id %q", cfg.SessionID)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `model = "glm-5.3"
base_url = "https://example.test/v1"
api_key_env = "MY_KEY"
session_id = "ses-fixed"
max_rounds = 3
max_stagnant_rounds = 7
temperature = 0.0
tools = ["shell"]

[[mcp]]
name = "fs"
cmd = "npx"
args = ["-y", "server"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://example.test/v1" || cfg.SessionID != "ses-fixed" || cfg.MaxRounds != 3 || cfg.MaxStagnantRounds != 7 {
		t.Fatalf("bad overrides: %+v", cfg)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0 {
		t.Fatalf("temperature 0 must stay explicit: %+v", cfg.Temperature)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "fs" || cfg.MCPServers[0].Args[1] != "server" {
		t.Fatalf("bad mcp: %+v", cfg.MCPServers)
	}
}

func TestLoadConfigRejectsMissingModel(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, "base_url = \"x\"\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsIncompleteMCP(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, "model = \"m\"\n[[mcp]]\nname = \"fs\"\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestHeaderTransportStampsIdentity(t *testing.T) {
	var gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := (&headerTransport{base: http.DefaultTransport, sessionID: "ses-test"}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotSession != "ses-test" || gotUA != userAgent {
		t.Fatalf("session=%q ua=%q", gotSession, gotUA)
	}
}

func completion(toolCalls []map[string]any, content string) []byte {
	msg := map[string]any{"role": "assistant", "content": content}
	if toolCalls != nil {
		msg["content"] = ""
		msg["tool_calls"] = toolCalls
	}
	b, _ := json.Marshal(map[string]any{
		"id": "1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	return b
}

func toolCall(name, args string) []map[string]any {
	return []map[string]any{{"id": "call-1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
}

func fakeServer(t *testing.T, responses ...[]byte) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		i := min(calls, len(responses)-1)
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(responses[i])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestConfig(baseURL string) Config {
	return Config{Model: "m", BaseURL: baseURL, SessionID: "ses-test", MaxRounds: 4}
}

func TestChatRunsToolLoop(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("shell", `{"command":"echo hi"}`), ""),
		completion(nil, "all done"),
	)
	registry := NewRegistry()
	if unknown := registry.LoadBuiltins([]string{"shell"}); len(unknown) != 0 {
		t.Fatalf("unknown tools: %v", unknown)
	}
	cfg := newTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "all done") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[tool shell]") || !strings.Contains(stderr.String(), "tokens in=20 out=10") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if *calls != 2 {
		t.Fatalf("calls=%d", *calls)
	}
}

func TestChatFeedsToolFailureBackToModel(t *testing.T) {
	srv, _ := fakeServer(t,
		completion(toolCall("shell", `{"command":"exit 3"}`), ""),
		completion(nil, "reported"),
	)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "reported") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestChatStopsAtMaxRounds(t *testing.T) {
	srv, _ := fakeServer(t, completion(toolCall("shell", `{"command":"true"}`), ""))
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 2

	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "2 rounds") {
		t.Fatalf("err=%v", err)
	}
}

// TestRunTurnStopsDoomLoop: the same single tool call a third consecutive
// time aborts the turn instead of executing again — no 4th request.
func TestRunTurnStopsDoomLoop(t *testing.T) {
	same := completion(toolCall("shell", `{"command":"true"}`), "")
	srv, calls := fakeServer(t, same, same, same, completion(nil, "done"))
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)

	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "identical input") {
		t.Fatalf("err=%v", err)
	}
	if *calls != 3 {
		t.Fatalf("calls=%d, want 3 (abort before a 4th request)", *calls)
	}
}

func TestDoomLoopToolShapes(t *testing.T) {
	call := func(id, args string) openai.ToolCall {
		return openai.ToolCall{ID: id, Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "read", Arguments: args}}
	}
	asst := func(calls ...openai.ToolCall) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: calls}
	}
	twoPrior := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		asst(call("1", `{"path":"a"}`)),
		{Role: openai.ChatMessageRoleTool, ToolCallID: "1", Content: "A"},
		asst(call("2", `{"path":"a"}`)),
		{Role: openai.ChatMessageRoleTool, ToolCallID: "2", Content: "A"},
	}
	if got := doomLoopTool(twoPrior, []openai.ToolCall{call("3", `{"path":"a"}`)}); got != "read" {
		t.Fatalf("third identical repeat must trip, got %q", got)
	}
	if got := doomLoopTool(twoPrior, []openai.ToolCall{call("3", `{"path":"b"}`)}); got != "" {
		t.Fatalf("different args must not trip, got %q", got)
	}
	if got := doomLoopTool(twoPrior, []openai.ToolCall{call("3", `{"path":"a"}`), call("4", `{"path":"a"}`)}); got != "" {
		t.Fatalf("multi-call batches break the chain, got %q", got)
	}
	onePrior := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		asst(call("1", `{"path":"a"}`)),
		{Role: openai.ChatMessageRoleTool, ToolCallID: "1", Content: "A"},
	}
	if got := doomLoopTool(onePrior, []openai.ToolCall{call("2", `{"path":"a"}`)}); got != "" {
		t.Fatalf("second repeat is still allowed, got %q", got)
	}
}

// TestRunTurnRejectsOversizeContext: a bloated history fail-fasts before any
// request leaves the process — the server must see zero calls.
func TestRunTurnRejectsOversizeContext(t *testing.T) {
	srv, calls := fakeServer(t, completion(nil, "unreached"))
	cfg := newTestConfig(srv.URL)
	cfg.MaxContextTokens = 100
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("x", 1000)},
	}
	_, _, err := runTurn(context.Background(), cfg, newClient(cfg, "key"), "key", newHTTPClient(cfg), NewRegistry(), &history, nil)
	if err == nil || !strings.Contains(err.Error(), "context too large") {
		t.Fatalf("err=%v", err)
	}
	if *calls != 0 {
		t.Fatalf("calls=%d, want 0 (fail fast, good citizen)", *calls)
	}
}

func TestChatRejectsEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[]}`))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("err=%v", err)
	}
}

func TestChatSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"message":"Insufficient account funds","type":"billing"}}`))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "api:") {
		t.Fatalf("err=%v", err)
	}
}

func TestChatSendsSystemPromptAndTools(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell", "web_search"})
	cfg := newTestConfig(srv.URL)
	cfg.System = "be brief"

	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Fatalf("first message=%v", first)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%v", tools)
	}
}

func TestChatOmitsTemperatureWhenUnset(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, present := gotBody["temperature"]; present {
		t.Fatalf("temperature should be omitted, got %v", gotBody["temperature"])
	}
}

func sleepTool(name string, d time.Duration, out string) Tool {
	return Tool{
		Name:        name,
		Description: "test sleeper",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			select {
			case <-time.After(d):
				return out, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
}

func openaiToolCall(id, name string) openai.ToolCall {
	return openai.ToolCall{
		ID:   id,
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      name,
			Arguments: "{}",
		},
	}
}

// TestRequestEncodingStable locks the stable-prefix property: identical
// history + registry must marshal to identical request bytes, so prompt-cache
// affinity isn't broken by per-round churn (tool order, system join).
func TestRequestEncodingStable(t *testing.T) {
	r := NewRegistry()
	r.LoadBuiltins([]string{"shell", "read", "grep"})
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: "ok", ToolCalls: []openai.ToolCall{openaiToolCall("1", "shell")}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "1", Content: "out"},
	}
	marshal := func() string {
		req := openai.ChatCompletionRequest{Model: "m", Messages: history, Tools: r.OpenAITools()}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	a, b := marshal(), marshal()
	if a != b {
		t.Fatal("request encoding must be deterministic")
	}
	sys1, msgs1 := splitChatHistory(history)
	sys2, msgs2 := splitChatHistory(history)
	j1, _ := json.Marshal(map[string]any{"system": sys1, "messages": msgs1})
	j2, _ := json.Marshal(map[string]any{"system": sys2, "messages": msgs2})
	if string(j1) != string(j2) {
		t.Fatal("messages history conversion must be deterministic")
	}
	_, input1 := splitResponsesInput(history)
	_, input2 := splitResponsesInput(history)
	k1, _ := json.Marshal(input1)
	k2, _ := json.Marshal(input2)
	if string(k1) != string(k2) {
		t.Fatal("responses input conversion must be deterministic")
	}
}

// TestEstimateCountsEnvelopeAndSchemas: the estimator sees per-message
// envelope overhead plus the re-sent tool-schema block, so the budget guard
// trips on real prefill instead of content alone.
func TestEstimateCountsEnvelopeAndSchemas(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleUser, Content: "yo"},
	}
	if got := estimateTokens(msgs); got != 2*messageOverheadTokens+1 {
		t.Fatalf("est=%d, want envelope overhead", got)
	}
	r := NewRegistry()
	r.LoadBuiltins([]string{"shell", "read", "grep"})
	if got := requestToolsTokens(r); got < 100 {
		t.Fatalf("schema block must be substantial: %d", got)
	}
	if got := requestToolsTokens(nil); got != 0 {
		t.Fatalf("nil registry must cost nothing: %d", got)
	}
	cfg := newTestConfig("http://127.0.0.1:1")
	cfg.MaxContextTokens = estimateTokens(msgs) + requestToolsTokens(r)
	if err := checkContextBudget(cfg, msgs, requestToolsTokens(r)); err != nil {
		t.Fatalf("at-budget must pass: %v", err)
	}
	cfg.MaxContextTokens--
	if err := checkContextBudget(cfg, msgs, requestToolsTokens(r)); err == nil {
		t.Fatal("over-budget must fail fast")
	}
}

// TestChatSendMessagesBudget: the chat wire copy shrinks tool results to the
// send budget while stored history keeps full outputs.
func TestChatSendMessagesBudget(t *testing.T) {
	long := strings.Repeat("z", sendToolOutputChars+1000)
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: long},
	}
	sent := chatSendMessages(history)
	if len([]rune(sent[1].Content)) > sendToolOutputChars+200 || !strings.Contains(sent[1].Content, "send-truncated") {
		t.Fatalf("wire copy must shrink: %d runes", len([]rune(sent[1].Content)))
	}
	if history[1].Content != long || sent[0].Content != "hi" {
		t.Fatal("stored history and non-tool messages must pass through")
	}
}

// TestRunToolCallsParallel verifies one round's independent calls overlap:
// 3×~100ms must finish well under the 350ms sequential sum, with history and
// events in request order even when the first call is slowest.
func TestRunToolCallsParallel(t *testing.T) {
	r := NewRegistry()
	for _, tc := range []struct {
		name string
		d    time.Duration
		out  string
	}{
		{"tool_a", 150 * time.Millisecond, "A"},
		{"tool_b", 100 * time.Millisecond, "B"},
		{"tool_c", 100 * time.Millisecond, "C"},
	} {
		if err := r.Add(sleepTool(tc.name, tc.d, tc.out)); err != nil {
			t.Fatal(err)
		}
	}
	calls := []openai.ToolCall{
		openaiToolCall("id-a", "tool_a"),
		openaiToolCall("id-b", "tool_b"),
		openaiToolCall("id-c", "tool_c"),
	}
	var events []Event
	start := time.Now()
	msgs := runToolCalls(context.Background(), r, calls, func(ev Event) { events = append(events, ev) })
	elapsed := time.Since(start)

	if elapsed >= 300*time.Millisecond {
		t.Fatalf("expected parallel overlap, took %v (sequential sum is 350ms)", elapsed)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages=%d", len(msgs))
	}
	for i, want := range []struct{ id, content string }{
		{"id-a", "A"}, {"id-b", "B"}, {"id-c", "C"},
	} {
		if msgs[i].ToolCallID != want.id || msgs[i].Content != want.content {
			t.Fatalf("msgs[%d]=%+v, want id=%s content=%s", i, msgs[i], want.id, want.content)
		}
		if msgs[i].Role != openai.ChatMessageRoleTool {
			t.Fatalf("msgs[%d].Role=%q", i, msgs[i].Role)
		}
	}
	// Events stay in request order: all starts, then all results.
	var kinds []string
	var names []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		names = append(names, ev.Text)
	}
	wantKinds := []string{"tool_start", "tool_start", "tool_start", "tool_result", "tool_result", "tool_result"}
	wantNames := []string{"tool_a", "tool_b", "tool_c", "tool_a", "tool_b", "tool_c"}
	if strings.Join(kinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("event kinds=%v", kinds)
	}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("event names=%v", names)
	}
}

// TestRunToolCallsMCPPerServerLock: same-server MCP calls serialize on one
// stdin pipe while different servers overlap — a slow server must not stall
// another server's batch.
func TestRunToolCallsMCPPerServerLock(t *testing.T) {
	r := NewRegistry()
	for _, tc := range []struct {
		name string
		d    time.Duration
		out  string
	}{
		{"slow__x", 200 * time.Millisecond, "X"},
		{"slow__y", 200 * time.Millisecond, "Y"},
		{"fast__z", 200 * time.Millisecond, "Z"},
	} {
		if err := r.Add(sleepTool(tc.name, tc.d, tc.out)); err != nil {
			t.Fatal(err)
		}
	}
	same := []openai.ToolCall{
		openaiToolCall("id-x", "slow__x"),
		openaiToolCall("id-y", "slow__y"),
	}
	start := time.Now()
	msgs := runToolCalls(context.Background(), r, same, nil)
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("same-server calls must serialize, took %v", elapsed)
	}
	if len(msgs) != 2 || msgs[0].Content != "X" || msgs[1].Content != "Y" {
		t.Fatalf("msgs=%+v", msgs)
	}

	mixed := []openai.ToolCall{
		openaiToolCall("id-x", "slow__x"),
		openaiToolCall("id-z", "fast__z"),
	}
	start = time.Now()
	msgs = runToolCalls(context.Background(), r, mixed, nil)
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("cross-server calls must overlap, took %v", elapsed)
	}
	if len(msgs) != 2 || msgs[0].Content != "X" || msgs[1].Content != "Z" {
		t.Fatalf("msgs=%+v", msgs)
	}
}

// TestRunToolCallsAskSerial verifies batches containing AskUserQuestion still
// run sequentially through the stubbed prompter with ordered output.
func TestRunToolCallsAskSerial(t *testing.T) {
	old := askPrompter
	askPrompter = func(ctx context.Context, qs []AskQuestion) ([]string, error) {
		return []string{"blue"}, nil
	}
	defer func() { askPrompter = old }()

	r := NewRegistry()
	if err := r.Add(sleepTool("tool_a", 10*time.Millisecond, "A")); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(AskUserQuestionTool()); err != nil {
		t.Fatal(err)
	}
	calls := []openai.ToolCall{
		{
			ID:   "id-q",
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      "AskUserQuestion",
				Arguments: `{"questions":[{"question":"color?","options":[{"label":"blue"}]}]}`,
			},
		},
		openaiToolCall("id-a", "tool_a"),
	}
	var events []Event
	msgs := runToolCalls(context.Background(), r, calls, func(ev Event) { events = append(events, ev) })
	if len(msgs) != 2 {
		t.Fatalf("messages=%d", len(msgs))
	}
	if msgs[0].ToolCallID != "id-q" || !strings.Contains(msgs[0].Content, "blue") {
		t.Fatalf("ask output=%+v", msgs[0])
	}
	if msgs[1].ToolCallID != "id-a" || msgs[1].Content != "A" {
		t.Fatalf("tool output=%+v", msgs[1])
	}
	if len(events) != 4 {
		t.Fatalf("events=%v", events)
	}
	if events[0].Kind != "tool_start" || events[0].Text != "AskUserQuestion" {
		t.Fatalf("first event=%+v", events[0])
	}
	if events[2].Kind != "tool_start" || events[2].Text != "tool_a" {
		t.Fatalf("third event=%+v", events[2])
	}
}
