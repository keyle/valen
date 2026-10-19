package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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

func TestTUIInitRunsInitialPrompt(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.initialPrompt = "do the thing"
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init returned nil cmd")
	}
	if m.initialPrompt != "" {
		t.Fatalf("initialPrompt not cleared: %q", m.initialPrompt)
	}
	if !m.busy || m.pendingPrompt != "do the thing" {
		t.Fatalf("initial prompt not submitted: busy=%v pending=%q", m.busy, m.pendingPrompt)
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
	cfg, err := loadConfig(writeConfig(t, "model = \"m\"\n[tui]\nmax_lines = 250\nselect_scroll_ms = 80\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiMaxLines(cfg); got != 250 {
		t.Fatalf("max_lines = %d, want 250", got)
	}
	if got := tuiSelScrollInterval(cfg); got != 80*time.Millisecond {
		t.Fatalf("select_scroll_ms = %v, want 80ms", got)
	}
	// Unset keeps the 1000-line default; explicit 0 disables trimming.
	cfg2, err := loadConfig(writeConfig(t, "model = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiMaxLines(cfg2); got != 1000 {
		t.Fatalf("default max_lines = %d, want 1000", got)
	}
	if got := tuiSelScrollInterval(cfg2); got != 50*time.Millisecond {
		t.Fatalf("default select_scroll_ms = %v, want 50ms", got)
	}
	// A non-positive cadence falls back to the default.
	cfg4, err := loadConfig(writeConfig(t, "model = \"m\"\n[tui]\nselect_scroll_ms = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiSelScrollInterval(cfg4); got != 50*time.Millisecond {
		t.Fatalf("zero select_scroll_ms = %v, want 50ms", got)
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

// TestTUITransientNoticeClearsOnTyping: informational (yellow) status
// notices restore the turns/token row on the next chatbox edit, while
// errors stay sticky.
func TestTUITransientNoticeClearsOnTyping(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.notice, m.noticeErr = "informational", false
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
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

func TestTUIEscKeepsTypedText(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.input.SetValue("half-written prompt")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.input.Value(); got != "half-written prompt" {
		t.Fatalf("esc must not clear the chatbox: %q", got)
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

// TestTUIHelpKey: ctrl+h (and the ctrl+g alias) open the help view from the
// chat, and the table documents the bindings.
func TestTUIHelpKey(t *testing.T) {
	for _, kt := range []tea.KeyType{tea.KeyCtrlH, tea.KeyCtrlG} {
		m := newTestModel(t, testConfig("https://unused"))
		mm, _ := m.Update(tea.KeyMsg{Type: kt})
		m = mm.(*tuiModel)
		if m.mode != viewHelp {
			t.Fatalf("key %v: mode=%v, want help", kt, m.mode)
		}
	}
	m := newTestModel(t, testConfig("https://unused"))
	help := m.renderHelp()
	for _, want := range []string{"ctrl+h · ctrl+g", "this help", "/compact", "PLAN (cyan chatbox)", "select_scroll_ms"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
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
