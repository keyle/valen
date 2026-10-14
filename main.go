package main

import (
	"bufio"
	"bytes"
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

	"github.com/keyle/valen/internal/dvlog"
	"github.com/keyle/valen/internal/guard"
	"github.com/keyle/valen/internal/prompts"
	"github.com/sashabaranov/go-openai"
)

// version is the binary version; `make prod` stamps it via
// -ldflags -X main.version (dev builds report "dev").
var version = "dev"

// userAgent identifies valen to the OpenCode APIs: an honest SDK string,
// not a spoofed browser — Go's TLS fingerprint doesn't match Safari anyway,
// so a Safari UA to an API looks more bot-like, not less.
var userAgent = "github.com/keyle/valen/" + version + " (opencode-compatible agent harness)"

// browserUserAgent is only for web_search/web_fetch, where the scraped
// sites' bot defenses expect a browser.
const browserUserAgent = "Mozilla/5.0 (iPad; CPU OS 18_7_8 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/27.0 Mobile/15E148 Safari/604.1"

// effectiveSystem returns the configured system prompt, or the harness
// prompt files (plus plan overlay and the user's AGENTS.md) when none is set.
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
	return prompts.SystemPrompt(cfg.Model, cfg.Plan, cwd) + userAgentsSection() + localAgentsSection(cwd)
}

// seedHistory builds the initial history for a turn.
func seedHistory(cfg Config) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)}}
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
	cfgPath := flag.String("config", "", "path to config (default: ~/.config/valen/config.toml, seeded on first run)")
	dirFlag := flag.String("dir", "", "working directory: sandbox root for file tools and shell")
	printMode := flag.Bool("p", false, "one-shot: answer the prompt and exit")
	planFlag := flag.Bool("plan", false, "plan mode: read-only investigation, present the plan as the reply")
	dvFlag := flag.Bool("dv", false, "debug verbose: timestamped round/tool/size trace to stderr")
	dvLogFlag := flag.String("dvlog", "", "debug verbose log file (implies -dv; use in TUI mode)")
	flag.Parse()

	// Resolve paths before any chdir so relative paths keep working.
	// Default: the user config dir, seeded from the embedded default on first
	// run. An explicit -config pins that file: no seeding, and no saved
	// model/thinking overlay (so the file alone decides).
	cfgAbs := *cfgPath
	statePath := ""
	if cfgAbs == "" {
		dir, err := userConfigDir()
		if err != nil {
			return fmt.Errorf("config dir: %w", err)
		}
		if cfgAbs, err = ensureUserConfig(dir); err != nil {
			return fmt.Errorf("config dir %s: %w", dir, err)
		}
		statePath = filepath.Join(dir, userStateName)
		setAccessFile(filepath.Join(dir, userAccessName))
	} else if abs, err := filepath.Abs(cfgAbs); err == nil {
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
	setSandboxConfig(cfg.Sandbox)
	if statePath != "" {
		cfg = applyUserState(cfg, statePath)
	}
	if *planFlag {
		cfg.Plan = true
	}

	apiKey := effectiveOpenCodeKey(cfg, "")
	// Missing keys never fail boot: the TUI still opens so the user can
	// browse /model, and the missing key surfaces when a model needing it
	// is picked or a turn is sent (see missingKeyError + dispatchTurn).
	// Trimmed: pasted keys with stray whitespace 401 otherwise.
	cfg.OpenRouterKey = effectiveOpenRouterKey(cfg)

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
		return runTUI(ctx, cfg, statePath, client, registry, apiKey)
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
	// summary_index, messages content-block index). Fragments that share an
	// index concatenate exactly; the first delta of a new part starts on its
	// own line, mirroring the committed join of separate summary parts.
	Index int
	// ORMeta carries OpenRouter serving metadata (provider, billed cost,
	// locally-measured throughput) on an "or_meta" event.
	ORMeta *ORMeta
}

// ORMeta is OpenRouter's best-effort per-round serving metadata: the upstream
// provider that served the request, the billed cost and the throughput
// measured from our own stream. Any field may be absent.
type ORMeta struct {
	Provider string
	Cost     float64
	HasCost  bool
	TPS      float64
}

// parseOpenRouterMeta folds the serving provider and billed cost from one
// completion body (unary) or SSE chunk into the targets. Missing fields leave
// them untouched; `usage.cost` is preferred, falling back to
// `cost_details.upstream_inference_cost`.
func parseOpenRouterMeta(raw []byte, provider *string, cost *float64, hasCost *bool) {
	if !bytes.Contains(raw, []byte("provider")) && !bytes.Contains(raw, []byte("cost")) {
		return
	}
	var m struct {
		Provider string `json:"provider"`
		Usage    *struct {
			Cost        float64 `json:"cost"`
			CostDetails *struct {
				UpstreamInferenceCost float64 `json:"upstream_inference_cost"`
			} `json:"cost_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if m.Provider != "" {
		*provider = m.Provider
	}
	if m.Usage != nil {
		c := m.Usage.Cost
		if c == 0 && m.Usage.CostDetails != nil {
			c = m.Usage.CostDetails.UpstreamInferenceCost
		}
		if c != 0 {
			*cost, *hasCost = c, true
		}
	}
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
		// long: compact send-side, then retry the turn once. A non-round
		// failure after the retry is terminal.
		if isContextOverflow(err) && compactionAuto(cfg) {
			if _, ok, cerr := compactHistory(ctx, cfg, client, apiKey, hc, history); cerr == nil && ok {
				m2, u2, err2 := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
				usage.PromptTokens += u2.PromptTokens
				usage.CompletionTokens += u2.CompletionTokens
				usage.TotalTokens += u2.TotalTokens
				if err2 != nil {
					maybeLearnThinkingLevels(cfg.Model, err2.Error())
				}
				if err2 == nil || !isMaxRoundsStop(err2) {
					return m2, usage, err2
				}
				msg, err = m2, err2
			}
		}
		// Long-job continuation: the round backstop fired but the turn may
		// still be productive, so compact send-side and grant exactly one
		// more window. A second exhaustion is terminal and says so.
		if isMaxRoundsStop(err) && compactionAuto(cfg) {
			if _, ok, cerr := compactHistory(ctx, cfg, client, apiKey, hc, history); cerr == nil && ok {
				dvlog.Logf("maxrounds extend: compacted, second window of %d rounds", cfg.MaxRounds)
				m3, u3, err3 := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
				usage.PromptTokens += u3.PromptTokens
				usage.CompletionTokens += u3.CompletionTokens
				usage.TotalTokens += u3.TotalTokens
				if err3 != nil {
					maybeLearnThinkingLevels(cfg.Model, err3.Error())
				}
				if err3 == nil {
					return m3, usage, nil
				}
				if isMaxRoundsStop(err3) {
					return m3, usage, fmt.Errorf("stopped after %d rounds across 2 compacted windows without a final answer — ask the model to continue with a narrower ask or /clear", cfg.MaxRounds*2)
				}
				return m3, usage, err3
			}
		}
	}
	return msg, usage, err
}

// effectiveOpenCodeKey resolves the OpenCode credential (one key serves Go
// and Zen): the boot snapshot first, then the opencode_key setting, then a
// live env read (OPENCODE_API_KEY) so an export after boot works without a
// restart.
func effectiveOpenCodeKey(cfg Config, apiKey string) string {
	if k := strings.TrimSpace(apiKey); k != "" {
		return k
	}
	if k := strings.TrimSpace(cfg.OpenCodeKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(openCodeKeyEnv))
}

// effectiveOpenRouterKey resolves the OpenRouter credential the same way.
func effectiveOpenRouterKey(cfg Config) string {
	if k := strings.TrimSpace(cfg.OpenRouterKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(openRouterKeyEnv))
}

// missingKeyError is nil when the model in cfg can be called; otherwise an
// "is not set" error naming the env var the provider needs. Boot never
// fails on this — it surfaces on model pick (TUI notice) and on send
// (dispatchTurn), where friendlyError turns it into a transcript message.
func missingKeyError(cfg Config, apiKey string) error {
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		if effectiveOpenRouterKey(cfg) == "" {
			return fmt.Errorf("%s is not set (required by model %q)", openRouterKeyEnv, cfg.Model)
		}
		return nil
	}
	if effectiveOpenCodeKey(cfg, apiKey) == "" {
		return fmt.Errorf("%s is not set (required by model %q)", openCodeKeyEnv, cfg.Model)
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
	apiKey = effectiveOpenCodeKey(cfg, apiKey)
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
// It is also the last line of defense against poisoned history: the client
// serializes empty `content` as an absent key, which Anthropic-backed
// providers reject (400 "text content parts must carry a string text") on
// every later turn. So no message leaves here with blank content unless it
// legitimately carries tool_calls.
func chatSendMessages(history []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(history))
	for _, m := range history {
		blank := strings.TrimSpace(m.Content) == "" && len(m.MultiContent) == 0
		switch m.Role {
		case openai.ChatMessageRoleTool:
			m.Content = fitSendBudget(m.Content)
		case openai.ChatMessageRoleUser:
			if blank {
				m.Content = "(empty message)"
			}
		case openai.ChatMessageRoleAssistant:
			if blank && len(m.ToolCalls) == 0 {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// maxParallelTools bounds concurrent tool execution in one round. Outputs
// stay capped by truncate, so memory stays flat; the semaphore only caps how
// many run at once.
const maxParallelTools = 8

// maxParallelTasks bounds concurrent subagents in one round; extra task calls
// queue. Each is a full provider loop, so the cap guards rate limits.
const maxParallelTasks = 5

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
	// Auto: the model's own window when known, else the safe default.
	if w := modelContextWindow(cfg.Model); w > 0 {
		return w
	}
	return defaultMaxContextTokens
}

// isMaxRoundsStop reports the round-backstop error: the loop spent its whole
// window without a final answer. Budget and overflow errors never match.
func isMaxRoundsStop(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "stopped after")
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

// Only known read-only builtins and task subagents may overlap. Mutating,
// interactive and unknown tools run serially; the registry owns its central
// gates (including MCP). Subagent file writes are safe via the per-path lock
// in write/edit; shell writes are not covered (the prompt says so).
// The coordinator emits results as completed, but history stays request-ordered.
func runToolCalls(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	if len(calls) == 0 {
		return nil
	}
	for _, tc := range calls {
		switch tc.Function.Name {
		case "read", "glob", "grep", "web_search", "web_fetch", "task":
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
	taskSem := make(chan struct{}, maxParallelTasks)
	for i, tc := range calls {
		go func(i int, tc openai.ToolCall) {
			// Subagents queue on their own cap and skip the read-tool slots.
			s := sem
			if tc.Function.Name == "task" {
				s = taskSem
			}
			select {
			case s <- struct{}{}:
			case <-ctx.Done():
				results <- result{i, "error: " + ctx.Err().Error(), 0}
				return
			}
			defer func() { <-s }()
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
		msg, u, err := roundWithRetry(ctx, onEvent, *history, round)
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
		req := openai.ChatCompletionRequest{Model: wireModelID(cfg.Model, cfg.Routing), Messages: chatSendMessages(history), Tools: tools, MaxTokens: outputTokens(cfg)}
		if cfg.Temperature != nil {
			req.Temperature = *cfg.Temperature
		}
		applyThinkingToRequest(&req, cfg.Model, cfg.Thinking)
		round := func() (openai.ChatCompletionMessage, openai.Usage, error) {
			if stream {
				msg, usage, err := chatRoundStream(ctx, cfg, apiKey, hc, req, onEvent)
				if !isStreamRejected(err) {
					return msg, usage, err
				}
				stream = false
			}
			return chatRoundOnce(ctx, cfg, apiKey, hc, req)
		}
		msg, usage, err := round()
		if errors.Is(err, errContentFilter) && ctx.Err() == nil {
			// Provider filters are often false positives and non-deterministic;
			// retry the round once. Nothing was executed from the partial reply.
			msg, usage, err = round()
		}
		return msg, usage, err
	})
}
