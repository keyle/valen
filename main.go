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
	qFlag := flag.Bool("q", false, "start the TUI and run the prompt immediately")
	planFlag := flag.Bool("plan", false, "plan mode: read-only investigation, present the plan as the reply")
	dvFlag := flag.Bool("dv", false, "debug verbose: timestamped round/tool/size trace to stderr")
	dvLogFlag := flag.String("dvlog", "", "debug verbose log file (implies -dv; use in TUI mode)")
	// -f / --file attach files, folders and images to the headless prompt
	// (repeatable); @path mentions in the prompt attach too.
	var attachFiles fileFlag
	flag.Var(&attachFiles, "f", "attach a file, folder or image (repeatable, headless)")
	flag.Var(&attachFiles, "file", "attach a file, folder or image (alias for -f)")
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

	qMode := *qFlag
	// -q forces the TUI regardless of stdin (its prompt comes from the args,
	// not a pipe), so stdout still has to be a TTY for the alt screen.
	if qMode && !isTTY(os.Stdout) {
		return errors.New("-q needs a terminal (stdout is not a TTY)")
	}
	tuiMode := qMode || (!*printMode && isTTY(os.Stdin) && isTTY(os.Stdout))
	if tuiMode && len(attachFiles) > 0 {
		return errors.New("-f/--file attaches to a one-shot prompt (-p); in the TUI use @ or paste")
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

	prompt, err := readPrompt(rest)
	if err != nil {
		return err
	}
	if prompt == "" {
		return errors.New("empty prompt")
	}
	return chatWithAttachments(ctx, cfg, client, apiKey, newHTTPClient(cfg), registry, prompt, attachFiles, os.Stdout, os.Stderr)
}

// fileFlag collects repeated -f/--file attachment paths in order.
type fileFlag []string

func (f *fileFlag) String() string { return strings.Join(*f, ", ") }

func (f *fileFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
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
