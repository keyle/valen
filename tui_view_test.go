package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sashabaranov/go-openai"
)

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
	if !strings.Contains(got, "1 turn") {
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

func TestTUISlotsView(t *testing.T) {
	m, statePath := newTestModelWithConfig(t, "model = \"glm-5.3\"\nthinking = \"high\"\nbase_url = \"https://unused\"\n")
	alt := func(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true} }

	// Store two slots: 2 ← glm-5.3/high, 3 ← kimi-k2.6.
	_, _ = m.handleKey(alt('@'))
	m.handleCommand("/model kimi-k2.6")
	_, _ = m.handleKey(alt('#'))

	// ctrl+s opens the slots view.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.mode != viewSlots {
		t.Fatalf("ctrl+s mode=%v", m.mode)
	}
	got := ansi.Strip(m.renderViewportContent())
	for _, want := range []string{"alt+1", "alt+2", "alt+3", "glm-5.3", "kimi-k2.6", "empty"} {
		if !strings.Contains(got, want) {
			t.Fatalf("slots view %q missing %q", got, want)
		}
	}

	// Enter on slot 2 switches back to its model + thinking.
	m.cursor = 1
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.cfg.Model != "glm-5.3" || m.cfg.Thinking != "high" {
		t.Fatalf("enter slot 2 → %q/%q", m.cfg.Model, m.cfg.Thinking)
	}

	// x unbinds slot 3 and persists the removal.
	m.show(viewSlots)
	m.cursor = 2
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !strings.Contains(m.notice, "Slot 3 unbound") || m.noticeErr {
		t.Fatalf("unbind notice=%q err=%v", m.notice, m.noticeErr)
	}
	if raw, _ := os.ReadFile(slotsPathFor(statePath)); string(raw) != "2: glm-5.3 high\n" {
		t.Fatalf("slots.yaml after unbind=%q", raw)
	}

	// x on an empty slot reports instead of erroring.
	m.cursor = 4
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if m.noticeErr || !strings.Contains(m.notice, "already empty") {
		t.Fatalf("empty unbind notice=%q err=%v", m.notice, m.noticeErr)
	}
}

func TestSessionsViewDeletesSelected(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	old, err := newSession(Config{SessionID: "ses-old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.append(Turn{Prompt: "old work"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	keep, err := newSession(Config{SessionID: "ses-keep"})
	if err != nil {
		t.Fatal(err)
	}
	if err := keep.append(Turn{Prompt: "keep work"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionMeta(metaPath(old.Path), SessionMeta{Title: "old work"}); err != nil {
		t.Fatal(err)
	}

	m.show(viewSessions)
	items := m.sessionItems()
	if len(items) != 2 || items[0].path != keep.Path {
		t.Fatalf("sessions=%+v", items)
	}
	m.cursor = 1
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
		t.Fatalf("selected session file must be deleted: %v", err)
	}
	if _, err := os.Stat(metaPath(old.Path)); !os.IsNotExist(err) {
		t.Fatalf("selected session meta must be deleted: %v", err)
	}
	if _, err := os.Stat(keep.Path); err != nil {
		t.Fatalf("other session must survive: %v", err)
	}
	if m.mode != viewSessions || len(m.sessionItems()) != 1 {
		t.Fatalf("list not refreshed: mode=%v n=%d", m.mode, len(m.sessionItems()))
	}
	if !strings.Contains(m.notice, "Deleted session old work") || m.noticeErr {
		t.Fatalf("notice=%q err=%v", m.notice, m.noticeErr)
	}
}

func TestSessionsViewRenamesSelected(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	s, err := newSession(Config{SessionID: "ses-rename"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(Turn{Prompt: "old work"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionMeta(metaPath(s.Path), SessionMeta{Title: "old work", Model: "openrouter/z-ai/glm-5.3", Effort: "high"}); err != nil {
		t.Fatal(err)
	}

	m.show(viewSessions)
	idx := -1
	for i, it := range m.sessionItems() {
		if it.path == s.Path {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("session missing from list: %+v", m.sessionItems())
	}
	m.cursor = idx

	// r prefills the editor with the current title.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if !m.renaming || m.sessionRename.Value() != "old work" {
		t.Fatalf("rename not open/prefilled: open=%v value=%q", m.renaming, m.sessionRename.Value())
	}
	// Esc cancels without saving.
	m.sessionRename.SetValue("discard me")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.renaming {
		t.Fatal("esc must close the editor")
	}
	if meta, _ := loadSessionMeta(metaPath(s.Path)); meta.Title != "old work" {
		t.Fatalf("esc wrote the draft: %q", meta.Title)
	}

	// Enter saves the typed title and keeps the model/effort metadata.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m.sessionRename.SetValue("")
	for _, r := range "renamed title" {
		_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// Typing must repaint the viewport, not just mutate the field.
	if !strings.Contains(m.vpContent, "renamed title") {
		t.Fatalf("typed title not rendered:\n%s", m.vpContent)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.renaming {
		t.Fatal("enter must close the editor")
	}
	meta, err := loadSessionMeta(metaPath(s.Path))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "renamed title" || meta.Model != "openrouter/z-ai/glm-5.3" || meta.Effort != "high" {
		t.Fatalf("meta after rename=%+v", meta)
	}
	if got := m.sessionItems()[idx].title; got != "renamed title" {
		t.Fatalf("list title=%q", got)
	}
	if !strings.Contains(m.notice, "Renamed session to renamed title") || m.noticeErr {
		t.Fatalf("notice=%q err=%v", m.notice, m.noticeErr)
	}
}

func TestSessionsRenameShowsTitleStartOnOpen(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 160
	s, err := newSession(Config{SessionID: "ses-long-title"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(Turn{Prompt: "long title"}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 30) + strings.Repeat("b", 30)
	if err := saveSessionMeta(metaPath(s.Path), SessionMeta{Title: long}); err != nil {
		t.Fatal(err)
	}
	m.show(viewSessions)
	m.cursor = 0
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if got := m.sessionRename.Value(); got != long {
		t.Fatalf("prefill=%q", got)
	}
	// The box is wide enough for the whole title, so opening it must show the
	// start rather than a tail scrolled by a stale narrow width.
	if text := ansi.Strip(m.renderSessionsList()); !strings.Contains(text, "aaaaaaaaaa") {
		t.Fatalf("title start missing on open:\n%s", text)
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
	m.orMeta = &ORMeta{Provider: "Infermatic", TTFT: 4.9, TPS: 42.4}
	m.orPricing = map[string]orPricing{
		"openrouter/moonshotai/kimi-k3": {prompt: 0.0000001, completion: 0.0000004},
	}
	m.orSpent, m.orSpentSet = 0.0034, true
	got := ansi.Strip(m.footerView())
	for _, want := range []string{"O.R. $12.34", "Infermatic", "4.9s TTFT / 42 TPS", "$ 0.10/0.40", "$0.0034 used"} {
		if !strings.Contains(got, want) {
			t.Fatalf("footer %q missing %q", got, want)
		}
	}
	if status := ansi.Strip(m.statusView()); strings.Contains(status, "$ 0.10/0.40") {
		t.Fatalf("rates belong in the footer, not the status row: %q", status)
	}
	// A notice takes the row: totals hide until it clears.
	m.notice = "thinking · high"
	if status := ansi.Strip(m.statusView()); strings.Contains(status, "turns") {
		t.Fatalf("notice must hide totals: %q", status)
	}
	m.notice = ""

	// Segments drop independently: no provider, no catalog rates.
	m.orMeta = &ORMeta{TPS: 42.4}
	m.orPricing = nil
	got = ansi.Strip(m.footerView())
	if strings.Contains(got, "Infermatic") || strings.Contains(got, "$ 0.10") {
		t.Fatalf("missing metadata must drop: %q", got)
	}
	if !strings.Contains(got, "42 TPS") {
		t.Fatalf("tps should survive: %q", got)
	}

	// Nothing to add leaves the plain balance.
	m.orMeta = nil
	if got := ansi.Strip(m.footerView()); !strings.Contains(got, "O.R.") || strings.Contains(got, "TPS") {
		t.Fatalf("empty metadata footer=%q", got)
	}

	// Narrow terminals yield the whole suffix via the width rule.
	m.width, m.viewport.Width = 24, 24
	m.orMeta = &ORMeta{Provider: "Infermatic", TPS: 42}
	m.orPricing = map[string]orPricing{"openrouter/moonshotai/kimi-k3": {prompt: 0.0000001, completion: 0.0000004}}
	if got := ansi.Strip(m.footerView()); strings.Contains(got, "Infermatic") || strings.Contains(got, "$ 0.10") || strings.Contains(got, "used") {
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
	if got := ansi.Strip(m.footerView()); !strings.Contains(got, "$0.0022 used") {
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

// TestRoutingFooterRestoredOnResume: the footer's OpenRouter details (last
// turn's provider/TTFT/TPS, cumulative spend, and catalog rates) are persisted
// with the session and restored when it is resumed.
func TestRoutingFooterRestoredOnResume(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.width, m.viewport.Width = 200, 200
	m.session.Turns = nil
	m.orPricing = map[string]orPricing{
		m.cfg.Model: {prompt: 0.0000001, completion: 0.0000004},
	}
	meta := &ORMeta{Provider: "Infermatic", TTFT: 4.9, TPS: 42.4, Cost: 0.0012, HasCost: true}
	_, _ = m.Update(turnDoneMsg{
		prompt: "a", answer: "1", history: []openai.ChatCompletionMessage{},
		model: m.cfg.Model, orMeta: meta, turnCost: 0.0012, hasTurnCost: true,
	})
	path := m.session.Path

	m.orMeta = nil
	m.orSpent, m.orSpentSet = 0, false
	m.orPricing = nil
	m.session.Turns = nil
	m.resumeSessionPath(path)

	for _, want := range []string{"Infermatic", "4.9s TTFT / 42 TPS", "$ 0.10/0.40", "$0.0012 used"} {
		if got := ansi.Strip(m.footerView()); !strings.Contains(got, want) {
			t.Fatalf("resumed footer %q missing %q", got, want)
		}
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
	if !strings.Contains(got, "$0.50 used") {
		t.Fatalf("footer %q missing session cost without balance", got)
	}
	if strings.Contains(got, "O.R.") {
		t.Fatalf("no balance must mean no left segment: %q", got)
	}
}
