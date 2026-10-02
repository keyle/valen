package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sashabaranov/go-openai"
)

func testConfig(url string) Config {
	return Config{Model: "m", BaseURL: url, APIKeyEnv: "TEST_KEY", SessionID: "ses-tui", MaxRounds: 4}
}

func newTestModel(t *testing.T, cfg Config) *tuiModel {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	session, err := newSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg, newClient(cfg, "key"), NewRegistry(), session)
	m.ctx = context.Background()
	return m
}

func runSubmit(t *testing.T, m *tuiModel, prompt string) {
	t.Helper()
	cmd := m.submit(prompt)
	if cmd == nil {
		t.Fatalf("submit %q returned nil cmd (busy=%v notice=%q)", prompt, m.busy, m.notice)
	}
	msg := cmd()
	_, _ = m.Update(msg)
}

func TestTUIRendersConversationWithoutRolePrefixes(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.session.Turns = []Turn{{At: time.Now(), Prompt: "hi", Reply: "hello", Thinking: "secret plan", Usage: Usage{Prompt: 1, Completion: 2, Total: 3}}}
	text := m.renderChat()
	for _, want := range []string{"hi", "hello", "secret plan"} {
		if !strings.Contains(text, want) {
			t.Fatalf("chat missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "user:") || strings.Contains(text, "assistant:") {
		t.Fatalf("chat must not use user:/assistant: prefixes:\n%s", text)
	}
	// Thinking is visible by default.
	if !m.showThink {
		t.Fatal("thinking should be shown by default")
	}
	m.showThink = false
	m.refreshViewport()
	collapsed := m.renderChat()
	if strings.Contains(collapsed, "secret plan") {
		t.Fatal("hidden thinking should collapse")
	}
	if !strings.Contains(collapsed, "thinking") {
		t.Fatalf("collapsed marker missing:\n%s", collapsed)
	}
}

func TestTUIPendingShowsImmediately(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"late pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	m := newTestModel(t, testConfig(srv.URL))
	cmd := m.submit("hello pending")
	if cmd == nil {
		t.Fatal("submit returned nil")
	}
	if !m.busy {
		t.Fatal("model should be busy right after submit")
	}
	// User message is visible before the response arrives.
	if !strings.Contains(m.renderViewportContent(), "hello pending") {
		t.Fatalf("pending user message not rendered:\n%s", m.renderViewportContent())
	}
	if len(m.session.Turns) != 0 {
		t.Fatalf("turn must not be committed before response: %+v", m.session.Turns)
	}
	done := make(chan any, 1)
	go func() { done <- cmd() }()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn cmd stuck")
	}
}

func TestTUISubmitTurnCompletes(t *testing.T) {
	var gotAuth, gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer srv.Close()

	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "ping")
	if m.busy {
		t.Fatal("still busy after done")
	}
	if len(m.session.Turns) != 1 || m.session.Turns[0].Reply != "pong" {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	if !strings.Contains(m.renderChat(), "pong") {
		t.Fatal("reply not rendered")
	}
	if !strings.Contains(m.renderChat(), "ping") {
		t.Fatal("user prompt missing from transcript")
	}
	if gotAuth != "Bearer key" || gotSession != "ses-tui" || gotUA != "mods/1.0" {
		t.Fatalf("headers: %q %q %q", gotAuth, gotSession, gotUA)
	}
	if m.session.TotalUsage.Total != 3 {
		t.Fatalf("usage: %+v", m.session.TotalUsage)
	}
}

func TestTUIFriendlyErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		fmt.Fprint(w, `{"error":{"message":"insufficient account funds","type":"billing"}}`)
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "ping")
	if len(m.session.Turns) != 1 || m.session.Turns[0].Error == nil {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	if !strings.Contains(m.session.Turns[0].Error.Title, "Out of credits") {
		t.Fatalf("error title: %+v", m.session.Turns[0].Error)
	}
	if !strings.Contains(m.renderChat(), "Out of credits") {
		t.Fatal("error not rendered")
	}
}

func TestTUIHistoryAndToolsViews(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.session.Turns = []Turn{{At: time.Now(), Prompt: "go", Reply: "ok", Tools: []ToolRecord{{Name: "shell", Args: `{"command":"ls"}`, Output: "main.go"}}}}
	m.show(viewHistory)
	hist := m.renderViewportContent()
	if !strings.Contains(hist, "go") {
		t.Fatalf("history missing prompt:\n%s", hist)
	}
	m.show(viewTools)
	tools := m.renderViewportContent()
	if !strings.Contains(tools, "shell") {
		t.Fatalf("tools missing shell:\n%s", tools)
	}
	m.openSelected()
	if m.mode != viewDetail || !strings.Contains(m.detailBody, "main.go") {
		t.Fatalf("tool detail not rendered: mode=%v body=%q", m.mode, m.detailBody)
	}
}

func TestTUICommandClear(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.System = "sys"
	m.history = seedHistory(m.cfg)
	m.history = append(m.history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "a"})
	m.session.Turns = append(m.session.Turns, Turn{Prompt: "a"})
	m.handleCommand("/clear")
	if len(m.history) != 1 || len(m.session.Turns) != 0 {
		t.Fatalf("history=%d turns=%d", len(m.history), len(m.session.Turns))
	}
}

func TestTUIThinkCommandToggles(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	before := m.showThink
	m.handleCommand("/think")
	if m.showThink == before {
		t.Fatal("/think should toggle")
	}
}

func TestTUICtrlShortcuts(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))

	if footer := m.footerView(); strings.Contains(footer, "F1") || strings.Contains(footer, "ctrl+t") {
		t.Fatalf("footer must not list shortcuts:\n%s", footer)
	}
	if help := m.renderHelp(); !strings.Contains(help, "ctrl+g help") || strings.Contains(help, "F1 help") {
		t.Fatalf("help must list the ctrl shortcuts:\n%s", help)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.mode != viewHelp {
		t.Fatalf("ctrl+g: mode=%v", m.mode)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m.mode != viewHistory {
		t.Fatalf("ctrl+r: mode=%v", m.mode)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.mode != viewTools {
		t.Fatalf("ctrl+t: mode=%v", m.mode)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	if m.mode != viewErrors {
		t.Fatalf("ctrl+x: mode=%v", m.mode)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlY})
	if m.showThink {
		t.Fatal("ctrl+y should toggle thinking off")
	}
	beforeMeta := m.showMeta
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.showMeta == beforeMeta {
		t.Fatal("ctrl+o should toggle status details")
	}
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlP}); cmd == nil || !strings.Contains(m.notice, "Loading models") {
		t.Fatalf("ctrl+p should open the model picker: notice=%q cmd=%v", m.notice, cmd)
	}
	m.session = nil
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !m.noticeErr || !strings.Contains(m.notice, "No session") {
		t.Fatalf("ctrl+s without session: notice=%q err=%v", m.notice, m.noticeErr)
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if cmd == nil {
		t.Fatal("ctrl+q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+q should return tea.Quit")
	}
}

func TestTUIUnknownCommandNotices(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.handleCommand("/nope")
	if !m.noticeErr || !strings.Contains(m.notice, "/nope") {
		t.Fatalf("notice=%q err=%v", m.notice, m.noticeErr)
	}
}

func newTestModelWithConfig(t *testing.T, body string) (*tuiModel, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := newSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg, newClient(cfg, "key"), NewRegistry(), session)
	m.ctx = context.Background()
	m.cfgPath = path
	return m, path
}

func TestTUIModelDirectSwitchPersists(t *testing.T) {
	m, path := newTestModelWithConfig(t, "model = \"glm-5.3-flash\"\nthinking = \"max\"\nbase_url = \"https://unused\"\n")
	m.handleCommand("/model kimi-k3")
	if m.cfg.Model != "kimi-k3" {
		t.Fatalf("model=%q notice=%q", m.cfg.Model, m.notice)
	}
	if m.cfg.Thinking != "" {
		t.Fatalf("max is invalid on kimi and must reset, got %q", m.cfg.Thinking)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `model = "kimi-k3"`) || strings.Contains(string(raw), "thinking") {
		t.Fatalf("config not persisted:\n%s", raw)
	}
	if !strings.Contains(m.notice, "Switched") || !strings.Contains(m.notice, "Saved") {
		t.Fatalf("notice=%q", m.notice)
	}
}

func TestTUIModelSwitchSessionOnlyWithoutPath(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.handleCommand("/model glm-5.3")
	if m.cfg.Model != "glm-5.3" {
		t.Fatalf("model=%q", m.cfg.Model)
	}
	if !strings.Contains(m.notice, "session only") {
		t.Fatalf("notice=%q", m.notice)
	}
}

func TestTUIModelPickerListsAndSelects(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"kimi-k3", "glm-5.3", "gpt-5.6-luna"}, filter: ""})
	if m.mode != viewModels || len(m.modelChoices) != 3 {
		t.Fatalf("mode=%v choices=%v", m.mode, m.modelChoices)
	}
	if m.modelChoices[0].id != "glm-5.3" {
		t.Fatalf("choices should be sorted: %v", m.modelChoices)
	}
	text := m.renderViewportContent()
	if !strings.Contains(text, "gpt-5.6-luna") || !strings.Contains(text, "/responses") {
		t.Fatalf("incompatible transport must be marked:\n%s", text)
	}
	m.openSelected() // cursor 0 → glm-5.3
	if m.cfg.Model != "glm-5.3" || m.mode != viewChat {
		t.Fatalf("model=%q mode=%v notice=%q", m.cfg.Model, m.mode, m.notice)
	}
}

func TestTUIModelPickerFallbackOnError(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(modelsLoadedMsg{err: fmt.Errorf("boom"), filter: ""})
	if m.mode != viewModels || len(m.modelChoices) == 0 {
		t.Fatalf("mode=%v choices=%d", m.mode, len(m.modelChoices))
	}
	if !strings.Contains(m.notice, "known models") {
		t.Fatalf("notice=%q", m.notice)
	}
}

func TestTUIModelFilterSwitchesDirectly(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_ = m.openModelPicker("kimi")
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"glm-5.3", "kimi-k3"}, filter: "kimi"})
	if m.cfg.Model != "kimi-k3" || m.mode != viewChat {
		t.Fatalf("model=%q mode=%v", m.cfg.Model, m.mode)
	}
	_ = m.openModelPicker("zzz")
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"glm-5.3", "kimi-k3"}, filter: "zzz"})
	if !m.noticeErr || !strings.Contains(m.notice, "No models match") {
		t.Fatalf("notice=%q", m.notice)
	}
}

func TestTUIModelStaleFetchIgnored(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_ = m.openModelPicker("new")
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"glm-5.3"}, filter: "old"})
	if m.mode == viewModels {
		t.Fatal("stale fetch must not open the picker")
	}
}

func TestTUIEffortPickerAndSet(t *testing.T) {
	m, path := newTestModelWithConfig(t, "model = \"glm-5.3\"\nbase_url = \"https://unused\"\n")
	m.openEffortPicker()
	if m.mode != viewEffort || len(m.effortLevels) != 3 {
		t.Fatalf("mode=%v levels=%v", m.mode, m.effortLevels)
	}
	m.applyThinkingSwitch("bogus")
	if !m.noticeErr {
		t.Fatalf("notice=%q", m.notice)
	}
	m.applyThinkingSwitch("max")
	if m.cfg.Thinking != "max" {
		t.Fatalf("thinking=%q", m.cfg.Thinking)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `thinking = "max"`) {
		t.Fatalf("thinking not persisted:\n%s", raw)
	}
	if !strings.Contains(m.headerView(), "max") {
		t.Fatal("header should show the thinking level")
	}
}

func TestTUIEffortAliasAndClear(t *testing.T) {
	m, _ := newTestModelWithConfig(t, "model = \"kimi-k3\"\nthinking = \"high\"\nbase_url = \"https://unused\"\n")
	m.handleCommand("/thinking default")
	if m.cfg.Thinking != "" {
		t.Fatalf("thinking=%q", m.cfg.Thinking)
	}
	m.handleCommand("/effort none")
	if m.cfg.Thinking != "none" {
		t.Fatalf("thinking=%q", m.cfg.Thinking)
	}
}
