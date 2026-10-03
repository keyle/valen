package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/sashabaranov/go-openai"
	"valen/internal/dvlog"
	"valen/internal/guard"
	"valen/internal/prompts"
)

const (
	defaultConfigPath            = "config.toml"
	defaultBaseURL               = "https://opencode.ai/zen/go/v1"
	defaultAPIKeyEnv             = "OPENCODE_GO_API_KEY"
	defaultOpenRouterKeyEnv      = "OPENROUTER_API_KEY"
	defaultMaxRounds             = 100
	defaultMaxTokens             = 16384
	defaultRequestTimeoutSeconds = 300
	// defaultMaxContextTokens fail-fasts absurd sends: stateless transports
	// resend all of history every round, so burn compounds as rounds ×
	// history. Assumption (reversible via max_context_tokens): well under
	// the smallest catalog model's limit, over any healthy session.
	defaultMaxContextTokens = 300000
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

// effectiveSystem returns the configured system prompt, or the harness
// prompt files (plus plan overlay) when none is set.
func effectiveSystem(cfg Config) string {
	if strings.TrimSpace(cfg.System) != "" {
		return cfg.System
	}
	cwd := getFileRoot()
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	return prompts.SystemPrompt(cfg.Model, cfg.Plan, cwd)
}

// seedHistory builds the initial history for a turn.
func seedHistory(cfg Config) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)}}
}

// Config mirrors config.toml. See SPEC.md for the full schema.
type Config struct {
	Model            string `toml:"model"`
	Thinking         string `toml:"thinking"`
	BaseURL          string `toml:"base_url"`
	APIKeyEnv        string `toml:"api_key_env"`
	OpenRouterKeyEnv string `toml:"openrouter_api_key_env"`
	// OpenRouterKey is in-memory only (toml:"-" never persists): the
	// OpenRouter credential read from OpenRouterKeyEnv at boot. Kept out
	// of the file like the Go key — persistModelSelection only writes
	// model/thinking lines, and nothing logs it.
	OpenRouterKey         string `toml:"-"`
	SessionID             string `toml:"session_id"`
	System                string `toml:"system"`
	MaxRounds             int    `toml:"max_rounds"`
	MaxContextTokens      int    `toml:"max_context_tokens"`
	MaxTokens             int    `toml:"max_tokens"`
	RequestTimeoutSeconds int    `toml:"request_timeout_seconds"`
	// MaxStagnantRounds bounds consecutive identical tool batches, not research.
	MaxStagnantRounds int `toml:"max_stagnant_rounds"`
	// Plan starts the session in plan mode: read-only investigation
	// (write/edit/mutating shell blocked, task explore-only) with the plan
	// overlay appended to the system prompt. The TUI /plan command toggles
	// the same flag at runtime without persisting it.
	Plan        bool             `toml:"plan"`
	Temperature *float32         `toml:"temperature"`
	Tools       []string         `toml:"tools"`
	MCPServers  []MCPServer      `toml:"mcp"`
	Compaction  CompactionConfig `toml:"compaction"`
	Input       InputConfig      `toml:"input"`
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
	// Bare IDs ride Go; "openrouter/…" rides OpenRouter. Anything else with
	// a slash is almost certainly a pasted OpenRouter ID missing its
	// prefix — reject it explicitly rather than sending it somewhere odd.
	if i := strings.Index(cfg.Model, "/"); i >= 0 && !strings.HasPrefix(normalizeModelID(cfg.Model), ProviderOpenRouter+"/") {
		return cfg, fmt.Errorf("model %q has an unknown provider prefix (OpenRouter models need %q)", cfg.Model, ProviderOpenRouter+"/vendor/model")
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
	if cfg.OpenRouterKeyEnv == "" {
		cfg.OpenRouterKeyEnv = defaultOpenRouterKeyEnv
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
	if cfg.RequestTimeoutSeconds <= 0 {
		cfg.RequestTimeoutSeconds = defaultRequestTimeoutSeconds
	}
	if cfg.MaxStagnantRounds <= 0 {
		cfg.MaxStagnantRounds = guard.DefaultMaxStagnantRounds
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
	planFlag := flag.Bool("plan", false, "plan mode: read-only investigation, present the plan as the reply")
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
	if err := dvlog.Init(*dvFlag, dvLogAbs); err != nil {
		return err
	}
	defer dvlog.Close()

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
	if *planFlag {
		cfg.Plan = true
	}

	apiKey := strings.TrimSpace(os.Getenv(cfg.APIKeyEnv))
	// Missing keys never fail boot: the TUI still opens so the user can
	// browse /model, and the missing key surfaces when a model needing it
	// is picked or a turn is sent (see missingKeyError + dispatchTurn).
	// Trimmed: pasted keys with stray whitespace 401 otherwise.
	cfg.OpenRouterKey = strings.TrimSpace(os.Getenv(cfg.OpenRouterKeyEnv))

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
			turnCfg := currentTurnConfig(ctx, cfg)
			return runTask(ctx, turnCfg, newClient(turnCfg, apiKey), apiKey, newHTTPClient(turnCfg), prompt, kind)
		})); err != nil {
			return fmt.Errorf("register task: %w", err)
		}
	}
	if !*printMode && isTTY(os.Stdin) && isTTY(os.Stdout) {
		dvlog.Logf("start model=%s transport=%s thinking=%s maxrounds=%d budget~%dtok schemastate~%dtok tools=%d tui=true",
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
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
	IdleConnTimeout:       90 * time.Second,
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
	Kind string // "round_start", "tool_start", "tool_result", "text_delta", "thinking_delta"
	Text string
	// Args carries tool JSON arguments on tool_start; Output carries the
	// tool result on tool_result.
	Args   string
	Output string
	// ID carries the tool call ID on tool_start/tool_result so the UI
	// matches rows by call, not by name (parallel same-name calls).
	ID string
	// DurMs carries wall execution time on tool_result.
	DurMs int64
	// Index carries the reasoning part index on thinking_delta (Responses
	// summary_index, messages content-block index). The live view starts
	// every thinking addition on its own line; the index is retained so
	// callers can still tell summary parts apart.
	Index int
}

type turnConfigKey struct{}

func currentTurnConfig(ctx context.Context, fallback Config) Config {
	if cfg, ok := ctx.Value(turnConfigKey{}).(Config); ok {
		return cfg
	}
	return fallback
}

func requestTimeout(cfg Config) time.Duration {
	seconds := cfg.RequestTimeoutSeconds
	if seconds <= 0 {
		seconds = defaultRequestTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// runTurn drives the tool loop for one user turn. history grows in place with
// assistant and tool messages so the caller can keep the conversation going.
// It dispatches on model transport: chat (/chat/completions), messages
// (/messages) or responses (/responses). History stays OpenAI-shaped; the
// non-chat transports convert per round and fold replies back. A 400 that
// lists valid thinking levels is learned to disk for next time.
func runTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	ctx = context.WithValue(ctx, turnConfigKey{}, cfg)
	// Plan mode gates at the tool Fns via ctx, so mark it once here and
	// every nested call (tools, task subagent) inherits read-only.
	if cfg.Plan {
		ctx = prompts.WithPlanMode(ctx)
	}
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

// effectiveGoKey resolves the Go credential: the boot snapshot first, then
// a live env read so an export after boot still works without a restart.
func effectiveGoKey(cfg Config, apiKey string) string {
	if k := strings.TrimSpace(apiKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(goKeyEnvName(cfg)))
}

// effectiveOpenRouterKey resolves the OpenRouter credential the same way.
func effectiveOpenRouterKey(cfg Config) string {
	if k := strings.TrimSpace(cfg.OpenRouterKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(openRouterKeyEnvName(cfg)))
}

// goKeyEnvName reports the configured Go env var for error messages.
func goKeyEnvName(cfg Config) string {
	if cfg.APIKeyEnv != "" {
		return cfg.APIKeyEnv
	}
	return defaultAPIKeyEnv
}

// openRouterKeyEnvName reports the configured OpenRouter env var.
func openRouterKeyEnvName(cfg Config) string {
	if cfg.OpenRouterKeyEnv != "" {
		return cfg.OpenRouterKeyEnv
	}
	return defaultOpenRouterKeyEnv
}

// missingKeyError is nil when the model in cfg can be called; otherwise an
// "is not set" error naming the env var the provider needs. Boot never
// fails on this — it surfaces on model pick (TUI notice) and on send
// (dispatchTurn), where friendlyError turns it into a transcript message.
func missingKeyError(cfg Config, apiKey string) error {
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		if effectiveOpenRouterKey(cfg) == "" {
			return fmt.Errorf("%s is not set (required by model %q)", openRouterKeyEnvName(cfg), cfg.Model)
		}
		return nil
	}
	if effectiveGoKey(cfg, apiKey) == "" {
		return fmt.Errorf("%s is not set (required by model %q)", goKeyEnvName(cfg), cfg.Model)
	}
	return nil
}

// dispatchTurn runs one pre-send auto-compaction when over threshold, then
// the model transport. Shared by all three transports so every model gets
// the same send-side bound.
func dispatchTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	if ctx.Err() != nil {
		return openai.ChatCompletionMessage{}, openai.Usage{}, ctx.Err()
	}
	if err := missingKeyError(cfg, apiKey); err != nil {
		return openai.ChatCompletionMessage{}, openai.Usage{}, err
	}
	// Refresh from the live env so an export after boot works without a
	// restart; downstream transports must see the usable credential, not
	// the possibly-empty boot snapshot.
	apiKey = effectiveGoKey(cfg, apiKey)
	cfg.OpenRouterKey = effectiveOpenRouterKey(cfg)
	t0 := time.Now()
	dvlog.Logf("turn start transport=%s %s", modelTransport(cfg.Model), dvlog.HistoryLine(*history, estimateTokens(*history)))
	before := estimateTokens(*history)
	maybeAutoCompact(ctx, cfg, client, apiKey, hc, history)
	if after := estimateTokens(*history); after != before {
		dvlog.Logf("pre-send compact %dtok->%dtok", before, after)
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
	dvlog.Logf("turn end dur=%s err=%v in=%d out=%d total=%d reply=%dB",
		dvlog.Dur(time.Since(t0)), err, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, len(msg.Content))
	return msg, usage, err
}

// chatSendMessages builds the wire copy without modifying stored history.
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

// loopFingerprint identifies a tool call for repeat detection: same tool
// with the same (normalized) arguments doing the same work.
func loopFingerprint(tc openai.ToolCall) string {
	args := normalizeArgs(tc.Function.Arguments)
	var value any
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			args = string(canonical)
		}
	}
	return tc.Function.Name + "\x00" + args
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
		if m.Role == openai.ChatMessageRoleUser || (m.Role == openai.ChatMessageRoleAssistant && len(m.ToolCalls) == 0) {
			break
		}
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
		runes += len([]rune(m.ReasoningContent))
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

// Only known read-only builtins may overlap. Mutating, interactive and unknown
// tools run serially; the registry owns its central gates (including MCP).
// The coordinator emits results as completed, but history stays request-ordered.
func runToolCalls(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	if len(calls) == 0 {
		return nil
	}
	for _, tc := range calls {
		switch tc.Function.Name {
		case "read", "glob", "grep", "web_search", "web_fetch":
		default:
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
	type result struct {
		index  int
		output string
		durMs  int64
	}
	results := make(chan result, len(calls))
	out := make([]openai.ChatCompletionMessage, len(calls))
	sem := make(chan struct{}, maxParallelTools)
	for i, tc := range calls {
		go func(i int, tc openai.ToolCall) {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- result{i, "error: " + ctx.Err().Error(), 0}
				return
			}
			defer func() { <-sem }()
			t0 := time.Now()
			var output string
			if err := ctx.Err(); err != nil {
				output = "error: " + err.Error()
			} else {
				output = registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
			}
			durMs := time.Since(t0).Milliseconds()
			dvlog.Logf("tool %s args=%dB dur=%s out=%dB %q", tc.Function.Name, len(tc.Function.Arguments), dvlog.Dur(time.Since(t0)), len(output), dvlog.Preview(tc.Function.Arguments, 160))
			results <- result{i, output, durMs}
		}(i, tc)
	}
	for range calls {
		res := <-results
		tc := calls[res.index]
		out[res.index] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: res.output}
		if onEvent != nil {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: res.output, ID: tc.ID, DurMs: res.durMs})
		}
	}
	dvlog.Logf("batch %d tools total=%s", len(calls), dvlog.Dur(time.Since(batch0)))
	return out
}

// runToolCallsSerial preserves execution order for potentially dependent calls.
func runToolCallsSerial(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	batch0 := time.Now()
	out := make([]openai.ChatCompletionMessage, 0, len(calls))
	for _, tc := range calls {
		if onEvent != nil {
			onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments, ID: tc.ID})
		}
		t0 := time.Now()
		var res string
		if err := ctx.Err(); err != nil {
			res = "error: " + err.Error()
		} else {
			res = registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
		}
		durMs := time.Since(t0).Milliseconds()
		dvlog.Logf("tool %s args=%dB dur=%s out=%dB serial %q", tc.Function.Name, len(tc.Function.Arguments), dvlog.Dur(time.Since(t0)), len(res), dvlog.Preview(tc.Function.Arguments, 160))
		if onEvent != nil {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: res, ID: tc.ID, DurMs: durMs})
		}
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: res})
	}
	dvlog.Logf("batch %d tools serial total=%s", len(calls), dvlog.Dur(time.Since(batch0)))
	return out
}

// runModelTurn owns execution policy; round owns only transport and replay.
func runModelTurn(ctx context.Context, cfg Config, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event), round func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	guard.StripNudges(history)
	toolsTok := requestToolsTokens(registry)
	maxStagnant := cfg.MaxStagnantRounds
	if maxStagnant <= 0 {
		maxStagnant = guard.DefaultMaxStagnantRounds
	}
	var previousBatch string
	identical := 0
	for n := 0; n < cfg.MaxRounds; n++ {
		if err := ctx.Err(); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if err := checkContextBudget(cfg, *history, toolsTok); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if onEvent != nil {
			onEvent(Event{Kind: "round_start", Text: fmt.Sprintf("Round %d/%d", n+1, cfg.MaxRounds)})
		}
		t0 := time.Now()
		msg, u, err := round(ctx, *history)
		usage.PromptTokens += u.PromptTokens
		usage.CompletionTokens += u.CompletionTokens
		usage.TotalTokens += u.TotalTokens
		dvlog.Logf("round %d %s dur=%s in=%d out=%d content=%dB calls=%s err=%v", n+1, modelTransport(cfg.Model), dvlog.Dur(time.Since(t0)), u.PromptTokens, u.CompletionTokens, len(msg.Content), dvlog.CallsLine(msg.ToolCalls), err)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if err := ctx.Err(); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if err := validateModelReply(msg); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if len(msg.ToolCalls) == 0 {
			*history = append(*history, msg)
			return msg, usage, nil
		}
		if name := doomLoopTool(*history, msg.ToolCalls); name != "" {
			return openai.ChatCompletionMessage{}, usage, doomLoopError(name)
		}
		fingerprints := make([]string, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			fingerprints[i] = loopFingerprint(tc)
		}
		// Preserve batch order: serial writes with reversed order are different work.
		batch, _ := json.Marshal(fingerprints)
		if string(batch) == previousBatch {
			identical++
		} else {
			previousBatch, identical = string(batch), 1
		}
		if identical > maxStagnant {
			return openai.ChatCompletionMessage{}, usage, fmt.Errorf("stopped: tool batch repeated for %d consecutive rounds (max_stagnant_rounds=%d)", identical, maxStagnant)
		}
		*history = append(*history, msg)
		*history = append(*history, runToolCalls(ctx, registry, msg.ToolCalls, onEvent)...)
		if err := ctx.Err(); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		leanHistoryForNextRound(cfg, history)
	}
	return openai.ChatCompletionMessage{}, usage, fmt.Errorf("stopped after %d rounds without a final answer", cfg.MaxRounds)
}

// Visible completion and valid calls are required regardless of transport.
func validateModelReply(msg openai.ChatCompletionMessage) error {
	if len(msg.ToolCalls) == 0 {
		_, answer := splitThinking(msg.Content)
		if strings.TrimSpace(answer) == "" {
			return errors.New("api: empty choices in response (no visible answer or tool calls)")
		}
	}
	ids := make(map[string]bool, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Function.Name) == "" || tc.Type != openai.ToolTypeFunction {
			return errors.New("api: incomplete tool call (missing id, name or function type)")
		}
		if ids[tc.ID] {
			return fmt.Errorf("api: duplicate tool call ID %q", tc.ID)
		}
		ids[tc.ID] = true
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &object); err != nil || object == nil {
			return fmt.Errorf("api: invalid tool arguments for %q", tc.ID)
		}
	}
	return nil
}

func outputTokens(cfg Config) int {
	if cfg.MaxTokens > 0 {
		return cfg.MaxTokens
	}
	return defaultMaxTokens
}

// runChatTurn keeps fallback capability local to this execution loop.
func runChatTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	stream := true
	tools := registry.OpenAITools()
	return runModelTurn(ctx, cfg, registry, history, onEvent, func(ctx context.Context, history []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		req := openai.ChatCompletionRequest{Model: wireModelID(cfg.Model), Messages: chatSendMessages(history), Tools: tools, MaxTokens: outputTokens(cfg)}
		if cfg.Temperature != nil {
			req.Temperature = *cfg.Temperature
		}
		applyThinkingToRequest(&req, cfg.Model, cfg.Thinking)
		if stream {
			msg, usage, err := chatRoundStream(ctx, cfg, apiKey, hc, req, onEvent)
			if !isStreamRejected(err) {
				return msg, usage, err
			}
			stream = false
		}
		return chatRoundOnce(ctx, cfg, apiKey, hc, req)
	})
}

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
	return chatEndpoint(cfg.BaseURL), chatHeaders(apiKey), cfg, hc
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

// chatRoundOnce is the non-streaming round, kept as the fallback when a
// provider rejects `stream: true`.
func chatRoundOnce(ctx context.Context, cfg Config, apiKey string, hc *http.Client, req openai.ChatCompletionRequest) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	endpoint, headers, sendCfg, httpClient := chatTarget(cfg, apiKey, hc)
	resp, err := postAPI(ctx, httpClient, sendCfg, endpoint, req, headers)
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
	return msg, out.Usage, validateModelReply(msg)
}

func chatFinishError(reason string) error {
	switch reason {
	case "", "stop", "tool_calls", "function_call":
		return nil
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
	resp, err := postAPI(ctx, httpClient, sendCfg, endpoint, req, headers)
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
		return msg, out.Usage, validateModelReply(msg)
	}

	var content, thinking strings.Builder
	accs := map[int]*chatToolAcc{}
	var order []int
	var eventErr error
	var terminal bool
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
				if idx < 0 || (tc.Type != "" && tc.Type != openai.ToolTypeFunction) {
					eventErr = errors.New("api: chat: invalid streamed tool call")
					return false
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
	if eventErr != nil {
		return openai.ChatCompletionMessage{}, usage, eventErr
	}
	if scanErr != nil {
		return openai.ChatCompletionMessage{}, usage, scanErr
	}
	if !terminal {
		return openai.ChatCompletionMessage{}, usage, errors.New("api: chat: incomplete stream (missing [DONE] or finish_reason)")
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
