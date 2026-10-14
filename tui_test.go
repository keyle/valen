package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sashabaranov/go-openai"
)

func TestTUIStreamChunksPreserveTextAndRollingTail(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	for _, text := range []string{"Hel", "lo", " ", "world", "\n", "\t", "next", "\n\n"} {
		_, _ = m.Update(toolEventMsg{kind: "text_delta", text: text})
	}
	if m.pendingText != "Hello world\n\tnext\n\n" {
		t.Fatalf("chunks changed: %q", m.pendingText)
	}
	for _, appendText := range []func(string, string) string{appendStreamLine, appendStreamText} {
		s := appendText(strings.Repeat("x", 32768), "latest")
		if len(s) != 32768 || !strings.HasSuffix(s, "latest") {
			t.Fatal("capped stream froze instead of keeping the newest text")
		}
		s = appendText("old", strings.Repeat("y", 40000)+"newest")
		if len(s) != 32768 || !strings.HasSuffix(s, "newest") || strings.Contains(s, "old") {
			t.Fatal("oversized chunk did not keep its tail")
		}
		s = appendText(strings.Repeat("\u00e9", 16384), "z")
		if !utf8.ValidString(s) || len(s) > 32768 || !strings.HasSuffix(s, "z") {
			t.Fatal("rolling cap split a UTF-8 rune")
		}
	}
}

func TestTUIModelSwitchReseedsFamilyWithoutLosingHistory(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.history = append(m.history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "working state"})
	for _, model := range []string{"kimi-k3", "claude-sonnet-4-6", "gpt-6"} {
		m.applyModelSwitch(model)
		if len(m.history) != 2 || m.history[1].Content != "working state" || m.history[0].Content != effectiveSystem(m.cfg) {
			t.Fatalf("switch to %s lost history or retained old system", model)
		}
	}
	m.cfg.System = "custom system"
	m.applyModelSwitch("glm-5.3")
	if m.history[0].Content != "custom system" {
		t.Fatal("model switch ignored explicit system override")
	}
	m.history = m.history[1:]
	m.applyModelSwitch("kimi-k3")
	if len(m.history) != 2 || m.history[1].Content != "working state" {
		t.Fatal("reseeding missing system dropped conversation")
	}
}

func TestTUICompletionUsesRequestTimeModelAndEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"reply"}}]}`)
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.Model, cfg.Thinking = "glm-5.3", "high"
	m := newTestModel(t, cfg)
	cmd := m.submit("work")
	m.applyModelSwitch("kimi-k3")
	m.applyThinkingSwitch("max")
	_, _ = m.Update(cmd())
	turn := m.session.Turns[0]
	if turn.Model != "glm-5.3" || turn.Effort != "high" || m.session.Model != "glm-5.3" || m.session.Effort != "high" {
		t.Fatalf("request attributed to later settings: %+v", turn)
	}
	if turn.History[0].Content != effectiveSystem(cfg) || m.history[0].Content != effectiveSystem(m.cfg) {
		t.Fatal("completion overwrote the next model's system or mutated the checkpoint")
	}
}

func TestTUIFailedStreamRetainsPartialProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Force an incomplete body after delivering a valid chunk.
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial thinking\",\"content\":\"partial answer\"}}]}\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	turns, err := loadSession(m.session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Error == nil || turns[0].Reply != "partial answer" || turns[0].Thinking != "partial thinking" || len(turns[0].History) != 2 {
		t.Fatalf("failed stream discarded partial progress: %+v", turns)
	}
}

func TestTUISessionsListShowsTitlesAndResumes(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	// Seed two other sessions directly on disk.
	mk := func(id, prompt, model, effort string) {
		s, err := newSession(Config{SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.append(Turn{At: time.Now(), Prompt: prompt, Reply: "r", Model: model, Effort: effort}); err != nil {
			t.Fatal(err)
		}
		s.ensureMeta(model, effort)
	}
	mk("ses-alpha", "fix the login redirect bug on retry", "glm-5.3", "high")
	mk("ses-beta", "second session prompt here", "kimi-k3", "max")

	m.show(viewSessions)
	text := m.renderSessionsList()
	for _, want := range []string{"fix the login redirect bug", "second session prompt", "glm-5.3", "high", "kimi-k3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sessions list missing %q:\n%s", want, text)
		}
	}
	items := m.sessionItems()
	if len(items) != 2 {
		t.Fatalf("want 2 other sessions, got %d", len(items))
	}
	// Resume the older one: transcript + history rebuilt, mode back to chat.
	var idx int = -1
	for i, it := range items {
		if strings.Contains(it.title, "login redirect") {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("title backfill missing: %+v", items)
	}
	m.cursor = idx
	m.openSelected()
	if m.mode != viewChat {
		t.Fatalf("resume: mode=%v", m.mode)
	}
	if len(m.session.Turns) != 1 || m.session.Turns[0].Prompt != "fix the login redirect bug on retry" {
		t.Fatalf("resume turns: %+v", m.session.Turns)
	}
	if len(m.history) != 3 { // system + user + assistant
		t.Fatalf("resume history: %d msgs", len(m.history))
	}
}

func TestTUIHeaderShowsTitleOnly(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.session.Title = "fix the login redirect bug"
	m.cfg.Model = "deepseek-v4.1-flash"
	header := ansi.Strip(m.headerView())
	if !strings.Contains(header, "fix the login redirect bug") {
		t.Fatalf("header must show the session title, not the ses- id:\n%s", header)
	}
	if strings.Contains(header, "ses-tui") || strings.Contains(header, "tools") {
		t.Fatalf("header must drop the ses- id and tool count:\n%s", header)
	}
	if strings.Contains(header, "alt+m") {
		t.Fatalf("header must not show key hints:\n%s", header)
	}
	if !strings.HasSuffix(strings.TrimRight(header, " "), "deepseek-v4.1-flash") || strings.Contains(header, "go/") {
		t.Fatalf("header must show the bare model name at the right:\n%s", header)
	}
}

// TestTUIHeaderModelFlushRight: the logo and version sit on the left, the bare
// model and thinking level sit at the far right, and a title or mode tag
// lands between them.
func TestTUIHeaderModelFlushRight(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.cfg.Thinking = "max"
	header := ansi.Strip(m.headerView())
	if got := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(header), "kimi-k3 · max")); got != "✦ valen "+version {
		t.Fatalf("untitled header must be the logo and version on the left, got %q:\n%s", got, header)
	}
	if lipgloss.Width(header) != m.columnWidth() {
		t.Fatalf("header width=%d want %d", lipgloss.Width(header), m.columnWidth())
	}
	m.session.Title = "fix the login redirect bug"
	m.cfg.Plan = true
	header = ansi.Strip(m.headerView())
	if !strings.HasPrefix(header, " ✦ valen  ·  fix the login redirect bug  ·  plan") {
		t.Fatalf("title and plan tag must follow the logo:\n%s", header)
	}
	if !strings.HasSuffix(strings.TrimRight(header, " "), "kimi-k3 · max") {
		t.Fatalf("model must stay at the far right:\n%s", header)
	}
}

// TestTUIHeaderRoutingAffinity: an OpenRouter model shows the routing mode
// after the thinking level; Go/Zen and "none" omit it.
func TestTUIHeaderRoutingAffinity(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.cfg.Thinking = "high"
	m.cfg.Routing = "nitro"
	if got := ansi.Strip(m.headerView()); !strings.HasSuffix(strings.TrimRight(got, " "), "kimi-k3 · high · nitro") {
		t.Fatalf("openrouter header must show routing after thinking:\n%s", got)
	}
	m.cfg.Routing = "none"
	if got := ansi.Strip(m.headerView()); !strings.HasSuffix(strings.TrimRight(got, " "), "kimi-k3 · high") || strings.Contains(got, "none") {
		t.Fatalf("none routing must be omitted:\n%s", got)
	}
	m.cfg.Model = "glm-5.3-flash"
	m.cfg.Routing = "nitro"
	if got := ansi.Strip(m.headerView()); !strings.HasSuffix(strings.TrimRight(got, " "), "glm-5.3-flash · high") || strings.Contains(got, "nitro") {
		t.Fatalf("go/zen header must not show routing:\n%s", got)
	}
}

func TestTUIHeaderHidesUnnamedSessionID(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if m.session.Title != "" {
		t.Fatalf("precondition: fresh session should be unnamed, got %q", m.session.Title)
	}
	header := ansi.Strip(m.headerView())
	if strings.Contains(header, "ses-") {
		t.Fatalf("header must hide the ses- id until the session is named:\n%s", header)
	}
	if strings.Contains(header, "  ·  ·") || strings.HasSuffix(header, "·") {
		t.Fatalf("header must not leave a dangling separator:\n%s", header)
	}
}

func TestTUINamesSessionOnSubmit(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if m.submit("  fix the login redirect bug on retry\nsecond line  ") == nil {
		t.Fatal("submit returned nil cmd")
	}
	if want := "fix the login redirect bug on retry"; m.session.Title != want {
		t.Fatalf("session should be named on submit: got %q want %q", m.session.Title, want)
	}
	header := ansi.Strip(m.headerView())
	if !strings.Contains(header, "fix the login redirect bug on retry") {
		t.Fatalf("header must show the title while the first turn runs:\n%s", header)
	}
	if strings.Contains(header, "ses-") {
		t.Fatalf("header must not fall back to the ses- id:\n%s", header)
	}
}

func TestTUIEscInSessionsBacksWithoutArmingCancel(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	cancelled := 0
	m.busy = true
	m.cancel = func() { cancelled++ }
	m.show(viewSessions)

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewChat {
		t.Fatalf("esc in sessions must return to the current session: mode=%v", m.mode)
	}
	if cancelled != 0 || !m.escArmedAt.IsZero() {
		t.Fatalf("esc in sessions must not arm or trigger cancel: cancelled=%d armed=%v", cancelled, !m.escArmedAt.IsZero())
	}
}

func TestTUIAssistantHeaderAlignsWithValen(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "deepseek-v4.1-flash"
	m.session.Turns = []Turn{{At: time.Now(), Prompt: "hi", Reply: "hello"}}
	if out := ansi.Strip(m.renderChat()); !strings.Contains(out, " ✦ deepseek-v4.1-flash") {
		t.Fatalf("assistant header must carry a leading space:\n%s", out)
	}
}

func TestSessionTitleCapturedOnce(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	s, err := newSession(Config{SessionID: "ses-title"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(Turn{Prompt: "first prompt names the session\nsecond line dropped"}); err != nil {
		t.Fatal(err)
	}
	s.ensureMeta("m", "low")
	if s.Title != "first prompt names the session" {
		t.Fatalf("title=%q", s.Title)
	}
	// Second turn must not rename; title stays put.
	if err := s.append(Turn{Prompt: "a different later prompt"}); err != nil {
		t.Fatal(err)
	}
	s.ensureMeta("m", "low")
	if s.Title != "first prompt names the session" {
		t.Fatalf("title changed: %q", s.Title)
	}
	long := strings.Repeat("x", 100)
	if got := sessionTitle(long); len([]rune(got)) != 60 {
		t.Fatalf("title must truncate to 60 runes, got %d", len([]rune(got)))
	}
	meta, err := loadSessionMeta(metaPath(s.Path))
	if err != nil || meta.Title != s.Title || meta.Model != "m" || meta.Effort != "low" {
		t.Fatalf("meta round trip: %+v %v", meta, err)
	}
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
	if gotAuth != "Bearer key" || gotSession != "ses-tui" || gotUA != userAgent {
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

func TestTUINewSessionMintsBlankID(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.System = "sys"
	m.history = append(seedHistory(m.cfg), openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "a"})
	m.session.Turns = append(m.session.Turns, Turn{Prompt: "a", Reply: "b", Usage: Usage{Prompt: 5, Completion: 6, Total: 11}})
	oldID, oldPath := m.session.ID, m.session.Path
	m.pendingText = "streamed scrap"
	m.input.SetValue("typed scrap")

	m.handleCommand("/new")

	if m.session.ID == oldID || m.session.Path == oldPath {
		t.Fatalf("/new must mint a fresh id+file, got %q %q", m.session.ID, m.session.Path)
	}
	if len(m.history) != 1 || len(m.session.Turns) != 0 || m.session.TotalUsage.Total != 0 {
		t.Fatalf("fresh session must reset memory: history=%d turns=%d usage=%+v", len(m.history), len(m.session.Turns), m.session.TotalUsage)
	}
	if m.pendingText != "" || m.input.Value() != "" || m.chatCacheTurns != 0 || len(m.chatCache) != 0 {
		t.Fatalf("pending/live state not released: text=%q input=%q cache=%d turns/%d", m.pendingText, m.input.Value(), len(m.chatCache), m.chatCacheTurns)
	}
	if _, err := os.Stat(oldPath); err != nil {
		// Old file may not exist (nothing committed) — but the pointer must
		// have moved on regardless.
		t.Logf("old path absent (ok): %v", err)
	}
	// Old session with committed turns stays resumable on disk.
	m2 := newTestModel(t, testConfig("https://unused"))
	s, err := newSession(Config{SessionID: "ses-old-resume"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(Turn{At: time.Now(), Prompt: "old work", Reply: "r"}); err != nil {
		t.Fatal(err)
	}
	oldPath2 := s.Path
	m2.handleCommand("/new")
	if items := m2.sessionItems(); len(items) == 0 {
		t.Fatal("old session should remain listed after /new")
	} else {
		found := false
		for _, it := range items {
			if it.path == oldPath2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("old session %q lost from list: %+v", oldPath2, items)
		}
	}
}

func TestTUINewSessionBlockedWhileBusy(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.busy = true
	m.cancel = func() {}
	oldID := m.session.ID
	m.handleCommand("/new")
	if m.session.ID != oldID {
		t.Fatal("/new while busy must not mint a session")
	}
	if !strings.Contains(m.notice, "Working") {
		t.Fatalf("notice=%q", m.notice)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.session.ID != oldID {
		t.Fatal("ctrl+n while busy must not mint a session")
	}
}

func TestTUICtrlNStartsNewSession(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.session.Turns = append(m.session.Turns, Turn{Prompt: "a", Reply: "b"})
	oldID := m.session.ID
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.session.ID == oldID {
		t.Fatal("ctrl+n must mint a fresh session")
	}
	if len(m.session.Turns) != 0 || len(m.history) != 1 {
		t.Fatalf("fresh session must be blank: turns=%d history=%d", len(m.session.Turns), len(m.history))
	}
	if !strings.Contains(m.notice, "New session") {
		t.Fatalf("notice=%q", m.notice)
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

func TestTUICompactCommandSmallHistory(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.history = seedHistory(m.cfg)
	if cmd := m.handleCommand("/compact"); cmd != nil {
		t.Fatal("/compact on a tiny history must not launch network")
	}
	if !strings.Contains(m.notice, "Nothing to compact") {
		t.Fatalf("notice=%q", m.notice)
	}
	m.history = seedHistory(m.cfg)
	if cmd := m.handleCommand("/summarize"); cmd != nil {
		t.Fatal("/summarize is the /compact alias and must behave the same")
	}
	if !strings.Contains(m.renderHelp(), "/compact") {
		t.Fatal("help must list /compact")
	}
}

func TestTUIThinkingCycleKey(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "glm-5.3" // levels low,high,max
	m.cfg.Thinking = "high"

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT}) // high→max
	if m.cfg.Thinking != "max" {
		t.Fatalf("ctrl+t should cycle high→max, got %q", m.cfg.Thinking)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT}) // max→low (wrap)
	if m.cfg.Thinking != "low" {
		t.Fatalf("ctrl+t should wrap max→low, got %q", m.cfg.Thinking)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT}) // low→high
	if m.cfg.Thinking != "high" {
		t.Fatalf("ctrl+t should cycle low→high, got %q", m.cfg.Thinking)
	}
	// Default goes to the lowest.
	m.cfg.Thinking = ""
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.cfg.Thinking != "low" {
		t.Fatalf("ctrl+t from default should go to low, got %q", m.cfg.Thinking)
	}
	// Single-level models stay put with a note.
	m.cfg.Model = "kimi-k3"
	m.cfg.Thinking = "max"
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.cfg.Thinking != "max" || !strings.Contains(m.notice, "only offers") {
		t.Fatalf("single level: thinking=%q notice=%q", m.cfg.Thinking, m.notice)
	}
	// No-control models report it.
	m.cfg.Model = "mimo-v2.5"
	m.cfg.Thinking = ""
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !strings.Contains(m.notice, "No adjustable thinking") {
		t.Fatalf("no-control notice=%q", m.notice)
	}
}

func TestTUIAskAndTranscriptWrap(t *testing.T) {
	strip := func(s string) string {
		re := regexp.MustCompile("\x1b\\[[0-9;]*m")
		return re.ReplaceAllString(s, "")
	}
	maxLine := func(s string) int {
		n := 0
		for _, line := range strings.Split(s, "\n") {
			if w := len([]rune(strings.TrimRight(line, " "))); w > n {
				n = w
			}
		}
		return n
	}
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 50 // contentWidth 46
	cw := m.contentWidth()
	longWord := strings.Repeat("x", 200)
	longSentence := strings.TrimSpace(strings.Repeat("word ", 60))

	// Question overlay with long content.
	picks := []map[int]bool{{}}
	m.ask = &askState{
		qs:      []AskQuestion{{Question: longSentence, Options: []AskOption{{Label: longSentence, Description: longSentence}}}},
		picks:   picks,
		ownText: make([]string, 1),
		phase:   "pick",
	}
	if got := maxLine(strip(m.renderAsk())); got > cw {
		t.Fatalf("ask overflows: max line %d > %d", got, cw)
	}
	m.ask = nil

	// Committed turn with long thinking + unbroken token reply.
	m.session.Turns = []Turn{{At: time.Now(), Prompt: longSentence, Thinking: longWord, Reply: longWord,
		Tools: []ToolRecord{{Name: "shell", Args: longWord, Output: longWord}}, Usage: Usage{Total: 1}}}
	if got := maxLine(strip(m.renderChat())); got > cw {
		t.Fatalf("chat overflows: max line %d > %d", got, cw)
	}

	// Live pending block with long content.
	m.pendingPrompt = longSentence
	m.pendingThinking = longWord
	m.pendingText = longWord
	m.pendingTools = []pendingTool{{name: "shell", args: longWord, output: longWord}}
	if got := maxLine(strip(m.renderChat())); got > cw {
		t.Fatalf("pending overflows: max line %d > %d", got, cw)
	}
	m.pendingPrompt, m.pendingThinking, m.pendingText, m.pendingTools = "", "", "", nil

	// Detail view with long content.
	m.detailTitle = "Tool shell"
	m.detailBody = longSentence + "\n" + longWord
	if got := maxLine(strip(m.renderDetail())); got > cw {
		t.Fatalf("detail overflows: max line %d > %d", got, cw)
	}
}

func TestTUIAltEnterAndCtrlJInsertNewline(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEnter, Alt: true},
		{Type: tea.KeyCtrlJ},
	} {
		m := newTestModel(t, testConfig("https://unused"))
		m.input.SetValue("hello")
		_, _ = m.handleKey(key)
		if m.busy {
			t.Fatalf("%v should not submit", key)
		}
		if m.input.Value() != "hello\n" {
			t.Fatalf("%v should insert newline, got %q", key, m.input.Value())
		}
	}
}

// csiMsg stands in for bubbletea's unexported unknown-CSI message: the
// matcher only relies on the []byte kind plus exact content, which is all
// bubbletea puts on the loop for unrecognized CSI sequences.
type csiMsg []byte

func TestParseEnterCSI(t *testing.T) {
	cases := []struct {
		seq     string
		mod     int
		ok      bool
		newline bool // mod > 1 inserts a newline instead of submitting
	}{
		{"\x1b[13;2u", 2, true, true},  // Kitty shift+enter
		{"\x1b[13;2~", 2, true, true},  // xterm modifyOtherKeys shift+enter
		{"\x1b[13;3u", 3, true, true},  // alt+enter under enhancement
		{"\x1b[13;5u", 5, true, true},  // ctrl+enter under enhancement
		{"\x1b[13u", 1, true, false},   // unmodified enter stays submit
		{"\x1b[13~", 1, true, false},   // unmodified enter stays submit
		{"\x1b[A", 0, false, false},    // unrelated CSI (up)
		{"\x1b[13;2", 0, false, false}, // truncated
		{"\x1b[13;xu", 0, false, false},
		{"hello", 0, false, false},
		{"", 0, false, false},
	}
	for _, c := range cases {
		mod, ok := parseEnterCSI([]byte(c.seq))
		if ok != c.ok || mod != c.mod {
			t.Fatalf("parseEnterCSI(%q) = (%d,%v), want (%d,%v)", c.seq, mod, ok, c.mod, c.ok)
		}
	}
}

func TestTUIShiftEnterCSIInsertsNewline(t *testing.T) {
	for _, seq := range []string{"\x1b[13;2u", "\x1b[13;2~", "\x1b[13;3u", "\x1b[13;5u"} {
		m := newTestModel(t, testConfig("https://unused"))
		m.input.SetValue("hello")
		updated, _ := m.Update(csiMsg(seq))
		if updated.(*tuiModel).busy {
			t.Fatalf("%q should not submit", seq)
		}
		if got := m.input.Value(); got != "hello\n" {
			t.Fatalf("%q should insert newline, got %q", seq, got)
		}
	}
}

func TestTUIEnterCSINoModifierSubmitsLikeEnter(t *testing.T) {
	// Bare CSI 13u is unmodified enter: empty input is a no-op (same as the
	// enter key), and it must never insert a newline.
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(csiMsg("\x1b[13u"))
	if m.busy {
		t.Fatal("bare CSI enter with empty input should not submit")
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("bare CSI enter should not insert newline, got %q", got)
	}
}

func TestTUIDefaultSystemHintsAgents(t *testing.T) {
	h := seedHistory(Config{})
	if len(h) != 1 || h[0].Role != openai.ChatMessageRoleSystem {
		t.Fatalf("blank config should seed one system message, got %+v", h)
	}
	if !strings.Contains(h[0].Content, "AGENTS.md") {
		t.Fatalf("default system should hint AGENTS.md, got %q", h[0].Content)
	}
	m := newTestModel(t, testConfig("https://unused"))
	got := m.renderChat()
	if strings.Contains(got, "AGENTS.md") || strings.Contains(got, "Ask anything") {
		t.Fatalf("empty chat should be a simple label, got:\n%s", got)
	}
	if header := ansi.Strip(m.headerView()); !strings.Contains(header, "valen "+version) {
		t.Fatalf("blank session header should show the version beside the logo, got:\n%s", header)
	}
}

// TestTUIHeaderVersionUntilTitled: the version sits beside the logo while the
// session is unnamed and drops out once a title replaces it.
func TestTUIHeaderVersionUntilTitled(t *testing.T) {
	old := version
	version = "1.2.3"
	defer func() { version = old }()
	m := newTestModel(t, testConfig("https://unused"))
	if header := ansi.Strip(m.headerView()); !strings.HasPrefix(header, " ✦ valen 1.2.3") {
		t.Fatalf("untitled header must show the version beside the logo:\n%s", header)
	}
	m.session.Title = "fix the login redirect bug"
	if header := ansi.Strip(m.headerView()); strings.Contains(header, "1.2.3") {
		t.Fatalf("titled header must drop the version:\n%s", header)
	}
}

// TestChatCacheIncrementalMatchesFull verifies the committed-turn cache:
// appending turns extends the cached prefix (no full re-render) and key
// changes (thinking toggle, width) rebuild identical output.
func TestChatCacheIncrementalMatchesFull(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	mkTurn := func(prompt string) Turn {
		return Turn{At: time.Now(), Prompt: prompt, Thinking: "plan", Reply: "reply " + prompt, Usage: Usage{Total: 1}}
	}
	m.session.Turns = []Turn{mkTurn("one"), mkTurn("two")}
	_ = m.renderChat()
	if m.chatCacheTurns != 2 {
		t.Fatalf("cache should hold 2 turns, holds %d", m.chatCacheTurns)
	}
	m.session.Turns = append(m.session.Turns, mkTurn("three"))
	extended := m.renderChat()
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(extended, want) {
			t.Fatalf("extended chat missing %q", want)
		}
	}
	// Toggling thinking rebuilds (cache key change) and round-trips exactly.
	m.showThink = false
	hidden := m.renderChat()
	if strings.Contains(hidden, "plan") {
		t.Fatalf("hidden thinking should collapse:\n%s", hidden)
	}
	m.showThink = true
	reshown := m.renderChat()
	if reshown != extended {
		t.Fatalf("toggle round-trip should rebuild identical output")
	}
	m.viewport.Width = 100
	wide := m.renderChat()
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide chat missing %q", want)
		}
	}
}

// TestTUICommittedTrimCollapsesReasoningKeepsPromptReplyAndTools pins the
// scrollback budget contract: once a turn alone exceeds max_lines, its
// reasoning collapses while the user prompt, tool rows and the full reply
// survive. Replies are never cut, even past the budget.
func TestTUICommittedTrimCollapsesReasoningKeepsPromptReplyAndTools(t *testing.T) {
	limit := 30
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.TUI.MaxLines = &limit
	m.viewport.Width = 80
	var thinking strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&thinking, "reasoning line %d\n", i)
	}
	var reply strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&reply, "reply line %d\n", i)
	}
	m.session.Turns = []Turn{{
		At:       time.Now(),
		Prompt:   "the pristine user request",
		Thinking: thinking.String(),
		Reply:    reply.String(),
		Tools:    []ToolRecord{{Name: "read", Args: `{"path":"foo.go"}`, Output: "package main"}},
	}}

	out := m.renderChat()
	if strings.Contains(out, "reasoning line 199") {
		t.Fatalf("reasoning body must be trimmed past the budget:\n%s", out)
	}
	// Glamour wraps and styles the reply, so compare on whitespace-collapsed plain text.
	plain := strings.Join(strings.Fields(ansi.Strip(out)), " ")
	for _, want := range []string{"pristine user request", "read", "foo.go", "trimmed", "reply line 0 ", "reply line 39"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("priority content %q missing from compact transcript:\n%s", want, out)
		}
	}
	if strings.Contains(plain, "more lines") {
		t.Fatalf("reply must never be cut in the scrollback:\n%s", out)
	}
}

// TestTUICommittedTrimBudgetAcrossTurns verifies the budget drops the oldest
// turns from scrollback with a notice while keeping the newest prompt, and
// that toggling the cap back to unlimited restores every turn (disk history
// stays lossless; the view is what shrinks).
func TestTUICommittedTrimBudgetAcrossTurns(t *testing.T) {
	limit := 20
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.TUI.MaxLines = &limit
	m.viewport.Width = 80
	for i := 0; i < 6; i++ {
		m.session.Turns = append(m.session.Turns, Turn{
			At: time.Now(), Prompt: fmt.Sprintf("prompt-%d", i), Reply: "ok",
		})
	}
	out := m.renderChat()
	if !strings.Contains(out, "earlier turn(s) trimmed") {
		t.Fatalf("dropped turns need a notice:\n%s", out)
	}
	if !strings.Contains(out, "prompt-5") {
		t.Fatalf("newest turn must survive the budget:\n%s", out)
	}
	if strings.Contains(out, "prompt-0") {
		t.Fatalf("oldest turn should be trimmed at budget %d:\n%s", limit, out)
	}
	unlimited := 0
	m.cfg.TUI.MaxLines = &unlimited
	full := m.renderChat()
	if !strings.Contains(full, "prompt-0") || strings.Contains(full, "trimmed") {
		t.Fatalf("unlimited budget must restore the full transcript:\n%s", full)
	}
}

func TestTUICommittedTrimDefaultsTo1000(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if got := tuiMaxLines(m.cfg); got != 1000 {
		t.Fatalf("default max_lines=%d, want 1000", got)
	}
	zero := 0
	m.cfg.TUI.MaxLines = &zero
	if got := tuiMaxLines(m.cfg); got != 0 {
		t.Fatalf("explicit 0 should disable trimming, got %d", got)
	}
}

func TestTUIPendingRunningToolsShowNowMarker(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "do things"

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_search", args: `{"query":"go"}`})
	if m.toolStatus != "web_search…" {
		t.Fatalf("single running status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_fetch", args: `{"url":"https://x"}`})
	if m.toolStatus != "web_fetch… · 2 running" {
		t.Fatalf("parallel running status=%q", m.toolStatus)
	}

	// The scrollback no longer redraws on spinner ticks, so live rows read
	// "now: <action>" instead of a frozen spinner frame.
	out := m.renderPending(80)
	if strings.Count(out, "now:") != 2 {
		t.Fatalf("both open threads should carry the now marker:\n%s", out)
	}
	if strings.Contains(out, "· running") {
		t.Fatalf("open threads must not use the old running label:\n%s", out)
	}

	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_search", output: "1. release notes"})
	if m.toolStatus != "✓ web_search · 1 still running" {
		t.Fatalf("partial completion status=%q", m.toolStatus)
	}
	out3 := m.renderPending(80)
	if strings.Count(out3, "now:") != 1 {
		t.Fatalf("finished thread must lose its now marker:\n%s", out3)
	}
	if !strings.Contains(out3, "1. release notes") {
		t.Fatalf("finished thread keeps its preview:\n%s", out3)
	}

	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_fetch", output: "ok"})
	if m.toolStatus != "✓ web_fetch" {
		t.Fatalf("all-done status=%q", m.toolStatus)
	}
}

// TestTUIStatusShowsPhase: model-side deltas flip the footer from tool state
// to thinking/writing, so a long reasoning round never looks like stuck
// local work.
func TestTUIStatusShowsPhase(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.busy = true

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "shell", args: `{"command":"ls"}`})
	if m.toolStatus != "shell…" {
		t.Fatalf("tool status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "shell", output: "x"})
	if m.toolStatus != "✓ shell" {
		t.Fatalf("done status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "hmm"})
	if m.toolStatus != "Reasoning…" {
		t.Fatalf("thinking status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "text_delta", text: "answer"})
	if m.toolStatus != "writing…" {
		t.Fatalf("writing status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "read", args: `{"path":"a"}`})
	if m.toolStatus != "read…" {
		t.Fatalf("back-to-tool status=%q", m.toolStatus)
	}
}

func TestAppendThinkingDeltaBoundaries(t *testing.T) {
	// Fragments within one part concatenate exactly (no shredding).
	s, cur := appendThinkingDelta("", "Hel", 0, 0)
	s, cur = appendThinkingDelta(s, "lo", 0, cur)
	if s != "Hello" || cur != 0 {
		t.Fatalf("fragments glued wrong: %q/%d", s, cur)
	}
	// A new part starts on its own line.
	s, cur = appendThinkingDelta(s, "Reading config", 1, cur)
	if s != "Hello\nReading config" || cur != 1 {
		t.Fatalf("part boundary missing newline: %q/%d", s, cur)
	}
	// Same part continues on the same line without another break.
	s, cur = appendThinkingDelta(s, " more", 1, cur)
	if s != "Hello\nReading config more" {
		t.Fatalf("continuation broke: %q", s)
	}
	// No doubled newlines when either side already breaks.
	s, cur = appendThinkingDelta("a\n", "b", 1, 0)
	if s != "a\nb" {
		t.Fatalf("doubled head newline: %q", s)
	}
	s, _ = appendThinkingDelta("a", "\nb", 1, 0)
	if s != "a\nb" {
		t.Fatalf("doubled tail newline: %q", s)
	}
	// Empty deltas are no-ops and don't advance the part.
	s, cur = appendThinkingDelta("a", "", 1, 0)
	if s != "a" || cur != 0 {
		t.Fatalf("empty delta changed state: %q/%d", s, cur)
	}
}

// TestTUIThinkingPartsSeparateLiveLines: the first update of a new reasoning
// part lands on its own line in the live view, matching the committed join of
// final summary parts; fragments that share a part concatenate exactly.
func TestTUIThinkingPartsSeparateLiveLines(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "Checking the repo layout", idx: 0})
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "Reading the config file", idx: 1})
	if m.pendingThinking != "Checking the repo layout\nReading the config file" {
		t.Fatalf("live thinking glued across parts: %q", m.pendingThinking)
	}
	// Same-part fragments concatenate exactly; only a part boundary breaks.
	m2 := newTestModel(t, testConfig("https://unused"))
	_, _ = m2.Update(toolEventMsg{kind: "thinking_delta", text: "Checking the repo layout", idx: 0})
	_, _ = m2.Update(toolEventMsg{kind: "thinking_delta", text: " and more", idx: 0})
	if m2.pendingThinking != "Checking the repo layout and more" {
		t.Fatalf("same-part fragments shredded: %q", m2.pendingThinking)
	}
}

// TestThinkingFragmentsJoinIntoProse: reasoning arrives as short multi-word
// fragments (DeepSeek-style). They must concatenate into one flowing thought,
// not one line per delta.
func TestThinkingFragmentsJoinIntoProse(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	frags := []string{
		"passing", " network=true triggers", " the ask", ". Avoiding",
		" silly", " prompts means", ": don't invoke", " network", "=true",
	}
	for _, frag := range frags {
		_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: frag, idx: 0})
	}
	want := "passing network=true triggers the ask. Avoiding silly prompts means: don't invoke network=true"
	if m.pendingThinking != want {
		t.Fatalf("reasoning shredded:\n got %q\nwant %q", m.pendingThinking, want)
	}
}

// TestTUICommittedThinkingKeepsStreamedText: the success-path commit must
// keep the streamed thinking buffer for display; replay history keeps the
// exactly-concatenated authoritative reasoning.
func TestTUICommittedThinkingKeepsStreamedText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Checking the repo layout\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" and reading the config\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	if len(m.session.Turns) != 1 {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	turn := m.session.Turns[0]
	if turn.Thinking != "Checking the repo layout and reading the config" {
		t.Fatalf("committed thinking altered: %q", turn.Thinking)
	}
	if turn.Reply != "done" {
		t.Fatalf("reply: %q", turn.Reply)
	}
	// History keeps the exact concatenation for provider replay.
	last := m.history[len(m.history)-1]
	if last.ReasoningContent != "Checking the repo layout and reading the config" {
		t.Fatalf("replay reasoning altered: %q", last.ReasoningContent)
	}
	if !strings.Contains(m.renderChat(), "Checking the repo layout and reading the config") {
		t.Fatalf("scrollback missing thinking:\n%s", m.renderChat())
	}
}

// TestThinkingFragmentsJoinWithinRound: fragments in one round join into a
// single flowing thought instead of one line per delta.
func TestThinkingFragmentsJoinWithinRound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"first round\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" second part\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" third part\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	if len(m.session.Turns) != 1 {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	if thinking := m.session.Turns[0].Thinking; thinking != "first round second part third part" {
		t.Fatalf("thinking fragments not joined: %q", thinking)
	}
}

func TestThinkDurShowsSubSecond(t *testing.T) {
	for durMs, want := range map[int64]string{
		0:     "",
		1:     " (1ms)",
		342:   " (342ms)",
		1500:  " (1.5s)",
		12000: " (12s)",
		63000: " (1m3s)",
	} {
		if got := thinkDurSuffix(durMs); got != want {
			t.Fatalf("thinkDurSuffix(%d) = %q, want %q", durMs, got, want)
		}
		if durMs > 0 {
			if got := toolDurSuffix(durMs); got != want {
				t.Fatalf("toolDurSuffix(%d) = %q, want %q", durMs, got, want)
			}
		}
	}
}

// TestPendingThinkingHeaderBeforeText: the live thinking header (with
// elapsed) renders from turn start, before any thinking delta arrives, so a
// long tool-only stretch never hides the elapsed. The scrollback carries no
// duplicate status line — live state lives once in the spinner row above the
// chatbox, with the cancel hint pinned to its right edge.
func TestPendingThinkingHeaderBeforeText(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.width = 200
	m.busy = true
	m.pendingPrompt = "do the thing"
	m.pendingSince = time.Now().Add(-63 * time.Second)
	out := m.renderPending(80)
	if !strings.Contains(out, "✦ thinking (1m3s)") {
		t.Fatalf("live header must show elapsed before thinking text:\n%s", out)
	}
	if strings.Contains(out, "Working…") || strings.Contains(out, "esc esc") {
		t.Fatalf("scrollback must not repeat the live status or cancel hint:\n%s", out)
	}
	status := m.statusView()
	if !strings.Contains(status, "Working…") {
		t.Fatalf("spinner row must carry the live status:\n%s", status)
	}
	if !strings.Contains(status, "esc to stop") {
		t.Fatalf("cancel hint must sit in the spinner row:\n%s", status)
	}
	// Line return after the thinking state: header must not share a visual
	// line with body text.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "✦ thinking") && len(strings.TrimSpace(line)) > len("✦ thinking (1m3s)")+4 && strings.Contains(line, "do the thing") {
			t.Fatalf("thinking header glued to body text:\n%s", out)
		}
	}
}

// TestPendingThinkingHeaderHiddenStaysHidden: /think off keeps the live
// thinking state (and its elapsed) out of the pending block.
func TestPendingThinkingHeaderHiddenStaysHidden(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.showThink = false
	m.pendingPrompt = "do the thing"
	m.pendingSince = time.Now().Add(-63 * time.Second)
	m.pendingThinking = "secret plan"
	if out := m.renderPending(80); strings.Contains(out, "✦ thinking") {
		t.Fatalf("hidden thinking must stay out of live view:\n%s", out)
	}
}

// TestCommittedToolOnlyShowsDuration: a turn with tools but no thinking text
// still persists the wall time on a bare thinking header.
func TestCommittedToolOnlyShowsDuration(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.session.Turns = []Turn{{
		At: time.Now(), Prompt: "list it", DurMs: 63000,
		Tools: []ToolRecord{{Name: "shell", Args: "ls", Output: "x"}},
	}}
	out := m.renderChat()
	if !strings.Contains(out, "✦ thinking (1m3s)") {
		t.Fatalf("tool-only turn must persist elapsed:\n%s", out)
	}
}

// TestCommittedPlainTextNoBareHeader: a plain Q&A turn (no thinking, no
// tools) must not gain a bare thinking header just for the timestamp.
func TestCommittedPlainTextNoBareHeader(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.session.Turns = []Turn{{At: time.Now(), Prompt: "hi", Reply: "hello", DurMs: 2500}}
	if out := m.renderChat(); strings.Contains(out, "✦ thinking") {
		t.Fatalf("plain turn must not grow a bare thinking header:\n%s", out)
	}
}

// TestTurnDoneFloorsDuration: even an instant turn commits DurMs >= 1 so the
// persisted thinking header never drops its elapsed suffix.
func TestTurnDoneFloorsDuration(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.pendingSince = time.Now()
	m.session.Turns = nil
	_, _ = m.Update(turnDoneMsg{prompt: "hi", thinking: "plan", answer: "ok"})
	if len(m.session.Turns) != 1 {
		t.Fatalf("turn not committed: %+v", m.session.Turns)
	}
	if m.session.Turns[0].DurMs < 1 {
		t.Fatalf("DurMs must floor at 1, got %d", m.session.Turns[0].DurMs)
	}
	if got := thinkDurSuffix(m.session.Turns[0].DurMs); got == "" {
		t.Fatal("floored duration must render a suffix")
	}
}

// TestPendingToolMatchesByID: two parallel same-name calls keep their own
// outputs — the first-started finishing first must not credit the later row.
func TestPendingToolMatchesByID(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "read two files"

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "read", args: `{"path":"a"}`, id: "call-a"})
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "read", args: `{"path":"b"}`, id: "call-b"})
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "read", output: "A-content", id: "call-a"})
	if !m.pendingTools[0].done || m.pendingTools[0].output != "A-content" {
		t.Fatalf("call-a row must close with its own output: %+v", m.pendingTools)
	}
	if m.pendingTools[1].done {
		t.Fatalf("call-b must still be open: %+v", m.pendingTools)
	}
}

// TestTUITodoChecklistInScrollback: todowrite rows render the plan as a
// glyph checklist instead of raw JSON — live while running (from args) and
// committed (from the stored result).
func TestTUITodoChecklistInScrollback(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "slice it"

	args := `{"todos":[{"content":"Slice it","status":"completed","priority":"high"},{"content":"Wire UI","status":"in_progress","priority":"medium"}]}`
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "todowrite", args: args})
	live := m.renderPending(80)
	for _, want := range []string{"TODO", "[x] Slice it", "[.] Wire UI"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live todowrite must checklist %q:\n%s", want, live)
		}
	}
	if strings.Contains(live, `"todos"`) {
		t.Fatalf("live todowrite must not dump raw JSON:\n%s", live)
	}

	out := `[{"content":"Slice it","status":"completed","priority":"high"},{"content":"Wire UI","status":"in_progress","priority":"medium"}]`
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "todowrite", output: out})
	done := m.renderPending(80)
	for _, want := range []string{"[x] Slice it", "[.] Wire UI"} {
		if !strings.Contains(done, want) {
			t.Fatalf("finished todowrite must checklist %q:\n%s", want, done)
		}
	}

	row := m.renderToolLine(ToolRecord{Name: "todowrite", Args: args, Output: out})
	for _, want := range []string{"TODO", "[x] Slice it", "[.] Wire UI"} {
		if !strings.Contains(row, want) {
			t.Fatalf("committed todowrite must checklist %q:\n%s", want, row)
		}
	}
	if strings.Contains(row, `"todos"`) {
		t.Fatalf("committed todowrite must not dump raw JSON:\n%s", row)
	}

	// Non-todo tools keep the default row.
	plain := m.renderToolLine(ToolRecord{Name: "shell", Args: `{"command":"ls"}`, Output: "main.go"})
	if !strings.Contains(plain, "main.go") {
		t.Fatalf("other tools must keep default preview:\n%s", plain)
	}
}

// TestTUIToolSummariesInScrollback: file tools render plain-English headers
// (no raw JSON); edits render the old/new pair as a -/+ diff.
func TestTUIToolSummariesInScrollback(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80

	editArgs := `{"path":"foo.go","oldString":"a := 1\nb := 2","newString":"a := 1\nb := 3"}`
	// Strip styling: chroma/lipgloss split diff text with ANSI runs when
	// the terminal reports color support, so assert on visible text.
	row := ansi.Strip(m.renderToolLine(ToolRecord{Name: "edit", Args: editArgs, Output: "edited foo.go"}))
	for _, want := range []string{"edit", "foo.go", "- b := 2", "+ b := 3"} {
		if !strings.Contains(row, want) {
			t.Fatalf("edit row must show diff %q:\n%s", want, row)
		}
	}
	if strings.Contains(row, "oldString") {
		t.Fatalf("edit row must not dump raw JSON:\n%s", row)
	}

	readRow := ansi.Strip(m.renderToolLine(ToolRecord{Name: "read", Args: `{"path":"foo.go","offset":10,"limit":5}`, Output: "File foo.go (20 lines, showing 10-14):\n10: x"}))
	if !strings.Contains(readRow, "foo.go") || strings.Contains(readRow, `"path"`) {
		t.Fatalf("read row must summarize path, not JSON:\n%s", readRow)
	}

	grepRow := ansi.Strip(m.renderToolLine(ToolRecord{Name: "grep", Args: `{"pattern":"foo.*","path":"."}`, Output: "foo.go:1: foo bar"}))
	if !strings.Contains(grepRow, "foo.*") || strings.Contains(grepRow, `"pattern"`) {
		t.Fatalf("grep row must show the pattern, not JSON:\n%s", grepRow)
	}

	// Live running edit shows the same diff under the spinner header.
	m.busy = true
	m.pendingPrompt = "fix it"
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "edit", args: editArgs})
	live := ansi.Strip(m.renderPending(80))
	for _, want := range []string{"edit", "now:", "- b := 2", "+ b := 3"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live edit must diff %q:\n%s", want, live)
		}
	}
	if strings.Contains(live, "oldString") {
		t.Fatalf("live edit must not dump raw JSON:\n%s", live)
	}
}

// TestToolSummaryPrefersLocalPaths: an absolute path inside the workdir
// renders workdir-relative in tool headers; outside paths keep the full path.
func TestToolSummaryPrefersLocalPaths(t *testing.T) {
	root := useTempRoot(t)
	inside := filepath.Join(root, "sub", "foo.go")
	for _, name := range []string{"read", "write", "edit"} {
		args := fmt.Sprintf(`{"path":%q}`, inside)
		if got := toolSummary(name, args, 300); got != "sub/foo.go" {
			t.Fatalf("%s inside root = %q, want sub/foo.go", name, got)
		}
	}
	if got := toolSummary("read", `{"path":"foo.go"}`, 300); got != "foo.go" {
		t.Fatalf("relative path = %q, want foo.go", got)
	}
	outside := filepath.Join(filepath.Dir(root), "elsewhere.go")
	if got := toolSummary("read", fmt.Sprintf(`{"path":%q}`, outside), 300); got != outside {
		t.Fatalf("outside root = %q, want %q", got, outside)
	}
	grepArgs := fmt.Sprintf(`{"pattern":"foo","path":%q}`, inside)
	if got := toolSummary("grep", grepArgs, 300); !strings.Contains(got, "in sub/foo.go") {
		t.Fatalf("grep must localize path, got %q", got)
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
	m.statePath = filepath.Join(dir, "state.json")
	return m, m.statePath
}

func TestTUIModelDirectSwitchPersists(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "test-key")
	m, path := newTestModelWithConfig(t, "model = \"glm-5.3-flash\"\nthinking = \"max\"\nbase_url = \"https://unused\"\n")
	m.handleCommand("/model kimi-k2.6")
	if m.cfg.Model != "kimi-k2.6" {
		t.Fatalf("model=%q notice=%q", m.cfg.Model, m.notice)
	}
	if m.cfg.Thinking != "" {
		t.Fatalf("max is invalid on kimi toggle and must reset, got %q", m.cfg.Thinking)
	}
	st := loadUserState(path)
	if st.Model != "kimi-k2.6" || st.Thinking != "" {
		t.Fatalf("state not persisted: %+v", st)
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

func TestTUIModelPickerFavouritesFloatAndToggle(t *testing.T) {
	m, statePath := newTestModelWithConfig(t, "model = \"m\"\nbase_url = \"https://unused\"\n")
	favPath := filepath.Join(filepath.Dir(statePath), userFavouritesName)
	if err := os.WriteFile(favPath, []byte("- gpt-5.6-luna\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"kimi-k3", "glm-5.3", "gpt-5.6-luna"}})
	if m.mode != viewModels || len(m.modelChoices) != 3 {
		t.Fatalf("mode=%v choices=%v", m.mode, m.modelChoices)
	}
	if m.modelChoices[0].id != "gpt-5.6-luna" {
		t.Fatalf("favourite must sort to the top: %v", m.modelChoices)
	}
	if text := ansi.Strip(m.renderModelsList()); !strings.Contains(text, "★") {
		t.Fatalf("favourite must render a star:\n%s", text)
	}

	// Space on the highlighted favourite unstars it and persists.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if m.isFavourite("gpt-5.6-luna") {
		t.Fatal("space must unstar the highlighted model")
	}
	if m.modelChoices[m.cursor].id != "gpt-5.6-luna" {
		t.Fatalf("cursor should stay on the toggled model: cursor=%d choices=%v", m.cursor, m.modelChoices)
	}
	if raw, _ := os.ReadFile(favPath); strings.Contains(string(raw), "gpt-5.6-luna") {
		t.Fatalf("unstar must persist: %q", raw)
	}

	// Space on a plain row stars it, floats it to the top and writes the file.
	m.cursor = 0
	for i, c := range m.modelChoices {
		if c.id == "glm-5.3" {
			m.cursor = i
		}
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if !m.isFavourite("glm-5.3") {
		t.Fatal("space must star glm-5.3")
	}
	if m.modelChoices[0].id != "glm-5.3" {
		t.Fatalf("new favourite must float to top: %v", m.modelChoices)
	}
	if raw, _ := os.ReadFile(favPath); !strings.Contains(string(raw), "- glm-5.3") {
		t.Fatalf("star must persist: %q", raw)
	}

	// Enter still selects the highlighted model.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.cfg.Model != "glm-5.3" || m.mode != viewChat {
		t.Fatalf("enter must select: model=%q mode=%v", m.cfg.Model, m.mode)
	}
}

func TestTUIModelPickerSearchBoxKeepsFullWidth(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.width = 200
	m.modelChoices = []modelChoice{{id: "alpha"}, {id: "beta"}}
	boxWidth := func() int {
		out := m.renderModelsList()
		line, _, _ := strings.Cut(out, "\n")
		return lipgloss.Width(line)
	}
	m.modelSearch.SetValue("")
	empty := boxWidth()
	m.modelSearch.SetValue("anthropic/claude")
	typed := boxWidth()
	if empty != typed {
		t.Fatalf("search box resized while typing: empty=%d typed=%d", empty, typed)
	}
	// Pinned to the content width (plus the two border cells), like the
	// chatbox, rather than shrinking to the query.
	if want := m.contentWidth(); empty != want && empty != want+2 {
		t.Fatalf("search box width=%d, want content width %d (or +2 border)", empty, want)
	}
}

func TestTUIModelPickerSearchAndEscape(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"alpha", "j-model", "k-model"}})
	if m.mode != viewModels {
		t.Fatalf("mode=%v", m.mode)
	}
	if text := ansi.Strip(m.renderViewportContent()); !strings.Contains(text, "ctrl+f to filter models") {
		t.Fatalf("picker should render its search box:\n%s", text)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.cursor != 1 || m.modelSearch.Value() != "" {
		t.Fatalf("unfocused j should move down: cursor=%d query=%q", m.cursor, m.modelSearch.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.cursor != 0 {
		t.Fatalf("unfocused k should move up: cursor=%d", m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlF})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.modelSearch.Value() != "j" || len(m.modelChoices) != 1 || m.modelChoices[0].id != "j-model" {
		t.Fatalf("j should filter instead of moving the cursor: query=%q choices=%v cursor=%d", m.modelSearch.Value(), m.modelChoices, m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 0 || m.modelSearch.Value() != "j" {
		t.Fatalf("arrow navigation changed the query or moved past the only result: cursor=%d query=%q", m.cursor, m.modelSearch.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.modelSearch.Value() != "k" || len(m.modelChoices) != 1 || m.modelChoices[0].id != "k-model" {
		t.Fatalf("k should filter instead of moving the cursor: query=%q choices=%v cursor=%d", m.modelSearch.Value(), m.modelChoices, m.cursor)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewModels || m.modelSearch.Focused() || m.modelSearch.Value() != "k" {
		t.Fatalf("first esc should only unfocus the field: mode=%v query=%q", m.mode, m.modelSearch.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.modelSearch.Value() != "k" || m.cursor != 0 {
		t.Fatalf("unfocused j must navigate, not type: query=%q cursor=%d", m.modelSearch.Value(), m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewChat {
		t.Fatalf("esc without field focus should close the picker: mode=%v", m.mode)
	}
}

// TestTUICtrlMCSIParses: the disambiguated ctrl+m letter sequences open the
// model picker; the bare mod-less form must keep typing (it is a plain "m").
func TestTUICtrlMCSIParses(t *testing.T) {
	if !ctrlMCSI([]byte("\x1b[109;5u")) || !ctrlMCSI([]byte("\x1b[109;5~")) {
		t.Fatal("ctrl+m letter sequences must parse")
	}
	for _, seq := range [][]byte{
		[]byte("\x1b[109u"),   // no modifier: plain "m"
		[]byte("\x1b[109;2u"), // shift+m, not ours
		[]byte("\x1b[13;5u"),  // ctrl+enter, handled by the enter path
		[]byte("m"),
	} {
		if ctrlMCSI(seq) {
			t.Fatalf("must not parse as ctrl+m: %q", seq)
		}
	}
}

func TestTUICtrlMOpensModelPicker(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	mm, cmd := m.Update([]byte("\x1b[109;5u"))
	m = mm.(*tuiModel)
	if cmd == nil {
		t.Fatal("ctrl+m must start the model fetch")
	}
	if !strings.Contains(m.notice, "Loading models") {
		t.Fatalf("notice=%q", m.notice)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("ctrl+m leaked into input: %q", got)
	}
	// While a question overlay waits, ctrl+m stays swallowed like enter.
	m2 := newTestModel(t, testConfig("https://unused"))
	m2.ask = &askState{phase: "pick"}
	mm, cmd = m2.Update([]byte("\x1b[109;5u"))
	m2 = mm.(*tuiModel)
	if cmd != nil || strings.Contains(m2.notice, "Loading models") {
		t.Fatalf("ctrl+m must not hijack the ask overlay: cmd=%v notice=%q", cmd, m2.notice)
	}
}

// TestTUIOverlayCursorScrollsViewport: cursoring past the visible page must
// scroll the list with it, both directions, instead of losing the cursor.
func TestTUIOverlayCursorScrollsViewport(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	var ids []string
	for i := 0; i < 30; i++ {
		ids = append(ids, fmt.Sprintf("model-%02d", i))
	}
	_, _ = m.Update(modelsLoadedMsg{ids: ids, filter: ""})
	if m.mode != viewModels {
		t.Fatalf("mode=%v", m.mode)
	}
	m.viewport.Height = 8
	m.pinBottom() // cursor 0 is at top; re-pin to top for the downward run
	m.viewport.GotoTop()
	down := tea.KeyMsg{Type: tea.KeyDown}
	for i := 0; i < 20; i++ {
		mm, _ := m.Update(down)
		m = mm.(*tuiModel)
	}
	visible := func() bool {
		line := m.cursor + 4
		return line >= m.viewport.YOffset && line < m.viewport.YOffset+m.viewport.Height
	}
	if m.cursor != 20 || !visible() {
		t.Fatalf("cursor lost after paging down: cursor=%d yoff=%d h=%d", m.cursor, m.viewport.YOffset, m.viewport.Height)
	}
	up := tea.KeyMsg{Type: tea.KeyUp}
	for i := 0; i < 20; i++ {
		mm, _ := m.Update(up)
		m = mm.(*tuiModel)
	}
	if m.cursor != 0 || !visible() {
		t.Fatalf("cursor lost paging back up: cursor=%d yoff=%d", m.cursor, m.viewport.YOffset)
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

// TestTUIModelPickerMergesProviders: with the OpenRouter key configured the
// picker lists both catalogs, every row prefixed with its provider, while
// stored IDs stay bare (Go) or openrouter/-prefixed.
func TestTUIModelPickerMergesProviders(t *testing.T) {
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"glm-5.3-flash","object":"model"}]}`)
	}))
	defer goSrv.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"moonshotai/kimi-k3"}]}`)
	}))
	defer orSrv.Close()
	oldBase := openRouterBaseURL
	openRouterBaseURL = orSrv.URL
	defer func() { openRouterBaseURL = oldBase }()

	m := newTestModel(t, testConfig(goSrv.URL))
	m.cfg.OpenRouterKey = "or-key"
	msg := m.openModelPicker("")()
	loaded, ok := msg.(modelsLoadedMsg)
	if !ok {
		t.Fatalf("msg=%T", msg)
	}
	if len(loaded.ids) != 1+len(fallbackZenModels)+1 || loaded.ids[0] != "glm-5.3-flash" || loaded.ids[len(loaded.ids)-1] != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("merged ids=%v", loaded.ids)
	}
	if modelProvider(loaded.ids[1]) != ProviderZen {
		t.Fatalf("zen rows must sit between go and openrouter: %v", loaded.ids)
	}
	_, _ = m.Update(loaded)
	text := m.renderViewportContent()
	if !strings.Contains(text, "go/glm-5.3-flash") || !strings.Contains(text, "openrouter/moonshotai/kimi-k3") {
		t.Fatalf("rows must carry provider prefixes:\n%s", text)
	}
}

// TestTUIModelPickerOpenRouterFallback: an unreachable OpenRouter catalog
// falls back to the curated list instead of dropping the provider.
func TestTUIModelPickerOpenRouterFallback(t *testing.T) {
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"glm-5.3-flash","object":"model"}]}`)
	}))
	defer goSrv.Close()
	oldBase := openRouterBaseURL
	openRouterBaseURL = "http://127.0.0.1:1"
	defer func() { openRouterBaseURL = oldBase }()

	m := newTestModel(t, testConfig(goSrv.URL))
	m.cfg.OpenRouterKey = "or-key"
	msg := m.openModelPicker("")()
	loaded := msg.(modelsLoadedMsg)
	found := false
	for _, id := range loaded.ids {
		if id == "glm-5.3-flash" || modelProvider(id) == ProviderZen {
			continue
		}
		if modelProvider(id) != ProviderOpenRouter {
			t.Fatalf("non-go row %q must be openrouter-prefixed", id)
		}
		found = true
	}
	if !found {
		t.Fatalf("fallback must keep openrouter rows: %v", loaded.ids)
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
	if m.mode != viewEffort || len(m.effortLevels) != 4 {
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
	if st := loadUserState(path); st.Thinking != "max" || st.Model != "glm-5.3" {
		t.Fatalf("thinking not persisted: %+v", st)
	}
	if !strings.Contains(m.headerView(), "max") {
		t.Fatal("header should show the thinking level")
	}
}

func TestTUIEffortAliasAndClear(t *testing.T) {
	m, _ := newTestModelWithConfig(t, "model = \"kimi-k2.6\"\nthinking = \"high\"\nbase_url = \"https://unused\"\n")
	m.handleCommand("/thinking default")
	if m.cfg.Thinking != "" {
		t.Fatalf("thinking=%q", m.cfg.Thinking)
	}
	m.handleCommand("/effort none")
	if m.cfg.Thinking != "none" {
		t.Fatalf("thinking=%q", m.cfg.Thinking)
	}
}

func TestTUIInputDefaults(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if got := m.input.Height(); got != 3 {
		t.Fatalf("default input height = %d, want 3", got)
	}
	if m.input.Cursor.Mode().String() != "blink" {
		t.Fatalf("default cursor mode = %v, want blink", m.input.Cursor.Mode())
	}
	if m.input.Prompt != "" {
		t.Fatalf("input Prompt = %q, want empty (no white thick bar in chatbox)", m.input.Prompt)
	}
}

func TestTUIInputBlinkOffAndHeight(t *testing.T) {
	f := false
	m := newTestModel(t, Config{Model: "m", Input: InputConfig{Blink: &f, Height: 5}})
	if got := m.input.Height(); got != 5 {
		t.Fatalf("input height = %d, want 5", got)
	}
	if m.input.Cursor.Mode().String() != "static" {
		t.Fatalf("cursor mode = %v, want static", m.input.Cursor.Mode())
	}
	if cmd := m.Init(); cmd == nil {
		t.Fatalf("Init returned nil cmd")
	}
}

func TestLoadConfigInputTable(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"m\"\n[input]\nblink = false\nheight = 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if inputBlink(cfg) {
		t.Fatalf("blink should be off")
	}
	if got := inputHeight(cfg); got != 5 {
		t.Fatalf("height = %d, want 5", got)
	}
	cfg2, err := loadConfig(writeConfig(t, "model = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !inputBlink(cfg2) || inputHeight(cfg2) != 3 {
		t.Fatalf("defaults: blink=%v height=%d, want true 3", inputBlink(cfg2), inputHeight(cfg2))
	}
}

func TestLoadConfigTUITable(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"m\"\n[tui]\nmax_lines = 250\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiMaxLines(cfg); got != 250 {
		t.Fatalf("max_lines = %d, want 250", got)
	}
	// Unset keeps the 1000-line default; explicit 0 disables trimming.
	cfg2, err := loadConfig(writeConfig(t, "model = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiMaxLines(cfg2); got != 1000 {
		t.Fatalf("default max_lines = %d, want 1000", got)
	}
	cfg3, err := loadConfig(writeConfig(t, "model = \"m\"\n[tui]\nmax_lines = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiMaxLines(cfg3); got != 0 {
		t.Fatalf("explicit max_lines = %d, want 0 (unlimited)", got)
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	// Bare blink/height belong under [input]; at top level they must fail
	// loudly instead of decoding silently to defaults.
	if _, err := loadConfig(writeConfig(t, "model = \"m\"\nblink = false\nheight = 2\n")); err == nil {
		t.Fatal("expected error for top-level blink/height")
	} else if !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err=%v, want unknown key", err)
	}
}

func TestTUIWheelScrollPausesAutoscrollUntilBottom(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	for i := 0; i < 10; i++ {
		m.session.Turns = append(m.session.Turns, Turn{
			At:     time.Now(),
			Prompt: fmt.Sprintf("question %d with padding to fill lines", i),
			Reply:  fmt.Sprintf("answer %d with padding to fill lines", i),
		})
	}
	m.viewport.Width = 80
	m.viewport.Height = 10
	m.pinBottom()
	if !m.follow {
		t.Fatal("pinBottom should enable follow")
	}
	if !m.viewport.AtBottom() {
		t.Fatal("pinBottom should land on the bottom")
	}
	top := m.viewport.YOffset

	wheel := func(up bool) {
		t.Helper()
		btn := tea.MouseButtonWheelDown
		if up {
			btn = tea.MouseButtonWheelUp
		}
		_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: btn})
	}

	// Wheel up scrolls the scrollback and pauses autoscroll.
	wheel(true)
	if m.viewport.YOffset >= top {
		t.Fatalf("wheel up did not scroll: yoff=%d top=%d", m.viewport.YOffset, top)
	}
	if m.follow {
		t.Fatal("scrolling up should pause follow")
	}

	// A streaming flush while busy must not yank the view back down.
	m.busy = true
	m.pendingPrompt = "working"
	m.pendingSince = time.Now()
	m.lastStreamFlush = time.Time{}
	held := m.viewport.YOffset
	m.flushStream()
	if m.viewport.YOffset != held {
		t.Fatalf("stream flush smashed scrollback to bottom: %d -> %d", held, m.viewport.YOffset)
	}
	if m.viewport.AtBottom() {
		t.Fatal("view should still be scrolled up")
	}

	// Wheeling back down to the bottom resumes autoscroll.
	for i := 0; i < 100 && !m.viewport.AtBottom(); i++ {
		wheel(false)
	}
	if !m.viewport.AtBottom() || !m.follow {
		t.Fatalf("reaching bottom should resume follow: at=%v follow=%v", m.viewport.AtBottom(), m.follow)
	}

	// Committing a turn re-pins even after scrolling up again.
	wheel(true)
	if m.follow {
		t.Fatal("scrolling up should pause follow again")
	}
	_, _ = m.Update(turnDoneMsg{prompt: "q", answer: "a", history: m.history, model: "m"})
	if !m.follow || !m.viewport.AtBottom() {
		t.Fatal("turn commit should re-pin to the bottom")
	}
}

func TestTUIPgUpPausesAutoscroll(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	for i := 0; i < 10; i++ {
		m.session.Turns = append(m.session.Turns, Turn{
			At:     time.Now(),
			Prompt: fmt.Sprintf("question %d with padding to fill lines", i),
			Reply:  fmt.Sprintf("answer %d with padding to fill lines", i),
		})
	}
	m.viewport.Width = 80
	m.viewport.Height = 10
	m.pinBottom()
	top := m.viewport.YOffset
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.viewport.YOffset >= top {
		t.Fatalf("pgup did not scroll: yoff=%d top=%d", m.viewport.YOffset, top)
	}
	if m.follow {
		t.Fatal("pgup should pause follow")
	}
}

// TestTUIWheelDebrisSwallowed: split SGR mouse reports misparsed as key
// runes (rapid wheeling across TTY reads) must not land in the input box.
// Ambiguous fragments are held for reassembly — never inserted — while
// real typing (including lookalikes) still inserts, immediately when it
// cannot be mouse, or after the hold flushes.
func TestTUIWheelDebrisSwallowed(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80

	key := func(s string, alt, paste bool) tea.KeyMsg {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Alt: alt, Paste: paste}
	}
	// Whole reports arriving as one KeyMsg drop immediately.
	for _, debris := range []tea.KeyMsg{
		key("[<65;69;42M", false, false),
		key("<65;69;42M", false, false),
		key("<65;69;42m", false, false),
	} {
		before := m.input.Value()
		_, _ = m.Update(debris)
		if got := m.input.Value(); got != before {
			t.Fatalf("debris %q leaked into input: %q", string(debris.Runes), got)
		}
	}
	if len(m.mouseHold) != 0 {
		t.Fatalf("dropped reports left hold state: %q", m.mouseHoldText())
	}

	// A report split across reads reassembles and drops: feed it in
	// pieces, including single chars that match typing exactly.
	for _, frag := range []tea.KeyMsg{
		key("[", true, false),
		key("<64", false, false),
		key(";35", false, false),
		key(";12M", false, false),
	} {
		_, _ = m.Update(frag)
		if got := m.input.Value(); got != "" {
			t.Fatalf("split fragment %q leaked into input: %q", string(frag.Runes), got)
		}
	}
	if len(m.mouseHold) != 0 {
		t.Fatalf("completed split report left hold state: %q", m.mouseHoldText())
	}
	// Stale flush timers after a drop are no-ops.
	_, _ = m.Update(mouseHoldFlushMsg{gen: m.mouseHoldGen})
	if got := m.input.Value(); got != "" {
		t.Fatalf("flush after drop inserted: %q", got)
	}

	// Byte-wise split: every fragment is a plausible typed char, yet the
	// reassembled report still drops.
	m2 := newTestModel(t, testConfig("https://unused"))
	for _, s := range []string{"<", "6", "4", ";", "3", "5", ";", "1", "2", "M"} {
		_, _ = m2.Update(key(s, false, false))
	}
	if got := m2.input.Value(); got != "" {
		t.Fatalf("byte-wise report leaked: %q", got)
	}

	// Typing that cannot be mouse inserts immediately: ratios, counts,
	// arrows. A bare "[" is a valid report prefix, so it waits for the
	// flush like "<3".
	m.input.SetValue("")
	for _, legit := range []tea.KeyMsg{
		key("1;2", false, false),
		key("42M", false, false),
		key("<-", false, false),
		key("[", false, false),
		key("<3", false, false),
	} {
		_, _ = m.Update(legit)
	}
	for _, want := range []string{"1;2", "42M", "<-"} {
		if !strings.Contains(m.input.Value(), want) {
			t.Fatalf("legit typing %q lost, input=%q", want, m.input.Value())
		}
	}
	if strings.Contains(m.input.Value(), "<3") {
		t.Fatalf("held prefix should not insert early, input=%q", m.input.Value())
	}
	_, _ = m.Update(mouseHoldFlushMsg{gen: m.mouseHoldGen})
	for _, want := range []string{"[", "<3"} {
		if !strings.Contains(m.input.Value(), want) {
			t.Fatalf("held typing %q lost after flush, input=%q", want, m.input.Value())
		}
	}

	// Pasted mouse-shaped text bypasses the hold and inserts at once.
	m.input.SetValue("")
	_, _ = m.Update(key("[<65;69;42M", false, true))
	if !strings.Contains(m.input.Value(), "[<65;69;42M") {
		t.Fatalf("pasted text lost, input=%q", m.input.Value())
	}

	// Typing after held fragments flushes them first, preserving order.
	m.input.SetValue("")
	_, _ = m.Update(key("<", false, false))
	_, _ = m.Update(key("a", false, false))
	if got := m.input.Value(); got != "<a" {
		t.Fatalf("order broke, input=%q", got)
	}
}

// TestTUIMouseToggleSelection: alt+m flips terminal mouse reporting so both
// workflows work. Reporting on (default): wheel scrolls, bypass-modifier
// drag (shift-drag, opt-drag in iTerm2/Terminal.app) selects. Reporting
// off: plain drag selects + copies natively, wheel reports are ignored.
func TestTUIMouseToggleSelection(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	altM := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m"), Alt: true}
	if got := altM.String(); got != "alt+m" {
		t.Fatalf("test key should render as alt+m, got %q", got)
	}
	mm, cmd := m.Update(altM)
	m = mm.(*tuiModel)
	if cmd == nil {
		t.Fatal("alt+m must disable terminal mouse reporting")
	}
	if !m.mouseOff {
		t.Fatal("alt+m must set mouseOff")
	}
	if !strings.Contains(m.notice, "drag selects") {
		t.Fatalf("notice should point at plain-drag selection: %q", m.notice)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("alt+m leaked into input: %q", got)
	}

	// Wheel reports are ignored while off: the viewport must not move.
	for i := 0; i < 10; i++ {
		m.session.Turns = append(m.session.Turns, Turn{
			At:     time.Now(),
			Prompt: fmt.Sprintf("question %d with padding to fill lines", i),
			Reply:  fmt.Sprintf("answer %d with padding to fill lines", i),
		})
	}
	m.viewport.Width = 80
	m.viewport.Height = 10
	m.pinBottom()
	top := m.viewport.YOffset
	_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.viewport.YOffset != top {
		t.Fatalf("wheel must not scroll while mouse is off: yoff=%d top=%d", m.viewport.YOffset, top)
	}

	// Toggling back re-enables reporting and wheel scroll.
	mm, cmd = m.Update(altM)
	m = mm.(*tuiModel)
	if cmd == nil || m.mouseOff {
		t.Fatal("second alt+m must re-enable mouse reporting")
	}
	_, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.viewport.YOffset >= top {
		t.Fatalf("wheel up did not scroll after re-enable: yoff=%d top=%d", m.viewport.YOffset, top)
	}
}

// TestTUITransientNoticeClearsOnTyping: informational (yellow) status
// notices restore the turns/token row on the next chatbox edit, while
// errors stay sticky.
func TestTUITransientNoticeClearsOnTyping(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	altM := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m"), Alt: true}
	mm, _ := m.Update(altM)
	m = mm.(*tuiModel)
	if m.notice == "" || m.noticeErr {
		t.Fatalf("mouse toggle must set an informational notice, got %q err=%v", m.notice, m.noticeErr)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	m = mm.(*tuiModel)
	if m.notice != "" {
		t.Fatalf("typing must clear the informational notice, got %q", m.notice)
	}
	if got := m.statusView(); !strings.Contains(got, "turns") {
		t.Fatalf("status must restore totals after typing: %q", got)
	}

	m.notice = "boom"
	m.noticeErr = true
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m = mm.(*tuiModel)
	if m.notice != "boom" {
		t.Fatalf("error notice must stay after typing, got %q", m.notice)
	}
}

func TestTUIEscTwiceCancels(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	cancelled := 0
	m.busy = true
	m.cancel = func() { cancelled++ }
	esc := tea.KeyMsg{Type: tea.KeyEsc}

	_, cmd := m.Update(esc)
	if cancelled != 0 || cmd == nil || m.escArmedAt.IsZero() {
		t.Fatalf("first esc must only arm: cancelled=%d cmd=%v", cancelled, cmd)
	}

	// Expiry disarms and restores the working hint.
	at := m.escArmedAt
	_, _ = m.Update(escExpireMsg{at: at})
	if !m.escArmedAt.IsZero() || !strings.Contains(m.notice, "Working") {
		t.Fatalf("expiry must disarm: notice=%q", m.notice)
	}
	_, _ = m.Update(esc)
	if cancelled != 0 {
		t.Fatal("esc after expiry must re-arm, not cancel")
	}

	// Stale window: armed too long ago behaves as a first press.
	m.escArmedAt = time.Now().Add(-3 * time.Second)
	_, _ = m.Update(esc)
	if cancelled != 0 {
		t.Fatal("esc outside window must not cancel")
	}

	_, _ = m.Update(esc)
	if cancelled != 1 || !m.escArmedAt.IsZero() {
		t.Fatalf("second esc within window must cancel: cancelled=%d", cancelled)
	}
}

func TestTUICtrlCDClearsThenQuits(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyCtrlC, tea.KeyCtrlD} {
		m := newTestModel(t, testConfig("https://unused"))
		m.input.SetValue("draft")
		_, cmd := m.Update(tea.KeyMsg{Type: key})
		if m.input.Value() != "" || cmd != nil {
			t.Fatalf("key %v: first press must clear without quitting: %q", key, m.input.Value())
		}
		_, cmd = m.Update(tea.KeyMsg{Type: key})
		if cmd == nil {
			t.Fatalf("key %v: second press must quit", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("key %v: expected QuitMsg", key)
		}
	}
}

func TestTUIUpDownJumpToEdges(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.input.SetValue("one\ntwo\nthree")
	m.input.CursorEnd() // end of last line
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Line() != 1 {
		t.Fatalf("up from last line must move up, line=%d", m.input.Line())
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Line() != 0 || m.input.LineInfo().ColumnOffset != 0 {
		t.Fatalf("up on first line must go to start: line=%d col=%d", m.input.Line(), m.input.LineInfo().ColumnOffset)
	}
	for i := 0; i < 3; i++ {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.input.Line() != 2 || m.input.LineInfo().CharOffset != len("three") {
		t.Fatalf("down on last line must go to end: line=%d off=%d", m.input.Line(), m.input.LineInfo().CharOffset)
	}
}

// TestTUICSIUKeys: under Kitty disambiguate mode esc arrives as a CSI-u
// sequence that bubbletea reports as unknown; it must still cancel.
func TestTUICSIUKeys(t *testing.T) {
	type raw []byte // same shape as bubbletea's unexported unknown-CSI msg
	m := newTestModel(t, testConfig("https://unused"))
	cancelled := 0
	m.busy = true
	m.cancel = func() { cancelled++ }
	_, _ = m.Update(raw("\x1b[27u"))
	_, _ = m.Update(raw("\x1b[27u"))
	if cancelled != 1 {
		t.Fatalf("CSI 27u twice must cancel, cancelled=%d", cancelled)
	}
	if _, ok := csiUKey(raw("\x1b[109u")); ok {
		t.Fatal("bare CSI 109u must not be translated")
	}
	if km, ok := csiUKey(raw("\x1b[99;5u")); !ok || km.Type != tea.KeyCtrlC {
		t.Fatalf("CSI 99;5u must be ctrl+c, got %v %v", km, ok)
	}
}

func TestTUIColonQQuits(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	for _, in := range []string{" :q ", " :Q "} {
		cmd := m.submit(in)
		if cmd == nil {
			t.Fatalf("%q must quit", in)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%q: expected QuitMsg", in)
		}
	}
}

// TestTUIRoundsStartNewParagraph: text streamed by consecutive model rounds
// (separated by tool calls) must not glue into one paragraph.
func TestTUIRoundsStartNewParagraph(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	for _, round := range []string{"first.", "second.", "third."} {
		_, _ = m.Update(toolEventMsg{kind: "round_start"})
		_, _ = m.Update(toolEventMsg{kind: "text_delta", text: round})
		_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "t " + round})
	}
	if m.pendingText != "first.\n\nsecond.\n\nthird." {
		t.Fatalf("rounds glued: %q", m.pendingText)
	}
	if m.pendingThinking != "t first.\n\nt second.\n\nt third." {
		t.Fatalf("thinking rounds: %q", m.pendingThinking)
	}
	if roundBreak("a\n") != "a\n\n" || roundBreak("a\n\n") != "a\n\n" || roundBreak("") != "" {
		t.Fatal("roundBreak edge cases")
	}
}

// On wide terminals the whole UI (header, scrollback, input, footer) is
// capped to one column and centered, not just the transcript text.
func TestViewCentersCappedColumn(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	if got := m.columnWidth(); got != maxColumnWidth {
		t.Fatalf("columnWidth = %d, want %d", got, maxColumnWidth)
	}
	pad := (200 - maxColumnWidth) / 2
	// Strip SGR color codes before layout assertions: with color forced
	// (e.g. CLICOLOR_FORCE=1 or CI) the border carries escape sequences,
	// but the centering geometry is what this test pins down.
	strip := func(s string) string {
		re := regexp.MustCompile("\x1b\\[[0-9;]*m")
		return re.ReplaceAllString(s, "")
	}
	lines := strings.Split(m.View(), "\n")
	for i, l := range lines {
		if w := lipgloss.Width(l); w > pad+maxColumnWidth {
			t.Fatalf("line %d is %d wide, exceeds column %d", i, w, pad+maxColumnWidth)
		}
	}
	// The input box top border sits at the left edge of the column.
	var found bool
	for _, l := range lines {
		if strings.HasPrefix(strip(l), strings.Repeat(" ", pad)+"╭") {
			found = true
		}
	}
	if !found {
		t.Fatalf("input border not centered with %d-col pad", pad)
	}

	// Narrow terminals are unchanged: no padding, full width.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.columnWidth() != 100 {
		t.Fatalf("narrow columnWidth = %d", m.columnWidth())
	}
	for _, l := range strings.Split(m.View(), "\n") {
		if s := strip(l); strings.HasPrefix(s, " ╭") || strings.HasPrefix(s, "  ╭") {
			t.Fatalf("unexpected padding on narrow terminal: %q", l)
		}
	}
}

// The sessions/models pickers are navigation surfaces: the chatbox and quota
// footer are hidden there, while the ready/working status row stays visible
// and is pinned to the bottom edge (no dead space below it).
func TestPickerViewsHideChatboxAndFooter(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	for _, mode := range []viewMode{viewSessions, viewModels} {
		m.show(mode)
		out := ansi.Strip(m.View())
		if strings.Contains(out, "Type a message") {
			t.Fatalf("%v should hide the chatbox:\n%s", mode, out)
		}
		if strings.Contains(out, footerDir()) {
			t.Fatalf("%v should hide the footer:\n%s", mode, out)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		last := lines[len(lines)-1]
		if !strings.Contains(last, "ready") && !strings.Contains(last, "working") {
			t.Fatalf("%v should end with the status row, got %q", mode, last)
		}
		if len(lines) != m.height {
			t.Fatalf("%v view has %d lines, want %d (status pinned to bottom)", mode, len(lines), m.height)
		}
	}
}

func TestStatusFormatK(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1k", 275654: "276k", 300000: "300k"}
	for n, want := range cases {
		if got := fk(n); got != want {
			t.Errorf("fk(%d)=%q, want %q", n, got, want)
		}
	}
}

func TestStatusParsesRoundProgress(t *testing.T) {
	cur, max, ok := parseRoundProgress("Round 3/300")
	if !ok || cur != 3 || max != 300 {
		t.Fatalf("parse=%d/%d ok=%v, want 3/300 true", cur, max, ok)
	}
	if _, _, ok := parseRoundProgress("working"); ok {
		t.Fatal("non-round text must not parse")
	}
}

func TestStatusRowShowsWindowInK(t *testing.T) {
	m := newTestModel(t, Config{Model: "kimi-k3", BaseURL: "https://unused", SessionID: "ses-tui", MaxRounds: 4})
	m.session.Turns = []Turn{{At: time.Now(), Prompt: "hi", Reply: "yo"}}
	m.history = []openai.ChatCompletionMessage{{Role: "user", Content: strings.Repeat("x", 4000)}}
	m.roundCur, m.roundMax = 3, 300
	m.busy = true
	m.pendingSince = time.Now()
	m.spinner = spinner.Model{}
	got := m.statusView()
	if !strings.Contains(got, "round 3/300") {
		t.Fatalf("status missing round progress: %q", got)
	}
	if !strings.Contains(got, "256k") {
		t.Fatalf("status missing k-format budget: %q", got)
	}
	if !strings.Contains(got, "1 turns") {
		t.Fatalf("status missing turn count: %q", got)
	}
}

// TestTUIUpRecallsLastPromptAndRewinds: up on an empty chatbox loads the
// previous prompt; sending it replaces that turn instead of stacking.
func TestTUIUpRecallsLastPromptAndRewinds(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp}); m.input.Value() != "" {
		t.Fatal("no turns: up must leave the box empty")
	}
	m.history = append(seedHistory(m.cfg),
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "first"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "ok"})
	m.session.Turns = []Turn{{Prompt: "first", Reply: "ok", Usage: Usage{Total: 5}}}
	m.session.TotalUsage = Usage{Total: 5}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "first" || !m.retrying {
		t.Fatalf("up must recall the last prompt: %q retrying=%v", m.input.Value(), m.retrying)
	}
	// Up with text present is plain cursor movement, not a second recall.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "first" {
		t.Fatalf("value changed: %q", m.input.Value())
	}
	m.rewindLastTurn()
	if len(m.history) != 1 || len(m.session.Turns) != 0 || m.session.TotalUsage.Total != 0 {
		t.Fatalf("rewind must drop the turn: hist=%d turns=%d usage=%+v", len(m.history), len(m.session.Turns), m.session.TotalUsage)
	}
	// Emptying the box cancels the retry.
	m.history = append(seedHistory(m.cfg), openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "first"})
	m.session.Turns = []Turn{{Prompt: "first"}}
	m.input.SetValue("")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.input.SetValue("x")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if m.retrying {
		t.Fatal("empty box must clear retry")
	}
}

// TestTUIReadyShowsLastTurnDuration: the elapsed time stays visible once
// the turn finishes, and the bell only rings when [tui] sound is on.
func TestTUIReadyShowsLastTurnDuration(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.lastTurnDur = 63 * time.Second
	if got := m.statusView(); !strings.Contains(got, "ready · 1m3s") {
		t.Fatalf("ready row must show the last turn time: %q", got)
	}
	if m.dingCmd() == nil {
		t.Fatal("sound defaults on")
	}
	off := false
	m.cfg.TUI.Sound = &off
	if m.dingCmd() != nil {
		t.Fatal("sound=false must not ring")
	}
}

// TestTUIAskStaysVisibleOnShortTerminal: the overlay hides the chatbox so
// the question gets the whole height, starts at its heading, and follows
// the cursor past the fold.
func TestTUIAskStaysVisibleOnShortTerminal(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 14})
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, "line")
	}
	m.session.Turns = []Turn{{Prompt: "p", Reply: strings.Join(many, "\n\n")}}
	m.refreshViewport()
	m.viewport.GotoBottom()
	var opts []AskOption
	for i := 0; i < 20; i++ {
		opts = append(opts, AskOption{Label: fmt.Sprintf("opt%d", i)})
	}
	_, _ = m.Update(questionMsg{qs: []AskQuestion{{Question: "Which one?", Options: opts}}, reply: make(chan askReply, 1)})
	if m.viewport.Height != 12 || m.viewport.YOffset != 0 {
		t.Fatalf("ask must use full height from the top: h=%d off=%d", m.viewport.Height, m.viewport.YOffset)
	}
	if !strings.Contains(m.View(), "Which one?") {
		t.Fatal("question heading hidden")
	}
	for i := 0; i < 18; i++ {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if v := m.View(); !strings.Contains(v, "opt17") {
		t.Fatalf("selected option scrolled out of view:\n%s", v)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewChat || m.viewport.Height != m.viewportHeight() || m.isPickerMode() {
		t.Fatalf("dismiss must restore chat: mode=%v h=%d", m.mode, m.viewport.Height)
	}
}

// TestTUITabTogglesPlanMode: Tab in the chatbox swaps work and plan mode and
// the ready chatbox border turns cyan in plan mode.
func TestTUITabTogglesPlanMode(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if m.inputBorderColor() != mkGreen {
		t.Fatal("work mode ready border must be green")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.cfg.Plan || !strings.Contains(m.history[0].Content, "Plan Mode") || m.inputBorderColor() != mkCyan {
		t.Fatalf("tab must enter plan mode, reseed the overlay and turn the border cyan: plan=%v", m.cfg.Plan)
	}
	m.busy = true
	if m.inputBorderColor() != mkOrange {
		t.Fatal("busy border stays orange in plan mode")
	}
	m.busy = false
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.cfg.Plan || strings.Contains(m.history[0].Content, "Plan Mode") || m.inputBorderColor() != mkGreen {
		t.Fatal("second tab must return to work mode")
	}
}

// TestTUIChatboxGrowsWithContent: newlines and soft-wrapped paste grow the
// chatbox up to 10 rows, shrink the scrollback to match, and the frame never
// exceeds the terminal height.
func TestTUIChatboxGrowsWithContent(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	base, vp := m.input.Height(), m.viewport.Height
	for i := 0; i < 14; i++ {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	}
	if m.input.Height() != maxInputHeight {
		t.Fatalf("height=%d want cap %d", m.input.Height(), maxInputHeight)
	}
	if m.viewport.Height != vp-(maxInputHeight-base) || lipgloss.Height(m.View()) != 29 {
		t.Fatalf("viewport=%d frame=%d", m.viewport.Height, lipgloss.Height(m.View()))
	}
	m.clearInput()
	_, _ = m.Update(nil)
	if m.input.Height() != base || m.viewport.Height != vp {
		t.Fatalf("clearing must restore: input=%d vp=%d", m.input.Height(), m.viewport.Height)
	}
	m.input.SetValue(strings.Repeat("word ", 100))
	_, _ = m.Update(nil)
	if h := m.input.Height(); h <= base || h > maxInputHeight {
		t.Fatalf("soft-wrapped paste must grow the box, got %d", h)
	}
}

// TestTUIHeaderStatusNeverWrap: a long session title in plan mode and a long
// notice must clip to one row, otherwise the frame overflows the terminal and
// the chatbox lands one row off the bottom.
func TestTUIHeaderStatusNeverWrap(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	m.session.Title = strings.Repeat("a long title ", 10)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.notice = strings.Repeat("long notice ", 20)
	if h := lipgloss.Height(m.headerView()); h != 1 {
		t.Fatalf("header rows=%d", h)
	}
	if h := lipgloss.Height(m.statusView()); h != 1 {
		t.Fatalf("status rows=%d", h)
	}
	if h := lipgloss.Height(m.View()); h > 30 {
		t.Fatalf("frame rows=%d exceeds terminal", h)
	}
}

func TestBreakLongWordsKeepsANSIIntact(t *testing.T) {
	line := "\x1b[38;5;231mselect\x1b[0m \x1b[38;5;231mgift_cards.*\x1b[0m"
	if got := breakLongWords(line, 40); got != line {
		t.Fatalf("styled short line modified: %q", got)
	}
	long := "\x1b[38;5;231m" + strings.Repeat("x", 30) + "\x1b[0m"
	got := breakLongWords(long, 10)
	for _, ln := range strings.Split(got, "\n") {
		if ansi.StringWidth(ln) > 10 {
			t.Fatalf("line too wide: %q", ln)
		}
	}
	if ansi.Strip(got) != strings.Repeat("x", 30)+"" && strings.ReplaceAll(ansi.Strip(got), "\n", "") != strings.Repeat("x", 30) {
		t.Fatalf("content lost: %q", got)
	}
}

// TestTUICopyLastReply: a finished reply gets an underlined "copy" row as the
// last transcript line; clicking it (or /copy) sends the reply to the clipboard.
func TestTUICopyLastReply(t *testing.T) {
	var got string
	old := clipboardWrite
	clipboardWrite = func(s string) { got = s }
	t.Cleanup(func() { clipboardWrite = old })

	m := newTestModel(t, testConfig("https://unused"))
	m.handleCommand("/copy")
	if got != "" || !m.noticeErr {
		t.Fatalf("empty session must not copy: %q %q", got, m.notice)
	}
	m.session.Turns = append(m.session.Turns, Turn{At: time.Now(), Prompt: "q", Reply: "**answer**"})
	m.viewport.Width, m.viewport.Height = 80, 10
	m.pinBottom()
	lines := strings.Split(m.viewport.View(), "\n")
	row := -1
	for i, l := range lines {
		if strings.TrimSpace(ansi.Strip(l)) == copyLabel {
			row = i
		}
	}
	if row < 0 {
		t.Fatalf("no copy row in view:\n%s", m.viewport.View())
	}
	click := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: row + 1}
	_, _ = m.Update(click)
	if got != "**answer**" {
		t.Fatalf("click did not copy reply: %q", got)
	}
	got = ""
	click.Y = row // a different row must not copy
	_, _ = m.Update(click)
	if got != "" {
		t.Fatalf("click off the copy row copied: %q", got)
	}
}

// TestTUIModelPickerCachesAndRefreshesInPlace: the first open fetches and
// caches; the next open renders from the cache and a debounced live refresh
// swaps the catalog in without moving the cursor or search text.
func TestTUIModelPickerCachesAndRefreshesInPlace(t *testing.T) {
	m, statePath := newTestModelWithConfig(t, "model = \"m\"\nbase_url = \"https://unused\"\n")
	cache := modelsCachePathFor(statePath)
	if err := saveModelsCache(cache, []string{"glm-5.3", "kimi-k3", "qwen-4"}); err != nil {
		t.Fatal(err)
	}
	cmd := m.openModelPicker("")
	if cmd == nil || len(readModelsCache(cache)) != 3 {
		t.Fatalf("cmd=%v cache=%v", cmd, readModelsCache(cache))
	}
	_, _ = m.Update(modelsLoadedMsg{ids: readModelsCache(cache)})
	if m.mode != viewModels || len(m.modelChoices) != 3 {
		t.Fatalf("mode=%v choices=%v", m.mode, m.modelChoices)
	}
	m.cursor = 1 // kimi-k3
	_, cmd = m.Update(modelsRefreshMsg{filter: ""})
	if cmd == nil {
		t.Fatal("refresh must start a fetch while the picker is open")
	}
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"aaa-1", "glm-5.3", "kimi-k3", "qwen-4"}, refresh: true})
	if len(m.modelChoices) != 4 || m.modelChoices[m.cursor].id != "kimi-k3" {
		t.Fatalf("cursor must follow kimi-k3: cursor=%d choices=%v", m.cursor, m.modelChoices)
	}
	// Closed picker: no fetch, no resurrection.
	m.show(viewChat)
	if _, cmd = m.Update(modelsRefreshMsg{filter: ""}); cmd != nil {
		t.Fatal("refresh must be skipped once the picker is closed")
	}
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"x"}, refresh: true})
	if m.mode != viewChat {
		t.Fatalf("mode=%v", m.mode)
	}
}

func TestSlotKey(t *testing.T) {
	for in, want := range map[string]struct {
		key    string
		assign bool
		ok     bool
	}{
		"alt+1": {"1", false, true}, "alt+0": {"0", false, true}, "alt+!": {"1", true, true},
		"alt+)": {"0", true, true}, "alt+a": {}, "1": {}, "alt+enter": {},
	} {
		k, a, ok := slotKey(in)
		if k != want.key || a != want.assign || ok != want.ok {
			t.Fatalf("slotKey(%q)=%q,%v,%v want %+v", in, k, a, ok, want)
		}
	}
}

func TestTUIFavouriteSlotsAssignAndSwitch(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "test-key")
	m, statePath := newTestModelWithConfig(t, "model = \"glm-5.3\"\nthinking = \"high\"\nbase_url = \"https://unused\"\n")
	alt := func(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true} }

	// alt+shift+2 (alt+@) stores the current pair and says so.
	_, _ = m.handleKey(alt('@'))
	if !strings.Contains(m.notice, "Slot 2") || m.noticeErr {
		t.Fatalf("assign notice=%q", m.notice)
	}
	if raw, _ := os.ReadFile(slotsPathFor(statePath)); string(raw) != "2: glm-5.3 high\n" {
		t.Fatalf("slots.yaml=%q", raw)
	}

	// Switch elsewhere, then alt+2 returns to glm-5.3 + high.
	m.handleCommand("/model kimi-k2.6")
	if m.cfg.Model != "kimi-k2.6" {
		t.Fatalf("model=%q", m.cfg.Model)
	}
	_, _ = m.handleKey(alt('2'))
	if m.cfg.Model != "glm-5.3" || m.cfg.Thinking != "high" {
		t.Fatalf("slot 2 → %q/%q notice=%q", m.cfg.Model, m.cfg.Thinking, m.notice)
	}
	if st := loadUserState(statePath); st.Model != "glm-5.3" || st.Thinking != "high" {
		t.Fatalf("slot switch must persist state: %+v", st)
	}

	// Slots reload from disk on a fresh model; empty slots report.
	m.slots = nil
	_, _ = m.handleKey(alt('5'))
	if !m.noticeErr || !strings.Contains(m.notice, "empty") || m.cfg.Model != "glm-5.3" {
		t.Fatalf("empty slot notice=%q err=%v", m.notice, m.noticeErr)
	}
	_, _ = m.handleKey(alt('2'))
	if m.cfg.Model != "glm-5.3" || m.noticeErr {
		t.Fatalf("slot reload failed: %q %q", m.cfg.Model, m.notice)
	}
}

func TestTUIFavouriteSlotsKittyAltShift(t *testing.T) {
	type raw []byte
	km, ok := csiUKey(raw("\x1b[50;4u")) // alt+shift+2
	if !ok || km.String() != "alt+@" {
		t.Fatalf("got %q %v", km.String(), ok)
	}
}

func TestSlotsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), userSlotsName)
	if err := os.WriteFile(p, []byte("# mine\n1: \"glm-5.3\" high # fast\n0: openrouter/x/y\nbad\nz: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSlots(p)
	if len(got) != 2 || got["1"] != (modelSlot{model: "glm-5.3", thinking: "high"}) || got["0"] != (modelSlot{model: "openrouter/x/y"}) {
		t.Fatalf("parsed=%v", got)
	}
	if err := saveSlots(p, got); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != "1: glm-5.3 high\n0: openrouter/x/y\n" {
		t.Fatalf("saved=%q", raw)
	}
	if len(readSlots("")) != 0 || slotsPathFor("") != "" {
		t.Fatal("no state path must mean session-only")
	}
}

func TestRoutingCycleOpenRouter(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.statePath = filepath.Join(t.TempDir(), "state.json")
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"

	for _, want := range []string{"floor", "nitro", "exacto", ""} {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlY})
		if m.cfg.Routing != want {
			t.Fatalf("ctrl+y routing=%q want %q", m.cfg.Routing, want)
		}
	}
	if st := loadUserState(m.statePath); st.Routing != "" {
		t.Fatalf("wrapped-to-none state routing=%q", st.Routing)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlY}) // none → floor
	if st := loadUserState(m.statePath); st.Routing != "floor" {
		t.Fatalf("persisted routing=%q want floor", st.Routing)
	}
	if !strings.Contains(m.notice, "floor") {
		t.Fatalf("cycle notice must name the routing: %q", m.notice)
	}
}

func TestRoutingNoopGoZen(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.statePath = filepath.Join(t.TempDir(), "state.json")
	for _, model := range []string{"glm-5.3", "zen/claude-haiku-5-5"} {
		m.cfg.Model = model
		m.cfg.Routing = ""
		m.notice = ""
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlY})
		if m.cfg.Routing != "" {
			t.Fatalf("%s: ctrl+y changed routing to %q", model, m.cfg.Routing)
		}
		if m.notice != "" {
			t.Fatalf("%s: ctrl+y must be a silent no-op, notice=%q", model, m.notice)
		}
	}
	if _, err := os.Stat(m.statePath); !os.IsNotExist(err) {
		t.Fatalf("ctrl+y on Go/Zen must not write state (err=%v)", err)
	}
}

func TestRouteCommand(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.statePath = filepath.Join(t.TempDir(), "state.json")
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"

	m.handleCommand("/route floor")
	if m.cfg.Routing != "floor" {
		t.Fatalf("/route floor set %q", m.cfg.Routing)
	}
	m.notice = ""
	m.handleCommand("/route")
	if !strings.Contains(m.notice, "floor") {
		t.Fatalf("/route read=%q", m.notice)
	}
	m.handleCommand("/route none")
	if m.cfg.Routing != "" {
		t.Fatalf("/route none left %q", m.cfg.Routing)
	}
	m.handleCommand("/route bogus")
	if !m.noticeErr {
		t.Fatalf("/route bogus must error, notice=%q", m.notice)
	}

	// Go models are a no-op with an explanatory notice.
	m.cfg.Model = "glm-5.3"
	m.cfg.Routing = ""
	m.notice, m.noticeErr = "", false
	m.handleCommand("/route floor")
	if m.cfg.Routing != "" || m.noticeErr || !strings.Contains(m.notice, "OpenRouter") {
		t.Fatalf("go /route: routing=%q notice=%q err=%v", m.cfg.Routing, m.notice, m.noticeErr)
	}
}

func TestCtrlOThinkToggle(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if !m.showThink {
		t.Fatal("thinking must be shown by default")
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.showThink {
		t.Fatal("ctrl+o must hide thinking")
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.showThink {
		t.Fatal("ctrl+o must show thinking again")
	}
	before := m.showMeta
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlB})
	if m.showMeta == before {
		t.Fatal("ctrl+b must toggle the status details")
	}
}

func TestRoutingFooterMetadata(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.width, m.viewport.Width = 200, 200
	m.orCredits = &ORCredits{TotalCredits: 20, TotalUsage: 7.66}
	m.orMeta = &ORMeta{Provider: "Infermatic", TPS: 42.4}
	m.orPricing = map[string]orPricing{
		"openrouter/moonshotai/kimi-k3": {prompt: 0.0000001, completion: 0.0000004},
	}
	m.orSpent, m.orSpentSet = 0.0034, true
	got := ansi.Strip(m.footerView())
	for _, want := range []string{"openrouter $12.34 left", "Infermatic", "42 t/s", "in $0.10/out $0.40/M", "sess $0.0034"} {
		if !strings.Contains(got, want) {
			t.Fatalf("footer %q missing %q", got, want)
		}
	}

	// Segments drop independently: no provider, no catalog rates.
	m.orMeta = &ORMeta{TPS: 42.4}
	m.orPricing = nil
	got = ansi.Strip(m.footerView())
	if strings.Contains(got, "Infermatic") || strings.Contains(got, "/M") {
		t.Fatalf("missing metadata must drop: %q", got)
	}
	if !strings.Contains(got, "42 t/s") {
		t.Fatalf("tps should survive: %q", got)
	}

	// Nothing to add leaves the plain balance.
	m.orMeta = nil
	if got := ansi.Strip(m.footerView()); !strings.Contains(got, "left") || strings.Contains(got, "t/s") {
		t.Fatalf("empty metadata footer=%q", got)
	}

	// Narrow terminals yield the whole suffix via the width rule.
	m.width, m.viewport.Width = 24, 24
	m.orMeta = &ORMeta{Provider: "Infermatic", TPS: 42}
	m.orPricing = map[string]orPricing{"openrouter/moonshotai/kimi-k3": {prompt: 0.0000001, completion: 0.0000004}}
	if got := ansi.Strip(m.footerView()); strings.Contains(got, "Infermatic") || strings.Contains(got, "/M") || strings.Contains(got, "sess") {
		t.Fatalf("narrow footer must yield the suffix: %q", got)
	}
}

// TestRoutingSessionCostAccumulates: each OpenRouter turn's billed cost (summed
// across tool-loop rounds) adds to a running session total, a costless turn
// leaves it alone, and a new session zeroes it.
func TestRoutingSessionCostAccumulates(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.width, m.viewport.Width = 200, 200
	m.session.Turns = nil

	_, _ = m.Update(turnDoneMsg{prompt: "a", answer: "1", history: []openai.ChatCompletionMessage{}, model: m.cfg.Model, turnCost: 0.0012, hasTurnCost: true})
	_, _ = m.Update(turnDoneMsg{prompt: "b", answer: "2", history: []openai.ChatCompletionMessage{}, model: m.cfg.Model, turnCost: 0.001, hasTurnCost: true})
	if got := formatCost(m.orSpent); got != "$0.0022" {
		t.Fatalf("session spend = %s, want $0.0022", got)
	}
	if got := ansi.Strip(m.footerView()); !strings.Contains(got, "sess $0.0022") {
		t.Fatalf("footer %q missing session cost", got)
	}

	// A turn that reports no cost must not move the total.
	_, _ = m.Update(turnDoneMsg{prompt: "c", answer: "3", history: []openai.ChatCompletionMessage{}, model: m.cfg.Model})
	if got := formatCost(m.orSpent); got != "$0.0022" {
		t.Fatalf("costless turn changed spend: %s", got)
	}

	m.startFreshSession()
	if m.orSpent != 0 || m.orSpentSet {
		t.Fatalf("new session must reset spend: %v set=%v", m.orSpent, m.orSpentSet)
	}
}

// TestRoutingSessionCostWithoutBalance: the session cost is measured locally,
// so it shows even when the credits balance never landed (plain API key).
func TestRoutingSessionCostWithoutBalance(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.width, m.viewport.Width = 200, 200
	m.orCredits = nil
	m.orSpent, m.orSpentSet = 0.5, true
	got := ansi.Strip(m.footerView())
	if !strings.Contains(got, "sess $0.50") {
		t.Fatalf("footer %q missing session cost without balance", got)
	}
	if strings.Contains(got, "left") {
		t.Fatalf("no balance must mean no left segment: %q", got)
	}
}
