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

	"github.com/keyle/valen/internal/guard"
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
	if cfg.BaseURL != defaultBaseURL || cfg.OpenCodeKey != "" || cfg.OpenRouterKey != "" || cfg.MaxRounds != defaultMaxRounds || cfg.MaxContextTokens != 0 || cfg.MaxTokens != defaultMaxTokens || cfg.MaxStagnantRounds != guard.DefaultMaxStagnantRounds || cfg.RequestTimeoutSeconds != 300 {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if contextBudget(cfg) != modelContextWindow("kimi-k3") {
		t.Fatalf("auto budget must resolve the model's window, got %d", contextBudget(cfg))
	}
	if !strings.HasPrefix(cfg.SessionID, "ses-") {
		t.Fatalf("bad session id %q", cfg.SessionID)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `model = "glm-5.3"
base_url = "https://example.test/v1"
opencode_key = "go-secret"
openrouter_key = "or-secret"
session_id = "ses-fixed"
max_rounds = 3
max_stagnant_rounds = 7
request_timeout_seconds = 12
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
	if cfg.BaseURL != "https://example.test/v1" || cfg.SessionID != "ses-fixed" || cfg.MaxRounds != 3 || cfg.MaxStagnantRounds != 7 || requestTimeout(cfg) != 12*time.Second {
		t.Fatalf("bad overrides: %+v", cfg)
	}
	if cfg.OpenCodeKey != "go-secret" || cfg.OpenRouterKey != "or-secret" {
		t.Fatalf("keys not loaded: %+v", cfg)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0 {
		t.Fatalf("temperature 0 must stay explicit: %+v", cfg.Temperature)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "fs" || cfg.MCPServers[0].Args[1] != "server" {
		t.Fatalf("bad mcp: %+v", cfg.MCPServers)
	}
}

// TestProviderRoutingConfig: provider_ignore/provider_only load, normalize,
// keep unset as nil, and reject being set together.
func TestProviderRoutingConfig(t *testing.T) {
	base, err := loadConfig(writeConfig(t, "model = \"openrouter/moonshotai/kimi-k3\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if base.ProviderIgnore != nil || base.ProviderOnly != nil {
		t.Fatalf("unset provider lists must stay nil: %+v", base)
	}
	cfg, err := loadConfig(writeConfig(t, `model = "openrouter/moonshotai/kimi-k3"
provider_ignore = [" BaseTen ", "baseten", "deepinfra"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProviderIgnore) != 2 || cfg.ProviderIgnore[0] != "baseten" || cfg.ProviderIgnore[1] != "deepinfra" {
		t.Fatalf("ignore not normalized/deduped: %v", cfg.ProviderIgnore)
	}
	if cfg.ProviderOnly != nil {
		t.Fatalf("only must stay nil: %v", cfg.ProviderOnly)
	}
	only, err := loadConfig(writeConfig(t, `model = "openrouter/moonshotai/kimi-k3"
provider_only = ["Anthropic"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(only.ProviderOnly) != 1 || only.ProviderOnly[0] != "anthropic" {
		t.Fatalf("only not normalized: %v", only.ProviderOnly)
	}
	if _, err := loadConfig(writeConfig(t, `model = "openrouter/moonshotai/kimi-k3"
provider_ignore = ["baseten"]
provider_only = ["anthropic"]
`)); err == nil {
		t.Fatal("both provider lists must be rejected")
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

func TestUserAgentIdentities(t *testing.T) {
	if !strings.HasPrefix(userAgent, "github.com/keyle/valen/") || !strings.Contains(userAgent, "opencode") {
		t.Fatalf("API UA must be an honest valen identity: %q", userAgent)
	}
	if !strings.Contains(browserUserAgent, "Mozilla") || browserUserAgent == userAgent {
		t.Fatalf("web UA must stay a browser string: %q", browserUserAgent)
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

func TestHeadlessChatPersistsSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv, _ := fakeServer(t, completion(nil, "saved answer"))
	registry := NewRegistry()
	cfg := newTestConfig(srv.URL)
	cfg.Model = "m"
	cfg.Thinking = "low"

	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "remember me", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	paths, err := listSessions()
	if err != nil || len(paths) != 1 {
		t.Fatalf("sessions=%v err=%v", paths, err)
	}
	turns, err := loadSession(paths[0])
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns=%v err=%v", turns, err)
	}
	turn := turns[0]
	if turn.Prompt != "remember me" || turn.Reply != "saved answer" || turn.Model != "m" || turn.Effort != "low" {
		t.Fatalf("turn=%+v", turn)
	}
	if len(turn.History) == 0 || turn.Usage.Total == 0 {
		t.Fatalf("missing history/usage: %+v", turn)
	}
	meta, err := loadSessionMeta(metaPath(paths[0]))
	if err != nil || meta.Title != "remember me" || meta.Model != "m" || meta.Effort != "low" {
		t.Fatalf("meta=%+v err=%v", meta, err)
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

// Fresh registry-capped tool outputs are preserved on the wire.
func TestChatSendMessagesBudget(t *testing.T) {
	long := strings.Repeat("z", maxToolOutput)
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: long},
	}
	sent := chatSendMessages(history)
	if sent[1].Content != long {
		t.Fatalf("wire copy must preserve fresh output: %d runes", len([]rune(sent[1].Content)))
	}
	if history[1].Content != long || sent[0].Content != "hi" {
		t.Fatal("stored history and non-tool messages must pass through")
	}
}

// File reads ride the wire verbatim up to the registry output cap.
func TestChatSendMessagesKeepsReads(t *testing.T) {
	read := "File TODO.md (140 lines, showing 1-140):\n" + strings.Repeat("x", maxToolOutput-100)
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call-1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read", Arguments: `{"path":"TODO.md"}`}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: read},
	}
	sent := chatSendMessages(history)
	if sent[2].Content != read {
		t.Fatalf("read must stay verbatim: %d vs %d runes", len([]rune(sent[2].Content)), len([]rune(read)))
	}
}

// TestRunToolCallsParallel verifies one round's independent calls overlap:
// 3×~30-50ms must finish well under the 110ms sequential sum, with history and
// completion events live even when the first call is slowest.
func TestRunToolCallsParallel(t *testing.T) {
	r := NewRegistry()
	for _, tc := range []struct {
		name string
		d    time.Duration
		out  string
	}{
		{"read", 50 * time.Millisecond, "A"},
		{"glob", 30 * time.Millisecond, "B"},
		{"grep", 30 * time.Millisecond, "C"},
	} {
		if err := r.Add(sleepTool(tc.name, tc.d, tc.out)); err != nil {
			t.Fatal(err)
		}
	}
	calls := []openai.ToolCall{
		openaiToolCall("id-a", "read"),
		openaiToolCall("id-b", "glob"),
		openaiToolCall("id-c", "grep"),
	}
	var events []Event
	start := time.Now()
	msgs := runToolCalls(context.Background(), r, calls, func(ev Event) { events = append(events, ev) })
	elapsed := time.Since(start)

	if elapsed >= 80*time.Millisecond {
		t.Fatalf("expected parallel overlap, took %v (sequential sum is 110ms)", elapsed)
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
	// Starts stay ordered; results arrive as completed, not held for slow read.
	var kinds []string
	var names []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		names = append(names, ev.Text)
	}
	wantKinds := []string{"tool_start", "tool_start", "tool_start", "tool_result", "tool_result", "tool_result"}
	if strings.Join(kinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("event kinds=%v", kinds)
	}
	if strings.Join(names[:3], ",") != "read,glob,grep" || names[3] == "read" || names[4] == "read" || names[5] != "read" || names[3] == names[4] {
		t.Fatalf("event names=%v", names)
	}
}

// Unknown MCP calls conservatively serialize, including across servers.
func TestRunToolCallsMCPSerial(t *testing.T) {
	r := NewRegistry()
	for _, tc := range []struct {
		name string
		d    time.Duration
		out  string
	}{
		{"slow__x", 50 * time.Millisecond, "X"},
		{"slow__y", 50 * time.Millisecond, "Y"},
		{"fast__z", 50 * time.Millisecond, "Z"},
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
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
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
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("unknown cross-server calls must serialize, took %v", elapsed)
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

// TestRunTurnExtendsAfterMaxRounds: exhausting the round window compacts
// send-side and grants exactly one more window instead of dying — the
// scripted 5th response (2 tool rounds, summary, 1 tool round, answer)
// must all be consumed with no error.
func TestRunTurnExtendsAfterMaxRounds(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("read", `{"path":"a"}`), ""),
		completion(toolCall("read", `{"path":"b"}`), ""),
		completion(nil, "summary text"),
		completion(toolCall("read", `{"path":"c"}`), ""),
		completion(nil, "final answer"),
	)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"read"})
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 2
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 200}, contextBudget(cfg))
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "first job"},
		{Role: openai.ChatMessageRoleUser, Content: "second job"},
	}
	msg, _, err := runTurn(context.Background(), cfg, newClient(cfg, "key"), "key", newHTTPClient(cfg), registry, &history, nil)
	if err != nil {
		t.Fatalf("extended turn must finish, err=%v", err)
	}
	if _, answer := splitThinking(msg.Content); answer != "final answer" {
		t.Fatalf("reply=%q", msg.Content)
	}
	if *calls != 5 {
		t.Fatalf("calls=%d, want 5 (2 rounds + summary + 2 rounds)", *calls)
	}
	if len(history) < 2 || !strings.HasPrefix(history[1].Content, compactionSummaryPrefix) {
		t.Fatalf("history must carry the compacted summary second, got %d msgs", len(history))
	}
}

// TestRunTurnNoExtendWithoutCompactableHead: a lone oversized turn has no
// compactable head, so the backstop stays terminal with no extra requests.
func TestRunTurnNoExtendWithoutCompactableHead(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("read", `{"path":"a"}`), ""),
		completion(toolCall("read", `{"path":"b"}`), ""),
		completion(nil, "unreached"),
	)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"read"})
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 2
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{}, contextBudget(cfg))
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "only turn"},
	}
	_, _, err := runTurn(context.Background(), cfg, newClient(cfg, "key"), "key", newHTTPClient(cfg), registry, &history, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped after 2 rounds") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "compacted windows") {
		t.Fatalf("no head means no extension: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("calls=%d, want 2 (no summary, no second window)", *calls)
	}
}

func TestOpenRouterMetadataParse(t *testing.T) {
	// Unary body: provider + billed cost.
	var provider string
	var cost float64
	var hasCost bool
	parseOpenRouterMeta([]byte(`{"provider":"Infermatic","usage":{"cost":0.0012}}`), &provider, &cost, &hasCost)
	if provider != "Infermatic" || !hasCost || cost != 0.0012 {
		t.Fatalf("unary meta provider=%q cost=%v has=%v", provider, cost, hasCost)
	}

	// SSE usage chunk: provider plus upstream cost detail; usage.cost wins.
	provider, cost, hasCost = "", 0, false
	parseOpenRouterMeta([]byte(`{"provider":"Groq","usage":{"cost":0.002,"cost_details":{"upstream_inference_cost":0.003}}}`), &provider, &cost, &hasCost)
	if provider != "Groq" || !hasCost || cost != 0.002 {
		t.Fatalf("sse meta provider=%q cost=%v has=%v", provider, cost, hasCost)
	}

	// Missing usage.cost falls back to the upstream inference cost.
	provider, cost, hasCost = "", 0, false
	parseOpenRouterMeta([]byte(`{"usage":{"cost_details":{"upstream_inference_cost":0.003}}}`), &provider, &cost, &hasCost)
	if !hasCost || cost != 0.003 {
		t.Fatalf("fallback cost=%v has=%v", cost, hasCost)
	}

	// A body with neither provider nor cost leaves the targets untouched.
	provider, cost, hasCost = "keep", 1.5, true
	parseOpenRouterMeta([]byte(`{"choices":[]}`), &provider, &cost, &hasCost)
	if provider != "keep" || cost != 1.5 || !hasCost {
		t.Fatalf("clean body clobbered targets: %q %v %v", provider, cost, hasCost)
	}

	// Catalog price parsing tolerates strings, numbers and junk.
	if got := orPrice(`"0.0000015"`); got != 0.0000015 {
		t.Fatalf("orPrice string=%v", got)
	}
	if got := orPrice(`0.0000004`); got != 0.0000004 {
		t.Fatalf("orPrice number=%v", got)
	}
	if got := orPrice(`"bogus"`); got != 0 {
		t.Fatalf("orPrice junk=%v", got)
	}
}
