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
	"time"

	"github.com/BurntSushi/toml"
	"github.com/sashabaranov/go-openai"
)

const (
	defaultConfigPath = "config.toml"
	defaultBaseURL    = "https://opencode.ai/zen/go/v1"
	defaultAPIKeyEnv  = "OPENCODE_GO_API_KEY"
	defaultMaxRounds  = 9999
	userAgent         = "mods/1.0"
)

// Config mirrors config.toml. See SPEC.md for the full schema.
type Config struct {
	Model       string      `toml:"model"`
	Thinking    string      `toml:"thinking"`
	BaseURL     string      `toml:"base_url"`
	APIKeyEnv   string      `toml:"api_key_env"`
	SessionID   string      `toml:"session_id"`
	System      string      `toml:"system"`
	MaxRounds   int         `toml:"max_rounds"`
	Temperature *float32    `toml:"temperature"`
	Tools       []string    `toml:"tools"`
	MCPServers  []MCPServer `toml:"mcp"`
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
		fmt.Fprintln(os.Stderr, "mods:", err)
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

func newHTTPClient(cfg Config) *http.Client {
	return &http.Client{Transport: &headerTransport{base: http.DefaultTransport, sessionID: cfg.SessionID}}
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
}

// runTurn drives the tool loop for one user turn. history grows in place with
// assistant and tool messages so the caller can keep the conversation going.
// It dispatches on model transport: chat (/chat/completions), messages
// (/messages) or responses (/responses). History stays OpenAI-shaped; the
// non-chat transports convert per round and fold replies back.
func runTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	switch modelTransport(cfg.Model) {
	case "messages":
		return runMessagesTurn(ctx, cfg, apiKey, hc, registry, history, onEvent)
	case "responses":
		return runResponsesTurn(ctx, cfg, apiKey, hc, registry, history, onEvent)
	default:
		return runChatTurn(ctx, cfg, apiKey, hc, registry, history, onEvent)
	}
}

// runChatTurn is the /chat/completions tool loop, streaming each round.
func runChatTurn(ctx context.Context, cfg Config, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	for round := 0; round < cfg.MaxRounds; round++ {
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

		*history = append(*history, msg)
		if len(msg.ToolCalls) == 0 {
			return msg, usage, nil
		}

		for _, tc := range msg.ToolCalls {
			if onEvent != nil {
				onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments})
			}
			out := registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
			if onEvent != nil {
				onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: out})
			}
			*history = append(*history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: out})
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
	history := make([]openai.ChatCompletionMessage, 0, 8)
	if cfg.System != "" {
		history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: cfg.System})
	}
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
