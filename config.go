package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/keyle/valen/internal/guard"
)

const (
	defaultBaseURL               = "https://opencode.ai/zen/go/v1"
	openCodeKeyEnv               = "OPENCODE_API_KEY"
	openRouterKeyEnv             = "OPENROUTER_API_KEY"
	defaultMaxRounds             = 300
	defaultMaxTokens             = 16384
	defaultRequestTimeoutSeconds = 300
	// defaultMaxContextTokens is the fail-fast ceiling for models with no
	// known window (see modelContextWindow): stateless transports resend
	// all of history every round, so burn compounds as rounds × history.
	// Explicit max_context_tokens always wins; 0 means per-model auto.
	defaultMaxContextTokens = 300000
)

// Config mirrors config.toml. See SPEC.md for the full schema.
type Config struct {
	Model    string `toml:"model"`
	Thinking string `toml:"thinking"`
	// Routing selects OpenRouter's provider-sorting variant (floor/nitro/
	// exacto; empty = none). Ignored on Go/Zen: the suffix rides the wire
	// model only, so the stored model never carries it.
	Routing string `toml:"routing"`
	BaseURL string `toml:"base_url"`
	// OpenCodeKey / OpenRouterKey are the API keys themselves. Empty by
	// default: the OPENCODE_API_KEY / OPENROUTER_API_KEY env vars are
	// used instead. persistModelSelection only writes model/thinking
	// lines, and nothing logs these.
	OpenCodeKey           string `toml:"opencode_key"`
	OpenRouterKey         string `toml:"openrouter_key"`
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
	TUI         TUIConfig        `toml:"tui"`
	Sandbox     SandboxConfig    `toml:"sandbox"`
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

// TUIConfig mirrors the [tui] table: transcript rendering and the
// attention bell (TUI only). Sound nil means on: ring the terminal bell
// when a question appears or a turn finishes.
// MaxLines nil means the 1000-line default; 0 or less disables trimming.
// When the rendered committed transcript exceeds the cap, older turns
// collapse their reasoning (prompts, tool lines and replies stay whole)
// and, if needed, the oldest turns drop from scrollback. History on disk
// stays lossless and the recall views still expand everything.
type TUIConfig struct {
	MaxLines *int  `toml:"max_lines"`
	Sound    *bool `toml:"sound"`
	// SelectScrollMs paces the drag auto-scroll: one viewport line per
	// interval. <= 0 means the 150ms default.
	SelectScrollMs int `toml:"select_scroll_ms"`
}

// defaultSelectScrollMs paces the transcript selection drag auto-scroll.
const defaultSelectScrollMs = 150

// tuiSelScrollInterval is the drag auto-scroll tick: the configured
// select_scroll_ms, else 150ms.
func tuiSelScrollInterval(cfg Config) time.Duration {
	ms := cfg.TUI.SelectScrollMs
	if ms <= 0 {
		ms = defaultSelectScrollMs
	}
	return time.Duration(ms) * time.Millisecond
}

// defaultTUIMaxLines bounds the rendered committed transcript so the
// viewport never re-parses an ever-growing scrollback (the TUI slowdown
// after long sessions). Reasoning collapses first; prompts/tools/replies survive.
const defaultTUIMaxLines = 1000

// tuiMaxLines is the transcript line budget: the default when unset, else
// the configured value (0 or less means unlimited).
func tuiMaxLines(cfg Config) int {
	if cfg.TUI.MaxLines == nil {
		return defaultTUIMaxLines
	}
	return *cfg.TUI.MaxLines
}

// tuiSound reports whether the attention bell is on, default on.
func tuiSound(cfg Config) bool {
	return cfg.TUI.Sound == nil || *cfg.TUI.Sound
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
	if err := validateModelID(cfg.Model); err != nil {
		return cfg, err
	}
	cfg.Thinking = strings.ToLower(strings.TrimSpace(cfg.Thinking))
	if !validThinkingLevel(cfg.Model, cfg.Thinking) {
		return cfg, fmt.Errorf("thinking %q is not available for model %q (try: %s)", cfg.Thinking, cfg.Model, strings.Join(AvailableThinkingLevels(cfg.Model), ", "))
	}
	cfg.Routing = normalizeRouting(cfg.Routing)
	if !validRouting(cfg.Routing) {
		return cfg, fmt.Errorf("routing %q is not valid (try: none, floor, nitro, exacto)", cfg.Routing)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.MaxRounds == 0 {
		cfg.MaxRounds = defaultMaxRounds
	}
	// MaxContextTokens 0 means auto: contextBudget resolves the model's own
	// window (modelContextWindow) or the safe default. Explicit values
	// always win and survive /model switches.
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
	cfg.Compaction = normalizeCompactionConfig(cfg.Compaction, contextBudget(cfg))
	cfg.Input.Height = inputHeight(cfg)
	if err := cfg.Sandbox.validate(); err != nil {
		return cfg, err
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
