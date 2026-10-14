package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
