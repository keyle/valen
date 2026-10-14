package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func selectionModel(t *testing.T, w, h int, content string) *tuiModel {
	t.Helper()
	m := newTestModel(t, testConfig("https://unused"))
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.viewport.SetContent(content)
	m.vpContent = content
	m.viewport.GotoTop()
	return m
}

// A drag across a full width-padded line yields the logical text: no leading
// border/indent, no trailing padding (the reported bug).
func TestTUISelectionTrimsPadding(t *testing.T) {
	m := selectionModel(t, 200, 20, "┃ hello world\n\nsecond line here")
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 2}, cursor: selPos{line: 0, col: 120}}
	if got := m.selectedText(); got != "hello world" {
		t.Fatalf("selectedText = %q, want %q", got, "hello world")
	}
}

func TestTUISelectionMultiLine(t *testing.T) {
	m := selectionModel(t, 80, 20, "┃ hello world\n\nsecond line here")
	// From the first content cell of row 0 through "second" on row 2.
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 2}, cursor: selPos{line: 2, col: 5}}
	if got := m.selectedText(); got != "hello world\n\nsecond" {
		t.Fatalf("selectedText = %q", got)
	}
}

// A drag across a rendered code sample keeps its relative indentation: the
// shared block margin is stripped, but the code's own leading spaces survive
// (the reported bug collapsed every line to its first glyph).
func TestTUISelectionKeepsCodeIndent(t *testing.T) {
	content := "  intro\n\n    func main() {\n        if x {\n            y()\n        }\n    }\n  outro"
	m := selectionModel(t, 80, 20, content)
	// From "func" (row 2, content col 4) through "}" (row 4... row 6).
	m.sel = selection{active: true, anchor: selPos{line: 2, col: 4}, cursor: selPos{line: 6, col: 4}}
	want := "func main() {\n    if x {\n        y()\n    }\n}"
	if got := m.selectedText(); got != want {
		t.Fatalf("selectedText =\n%q\nwant\n%q", got, want)
	}
}

// A selection that mixes a bordered line with un-bordered ones must not leak
// the border glyph, and must keep the un-bordered lines' indentation relative
// to the block margin rather than to the border's content column.
func TestTUISelectionNoBorderLeakOnMixed(t *testing.T) {
	content := "┃ prompt\n\n    func x() {\n        y()\n    }"
	m := selectionModel(t, 80, 20, content)
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 2}, cursor: selPos{line: 4, col: 4}}
	got := m.selectedText()
	if strings.Contains(got, "┃") {
		t.Fatalf("border glyph leaked into copy: %q", got)
	}
	want := "prompt\n\nfunc x() {\n    y()\n}"
	if got != want {
		t.Fatalf("selectedText =\n%q\nwant\n%q", got, want)
	}
}
func TestTUISelectionIgnoresShiftDrag(t *testing.T) {
	old := clipboardWrite
	called := ""
	clipboardWrite = func(s string) { called = s }
	t.Cleanup(func() { clipboardWrite = old })

	m := selectionModel(t, 80, 20, "┃ hello world\nsecond line")
	press := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Shift: true, X: 2, Y: 1}
	motion := tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, Shift: true, X: 9, Y: 1}
	release := tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, Shift: true, X: 9, Y: 1}
	for _, msg := range []tea.MouseMsg{press, motion, release} {
		m.Update(msg)
	}
	if m.sel.active {
		t.Fatal("shift+drag must not start a selection")
	}
	if called != "" {
		t.Fatalf("shift+drag copied: %q", called)
	}
}

func TestTUISelectionDragCopies(t *testing.T) {
	old := clipboardWrite
	got := ""
	clipboardWrite = func(s string) { got = s }
	t.Cleanup(func() { clipboardWrite = old })

	m := selectionModel(t, 80, 20, "┃ hello world\nsecond line")
	press := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 1}
	motion := tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 12, Y: 1}
	release := tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 12, Y: 1}
	m.Update(press)
	if !m.sel.active {
		t.Fatal("plain press must start a selection")
	}
	m.Update(motion)
	m.Update(release)
	if got != "hello world" {
		t.Fatalf("release copied %q, want %q", got, "hello world")
	}
	if m.sel.active {
		t.Fatal("release must clear the selection")
	}
	if !strings.Contains(m.notice, "Copied 11 bytes to clipboard") {
		t.Fatalf("status notice = %q", m.notice)
	}
}

func TestTUISelectionClearsOnScroll(t *testing.T) {
	m := selectionModel(t, 80, 20, "┃ hello world\nsecond line")
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 2}, cursor: selPos{line: 0, col: 5}}
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.sel.active {
		t.Fatal("wheel must clear the selection")
	}
}

func TestTUISelectionHighlight(t *testing.T) {
	m := selectionModel(t, 80, 20, "┃ hello world\nsecond line")
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 2}, cursor: selPos{line: 0, col: 5}}
	out := m.View()
	if !strings.Contains(out, "\x1b[7m") || !strings.Contains(out, "\x1b[27m") {
		t.Fatalf("highlight missing reverse-video codes:\n%q", out)
	}
	// The highlighted row keeps its border: reverse video composes, it does
	// not replace the surrounding styling.
	if !strings.Contains(ansi.Strip(out), "┃ hello world") {
		t.Fatal("highlight altered the underlying text")
	}
}

// A syntax-highlighted line emits a reset after every coloured span; the
// reverse video must be re-asserted after each one or only the first word
// stays highlighted (the reported bug).
func TestTUISelectionHighlightReassertsAfterReset(t *testing.T) {
	styled := "\x1b[38;5;1mhello\x1b[0m \x1b[38;5;2mworld\x1b[0m"
	out := reverseRange(styled, 0, len("hello world"))
	if !strings.Contains(out, "\x1b[0m\x1b[7m") {
		t.Fatalf("reverse not re-asserted after a reset:\n%q", out)
	}
	if n := strings.Count(out, "\x1b[7m"); n < 3 {
		t.Fatalf("reverse asserted only %d times; spans after the first are dark:\n%q", n, out)
	}
	// The plain text is untouched.
	if got := ansi.Strip(out); got != "hello world" {
		t.Fatalf("stripped = %q", got)
	}
}

func TestTUISelectionAutoScrollStep(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	m := selectionModel(t, 80, 12, strings.Join(lines, "\n"))
	h := m.viewport.Height
	m.viewport.GotoTop()
	m.sel = selection{active: true, anchor: selPos{line: 2, col: 0}, cursor: selPos{line: h - 1, col: 3}}
	if !m.selAtScrollEdge() {
		t.Fatalf("bottom edge must trigger auto-scroll (height=%d)", h)
	}
	before := m.viewport.YOffset
	if !m.selScrollStep() {
		t.Fatal("selScrollStep must scroll down at the bottom edge")
	}
	if m.viewport.YOffset != before+1 {
		t.Fatalf("YOffset = %d, want %d", m.viewport.YOffset, before+1)
	}
	if m.sel.anchor.line != 2 {
		t.Fatalf("anchor.line = %d, want 2 (pinned to the same content)", m.sel.anchor.line)
	}
	if want := m.viewport.YOffset + h - 1; m.sel.cursor.line != want {
		t.Fatalf("cursor.line = %d, want %d", m.sel.cursor.line, want)
	}
}

func TestTUISelectionSurvivesAutoScroll(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	m := selectionModel(t, 80, 12, strings.Join(lines, "\n"))
	h := m.viewport.Height
	m.viewport.GotoTop()
	// Anchor on the first content line, cursor on the bottom visible row.
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 0}, cursor: selPos{line: h - 1, col: 7}}
	for i := 0; i < 3; i++ {
		if !m.selScrollStep() {
			t.Fatalf("selScrollStep %d failed", i)
		}
	}
	if m.viewport.YOffset != 3 {
		t.Fatalf("YOffset = %d, want 3", m.viewport.YOffset)
	}
	// The content that scrolled off the top must still be in the copy: the
	// selection runs from line 00 to the revealed bottom row (the reported
	// bug dropped the lines that scrolled off).
	end := 3 + h - 1
	wantEnd := fmt.Sprintf("line %02d", end)
	got := m.selectedText()
	if !strings.HasPrefix(got, "line 00") {
		t.Fatalf("top content lost after scrolling: %q", got)
	}
	if !strings.HasSuffix(got, wantEnd) {
		t.Fatalf("selection end = %q, want it to reach %q", got, wantEnd)
	}
	if n := strings.Count(got, "\n"); n != end {
		t.Fatalf("selection has %d lines, want %d:\n%s", n+1, end+1, got)
	}
}

// After auto-scroll, highlight must follow the offset: every visible row is
// inside the selection and stays lit even though the anchor is off-screen.
func TestTUISelectionHighlightAfterScroll(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	m := selectionModel(t, 80, 12, strings.Join(lines, "\n"))
	h := m.viewport.Height
	m.viewport.GotoTop()
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 0}, cursor: selPos{line: h - 1, col: 7}}
	for i := 0; i < 3; i++ {
		if !m.selScrollStep() {
			t.Fatalf("selScrollStep %d failed", i)
		}
	}
	out := m.View()
	if n := strings.Count(out, "\x1b[7m"); n < h {
		t.Fatalf("highlight on %d rows, want at least %d:\n%q", n, h, out)
	}
	if !strings.Contains(ansi.Strip(out), "line 03") {
		t.Fatal("highlight altered the scrolled content")
	}
}

func TestTUISelectionAutoScrollStopsAtEnd(t *testing.T) {
	m := selectionModel(t, 80, 12, "one\ntwo\nthree")
	h := m.viewport.Height
	m.viewport.GotoBottom()
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 0}, cursor: selPos{line: h - 1, col: 0}}
	if m.selScrollStep() {
		t.Fatal("selScrollStep must not scroll past the end")
	}
}

func TestTUISelectionAutoScrollArmsAndClears(t *testing.T) {
	m := selectionModel(t, 80, 12, strings.Repeat("x\n", 200))
	h := m.viewport.Height
	m.viewport.GotoTop()
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 0}, cursor: selPos{line: h - 1, col: 0}}
	if cmd := m.scheduleSelScroll(); cmd == nil {
		t.Fatal("scheduleSelScroll must arm the loop at the edge")
	}
	if !m.selScrollTicking {
		t.Fatal("loop must be marked ticking")
	}
	// A second call while running must not spawn a duplicate ticker.
	if cmd := m.scheduleSelScroll(); cmd != nil {
		t.Fatal("scheduleSelScroll must be a no-op while ticking")
	}
	m.clearSelection()
	if m.selScrollTicking {
		t.Fatal("clearSelection must stop the loop")
	}
}

func TestTUISelectionAutoScrollTickExtends(t *testing.T) {
	m := selectionModel(t, 80, 12, strings.Repeat("x\n", 200))
	h := m.viewport.Height
	m.viewport.GotoTop()
	m.sel = selection{active: true, anchor: selPos{line: 0, col: 0}, cursor: selPos{line: h - 1, col: 0}}
	m.selScrollTicking = true
	before := m.viewport.YOffset
	_, cmd := m.Update(selScrollMsg{})
	if cmd == nil {
		t.Fatal("an active edge tick must reschedule itself")
	}
	if m.viewport.YOffset != before+1 {
		t.Fatalf("tick did not scroll: YOffset = %d, want %d", m.viewport.YOffset, before+1)
	}
}

func TestTUISelectionDragArmsAutoScroll(t *testing.T) {
	m := selectionModel(t, 80, 12, strings.Repeat("x\n", 200))
	m.viewport.GotoTop()
	bottomY := m.viewport.Height // viewport origin is screen row 1
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 2})
	_, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 2, Y: bottomY})
	if cmd == nil {
		t.Fatal("dragging to the bottom edge must arm auto-scroll")
	}
}

func TestTUILinkSpanParsing(t *testing.T) {
	gl := buildGridLine("see https://example.com/x now.")
	spans := linkSpans(gl)
	if len(spans) != 1 || spans[0].url != "https://example.com/x" {
		t.Fatalf("spans = %+v", spans)
	}
	// Trailing punctuation is trimmed from the URL.
	if gl2 := buildGridLine("(https://ex.com/a)."); linkSpans(gl2)[0].url != "https://ex.com/a" {
		t.Fatalf("punctuation not trimmed: %+v", linkSpans(gl2))
	}
}

func TestTUILinkCtrlClickOpens(t *testing.T) {
	var opened []string
	old := openURL
	openURL = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openURL = old })

	m := selectionModel(t, 80, 20, "docs https://example.com/x now")
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Ctrl: true, X: 6, Y: 1})
	if len(opened) != 1 || opened[0] != "https://example.com/x" {
		t.Fatalf("ctrl+click opened %v", opened)
	}
	if m.sel.active {
		t.Fatal("ctrl+click must not start a selection")
	}
}

func TestTUILinkCtrlClickNonLinkNoop(t *testing.T) {
	var opened []string
	old := openURL
	openURL = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openURL = old })

	m := selectionModel(t, 80, 20, "docs https://example.com/x now")
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Ctrl: true, X: 1, Y: 1})
	if len(opened) != 0 {
		t.Fatalf("non-link ctrl+click opened %v", opened)
	}
}

func TestTUILinkRejectsNonHTTP(t *testing.T) {
	gl := buildGridLine("ftp://example.com/file")
	if spans := linkSpans(gl); len(spans) != 0 {
		t.Fatalf("non-http link matched: %+v", spans)
	}
}
