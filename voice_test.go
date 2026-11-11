package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/keyle/valen/internal/prompts"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/sashabaranov/go-openai"
)

// extra_body is merged into the chat request body, and valen's own fields win.
func TestExtraBodyReachesChatRequest(t *testing.T) {
	var bodies []string
	srv := sseServer(t, &bodies, chatTextStream("ok"))
	cfg := newTestConfig(srv.URL)
	cfg.ExtraBody = map[string]any{"repetition_penalty": 1.1}
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	if _, _, err := runChatTurn(context.Background(), cfg, nil, "key", srv.Client(), NewRegistry(), &history, nil); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if got := body["repetition_penalty"]; got != 1.1 {
		t.Fatalf("repetition_penalty=%v body=%s", got, bodies[0])
	}
	if body["model"] != "m" || body["stream"] != true {
		t.Fatalf("valen fields lost: %s", bodies[0])
	}
}

func TestExtraBodyRejectsReservedAndNonChatTransport(t *testing.T) {
	cases := map[string]string{
		"reserved key":       "model = \"kimi-k3\"\n[extra_body]\nmessages = []\n",
		"messages transport": "model = \"claude-sonnet-4\"\n[extra_body]\nrepetition_penalty = 1.1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), "extra_body") {
				t.Fatalf("err=%v, want extra_body rejection", err)
			}
		})
	}
}

func TestExtraBodyLoadsOnChatTransport(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"kimi-k3\"\n[extra_body]\nrepetition_penalty = 1.1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExtraBody["repetition_penalty"] != 1.1 {
		t.Fatalf("extra_body=%v", cfg.ExtraBody)
	}
}

// --history takes only user and assistant messages, and drops anything else
// in them (tool calls would have no matching results).
func TestLoadHistoryFileKeepsOnlyUserAndAssistant(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	msgs, err := loadHistoryFile(write("ok.json", `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello","tool_calls":[{"id":"c"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].Content != "hello" || msgs[1].ToolCalls != nil {
		t.Fatalf("msgs=%+v", msgs)
	}
	for _, role := range []string{"system", "tool"} {
		body := fmt.Sprintf(`[{"role":%q,"content":"x"}]`, role)
		if _, err := loadHistoryFile(write(role+".json", body)); err == nil || !strings.Contains(err.Error(), role) {
			t.Fatalf("role %s: err=%v", role, err)
		}
	}
}

// The default run keeps the interactive and delegating tools; the voice and
// job switches remove them, even when the config tools list names them.
func TestBuildRegistryToolSet(t *testing.T) {
	warn := func(string, ...any) {}
	ctx := context.Background()

	reg, closers, err := buildRegistry(ctx, Config{}, "key", toolSet{}, warn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(closers)
	if want := []string{"AskUserQuestion", "todowrite", "todoread", "task"}; !slices.Equal(reg.Names(), want) {
		t.Fatalf("default names=%v", reg.Names())
	}

	cfg := Config{Tools: []string{"AskUserQuestion", "todowrite", "todoread", "read"}}
	reg, closers, err = buildRegistry(ctx, cfg, "key", toolSet{noQuestion: true, noTask: true, noTodo: true}, warn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(closers)
	if want := []string{"read"}; !slices.Equal(reg.Names(), want) {
		t.Fatalf("voice names=%v", reg.Names())
	}
}

// voiceFixtureServer is this test binary acting as the voice MCP stub: it
// serves the delegate and job_control tools with the schema the backend will
// validate against.
func voiceFixtureServer() MCPServer {
	return MCPServer{Name: "voice", Cmd: os.Args[0], Env: map[string]string{fixtureEnv: "voice"}}
}

func serveVoiceFixture() error {
	s := server.NewMCPServer("voice-fixture", "0.0.1")
	s.AddTool(mcp.NewToolWithRawSchema("delegate", "Hand a bounded task to a background job.", json.RawMessage(`{
		"type":"object",
		"properties":{
			"task":{"type":"string"},
			"kind":{"type":"string","enum":["search","research","code","other"]},
			"access":{"type":"string","enum":["read","write"]},
			"scope":{"type":"array","items":{"type":"string"},"description":"required when access is write"}
		},
		"required":["task","kind","access"]
	}`)), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("dispatched"), nil
	})
	s.AddTool(mcp.NewToolWithRawSchema("job_control", "Check, refine or cancel a job.", json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["status","refine","cancel"]},
			"job_id":{"type":"string"}
		},
		"required":["action","job_id"]
	}`)), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("job " + req.GetString("job_id", "")), nil
	})
	return server.ServeStdio(s)
}

// The voice profile's registry is exactly the read-only builtins plus the MCP
// tools, and in plan mode (used by read-only jobs) the MCP tools refuse while
// the read-only builtins keep working.
func TestVoiceRegistryIsExactAndReadOnlyInPlanMode(t *testing.T) {
	cfg := Config{
		Tools:      []string{"read", "glob", "grep", "web_search", "web_fetch"},
		MCPServers: []MCPServer{voiceFixtureServer()},
	}
	warns, warn := collectWarns()
	registry, closers, err := buildRegistry(context.Background(), cfg, "key", toolSet{noQuestion: true, noTask: true, noTodo: true}, warn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(closers)
	if len(*warns) != 0 {
		t.Fatalf("unexpected warnings: %v", *warns)
	}
	want := []string{"read", "glob", "grep", "web_search", "web_fetch", "voice__delegate", "voice__job_control"}
	if got := registry.Names(); !slices.Equal(got, want) {
		t.Fatalf("names=%v", got)
	}

	for _, tool := range registry.OpenAITools() {
		if tool.Function.Name != "voice__delegate" {
			continue
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(mustMarshal(t, tool.Function.Parameters), &schema); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(schema.Required, []string{"task", "kind", "access"}) {
			t.Fatalf("delegate required=%v", schema.Required)
		}
	}

	if out := registry.Run(context.Background(), "voice__job_control", `{"action":"status","job_id":"j1"}`); strings.TrimSpace(out) != "job j1" {
		t.Fatalf("job_control out=%q", out)
	}
	plan := prompts.WithPlanMode(context.Background())
	if out := registry.Run(plan, "voice__delegate", `{"task":"x","kind":"search","access":"read"}`); !strings.Contains(out, "plan mode") {
		t.Fatalf("MCP tool ran in plan mode: %q", out)
	}
	if out := registry.Run(plan, "glob", `{"pattern":"voice_test.go"}`); strings.Contains(out, "plan mode") {
		t.Fatalf("read-only builtin refused in plan mode: %q", out)
	}
}

// Child-process CLI runs: the test binary re-enters start() with the given
// valen arguments, so the real flag parsing, registry and headless path run
// against the fake SSE server. Parent tests set voiceCLIEnv and pass the
// arguments after "--".
const voiceCLIEnv = "VALEN_VOICE_CLI"

func TestVoiceCLIHelper(t *testing.T) {
	if os.Getenv(voiceCLIEnv) != "1" {
		t.Skip("child-process helper for the CLI tests")
	}
	args := os.Args
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Args = append([]string{"valen"}, args...)
	if err := start(); err != nil {
		fmt.Fprintln(os.Stderr, "valen:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runValenCLIFull(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestVoiceCLIHelper$", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), voiceCLIEnv+"=1", openCodeKeyEnv+"=test-key")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), err
}

func runValenCLI(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	stdout, _, err := runValenCLIFull(t, dir, args...)
	return stdout, err
}

// TestHeadlessFollowsConfigOverState: -p sends the model from valen.toml, not
// the TUI's last selection in state.json (the overlay is TUI-only).
func TestHeadlessFollowsConfigOverState(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "valen")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record, body := cliBodies(t)
	srv := sseServerWith(t, record, chatTextStream("ok"))
	cfgTOML := fmt.Sprintf("model = \"kimi-k3\"\nthinking = \"max\"\naffinity = \"nitro\"\nbase_url = %q\ntools = []\n", srv.URL)
	if err := os.WriteFile(filepath.Join(cfgDir, userConfigName), []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, userStateName), []byte(`{"model":"glm-5.3","thinking":"high"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	out, _, err := runValenCLIFull(t, dir, "-p", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if req := parseWireRequest(t, body()); req.Model != "kimi-k3" {
		t.Fatalf("wire model=%q, want config model kimi-k3 (state must not overlay -p): %s", req.Model, out)
	}
}

// cliBodies pairs with sseServerWith for child-process runs: the handler and
// the test hand requests over a channel, the only race-detector-visible
// happens-before edge across the kernel.
func cliBodies(t *testing.T) (func(string), func() string) {
	t.Helper()
	inbox := make(chan string, 16)
	return func(raw string) { inbox <- raw }, func() string {
		t.Helper()
		select {
		case raw := <-inbox:
			return raw
		case <-time.After(10 * time.Second):
			t.Fatal("fake server recorded no request")
			return ""
		}
	}
}

// wireRequest is the part of a chat request the CLI tests inspect.
type wireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func parseWireRequest(t *testing.T, raw string) wireRequest {
	t.Helper()
	var req wireRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("decode request: %v: %s", err, raw)
	}
	return req
}

func (r wireRequest) toolNames() []string {
	var names []string
	for _, t := range r.Tools {
		names = append(names, t.Function.Name)
	}
	return names
}

// toolCallRound is one SSE round in which the model calls a tool.
func toolCallRound(name, args string) string {
	return "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":" + jsonQuote(name) + ",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":" + jsonQuote(args) + "}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
}

// --stream writes key events as NDJSON lines, nothing else on stdout, and the
// done line last.
func TestStreamWritesNDJSONWithDoneLast(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := sseServer(t, &bodies, toolCallRound("glob", `{"pattern":"note.txt"}`), chatTextStream("done"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\ntools = [\"glob\"]\n", srv.URL))

	stdout, err := runValenCLI(t, dir, "-p", "--stream", "-config", cfgPath, "find the note")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var kinds []string
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		kinds = append(kinds, fmt.Sprint(ev["kind"]))
	}
	if want := []string{"round_start", "tool_start", "tool_result", "round_start", "done"}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds=%v want %v\n%s", kinds, want, stdout)
	}
	if !strings.Contains(lines[2], "note.txt") {
		t.Fatalf("tool_result line lacks the glob output: %s", lines[2])
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, `"usage"`) || strings.Contains(last, `"error"`) {
		t.Fatalf("done line=%s", last)
	}
}

// Reasoning must never reach the live answer: reasoning deltas stay out of the
// stderr stream and content-inlined think blocks are split out, so only the
// answer is shown. done.reply and plain stdout carry that same split answer;
// the NDJSON stream itself carries no live text.
func TestStreamDropsReasoningAndCarriesReply(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	round := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"private plan\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":" + jsonQuote(thinkOpenI+"private plan"+thinkCloseI+"public answer") + "}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	var bodies []string
	srv := sseServer(t, &bodies, round)
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	stdout, stderr, err := runValenCLIFull(t, t.TempDir(), "-p", "--stream", "-config", cfgPath, "reason")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, line := range lines {
		var ev struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		if ev.Kind == "thinking_delta" || ev.Kind == "text_delta" {
			t.Fatalf("live text leaked into the stream: %s", line)
		}
	}
	var done struct {
		Kind  string `json:"kind"`
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &done); err != nil {
		t.Fatal(err)
	}
	if done.Kind != "done" || done.Reply != "public answer" {
		t.Fatalf("done=%+v", done)
	}
	if !strings.Contains(stderr, "[answer] public answer") || strings.Contains(stderr, "private plan") {
		t.Fatalf("live answer on stderr=%q", stderr)
	}

	// Plain -p prints the split answer, never the reasoning block.
	out, err := runValenCLI(t, t.TempDir(), "-p", "-config", cfgPath, "reason")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "public answer" {
		t.Fatalf("stdout=%q", got)
	}
}

// A failed turn still ends the stream with a done line that carries the error,
// and the process exits non-zero.
func TestStreamEndsWithErrorDoneLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"model not found","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	stdout, err := runValenCLI(t, t.TempDir(), "-p", "--stream", "-config", cfgPath, "hi")
	if err == nil {
		t.Fatal("want non-zero exit")
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"kind":"done"`) || !strings.Contains(last, `"error"`) {
		t.Fatalf("last line=%s", last)
	}
}

// --one-shot-task runs with its own profile (model), offers no interactive or
// delegating tools, and continues from --history. Without it the same config
// gets the full default tool set.
func TestOneShotTaskProfileAndHistory(t *testing.T) {
	dir := t.TempDir()
	history := filepath.Join(dir, "history.json")
	if err := os.WriteFile(history, []byte(`[{"role":"user","content":"earlier question"},{"role":"assistant","content":"earlier answer"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	record, take := cliBodies(t)
	srv := sseServerWith(t, record, chatTextStream("done"))
	job := writeConfig(t, fmt.Sprintf("model = \"job-model\"\nbase_url = %q\ntools = [\"read\", \"glob\", \"AskUserQuestion\", \"todowrite\"]\n", srv.URL))

	stdout, err := runValenCLI(t, dir, "--one-shot-task", "--history", history, "-config", job, "new question")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "done" {
		t.Fatalf("stdout=%q, want the plain answer", stdout)
	}
	req := parseWireRequest(t, take())
	if req.Model != "job-model" {
		t.Fatalf("model=%q, want the job profile", req.Model)
	}
	if want := []string{"read", "glob"}; !slices.Equal(req.toolNames(), want) {
		t.Fatalf("tools=%v want %v", req.toolNames(), want)
	}
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	if want := []string{"system", "user", "assistant", "user"}; !slices.Equal(roles, want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	if got := req.Messages[1].Content + "|" + req.Messages[2].Content + "|" + req.Messages[3].Content; got != "earlier question|earlier answer|new question" {
		t.Fatalf("history not carried: %q", got)
	}

	// The same profile without the job switch keeps the default tools.
	if _, err := runValenCLI(t, dir, "-p", "-config", job, "again"); err != nil {
		t.Fatal(err)
	}
	if got := parseWireRequest(t, take()).toolNames(); !slices.Equal(got, []string{"read", "glob", "AskUserQuestion", "todowrite", "todoread", "task"}) {
		t.Fatalf("default tools=%v", got)
	}
}

// A background job cannot ask anyone: an out-of-scope read is denied with no
// prompt, so the job never waits on stdin or the user's terminal.
func TestOneShotTaskDeniesOutOfScopeReadWithoutPrompt(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, take := cliBodies(t)
	srv := sseServerWith(t, record, toolCallRound("read", `{"path":`+jsonQuote(secret)+`}`), chatTextStream("done"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"job-model\"\nbase_url = %q\ntools = [\"read\"]\n", srv.URL))

	if _, err := runValenCLI(t, root, "--one-shot-task", "-config", cfgPath, "read it"); err != nil {
		t.Fatal(err)
	}
	take()
	var result string
	for _, m := range parseWireRequest(t, take()).Messages {
		if m.Role == "tool" {
			result = m.Content
		}
	}
	if !strings.Contains(result, "no interactive prompter") || strings.Contains(result, "nope") {
		t.Fatalf("tool result=%q, want a denial without the file contents", result)
	}
}

// The OpenRouter generation id reaches the or_meta event, so a cancelled
// generation can be looked up for billing.
func TestOpenRouterStreamCarriesGenerationID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"gen-abc\",\"provider\":\"Infermatic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	oldBase := openRouterBaseURL
	openRouterBaseURL = srv.URL
	defer func() { openRouterBaseURL = oldBase }()

	cfg := orTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	var meta *ORMeta
	onEvent := func(ev Event) {
		if ev.Kind == "or_meta" {
			meta = mergeORMeta(meta, ev.ORMeta)
		}
	}
	if _, _, err := runChatTurn(context.Background(), cfg, nil, "or-key", srv.Client(), NewRegistry(), &history, onEvent); err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.ID != "gen-abc" || meta.Provider != "Infermatic" {
		t.Fatalf("meta=%+v", meta)
	}
}
