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

	"github.com/charmbracelet/bubbles/spinner"
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
	m.apiKey = "key"
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

func TestTUICtrlShortcuts(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))

	if footer := m.footerView(); strings.Contains(footer, "F1") || strings.Contains(footer, "ctrl+t") {
		t.Fatalf("footer must not list shortcuts:\n%s", footer)
	}
	if help := m.renderHelp(); !strings.Contains(help, "ctrl+g help") || strings.Contains(help, "F1 help") {
		t.Fatalf("help must list the ctrl shortcuts:\n%s", help)
	}
	if help := m.renderHelp(); !strings.Contains(help, "ctrl+t thinking") || !strings.Contains(help, "ctrl+l sessions") || !strings.Contains(help, "ctrl+u tools") {
		t.Fatalf("help must list ctrl+t thinking, ctrl+l sessions and ctrl+u tools:\n%s", help)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.mode != viewHelp {
		t.Fatalf("ctrl+g: mode=%v", m.mode)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m.mode != viewHistory {
		t.Fatalf("ctrl+r: mode=%v", m.mode)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlL})
	if m.mode != viewSessions {
		t.Fatalf("ctrl+l: mode=%v", m.mode)
	}
	m.show(viewChat)
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.mode != viewTools {
		t.Fatalf("ctrl+u: mode=%v", m.mode)
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
	if !strings.Contains(got, "No turns yet.") {
		t.Fatalf("empty chat should show the simple label, got:\n%s", got)
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

func TestTUIPendingRunningToolsShowSpinnerAndCounts(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "do things"

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_search", args: `{"query":"go"}`})
	if m.toolStatus != "⏺ web_search…" {
		t.Fatalf("single running status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_fetch", args: `{"url":"https://x"}`})
	if m.toolStatus != "⏺ web_fetch… · 2 running" {
		t.Fatalf("parallel running status=%q", m.toolStatus)
	}

	frame0 := m.spinner.View()
	out := m.renderPending(80)
	if !strings.Contains(out, frame0) || !strings.Contains(out, "running") {
		t.Fatalf("open threads must show the spinner frame %q:\n%s", frame0, out)
	}
	if strings.Count(out, "· running") != 2 {
		t.Fatalf("both open threads should be marked:\n%s", out)
	}

	// Spinner ticks advance the frame in the scrollback across consecutive
	// ticks, not just once.
	_, _ = m.Update(spinner.TickMsg{})
	frame1 := m.spinner.View()
	if frame1 == frame0 {
		t.Fatal("spinner frame should advance on tick")
	}
	if out2 := m.renderPending(80); !strings.Contains(out2, frame1) {
		t.Fatalf("scrolled frame should track the tick %q:\n%s", frame1, out2)
	}
	_, _ = m.Update(spinner.TickMsg{})
	frame2 := m.spinner.View()
	if frame2 == frame1 || frame2 == frame0 {
		t.Fatal("spinner frame should keep advancing across ticks")
	}
	if out2b := m.renderPending(80); !strings.Contains(out2b, frame2) {
		t.Fatalf("scrolled frame should track the second tick %q:\n%s", frame2, out2b)
	}

	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_search", output: "1. release notes"})
	if m.toolStatus != "✓ web_search · 1 still running" {
		t.Fatalf("partial completion status=%q", m.toolStatus)
	}
	out3 := m.renderPending(80)
	if strings.Count(out3, "· running") != 1 {
		t.Fatalf("finished thread must lose its spinner:\n%s", out3)
	}
	if !strings.Contains(out3, "1. release notes") {
		t.Fatalf("finished thread keeps its preview:\n%s", out3)
	}

	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_fetch", output: "ok"})
	if m.toolStatus != "✓ web_fetch" {
		t.Fatalf("all-done status=%q", m.toolStatus)
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
	m.handleCommand("/model kimi-k2.6")
	if m.cfg.Model != "kimi-k2.6" {
		t.Fatalf("model=%q notice=%q", m.cfg.Model, m.notice)
	}
	if m.cfg.Thinking != "" {
		t.Fatalf("max is invalid on kimi toggle and must reset, got %q", m.cfg.Thinking)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `model = "kimi-k2.6"`) || strings.Contains(string(raw), "thinking") {
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
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `thinking = "max"`) {
		t.Fatalf("thinking not persisted:\n%s", raw)
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
