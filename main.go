package main

import (
	"bufio"
	"context"
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
	cfgPath := flag.String("config", "", "path to config (default: ~/.config/valen/valen.toml, seeded on first run)")
	dirFlag := flag.String("dir", "", "working directory: sandbox root for file tools and shell")
	shellFlag := flag.String("shell", "", "shell interpreter for shell commands (default: sh)")
	printMode := flag.Bool("p", false, "one-shot: answer the quoted prompt and exit")
	qFlag := flag.Bool("q", false, "start the TUI and run the prompt immediately")
	planFlag := flag.Bool("plan", false, "plan mode: read-only investigation, present the plan as the reply")
	dvFlag := flag.Bool("dv", false, "debug verbose: timestamped round/tool/size trace to stderr")
	dvLogFlag := flag.String("dvlog", "", "debug verbose log file (implies -dv; use in TUI mode)")
	vFlag := flag.Bool("v", false, "headless verbose: stream thinking summaries and tool calls to stderr")
	vvFlag := flag.Bool("vv", false, "headless very verbose: also stream raw analysis, answer text and tool results to stderr")
	// -f / --file attach files, folders and images to the headless prompt
	// (repeatable); @path mentions in the prompt attach too.
	var attachFiles fileFlag
	flag.Var(&attachFiles, "f", "attach a file, folder or image (repeatable, headless)")
	flag.Var(&attachFiles, "file", "attach a file, folder or image (alias for -f)")
	streamFlag := flag.Bool("stream", false, "headless: write events as NDJSON on stdout, ending with a done line")
	historyFlag := flag.String("history", "", "headless: JSON array of prior user/assistant messages to continue from")
	resumeFlag := flag.String("r", "", "headless: continue a saved session (ID or JSONL path)")
	flag.StringVar(resumeFlag, "resume", "", "alias for -r")
	noQuestionFlag := flag.Bool("no-question", false, "headless: do not offer AskUserQuestion")
	noTaskFlag := flag.Bool("no-task", false, "headless: do not offer the task subagent")
	noTodoFlag := flag.Bool("no-todo", false, "headless: do not offer todowrite/todoread")
	oneShotFlag := flag.Bool("one-shot-task", false, "background job: -p with --no-question --no-task --no-todo")
	// flag.Parse stops at the first positional, but prompts are commonly
	// written before trailing flags: valen -p "..." -v. Reorder so flags win
	// regardless of position.
	flag.CommandLine.Parse(reorderFlags(os.Args[1:], map[string]bool{
		"config": true, "dir": true, "shell": true, "dvlog": true, "f": true, "file": true, "history": true, "r": true, "resume": true,
	}))
	setShellInterpreter(*shellFlag)
	if *oneShotFlag {
		*printMode, *noQuestionFlag, *noTaskFlag, *noTodoFlag = true, true, true, true
	}

	// Resolve paths before any chdir so relative paths keep working.
	// Default: the user config dir, seeded from the embedded default on first
	// run. An explicit -config pins that file: no seeding, and no saved
	// model/thinking overlay (so the file alone decides). -p also follows the
	// file: the state.json overlay is TUI-only.
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
	// Relative to the launch directory, like -config: -dir may chdir below.
	historyAbs := *historyFlag
	if historyAbs != "" {
		if abs, err := filepath.Abs(historyAbs); err == nil {
			historyAbs = abs
		}
	}
	// A path-like -r argument is resolved from the launch directory too; a
	// bare value stays a session ID and resolves in the project folder after
	// the chdir.
	resumeAbs := *resumeFlag
	if resumeAbs != "" && (filepath.IsAbs(resumeAbs) || strings.ContainsRune(resumeAbs, filepath.Separator) || strings.HasSuffix(resumeAbs, ".jsonl")) {
		if abs, err := filepath.Abs(resumeAbs); err == nil {
			resumeAbs = abs
		}
	}
	if err := dvlog.Init(*dvFlag, dvLogAbs); err != nil {
		return err
	}
	defer dvlog.Close()

	verbose := 0
	if *vvFlag {
		verbose = 2
	} else if *vFlag {
		verbose = 1
	}
	qMode := *qFlag
	// -q forces the TUI regardless of stdin (its prompt comes from the args,
	// not a pipe), so stdout still has to be a TTY for the alt screen.
	if qMode && (!isTTY(os.Stdin) || !isTTY(os.Stdout)) {
		return errors.New("-q needs a terminal (stdin and stdout must be TTYs)")
	}
	tuiMode := qMode || (!*printMode && isTTY(os.Stdin) && isTTY(os.Stdout))
	if tuiMode && len(attachFiles) > 0 {
		return errors.New("-f/--file attaches to a one-shot prompt (-p); in the TUI use @ or paste")
	}
	if tuiMode && verbose > 0 {
		return errors.New("-v/-vv apply to one-shot mode (-p)")
	}
	if tuiMode && (*streamFlag || historyAbs != "" || resumeAbs != "") {
		return errors.New("--stream, --history and -r/--resume apply to one-shot mode (-p)")
	}
	if resumeAbs != "" && historyAbs != "" {
		return errors.New("-r/--resume and --history cannot be combined")
	}
	// In -q mode the positional arg is the prompt, never a workdir.
	workdir, rest := splitWorkdirArg(*dirFlag, flag.Args(), tuiMode && !qMode)
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

	// Resume loads before the config so the saved ID can win over the
	// default new one, and before any client captures cfg.SessionID.
	var resumed *Session
	if resumeAbs != "" {
		path, err := resolveSessionArg(resumeAbs)
		if err != nil {
			return err
		}
		if resumed, err = openSession(path); err != nil {
			return fmt.Errorf("resume %s: %w", path, err)
		}
	}

	cfg, err := loadConfig(cfgAbs)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if resumed != nil {
		cfg.SessionID = resumed.ID
	}
	setSandboxConfig(cfg.Sandbox)
	if statePath != "" && tuiMode {
		cfg = applyUserState(cfg, statePath)
	}
	if *planFlag {
		cfg.Plan = true
	}

	apiKey := effectiveOpenCodeKey(cfg, "")
	// Missing keys never fail boot: the TUI still opens so the user can
	// browse /models, and the missing key surfaces when a model needing it
	// is picked or a turn is sent (see missingKeyError + dispatchTurn).
	// Trimmed: pasted keys with stray whitespace 401 otherwise.
	cfg.OpenRouterKey = effectiveOpenRouterKey(cfg)
	cfg.OpenAIKey = effectiveOpenAIKey(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), interruptSignals()...)
	defer stop()

	warn := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	set := toolSet{noQuestion: *noQuestionFlag, noTask: *noTaskFlag, noTodo: *noTodoFlag}
	registry, closers, err := buildRegistry(ctx, cfg, apiKey, set, warn)
	if err != nil {
		return err
	}
	defer closeAll(closers)

	client := newClient(cfg, apiKey)
	if tuiMode {
		dvlog.Logf("start model=%s transport=%s thinking=%s maxrounds=%d budget~%dtok schemastate~%dtok tools=%d tui=true",
			cfg.Model, modelTransport(cfg.Model), cfg.Thinking, cfg.MaxRounds, contextBudget(cfg), requestToolsTokens(registry), len(registry.Names()))
		initial := ""
		if qMode {
			if initial = strings.TrimSpace(strings.Join(rest, " ")); initial == "" {
				return errors.New("-q: empty prompt")
			}
		}
		return runTUI(ctx, cfg, statePath, client, registry, apiKey, initial)
	}

	prompt, err := readPrompt(rest, *printMode)
	if err != nil {
		return err
	}
	if prompt == "" {
		return errors.New("empty prompt")
	}
	opts := headlessOptions{files: attachFiles, verbose: verbose, stream: *streamFlag, noQuestion: *noQuestionFlag, resume: resumed}
	if historyAbs != "" {
		if opts.history, err = loadHistoryFile(historyAbs); err != nil {
			return err
		}
	}
	return chatWithAttachments(ctx, cfg, client, apiKey, newHTTPClient(cfg), registry, prompt, opts, os.Stdout, os.Stderr)
}

// toolSet switches off the tools a voice or job turn must not have: they must
// never block on stdin, spawn subagents, or keep todo state.
type toolSet struct {
	noQuestion, noTask, noTodo bool
}

// excludes reports whether a configured builtin is switched off by set. The
// config tools list cannot bring these back.
func (s toolSet) excludes(name string) bool {
	switch name {
	case "AskUserQuestion":
		return s.noQuestion
	case "todowrite", "todoread":
		return s.noTodo
	}
	return false
}

// buildRegistry assembles the tools for one run: the configured builtins, the
// interactive, plan and task tools unless set turns them off, then the MCP
// servers. The closers stop the MCP servers.
func buildRegistry(ctx context.Context, cfg Config, apiKey string, set toolSet, warn func(string, ...any)) (*Registry, []func(), error) {
	registry := NewRegistry()
	var builtins []string
	for _, name := range cfg.Tools {
		if !set.excludes(name) {
			builtins = append(builtins, name)
		}
	}
	for _, name := range registry.LoadBuiltins(builtins) {
		warn("config: unknown builtin tool: %s", name)
	}
	if !set.noQuestion && !registry.Has("AskUserQuestion") {
		if err := registry.Add(AskUserQuestionTool()); err != nil {
			return nil, nil, fmt.Errorf("register AskUserQuestion: %w", err)
		}
	}
	// Session plan tools are always present (like AskUserQuestion) so chunky
	// work can break into small todowrite steps even when valen.toml omits
	// them. In-memory only; the call records fold into the session JSONL.
	if !set.noTodo {
		for _, t := range []Tool{TodoWriteTool(), TodoReadTool()} {
			if !registry.Has(t.Name) {
				if err := registry.Add(t); err != nil {
					return nil, nil, fmt.Errorf("register %s: %w", t.Name, err)
				}
			}
		}
	}
	closers := LoadMCP(ctx, cfg.MCPServers, registry, warn)
	// task delegates to a same-model subagent so exploration stays out of
	// main history; always present like the plan tools. The runner closes
	// over the config here where it lives.
	if !set.noTask && !registry.Has("task") {
		if err := registry.Add(TaskTool(func(ctx context.Context, prompt, kind string) (string, error) {
			turnCfg := currentTurnConfig(ctx, cfg)
			return runTask(ctx, turnCfg, newClient(turnCfg, apiKey), apiKey, newHTTPClient(turnCfg), prompt, kind)
		})); err != nil {
			closeAll(closers)
			return nil, nil, fmt.Errorf("register task: %w", err)
		}
	}
	return registry, closers, nil
}

// fileFlag collects repeated -f/--file attachment paths in order.
type fileFlag []string

func (f *fileFlag) String() string { return strings.Join(*f, ", ") }

func (f *fileFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// reorderFlags moves flags (and the value they consume) ahead of positional
// args so flags may follow the prompt. A literal "--" ends flag parsing.
func reorderFlags(args []string, takesValue map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') || !takesValue[name] {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// splitWorkdirArg resolves the sandbox root: an explicit -dir flag wins,
// otherwise a single positional directory is taken as the workdir in TUI
// mode (where positionals carry no prompt). Headless prompts and -q's
// prompt are never stolen: -p "..." / -q "..." keep their args unless
// -dir is given.
func splitWorkdirArg(dirFlag string, args []string, stealDir bool) (string, []string) {
	if strings.TrimSpace(dirFlag) != "" {
		return dirFlag, args
	}
	if stealDir && len(args) > 0 {
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

// upstreamHTTPClient is the sessionless client for upstreams that don't use
// Go's identity headers (OpenRouter, OpenAI): the shared keep-alive pool
// without the x-opencode-session/UA transport.
var upstreamHTTPClient = &http.Client{Transport: sharedTransport}

// providerHTTPClient picks the client for cfg's provider. Go and Zen keep the
// Go identity transport; OpenAI and OpenRouter get the sessionless pool.
func providerHTTPClient(cfg Config, hc *http.Client) *http.Client {
	switch modelProvider(cfg.Model) {
	case ProviderOpenAI, ProviderOpenRouter:
		return upstreamHTTPClient
	}
	return hc
}

func newClient(cfg Config, apiKey string) *openai.Client {
	oc := openai.DefaultConfig(apiKey)
	oc.BaseURL = cfg.BaseURL
	oc.HTTPClient = newHTTPClient(cfg)
	return openai.NewClientWithConfig(oc)
}

// readPrompt resolves the one-shot prompt. With printMode the prompt must be
// a single quoted argument: -p review main.go would silently become
// "review main.go", so reject it instead of guessing where the prompt ends.
func readPrompt(args []string, printMode bool) (string, error) {
	if printMode && len(args) > 1 {
		return "", fmt.Errorf("-p: quote the prompt as one argument (got %d); e.g. -p \"review main.go\"", len(args))
	}
	if p := strings.TrimSpace(strings.Join(args, " ")); p != "" {
		return p, nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(b)), err
	}
	fmt.Fprint(os.Stderr, "prompt: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
