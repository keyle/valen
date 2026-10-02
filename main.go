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
	// defaultMaxContextTokens fail-fasts absurd sends: stateless transports
	// resend all of history every round, so burn compounds as rounds ×
	// history. Assumption (reversible via max_context_tokens): well under
	// the smallest catalog model's limit, over any healthy session.
	defaultMaxContextTokens = 300000
	// userAgent rides as iPad Safari, not a bot/generic SDK string.
	// Assumption (reversible by editing one const): providers return fuller,
	// cleaner responses to a browser UA than to an automation identity.
	userAgent = "Mozilla/5.0 (iPad; CPU OS 18_7_8 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/27.0 Mobile/15E148 Safari/604.1"
	// defaultSystem applies when `system` is unset in config.toml. It keeps
	// the agent MCP-capable with a fast tool loopback and points blank
	// sessions at AGENTS.md first.
	defaultSystem = "You are a helpful assistant. You are an MCP-capable thinking agent with a fast tool loopback (read, write, edit, glob, grep, shell, web_search, web_fetch, plus MCP tools). On a blank session, first check for AGENTS.md in the workdir and follow it."
)

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
	Model            string           `toml:"model"`
	Thinking         string           `toml:"thinking"`
	BaseURL          string           `toml:"base_url"`
	APIKeyEnv        string           `toml:"api_key_env"`
	SessionID        string           `toml:"session_id"`
	System           string           `toml:"system"`
	MaxRounds        int              `toml:"max_rounds"`
	MaxContextTokens int              `toml:"max_context_tokens"`
	Temperature      *float32         `toml:"temperature"`
	Tools            []string         `toml:"tools"`
	MCPServers       []MCPServer      `toml:"mcp"`
	Compaction       CompactionConfig `toml:"compaction"`
}

// CompactionConfig mirrors the [compaction] table: summary-mode only,
// send-side (disk JSONL stays lossless). Model selects the summarizer:
// empty means the session model; set a cheap flash model to keep large
// sessions cheap for the big model.
type CompactionConfig struct {
	Auto       *bool  `toml:"auto"`
	KeepTokens int    `toml:"keep_tokens"`
	Buffer     int    `toml:"buffer"`
	Model      string `toml:"model"`
}

type MCPServer struct {
	Name string            `toml:"name"`
	Cmd  string            `toml:"cmd"`
	Args []string          `toml:"args"`
	Env  map[string]string `toml:"env"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
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
	cfg.Compaction = normalizeCompactionConfig(cfg.Compaction, cfg.MaxContextTokens)
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
	flag.Parse()

	// Resolve the config path before any chdir so relative paths keep working.
	cfgAbs := *cfgPath
	if abs, err := filepath.Abs(cfgAbs); err == nil {
		cfgAbs = abs
	}

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

	warn := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	closers := LoadMCP(ctx, cfg.MCPServers, registry, warn)
	defer closeAll(closers)

	client := newClient(cfg, apiKey)
	if !*printMode && isTTY(os.Stdin) && isTTY(os.Stdout) {
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
	maybeAutoCompact(ctx, cfg, client, apiKey, hc, history)
	var msg openai.ChatCompletionMessage
	var usage openai.Usage
	var err error
	switch modelTransport(cfg.Model) {
	case "messages":
		msg, usage, err = runMessagesTurn(ctx, cfg, apiKey, hc, registry, history, onEvent)
	case "responses":
		msg, usage, err = runResponsesTurn(ctx, cfg, apiKey, hc, registry, history, onEvent)
	default:
		msg, usage, err = runChatTurn(ctx, cfg, apiKey, hc, registry, history, onEvent)
	}
	return msg, usage, err
}

// maxParallelTools bounds concurrent tool execution in one round. Outputs
// stay capped by truncate, so memory stays flat; the semaphore only caps how
// many run at once.
const maxParallelTools = 8

// mcpMu serializes MCP tool calls: stdio JSON-RPC servers share one stdin
// pipe, so concurrent writes could interleave requests. Builtins run fully
// parallel; MCP calls share this mutex.
var mcpMu sync.Mutex

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

// estimateTokens approximates prompt tokens as runes/4 over content plus
// tool args. No tokenizer dep; good enough for a fail-fast guard.
func estimateTokens(msgs []openai.ChatCompletionMessage) int {
	var runes int
	for _, m := range msgs {
		runes += len([]rune(m.Content))
		for _, tc := range m.ToolCalls {
			runes += len([]rune(tc.Function.Arguments))
		}
	}
	return runes / 4
}

func contextBudget(cfg Config) int {
	if cfg.MaxContextTokens > 0 {
		return cfg.MaxContextTokens
	}
	return defaultMaxContextTokens
}

// checkContextBudget fail-fasts a round that would send a ridiculous context
// (and burn tokens × rounds on stateless transports) instead of letting it
// crawl into a 429. Compaction is the real fix; this is the guardrail.
func checkContextBudget(cfg Config, history []openai.ChatCompletionMessage) error {
	budget := contextBudget(cfg)
	if est := estimateTokens(history); est > budget {
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
	outs := make([]string, len(calls))
	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc openai.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if isMCPTool(tc.Function.Name) {
				mcpMu.Lock()
				defer mcpMu.Unlock()
			}
			outs[i] = registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
		}(i, tc)
	}
	wg.Wait()
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
	out := make([]openai.ChatCompletionMessage, 0, len(calls))
	for _, tc := range calls {
		if onEvent != nil {
			onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments, ID: tc.ID})
		}
		res := registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
		if onEvent != nil {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: res, ID: tc.ID})
		}
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: res})
	}
	return out
}

// runChatTurn is the /chat/completions tool loop, streaming each round.
func runChatTurn(ctx context.Context, cfg Config, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	for round := 0; round < cfg.MaxRounds; round++ {
		if err := checkContextBudget(cfg, *history); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		req := openai.ChatCompletionRequest{Model: cfg.Model, Messages: *history, Tools: registry.OpenAITools()}
		if cfg.Temperature != nil {
			req.Temperature = *cfg.Temperature
		}
		applyThinkingToRequest(&req, cfg.Model, cfg.Thinking)

		msg, roundUsage, err := chatRoundStream(ctx, cfg, apiKey, hc, req, onEvent)
		if err != nil {
			if !isStreamRejected(err) {
				return openai.ChatCompletionMessage{}, usage, err
			}
			msg, roundUsage, err = chatRoundOnce(ctx, cfg, apiKey, hc, req)
			if err != nil {
				return openai.ChatCompletionMessage{}, usage, err
			}
		}
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

		*history = append(*history, runToolCalls(ctx, registry, msg.ToolCalls, onEvent)...)
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
	history := make([]openai.ChatCompletionMessage, 0, 8)
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)})
	history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})

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
