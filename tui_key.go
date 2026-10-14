package main

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// insertInputNewline adds a line break without submitting, shared by the
// modifier-newline keys (shift+enter / alt+enter / ctrl+j).
func (m *tuiModel) insertInputNewline() {
	m.input.InsertRune('\n')
	m.dismissTransientNotice()
}

// parseEnterCSI matches the byte sequences terminals send for modifier+enter
// once a disambiguating mode is on: Kitty's CSI 13;mod u and xterm
// modifyOtherKeys' CSI 13;mod ~. mod follows the CSI-u convention
// (1 none, 2 shift, 3 alt, 4 shift+alt, 5 ctrl, …); absent means 1.
func parseEnterCSI(seq []byte) (mod int, ok bool) {
	// ESC [ 1 3 [; mod] (u | ~)
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[2] != '1' || seq[3] != '3' {
		return 0, false
	}
	rest := seq[4:]
	if len(rest) == 1 && (rest[0] == 'u' || rest[0] == '~') {
		return 1, true
	}
	if len(rest) >= 3 && rest[0] == ';' && (rest[len(rest)-1] == 'u' || rest[len(rest)-1] == '~') {
		n := 0
		for _, c := range rest[1 : len(rest)-1] {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		if n >= 1 && n <= 16 {
			return n, true
		}
	}
	return 0, false
}

// ctrlMCSI reports whether msg carries a disambiguated ctrl+m letter
// sequence (CSI 109;5u or CSI 109;5~). Like enterCSIMod, the message is an
// unexported unknown-CSI []byte-kind value, so kind+content matching is
// precise without naming that type. Only the ctrl form (mod 5) counts: a
// bare CSI 109u would be a plain "m" keystroke, which must keep typing.
// Byte-13 ctrl+m can never arrive here — legacy terminals collapse it into
// plain enter by design — so there is deliberately no handleKey "ctrl+m"
// case (bubbletea aliases KeyCtrlM to KeyEnter and stringifies it "enter").
func ctrlMCSI(msg tea.Msg) bool {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return false
	}
	seq := v.Bytes()
	// ESC [ 1 0 9 ; 5 (u | ~), exact length: longer runs are not ours.
	if len(seq) != 8 {
		return false
	}
	if seq[0] != 0x1b || seq[1] != '[' || seq[2] != '1' || seq[3] != '0' || seq[4] != '9' || seq[5] != ';' || seq[6] != '5' {
		return false
	}
	return seq[7] == 'u' || seq[7] == '~'
}

// csiUKey decodes the Kitty "disambiguate" reports bubbletea v1 leaves as
// unknown CSI: ESC[27u (esc), ESC[<letter>;5u (ctrl+letter) and
// ESC[<char>;3u (alt+char). Anything else (enter, ctrl+m, unmodified
// text) is left to its own handler. Lock bits (caps/num) are ignored.
func csiUKey(msg tea.Msg) (tea.KeyMsg, bool) {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return tea.KeyMsg{}, false
	}
	seq := string(v.Bytes())
	if len(seq) < 4 || !strings.HasPrefix(seq, "\x1b[") || seq[len(seq)-1] != 'u' {
		return tea.KeyMsg{}, false
	}
	cpStr, modStr, _ := strings.Cut(seq[2:len(seq)-1], ";")
	cp, err := strconv.Atoi(cpStr)
	if err != nil {
		return tea.KeyMsg{}, false
	}
	mod := 1
	if modStr != "" {
		if mod, err = strconv.Atoi(modStr); err != nil || mod < 1 {
			return tea.KeyMsg{}, false
		}
	}
	switch bits := (mod - 1) & 0x3f; {
	case cp == 27 && bits == 0:
		return tea.KeyMsg{Type: tea.KeyEsc}, true
	case bits == 4 && cp >= 'a' && cp <= 'z' && cp != 'm':
		return tea.KeyMsg{Type: tea.KeyType(cp - 'a' + 1)}, true
	case bits == 2 && cp > 32 && cp < 127:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(cp)}, Alt: true}, true
	case bits == 3 && cp >= '0' && cp <= '9':
		// alt+shift+digit: Kitty reports the unshifted key plus the shift
		// bit; legacy terminals send the US shifted symbol (alt+!), so match.
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(slotShifted[strings.IndexRune(slotKeys, rune(cp))])}, Alt: true}, true
	}
	return tea.KeyMsg{}, false
}

// enterCSIMod reports the CSI-u/modifyOtherKeys enter modifier for a message
// carrying one of the parseEnterCSI sequences. Bubbletea reports those as an
// unexported unknown-CSI message — the only []byte-kind message on the loop —
// so kind+content matching is precise without naming that type.
func enterCSIMod(msg tea.Msg) (int, bool) {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return 0, false
	}
	return parseEnterCSI(v.Bytes())
}

// Split SGR mouse reports (ESC[<Cb;Cx;CyM, one per wheel tick) can land
// as KeyRunes when a report straddles two TTY reads; genuine wheel input
// still arrives as tea.MouseMsg and is untouched (see Update). Fragments
// are printable ([, <, digits, ;, M) so no content blacklist can catch
// them without also eating typing — instead, ambiguous fragments are
// held briefly and reassembled: a completed report is dropped, anything
// else is released as typing. Pasted text never participates: pastes
// arrive with Paste set and bypass the hold entirely.
//
// sgrReportComplete matches one reassembled report (button/cell/release);
// sgrReportPrefix matches its strict prefixes, the only shapes worth
// holding. Anything else (digits without "<", stray M, overlong runs)
// can never become a report and types immediately.
var (
	sgrReportCompleteRe = regexp.MustCompile(`^\[?<\d+;\d+;\d+[Mm]$`)
	sgrReportPrefixRe   = regexp.MustCompile(`^\[?$|^\[?<\d{0,6};?\d{0,6};?\d{0,6}$`)
)

// mouseHoldDelay bounds how long ambiguous fragments stay invisible while
// waiting for the rest of a possibly split report.
const mouseHoldDelay = 50 * time.Millisecond

// mouseHoldCap bounds the hold buffer: a real report is under 20 bytes,
// so anything longer is typing, not mouse.
const mouseHoldCap = 24

// mouseHoldFlushMsg releases held fragments as typing when no report
// completed in time. gen drops stale timers after newer holds.
type mouseHoldFlushMsg struct {
	gen int
}

// mouseFragmentText reports whether msg could be part of a split SGR
// report, returning its ESC-stripped text. Only plain single-stream
// runes qualify: pastes, alt combos (except the lone "[" left when ESC
// splits off), and special keys never do.
func mouseFragmentText(msg tea.KeyMsg) (string, bool) {
	if msg.Paste || msg.Type != tea.KeyRunes || len(msg.Runes) == 0 {
		return "", false
	}
	if msg.Alt {
		if len(msg.Runes) == 1 && msg.Runes[0] == '[' {
			return "[", true
		}
		return "", false
	}
	for _, r := range msg.Runes {
		switch r {
		case '[', '<', ';', 'M', 'm', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			return "", false
		}
	}
	return string(msg.Runes), true
}

func (m *tuiModel) mouseHoldText() string {
	var b strings.Builder
	for _, h := range m.mouseHold {
		if t, ok := mouseFragmentText(h); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

// flushMouseHold releases held fragments to their sink, replaying each
// exactly as the normal path would: the chat textarea in viewChat, the
// ask free-text box in its own phase. Overlays and pick/review ignore
// typing, so held runes there are dropped like typed ones would be.
func (m *tuiModel) flushMouseHold() {
	held := m.mouseHold
	m.mouseHold = nil
	if len(held) == 0 {
		return
	}
	if m.ask != nil && m.ask.phase == "own" {
		for _, h := range held {
			if s := h.String(); len(s) == 1 {
				m.ask.own += s
			}
		}
		return
	}
	if m.mode == viewChat && m.ask == nil {
		before := m.input.Value()
		for _, h := range held {
			m.input, _ = m.input.Update(h)
		}
		if m.input.Value() != before {
			m.dismissTransientNotice()
		}
	}
}

// filterMouseFragment holds a possible split-report fragment instead of
// typing it. It reports consumed=true when the key was held or dropped
// (caller returns); consumed=false means the hold was flushed (or empty)
// and the key must flow through the normal path.
func (m *tuiModel) filterMouseFragment(msg tea.KeyMsg) (tea.Cmd, bool) {
	frag, ok := mouseFragmentText(msg)
	if !ok {
		m.flushMouseHold()
		return nil, false
	}
	combined := m.mouseHoldText() + frag
	if sgrReportCompleteRe.MatchString(combined) {
		// A split report reassembled: drop it all, it was never typed.
		m.mouseHold = nil
		m.mouseHoldGen++
		return nil, true
	}
	if len(combined) > mouseHoldCap || !sgrReportPrefixRe.MatchString(combined) {
		// Can never become a mouse report: release as typing now.
		m.flushMouseHold()
		return nil, false
	}
	m.mouseHold = append(m.mouseHold, msg)
	m.mouseHoldGen++
	gen := m.mouseHoldGen
	return tea.Tick(mouseHoldDelay, func(time.Time) tea.Msg {
		return mouseHoldFlushMsg{gen: gen}
	}), true
}

func (m *tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Possible split wheel-mouse reports are held for reassembly, never
	// typed: only pasted or typed chars may reach the input box. Genuine
	// wheel input arrives as tea.MouseMsg (see Update).
	if cmd, consumed := m.filterMouseFragment(msg); consumed {
		return m, cmd
	}
	s := msg.String()

	// Global: cancel / quit.
	switch s {
	case "ctrl+c", "ctrl+d":
		if m.ask != nil {
			m.dismissAsk(errAskDismissed)
			return m, nil
		}
		if m.busy {
			m.cancelRun()
			if s == "ctrl+c" {
				m.notice = "Cancelling…"
				m.noticeErr = false
			}
			return m, nil
		}
		// Idle: first press clears a non-empty chatbox, next one quits.
		if m.input.Value() != "" {
			m.clearInput()
			return m, nil
		}
		return m, tea.Quit
	case "esc":
		if m.ask != nil {
			return m.handleAskKey(s)
		}
		if m.mode == viewModels {
			if m.modelSearch.Focused() {
				m.modelSearch.Blur() // keep the query, just leave the field
				return m, nil
			}
			m.show(viewChat)
			return m, nil
		}
		if m.mode != viewChat {
			// Overlays go back to chat; never arm the cancel confirm.
			m.show(viewChat)
			return m, nil
		}
		if m.busy {
			now := time.Now()
			if !m.escArmedAt.IsZero() && now.Sub(m.escArmedAt) <= escConfirmWindow {
				m.escArmedAt = time.Time{}
				m.cancelRun()
				m.notice = "Cancelling…"
				m.noticeErr = false
				return m, nil
			}
			m.escArmedAt = now
			m.notice = "Press esc again within 2s to stop."
			m.noticeErr = false
			return m, tea.Tick(escConfirmWindow, func(time.Time) tea.Msg { return escExpireMsg{at: now} })
		}
		if m.mode != viewChat {
			m.show(viewChat)
			return m, nil
		}
		if m.input.Value() != "" {
			m.clearInput()
			return m, nil
		}
		return m, nil
	}

	// Ctrl shortcuts work everywhere; /help lists them.
	if m.ask != nil {
		return m.handleAskKey(s)
	}
	if key, assign, ok := slotKey(s); ok {
		if assign {
			m.assignSlot(key)
			return m, nil
		}
		return m, m.applySlot(key)
	}
	switch s {
	case "alt+m":
		// Selection toggle: reporting on (default) keeps wheel-scroll
		// with bypass-modifier drag-select; off disables terminal
		// reporting so a plain drag selects + copies natively.
		m.mouseOff = !m.mouseOff
		if m.mouseOff {
			m.notice = "Mouse off — drag selects + copies natively (wheel paused) · alt+m re-enables"
			m.noticeErr = false
			return m, tea.DisableMouse
		}
		m.notice = "Mouse on — wheel scrolls · shift+drag (opt+drag in iTerm/Terminal) selects + copies"
		m.noticeErr = false
		return m, tea.EnableMouseCellMotion
	case "ctrl+g":
		m.show(viewHelp)
		return m, nil
	case "ctrl+r":
		m.show(viewHistory)
		return m, nil
	case "ctrl+l":
		m.show(viewSessions)
		return m, nil
	case "ctrl+u":
		m.show(viewTools)
		return m, nil
	case "ctrl+x":
		m.show(viewErrors)
		return m, nil
	case "ctrl+y":
		m.cycleRouting()
		return m, nil
	case "ctrl+o":
		m.showThink = !m.showThink
		m.refreshViewport()
		return m, nil
	case "ctrl+b":
		m.showMeta = !m.showMeta
		return m, nil
	case "ctrl+p":
		return m, m.openModelPicker("")
	case "ctrl+s":
		text, isErr := m.exportTranscript()
		m.notice = text
		m.noticeErr = isErr
		return m, nil
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+n":
		// Fresh blank session; blocked while busy (see startFreshSession).
		// Shadows textarea's down-history (unused: no history recall here).
		m.startFreshSession()
		return m, nil
	case "ctrl+t":
		// ctrl+t shadows textarea transpose (rarely used); it cycles the
		// reasoning level low→high and wraps to low.
		m.cycleThinking()
		return m, nil
	}

	if m.ask != nil {
		return m.handleAskKey(s)
	}
	if m.mode != viewChat {
		return m.handleOverlayKey(msg, s)
	}

	switch s {
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		if m.busy {
			m.notice = "Working… press esc twice to cancel."
			m.noticeErr = false
			return m, nil
		}
		m.input.SetValue("")
		cmd := m.submit(text) // consumes m.retrying
		m.retrying = false
		if cmd == nil {
			return m, nil
		}
		// Keep the spinner ticking while the turn runs.
		return m, tea.Batch(cmd, m.spinner.Tick)
	case "tab":
		// Tab swaps work and plan mode, same as /plan. The textarea has no
		// use for it (no tab character in prompts).
		m.applyPlanMode(!m.cfg.Plan)
		return m, nil
	case "shift+enter", "alt+enter", "ctrl+j":
		// "shift+enter" only arrives from terminals that report it as a
		// distinct key; most send plain enter unless Kitty enhancement is
		// on (pushed in runTUI, decoded above as CSI 13;2u). alt+enter and
		// ctrl+j are the bindings that work everywhere.
		m.insertInputNewline()
		return m, nil
	case "pgup", "pgdown", "ctrl+d":
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		m.follow = m.viewport.AtBottom()
		return m, vcmd
	case "up", "down":
		// Let the textarea move first; viewport scrolls with pgup/pgdn.
		// On the first/last visual row there is nowhere to go, so jump to
		// the start/end of the text instead (shell-style).
		if s == "up" && m.input.Value() == "" && !m.busy && m.recallLastPrompt() {
			return m, nil
		}
		li := m.input.LineInfo()
		if s == "up" && m.input.Line() == 0 && li.RowOffset == 0 {
			m.input.SetCursor(0)
			return m, nil
		}
		if s == "down" && m.input.Line() == m.input.LineCount()-1 && li.RowOffset >= li.Height-1 {
			m.input.CursorEnd()
			return m, nil
		}
		var icmd tea.Cmd
		before := m.input.Value()
		m.input, icmd = m.input.Update(msg)
		if m.input.Value() != before {
			m.dismissTransientNotice()
		}
		return m, icmd
	default:
		var icmd tea.Cmd
		before := m.input.Value()
		m.input, icmd = m.input.Update(msg)
		if m.input.Value() != before {
			m.dismissTransientNotice()
		}
		if m.input.Value() == "" {
			m.retrying = false
		}
		return m, icmd
	}
}

// ensureCursorVisible scrolls the overlay viewport just enough to keep the
// cursor row on screen. Regular lists put a two-line title above their rows;
// the model picker adds its search box and uses cursor+4 instead.
func (m *tuiModel) ensureCursorVisible() {
	line := m.cursor + 2
	if m.mode == viewModels {
		line = m.cursor + 4
	}
	if line < m.viewport.YOffset {
		m.viewport.SetYOffset(line)
	} else if bottom := m.viewport.YOffset + m.viewport.Height - 1; line > bottom {
		m.viewport.SetYOffset(line - m.viewport.Height + 1)
	}
}

func (m *tuiModel) handleOverlayKey(msg tea.KeyMsg, s string) (tea.Model, tea.Cmd) {
	if m.mode == viewModels {
		focused := m.modelSearch.Focused()
		if !focused {
			switch s {
			case "j":
				s = "down"
			case "k":
				s = "up"
			case " ":
				m.toggleFavourite()
				return m, nil
			}
		}
		switch s {
		case "up":
			if m.cursor > 0 {
				m.cursor--
				m.refreshViewport()
				m.ensureCursorVisible()
			}
			return m, nil
		case "down":
			if m.cursor < len(m.modelChoices)-1 {
				m.cursor++
				m.refreshViewport()
				m.ensureCursorVisible()
			}
			return m, nil
		case "enter":
			return m, m.openSelected()
		case "ctrl+f":
			m.modelSearch.Focus()
			return m, nil
		case "pgup", "pgdown":
			if s == "pgup" {
				m.viewport.PageUp()
			} else {
				m.viewport.PageDown()
			}
			return m, nil
		}
		if !focused {
			return m, nil
		}
		old := m.modelSearch.Value()
		var cmd tea.Cmd
		m.modelSearch, cmd = m.modelSearch.Update(msg)
		if next := m.modelSearch.Value(); next != old {
			m.modelFilter = next
			m.filterModelChoices()
			m.cursor = 0
			m.refreshViewport()
			m.viewport.GotoTop()
		}
		return m, cmd
	}
	switch s {
	case "q":
		m.show(viewChat)
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.refreshViewport()
			m.ensureCursorVisible()
		}
		return m, nil
	case "down", "j":
		m.cursor++
		m.refreshViewport()
		m.ensureCursorVisible()
		return m, nil
	case "enter", " ":
		return m, m.openSelected()
	case "pgup", "pgdown", "ctrl+d":
		// Paging scrolls the viewport; cursor moves with the arrow keys or j/k.
		if s == "pgup" || s == "ctrl+u" {
			m.viewport.PageUp()
		} else {
			m.viewport.PageDown()
		}
		return m, nil
	default:
		// j/k handled above; ignore typing in overlays.
		return m, nil
	}
}
