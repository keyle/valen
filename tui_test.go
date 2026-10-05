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
	if m.pendingText != "" || m.input.Value() != "" || m.chatCacheTurns != 0 || m.chatCache != "" {
		t.Fatalf("pending/live state not released: text=%q input=%q cache=%q/%d", m.pendingText, m.input.Value(), m.chatCache, m.chatCacheTurns)
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

func TestTUICtrlShortcuts(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))

	if footer := m.footerView(); strings.Contains(footer, "F1") || strings.Contains(footer, "ctrl+t") {
		t.Fatalf("footer must not list shortcuts:\n%s", footer)
	}
	if help := m.renderHelp(); !strings.Contains(help, "ctrl+g help") || strings.Contains(help, "F1 help") {
		t.Fatalf("help must list the ctrl shortcuts:\n%s", help)
	}
	// Phrases may wrap across lines at narrow widths; flatten before
	// matching so wording checks don't depend on wrap points.
	flatHelp := strings.Join(strings.Fields(m.renderHelp()), " ")
	if !strings.Contains(flatHelp, "ctrl+t thinking") || !strings.Contains(flatHelp, "ctrl+l sessions") || !strings.Contains(flatHelp, "ctrl+u tools") {
		t.Fatalf("help must list ctrl+t thinking, ctrl+l sessions and ctrl+u tools:\n%s", m.renderHelp())
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

// TestTUIStatusShowsPhase: model-side deltas flip the footer from tool state
// to thinking/writing, so a long reasoning round never looks like stuck
// local work.
func TestTUIStatusShowsPhase(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.busy = true

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "shell", args: `{"command":"ls"}`})
	if m.toolStatus != "⏺ shell…" {
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
	if m.toolStatus != "⏺ read…" {
		t.Fatalf("back-to-tool status=%q", m.toolStatus)
	}
}

func TestAppendThinkingDeltaBoundaries(t *testing.T) {
	// Every new addition starts on its own line so updates never pile
	// onto the same line.
	s, cur := appendThinkingDelta("", "Hel", 0, 0)
	s, cur = appendThinkingDelta(s, "lo", 0, cur)
	if s != "Hel\nlo" || cur != 0 {
		t.Fatalf("addition missing newline: %q/%d", s, cur)
	}
	// A new part also starts on its own line.
	s, cur = appendThinkingDelta(s, "Reading config", 1, cur)
	if s != "Hel\nlo\nReading config" || cur != 1 {
		t.Fatalf("part boundary missing newline: %q/%d", s, cur)
	}
	// Same part continues on its own line too.
	s, cur = appendThinkingDelta(s, " more", 1, cur)
	if s != "Hel\nlo\nReading config\n more" {
		t.Fatalf("continuation glued: %q", s)
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

// TestTUIThinkingPartsSeparateLiveLines: every reasoning update lands on
// its own line in the live view (muse streams status-like updates with
// no trailing newline, which used to glue into one wrapped line).
func TestTUIThinkingPartsSeparateLiveLines(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "Checking the repo layout", idx: 0})
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "Reading the config file", idx: 1})
	if m.pendingThinking != "Checking the repo layout\nReading the config file" {
		t.Fatalf("live thinking glued: %q", m.pendingThinking)
	}
	// Same-part updates separate too, not just part boundaries.
	m2 := newTestModel(t, testConfig("https://unused"))
	_, _ = m2.Update(toolEventMsg{kind: "thinking_delta", text: "Checking the repo layout", idx: 0})
	_, _ = m2.Update(toolEventMsg{kind: "thinking_delta", text: "Reading the config file", idx: 0})
	if m2.pendingThinking != "Checking the repo layout\nReading the config file" {
		t.Fatalf("live thinking glued on same part: %q", m2.pendingThinking)
	}
}

// TestTUICommittedThinkingKeepsLiveLineReturns: the success-path commit
// must keep the newline-separated streamed thinking for display instead
// of the exactly-concatenated authoritative reply (which glues additions
// into one wrapped line in scrollback). Replay history stays exact.
func TestTUICommittedThinkingKeepsLiveLineReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Checking the repo layout\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Reading the config file\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	if len(m.session.Turns) != 1 {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	turn := m.session.Turns[0]
	if turn.Thinking != "Checking the repo layout\nReading the config file" {
		t.Fatalf("committed thinking glued: %q", turn.Thinking)
	}
	if turn.Reply != "done" {
		t.Fatalf("reply: %q", turn.Reply)
	}
	// History keeps the exact concatenation for provider replay.
	last := m.history[len(m.history)-1]
	if last.ReasoningContent != "Checking the repo layoutReading the config file" {
		t.Fatalf("replay reasoning altered: %q", last.ReasoningContent)
	}
	if !strings.Contains(m.renderChat(), "Checking the repo layout") || !strings.Contains(m.renderChat(), "Reading the config file") {
		t.Fatalf("scrollback missing thinking:\n%s", m.renderChat())
	}
}

// TestThinkingRoundsKeepLineReturnBetweenAdditions: every thinking round
// (each thinking_delta addition) must land on its own line in the
// committed turn, not glued onto the previous round's text. Regression
// test: the success-path commit once overwrote the newline-separated
// live buffer with the exactly-concatenated reply.
func TestThinkingRoundsKeepLineReturnBetweenAdditions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"first round\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"second round\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"third round\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	if len(m.session.Turns) != 1 {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	thinking := m.session.Turns[0].Thinking
	lines := strings.Split(thinking, "\n")
	if len(lines) != 3 || lines[0] != "first round" || lines[1] != "second round" || lines[2] != "third round" {
		t.Fatalf("thinking rounds not one-per-line: %q", thinking)
	}
	for _, glued := range []string{"first roundsecond", "second roundthird"} {
		if strings.Contains(thinking, glued) {
			t.Fatalf("thinking rounds glued: %q", thinking)
		}
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
	if !strings.Contains(status, "esc esc to stop") {
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
	for _, want := range []string{"todowrite", "· running", "☑ Slice it", "◐ Wire UI"} {
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
	for _, want := range []string{"☑ Slice it", "◐ Wire UI"} {
		if !strings.Contains(done, want) {
			t.Fatalf("finished todowrite must checklist %q:\n%s", want, done)
		}
	}

	row := m.renderToolLine(ToolRecord{Name: "todowrite", Args: args, Output: out})
	for _, want := range []string{"⏺ todowrite", "☑ Slice it", "◐ Wire UI"} {
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
	row := m.renderToolLine(ToolRecord{Name: "edit", Args: editArgs, Output: "edited foo.go"})
	for _, want := range []string{"⏺ edit", "foo.go", "- b := 2", "+ b := 3"} {
		if !strings.Contains(row, want) {
			t.Fatalf("edit row must show diff %q:\n%s", want, row)
		}
	}
	if strings.Contains(row, "oldString") {
		t.Fatalf("edit row must not dump raw JSON:\n%s", row)
	}

	readRow := m.renderToolLine(ToolRecord{Name: "read", Args: `{"path":"foo.go","offset":10,"limit":5}`, Output: "File foo.go (20 lines, showing 10-14):\n10: x"})
	if !strings.Contains(readRow, "foo.go") || strings.Contains(readRow, `"path"`) {
		t.Fatalf("read row must summarize path, not JSON:\n%s", readRow)
	}

	grepRow := m.renderToolLine(ToolRecord{Name: "grep", Args: `{"pattern":"foo.*","path":"."}`, Output: "foo.go:1: foo bar"})
	if !strings.Contains(grepRow, "foo.*") || strings.Contains(grepRow, `"pattern"`) {
		t.Fatalf("grep row must show the pattern, not JSON:\n%s", grepRow)
	}

	// Live running edit shows the same diff under the spinner header.
	m.busy = true
	m.pendingPrompt = "fix it"
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "edit", args: editArgs})
	live := m.renderPending(80)
	for _, want := range []string{"edit", "· running", "- b := 2", "+ b := 3"} {
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
	if text := m.renderViewportContent(); !strings.Contains(text, "Search...") {
		t.Fatalf("picker should render its search box:\n%s", text)
	}

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
	if m.mode != viewModels || m.modelSearch.Value() != "" || len(m.modelChoices) != 3 {
		t.Fatalf("first esc should clear search only: mode=%v query=%q choices=%v", m.mode, m.modelSearch.Value(), m.modelChoices)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Fatalf("down arrow should move the selection: cursor=%d", m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewChat {
		t.Fatalf("esc on empty search should close the picker: mode=%v", m.mode)
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
	if len(loaded.ids) != 2 || loaded.ids[0] != "glm-5.3-flash" || loaded.ids[1] != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("merged ids=%v", loaded.ids)
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
		if id == "glm-5.3-flash" {
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
	cmd := m.submit(" :q ")
	if cmd == nil {
		t.Fatal(":q must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected QuitMsg")
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
	lines := strings.Split(m.View(), "\n")
	for i, l := range lines {
		if w := lipgloss.Width(l); w > pad+maxColumnWidth {
			t.Fatalf("line %d is %d wide, exceeds column %d", i, w, pad+maxColumnWidth)
		}
	}
	// The input box top border sits at the left edge of the column.
	var found bool
	for _, l := range lines {
		if strings.HasPrefix(l, strings.Repeat(" ", pad)+"╭") {
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
		if strings.HasPrefix(l, " ╭") || strings.HasPrefix(l, "  ╭") {
			t.Fatalf("unexpected padding on narrow terminal: %q", l)
		}
	}
}
