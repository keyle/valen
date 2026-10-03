package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/sashabaranov/go-openai"
)

const (
	defaultConfigPath = "config.toml"
	defaultBaseURL    = "https://opencode.ai/zen/go/v1"
	defaultAPIKeyEnv  = "OPENCODE_GO_API_KEY"
	defaultMaxRounds  = 100
	defaultMaxTokens  = 16384
	// defaultMaxContextTokens fail-fasts absurd sends: stateless transports
	// resend all of history every round, so burn compounds as rounds ×
	// history. Assumption (reversible via max_context_tokens): well under
	// the smallest catalog model's limit, over any healthy session.
	defaultMaxContextTokens = 300000
	// defaultSystem applies when `system` is unset in config.toml. It keeps
	// the agent MCP-capable with a fast tool loopback and points blank
	// sessions at AGENTS.md first. Batching guidance is explicit so the model
	// emits parallel calls in one block instead of one tool per round.
	defaultSystem = "You are a helpful assistant. You are an MCP-capable thinking agent with a fast tool loopback (read, write, edit, glob, grep, shell, web_search, web_fetch, todowrite, todoread, task, plus MCP tools). On a blank session, first check for AGENTS.md in the workdir and follow it. Work fast: batch all independent tool calls in the same block (parallel read/glob/grep/shell); prefer grep/glob to locate before reading. For research and bounded multi-step work, delegate to the task subagent (explore for locating code, general for one step) instead of sprawling the main loop. For any task needing 3+ steps, you MUST call todowrite FIRST before any file reads or edits, keep exactly one in_progress, and update it after each step (mark completed only after verification). Act fast: once you can state the change, call write/edit within the next round or two — do not re-read files you already hold, and do not research library internals for a local-edit task. Exploring without acting is failure. Keep each round lean: re-read files instead of resending them."
)

// version is the binary version; `make prod` stamps it via
// -ldflags -X main.version (dev builds report "dev").
var version = "dev"

// userAgent identifies valen to the OpenCode APIs: an honest SDK string,
// not a spoofed browser — Go's TLS fingerprint doesn't match Safari anyway,
// so a Safari UA to an API looks more bot-like, not less.
var userAgent = "valen/" + version + " (opencode-compatible agent harness)"

// browserUserAgent is only for web_search/web_fetch, where the scraped
// sites' bot defenses expect a browser.
const browserUserAgent = "Mozilla/5.0 (iPad; CPU OS 18_7_8 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/27.0 Mobile/15E148 Safari/604.1"

// effectiveSystem returns the configured system prompt, or the built-in
// default when none is set.
func effectiveSystem(cfg Config) string {
	if strings.TrimSpace(cfg.System) != "" {
		return cfg.System
	}
	return defaultSystem
}

// Config mirrors config.toml. See SPEC.md for the full schema.
type Config struct {
	Model            string `toml:"model"`
	Thinking         string `toml:"thinking"`
	BaseURL          string `toml:"base_url"`
	APIKeyEnv        string `toml:"api_key_env"`
	SessionID        string `toml:"session_id"`
	System           string `toml:"system"`
	MaxRounds        int    `toml:"max_rounds"`
	MaxContextTokens int    `toml:"max_context_tokens"`
	MaxTokens        int    `toml:"max_tokens"`
	// MaxStagnantRounds aborts a turn whose tool loop runs this many rounds
	// with calls but no write/edit/task (exploring without acting). Zero
	// means the default (25); raise for long research turns.
	MaxStagnantRounds int              `toml:"max_stagnant_rounds"`
	Temperature       *float32         `toml:"temperature"`
	Tools             []string         `toml:"tools"`
	MCPServers        []MCPServer      `toml:"mcp"`
	Compaction        CompactionConfig `toml:"compaction"`
	Input             InputConfig      `toml:"input"`
}

// CompactionConfig mirrors the [compaction] table: summary-mode only,
// send-side (disk JSONL stays lossless). Model selects the summarizer:
// empty means the session model; set a cheap flash model to keep large
// sessions cheap for the big model. Thinking selects the summarizer effort
// explicitly (default "medium" when the model accepts it, else the cheapest
// level); the main-turn effort stays in top-level `thinking`.
type CompactionConfig struct {
	Auto       *bool  `toml:"auto"`
	KeepTokens int    `toml:"keep_tokens"`
	Buffer     int    `toml:"buffer"`
	Model      string `toml:"model"`
	Thinking   string `toml:"thinking"`
}

// InputConfig mirrors the [input] table: chatbox chrome (TUI only).
// Blink nil means on; Height <= 0 means the 3-line default.
type InputConfig struct {
	Blink  *bool `toml:"blink"`
	Height int   `toml:"height"`
}

// defaultInputHeight is the chatbox height in lines.
const defaultInputHeight = 3

// inputHeight is the chatbox height in lines, default 3.
func inputHeight(cfg Config) int {
	if cfg.Input.Height <= 0 {
		return defaultInputHeight
	}
	return cfg.Input.Height
}

// inputBlink reports whether the chatbox block cursor blinks, default on.
func inputBlink(cfg Config) bool {
	return cfg.Input.Blink == nil || *cfg.Input.Blink
}

type MCPServer struct {
	Name string            `toml:"name"`
	Cmd  string            `toml:"cmd"`
	Args []string          `toml:"args"`
	Env  map[string]string `toml:"env"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
	}
	if bad := md.Undecoded(); len(bad) > 0 {
		names := make([]string, len(bad))
		for i, k := range bad {
			names[i] = k.String()
		}
		return cfg, fmt.Errorf("decode %s: unknown key(s): %s", path, strings.Join(names, ", "))
	}
	if cfg.Model == "" {
		return cfg, errors.New("model is required")
	}
	cfg.Thinking = strings.ToLower(strings.TrimSpace(cfg.Thinking))
	if !validThinkingLevel(cfg.Model, cfg.Thinking) {
		return cfg, fmt.Errorf("thinking %q is not available for model %q (try: %s)", cfg.Thinking, cfg.Model, strings.Join(AvailableThinkingLevels(cfg.Model), ", "))
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = defaultAPIKeyEnv
	}
	if cfg.MaxRounds == 0 {
		cfg.MaxRounds = defaultMaxRounds
	}
	if cfg.MaxContextTokens == 0 {
		cfg.MaxContextTokens = defaultMaxContextTokens
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = defaultMaxTokens
	}
	if cfg.MaxStagnantRounds <= 0 {
		cfg.MaxStagnantRounds = defaultMaxStagnantRounds
	}
	cfg.Compaction.Model = strings.TrimSpace(cfg.Compaction.Model)
	cfg.Compaction.Thinking = strings.ToLower(strings.TrimSpace(cfg.Compaction.Thinking))
	if err := validateExplicitCompactionThinking(compactionModel(cfg), cfg.Compaction.Thinking); err != nil {
		return cfg, err
	}
	cfg.Compaction = normalizeCompactionConfig(cfg.Compaction, cfg.MaxContextTokens)
	cfg.Input.Height = inputHeight(cfg)
	if cfg.SessionID == "" {
		cfg.SessionID = newSessionID()
	}
	for _, s := range cfg.MCPServers {
		if s.Name == "" || s.Cmd == "" {
			return cfg, errors.New("mcp server requires name and cmd")
		}
	}
	return cfg, nil
}

// headerTransport stamps the identity headers OpenCode Go requires for routing.
type headerTransport struct {
	base      http.RoundTripper
	sessionID string
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("x-opencode-session", t.sessionID)
	r.Header.Set("User-Agent", userAgent)
	return t.base.RoundTrip(r)
}

func closeAll(closers []func()) {
	for _, c := range closers {
		c()
	}
}

func bufioReadLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

func newSessionID() string {
	return fmt.Sprintf("ses-%d", time.Now().UnixNano())
}

func main() {
	if err := start(); err != nil {
		fmt.Fprintln(os.Stderr, "valen:", err)
		os.Exit(1)
	}
}

func start() error {
	cfgPath := flag.String("config", defaultConfigPath, "path to config")
	dirFlag := flag.String("dir", "", "working directory: sandbox root for file tools and shell")
	printMode := flag.Bool("p", false, "one-shot: answer the prompt and exit")
	dvFlag := flag.Bool("dv", false, "debug verbose: timestamped round/tool/size trace to stderr")
	dvLogFlag := flag.String("dvlog", "", "debug verbose log file (implies -dv; use in TUI mode)")
	flag.Parse()

	// Resolve paths before any chdir so relative paths keep working.
	cfgAbs := *cfgPath
	if abs, err := filepath.Abs(cfgAbs); err == nil {
		cfgAbs = abs
	}
	dvLogAbs := *dvLogFlag
	if dvLogAbs != "" {
		if abs, err := filepath.Abs(dvLogAbs); err == nil {
			dvLogAbs = abs
		}
	}
	if err := initDebugVerbose(*dvFlag, dvLogAbs); err != nil {
		return err
	}
	defer closeDebugLog()

	tuiMode := !*printMode && isTTY(os.Stdin) && isTTY(os.Stdout)
	workdir, rest := splitWorkdirArg(*dirFlag, flag.Args(), tuiMode)
	if workdir != "" {
		abs, err := filepath.Abs(workdir)
		if err != nil {
			return fmt.Errorf("dir: %w", err)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("dir %s: %w", abs, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("dir %s is not a directory", abs)
		}
		if err := os.Chdir(abs); err != nil {
			return fmt.Errorf("dir: %w", err)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		setFileRoot(cwd)
	} else {
		return fmt.Errorf("workdir: %w", err)
	}

	cfg, err := loadConfig(cfgAbs)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	apiKey := os.Getenv(cfg.APIKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("%s is not set", cfg.APIKeyEnv)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	registry := NewRegistry()
	unknown := registry.LoadBuiltins(cfg.Tools)
	for _, name := range unknown {
		fmt.Fprintln(os.Stderr, "config: unknown builtin tool:", name)
	}
	if !registry.Has("AskUserQuestion") {
		if err := registry.Add(AskUserQuestionTool()); err != nil {
			return fmt.Errorf("register AskUserQuestion: %w", err)
		}
	}
	// Session plan tools are always present (like AskUserQuestion) so chunky
	// work can break into small todowrite steps even when config.toml omits
	// them. In-memory only; the call records fold into the session JSONL.
	for _, t := range []Tool{TodoWriteTool(), TodoReadTool()} {
		if !registry.Has(t.Name) {
			if err := registry.Add(t); err != nil {
				return fmt.Errorf("register %s: %w", t.Name, err)
			}
		}
	}

	warn := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	closers := LoadMCP(ctx, cfg.MCPServers, registry, warn)
	defer closeAll(closers)

	client := newClient(cfg, apiKey)
	// task delegates to a same-model subagent so exploration stays out of
	// main history; always present like the plan tools. The runner closes
	// over client/config here where they live.
	if !registry.Has("task") {
		if err := registry.Add(TaskTool(func(ctx context.Context, prompt, kind string) (string, error) {
			return runTask(ctx, cfg, client, apiKey, newHTTPClient(cfg), prompt, kind)
		})); err != nil {
			return fmt.Errorf("register task: %w", err)
		}
	}
	if !*printMode && isTTY(os.Stdin) && isTTY(os.Stdout) {
		dlogf("start model=%s transport=%s thinking=%s maxrounds=%d budget~%dtok schemastate~%dtok tools=%d tui=true",
			cfg.Model, modelTransport(cfg.Model), cfg.Thinking, cfg.MaxRounds, contextBudget(cfg), requestToolsTokens(registry), len(registry.Names()))
		return runTUI(ctx, cfg, cfgAbs, client, registry, apiKey)
	}

	prompt, err := readPrompt(rest)
	if err != nil {
		return err
	}
	if prompt == "" {
		return errors.New("empty prompt")
	}
	return chat(ctx, cfg, client, apiKey, newHTTPClient(cfg), registry, prompt, os.Stdout, os.Stderr)
}

// splitWorkdirArg resolves the sandbox root: an explicit -dir flag wins,
// otherwise a single positional directory is taken as the workdir in TUI
// mode (where positionals carry no prompt). Headless prompts are never
// stolen: -p "..." keeps its args unless -dir is given.
func splitWorkdirArg(dirFlag string, args []string, tuiMode bool) (string, []string) {
	if strings.TrimSpace(dirFlag) != "" {
		return dirFlag, args
	}
	if tuiMode && len(args) > 0 {
		if fi, err := os.Stat(args[0]); err == nil && fi.IsDir() {
			return args[0], args[1:]
		}
	}
	return "", args
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// sharedTransport is the single tuned connection pool for chat API calls and
// web fetch/search. Keep-alive reuse avoids a fresh TCP+TLS handshake per
// round; caps stay on the tool-output side, not here.
var sharedTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 20,
	IdleConnTimeout:     90 * time.Second,
}

func newHTTPClient(cfg Config) *http.Client {
	return &http.Client{Transport: &headerTransport{base: sharedTransport, sessionID: cfg.SessionID}}
}

func newClient(cfg Config, apiKey string) *openai.Client {
	oc := openai.DefaultConfig(apiKey)
	oc.BaseURL = cfg.BaseURL
	oc.HTTPClient = newHTTPClient(cfg)
	return openai.NewClientWithConfig(oc)
}

func readPrompt(args []string) (string, error) {
	if p := strings.TrimSpace(strings.Join(args, " ")); p != "" {
		return p, nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(b)), err
	}
	fmt.Fprint(os.Stderr, "prompt: ")
	line, err := bufioReadLine(os.Stdin)
	return strings.TrimSpace(line), err
}

// Event reports progress during a turn so frontends stay responsive.
type Event struct {
	Kind string // "tool_start", "tool_result", "text_delta", "thinking_delta"
	Text string
	// Args carries tool JSON arguments on tool_start; Output carries the
	// tool result on tool_result.
	Args   string
	Output string
	// ID carries the tool call ID on tool_start/tool_result so the UI
	// matches rows by call, not by name (parallel same-name calls).
	ID string
}

// runTurn drives the tool loop for one user turn. history grows in place with
// assistant and tool messages so the caller can keep the conversation going.
// It dispatches on model transport: chat (/chat/completions), messages
// (/messages) or responses (/responses). History stays OpenAI-shaped; the
// non-chat transports convert per round and fold replies back. A 400 that
// lists valid thinking levels is learned to disk for next time.
func runTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	msg, usage, err := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	if err != nil {
		maybeLearnThinkingLevels(cfg.Model, err.Error())
		// One recovery compaction when the provider rejects history as too
		// long: compact send-side, then retry the turn once. Second failure
		// is terminal so we never loop on an irreducibly large request.
		if isContextOverflow(err) && compactionAuto(cfg) {
			if _, ok, cerr := compactHistory(ctx, cfg, client, apiKey, hc, history); cerr == nil && ok {
				m2, u2, err2 := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
				usage.PromptTokens += u2.PromptTokens
				usage.CompletionTokens += u2.CompletionTokens
				usage.TotalTokens += u2.TotalTokens
				if err2 != nil {
					maybeLearnThinkingLevels(cfg.Model, err2.Error())
				}
				return m2, usage, err2
			}
		}
	}
	return msg, usage, err
}

// dispatchTurn runs one pre-send auto-compaction when over threshold, then
// the model transport. Shared by all three transports so every model gets
// the same send-side bound.
func dispatchTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	if ctx.Err() != nil {
		return openai.ChatCompletionMessage{}, openai.Usage{}, ctx.Err()
	}
	t0 := time.Now()
	dlogf("turn start transport=%s %s", modelTransport(cfg.Model), dvHistoryLine(*history))
	before := estimateTokens(*history)
	maybeAutoCompact(ctx, cfg, client, apiKey, hc, history)
	if after := estimateTokens(*history); after != before {
		dlogf("pre-send compact %dtok->%dtok", before, after)
	}
	var msg openai.ChatCompletionMessage
	var usage openai.Usage
	var err error
	switch modelTransport(cfg.Model) {
	case "messages":
		msg, usage, err = runMessagesTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	case "responses":
		msg, usage, err = runResponsesTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	default:
		msg, usage, err = runChatTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	}
	dlogf("turn end dur=%s err=%v in=%d out=%d total=%d reply=%dB",
		dvDur(time.Since(t0)), err, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, len(msg.Content))
	return msg, usage, err
}

// chatSendMessages builds the per-round request copy: identical to history
// except tool results shrink to the send budget (A2). Stored history keeps
// full outputs; the wire copy stays small so stateless replays don't
// re-upload rounds × 16k results.
func chatSendMessages(history []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, len(history))
	for i, m := range history {
		out[i] = m
		if m.Role == openai.ChatMessageRoleTool {
			out[i].Content = fitSendBudget(m.Content)
		}
	}
	return out
}

// maxParallelTools bounds concurrent tool execution in one round. Outputs
// stay capped by truncate, so memory stays flat; the semaphore only caps how
// many run at once.
const maxParallelTools = 8

// mcpMu serializes MCP tool calls per server: one stdio JSON-RPC server
// shares one stdin pipe, so concurrent writes to the SAME server could
// interleave requests. Different servers are independent and run fully
// parallel; builtins run fully parallel. Ditched the old global mutex that
// let one slow MCP server stall every other server's batch.
var mcpMu = newMCPMutex()

// mcpMutex holds one mutex per MCP server prefix (<server>__<tool>).
type mcpMutex struct {
	mu      sync.Mutex
	servers map[string]*sync.Mutex
}

func newMCPMutex() *mcpMutex {
	return &mcpMutex{servers: map[string]*sync.Mutex{}}
}

// lock returns the mutex for the server prefix of a namespaced tool name.
func (m *mcpMutex) lock(name string) *sync.Mutex {
	server, _, _ := strings.Cut(name, "__")
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.servers[server]
	if !ok {
		l = &sync.Mutex{}
		m.servers[server] = l
	}
	return l
}

// isMCPTool reports namespaced MCP tools (<server>__<tool>).
func isMCPTool(name string) bool { return strings.Contains(name, "__") }

// loopFingerprint identifies a tool call for repeat detection: same tool
// with the same (normalized) arguments doing the same work.
func loopFingerprint(tc openai.ToolCall) string {
	return tc.Function.Name + "\x00" + normalizeArgs(tc.Function.Arguments)
}

// doomLoopTool reports the tool name when calls would repeat the same single
// tool call a third consecutive time — the death pattern (re-reading the
// same file, retrying the same command). Multi-call batches break the chain:
// they run as one parallel unit, not a loop step. "" means no loop.
func doomLoopTool(history []openai.ChatCompletionMessage, calls []openai.ToolCall) string {
	if len(calls) != 1 {
		return ""
	}
	want := loopFingerprint(calls[0])
	found := 0
	for i := len(history) - 1; i >= 0 && found < 2; i-- {
		m := history[i]
		if m.Role != openai.ChatMessageRoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		if len(m.ToolCalls) != 1 || loopFingerprint(m.ToolCalls[0]) != want {
			return ""
		}
		found++
	}
	if found < 2 {
		return ""
	}
	return calls[0].Function.Name
}

func doomLoopError(name string) error {
	return fmt.Errorf("stopped: tool %q called 3 times with identical input — ask for a different approach or /clear", name)
}

// messageOverheadTokens covers the per-message JSON envelope the runes/4
// content estimate misses (role, tool_call ids, structure): ~150 chars.
// Without it the estimator under-counts real prefill and compaction fires
// late, after the oversized send already burned.
const messageOverheadTokens = 40

// estimateTokens approximates prompt tokens as runes/4 over content plus
// tool args, plus envelope overhead per message. No tokenizer dep; good
// enough for a fail-fast guard now that schemas ride separately (A3).
func estimateTokens(msgs []openai.ChatCompletionMessage) int {
	var runes int
	for _, m := range msgs {
		runes += len([]rune(m.Content))
		for _, tc := range m.ToolCalls {
			runes += len([]rune(tc.Function.Arguments))
		}
	}
	return runes/4 + len(msgs)*messageOverheadTokens
}

// requestToolsTokens estimates the tool-schema block re-sent on every round
// (every builtin + MCP schema). Stateless transports pay this × rounds, so
// the budget guard must see it: a 30k schema block with a 45k history is a
// 75k send, not 45k.
func requestToolsTokens(registry *Registry) int {
	if registry == nil {
		return 0
	}
	raw, err := json.Marshal(registry.OpenAITools())
	if err != nil {
		return 0
	}
	return len([]rune(string(raw))) / 4
}

func contextBudget(cfg Config) int {
	if cfg.MaxContextTokens > 0 {
		return cfg.MaxContextTokens
	}
	return defaultMaxContextTokens
}

// checkContextBudget fail-fasts a round that would send a ridiculous context
// (and burn tokens × rounds on stateless transports) instead of letting it
// crawl into a 429. toolsTokens is the re-sent schema block (see
// requestToolsTokens). Compaction is the real fix; this is the guardrail.
func checkContextBudget(cfg Config, history []openai.ChatCompletionMessage, toolsTokens int) error {
	budget := contextBudget(cfg)
	if est := estimateTokens(history) + toolsTokens; est > budget {
		return fmt.Errorf("context too large: est. %d tokens exceeds budget %d — /compact to summarize or /clear for a fresh history (or raise max_context_tokens)", est, budget)
	}
	return nil
}

// runToolCalls executes one round's tool calls and returns the tool messages
// in request order. Batches without AskUserQuestion run with bounded
// parallelism; batches containing AskUserQuestion run sequentially so the
// single interactive overlay/stdin stays deterministic. onEvent emissions
// stay in request order for the TUI timeline.
func runToolCalls(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	if len(calls) == 0 {
		return nil
	}
	for _, tc := range calls {
		if tc.Function.Name == "AskUserQuestion" {
			return runToolCallsSerial(ctx, registry, calls, onEvent)
		}
	}
	if len(calls) == 1 {
		return runToolCallsSerial(ctx, registry, calls, onEvent)
	}
	if onEvent != nil {
		for _, tc := range calls {
			onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments, ID: tc.ID})
		}
	}
	batch0 := time.Now()
	outs := make([]string, len(calls))
	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc openai.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			if isMCPTool(tc.Function.Name) {
				mu := mcpMu.lock(tc.Function.Name)
				mu.Lock()
				defer mu.Unlock()
			}
			outs[i] = registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
			dlogf("tool %s args=%dB dur=%s out=%dB %q", tc.Function.Name, len(tc.Function.Arguments), dvDur(time.Since(t0)), len(outs[i]), dvPreview(tc.Function.Arguments, 160))
		}(i, tc)
	}
	wg.Wait()
	dlogf("batch %d tools total=%s", len(calls), dvDur(time.Since(batch0)))
	if onEvent != nil {
		for i, tc := range calls {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: outs[i], ID: tc.ID})
		}
	}
	out := make([]openai.ChatCompletionMessage, 0, len(calls))
	for i, tc := range calls {
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: outs[i]})
	}
	return out
}

// runToolCallsSerial is the sequential fallback for interactive batches.
func runToolCallsSerial(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	batch0 := time.Now()
	out := make([]openai.ChatCompletionMessage, 0, len(calls))
	for _, tc := range calls {
		if onEvent != nil {
			onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments, ID: tc.ID})
		}
		t0 := time.Now()
		res := registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
		dlogf("tool %s args=%dB dur=%s out=%dB serial %q", tc.Function.Name, len(tc.Function.Arguments), dvDur(time.Since(t0)), len(res), dvPreview(tc.Function.Arguments, 160))
		if onEvent != nil {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: res, ID: tc.ID})
		}
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: res})
	}
	dlogf("batch %d tools serial total=%s", len(calls), dvDur(time.Since(batch0)))
	return out
}

// runChatTurn is the /chat/completions tool loop, streaming each round.
func runChatTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	guards := newLoopGuards(cfg)
	recovering := false
	defer stripNudges(history)
	for round := 0; round < cfg.MaxRounds; round++ {
		toolsTok := requestToolsTokens(registry)
		if err := checkContextBudget(cfg, *history, toolsTok); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		req := openai.ChatCompletionRequest{Model: cfg.Model, Messages: chatSendMessages(*history), Tools: registry.OpenAITools()}
		if cfg.Temperature != nil {
			req.Temperature = *cfg.Temperature
		}
		applyThinkingToRequest(&req, cfg.Model, cfg.Thinking)
		if debugVerbose {
			if sendBytes, ok := dvSendBytes(req); ok {
				dlogf("round %d chat %s toolstok~%d sendbytes=%d\n%s", round+1, dvHistoryLine(*history), toolsTok, sendBytes, dvTailDetail(req.Messages, 6))
			} else {
				dlogf("round %d chat %s toolstok~%d", round+1, dvHistoryLine(*history), toolsTok)
			}
		}

		r0 := time.Now()
		via := "stream"
		msg, roundUsage, err := chatRoundStream(ctx, cfg, apiKey, hc, req, onEvent)
		if err != nil {
			if !isStreamRejected(err) {
				dlogf("round %d chat err=%v dur=%s", round+1, err, dvDur(time.Since(r0)))
				return openai.ChatCompletionMessage{}, usage, err
			}
			via = "unary"
			msg, roundUsage, err = chatRoundOnce(ctx, cfg, apiKey, hc, req)
			if err != nil {
				dlogf("round %d chat unary err=%v dur=%s", round+1, err, dvDur(time.Since(r0)))
				return openai.ChatCompletionMessage{}, usage, err
			}
		}
		dlogf("round %d chat %s dur=%s in=%d out=%d content=%dB calls=%s", round+1, via, dvDur(time.Since(r0)),
			roundUsage.PromptTokens, roundUsage.CompletionTokens, len(msg.Content), dvCallsLine(msg.ToolCalls))
		usage.PromptTokens += roundUsage.PromptTokens
		usage.CompletionTokens += roundUsage.CompletionTokens
		usage.TotalTokens += roundUsage.TotalTokens

		if name := doomLoopTool(*history, msg.ToolCalls); name != "" {
			return openai.ChatCompletionMessage{}, usage, doomLoopError(name)
		}
		*history = append(*history, msg)
		if len(msg.ToolCalls) == 0 {
			return msg, usage, nil
		}
		if recovering {
			// Recovery round answered with tools attached: take the text,
			// leave the calls unexecuted, and finish the turn instead of
			// dying on the stall.
			if strings.TrimSpace(msg.Content) == "" {
				return openai.ChatCompletionMessage{}, usage, stagnantError(guards.stagnant)
			}
			(*history)[len(*history)-1].ToolCalls = nil
			dlogf("stall recovery: finishing with text, %d tool call(s) declined", len(msg.ToolCalls))
			final := msg
			final.ToolCalls = nil
			return final, usage, nil
		}

		*history = append(*history, runToolCalls(ctx, registry, msg.ToolCalls, onEvent)...)
		// Keep long turns lean between rounds: prune-only, no blocking
		// summary in the hot loop (summaries fire between turns).
		leanHistoryForNextRound(cfg, history)
		if nudge, act, stop := guards.note(msg.ToolCalls); stop != nil {
			dlogf("stall guard tripped after %d stagnant rounds", guards.stagnant)
			if recovering {
				return openai.ChatCompletionMessage{}, usage, stop
			}
			recovering = true
			dlogf("stall recovery: one final round under no-tools instruction")
			*history = append(*history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: stallRecoveryText})
			continue
		} else {
			if nudge {
				dlogf("batch nudge appended after single-call streak")
				*history = append(*history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: batchNudgeText})
			}
			if act {
				dlogf("act nudge appended after %d stagnant rounds", guards.stagnant)
				*history = append(*history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: actNudgeText})
			}
		}
	}
	return openai.ChatCompletionMessage{}, usage, fmt.Errorf("stopped after %d rounds without a final answer", cfg.MaxRounds)
}

func chatEndpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

func chatHeaders(apiKey string) map[string]string {
	if apiKey == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + apiKey}
}

// chatRoundOnce is the non-streaming round, kept as the fallback when a
// provider rejects `stream: true`.
func chatRoundOnce(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	resp, err := postAPI(ctx, hc, cfg, chatEndpoint(cfg.BaseURL), req, chatHeaders(apiKey))
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return openai.ChatCompletionMessage{}, usage, err
	}
	if resp.StatusCode != http.StatusOK {
		return openai.ChatCompletionMessage{}, usage, parseTransportError("chat", resp.StatusCode, raw)
	}
	var out openai.ChatCompletionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return openai.ChatCompletionMessage{}, usage, fmt.Errorf("api: chat: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: empty choices in response")
	}
	msg := out.Choices[0].Message
	return msg, out.Usage, nil
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
	resp, err := postAPI(ctx, hc, cfg, chatEndpoint(cfg.BaseURL), req, chatHeaders(apiKey))
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
		return openai.ChatCompletionMessage{}, usage, parseTransportError("chat", resp.StatusCode, raw)
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
		return out.Choices[0].Message, out.Usage, nil
	}

	var content, thinking strings.Builder
	accs := map[int]*chatToolAcc{}
	var order []int
	scanErr := scanSSE(ctx, br, func(ev sseEvent) bool {
		if strings.TrimSpace(ev.data) == "[DONE]" {
			return false
		}
		var chunk openai.ChatCompletionStreamResponse
		if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
			return true
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			d := choice.Delta
			if d.Content != "" {
				content.WriteString(d.Content)
				emit("text_delta", d.Content)
			}
			if d.ReasoningContent != "" {
				thinking.WriteString(d.ReasoningContent)
				emit("thinking_delta", d.ReasoningContent)
			}
			for _, tc := range d.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
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
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, scanErr
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
	text := content.String()
	if t := strings.TrimSpace(thinking.String()); t != "" {
		text = "<think>" + t + "</think>" + text
	}
	if strings.TrimSpace(text) == "" && len(calls) == 0 {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: empty choices in response")
	}
	return openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   text,
		ToolCalls: calls,
	}, usage, nil
}

// chat is the headless one-shot path: one prompt, one answer, stdout.
func chat(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, prompt string, stdout, stderr io.Writer) error {
	resetTodos()
	history := make([]openai.ChatCompletionMessage, 0, 8)
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)})
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})
	dlogf("start model=%s transport=%s thinking=%s maxrounds=%d budget~%dtok schemastate~%dtok tools=%d headless prompt=%dB %q",
		cfg.Model, modelTransport(cfg.Model), cfg.Thinking, cfg.MaxRounds, contextBudget(cfg), requestToolsTokens(registry), len(registry.Names()), len(prompt), dvPreview(prompt, 200))

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
