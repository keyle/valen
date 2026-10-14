package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// selPos is a point in the visible transcript: line is the viewport row
// (0 = top of m.viewport.View()), col is a display cell (0-based).
type selPos struct{ line, col int }

// selection is the in-app drag selection over the chat transcript.
type selection struct {
	active bool
	anchor selPos
	cursor selPos
}

// gridCell is one grapheme cluster of a stripped viewport line, with the
// display cell it starts at and its width.
type gridCell struct {
	text string
	col  int
	w    int
}

// gridLine is a viewport line's plain content plus the cell bounds of that
// content: leading border/padding and trailing width padding are decoration,
// not content, and never enter a copy.
type gridLine struct {
	cells  []gridCell
	width  int
	cStart int
	cEnd   int
}

// borderRunes are the left-edge box-drawing glyphs a lipgloss border emits.
const borderRunes = "│┃┆┊┋╎╏▏▕|"

func isBorderGlyph(c gridCell) bool {
	r := []rune(c.text)
	return len(r) > 0 && strings.ContainsRune(borderRunes, r[0])
}

func isSpaceCell(c gridCell) bool {
	return c.text != "" && strings.TrimFunc(c.text, unicode.IsSpace) == ""
}

// buildGridLine strips ANSI and maps each grapheme to its display cell.
func buildGridLine(styled string) gridLine {
	plain := ansi.Strip(styled)
	var gl gridLine
	col := 0
	var state byte
	for i := 0; i < len(plain); {
		seq, w, n, ns := ansi.DecodeSequence(plain[i:], state, nil)
		state = ns
		if n <= 0 {
			break
		}
		gl.cells = append(gl.cells, gridCell{text: string(seq), col: col, w: w})
		col += w
		i += n
	}
	gl.width = col
	gl.cStart, gl.cEnd = gridContent(gl.cells, col)
	return gl
}

// gridContent returns the cell bounds [start, end) of a line's content,
// skipping the leading run of border glyphs and spaces and the trailing run of
// spaces. An all-decoration line yields an empty range (start > end).
func gridContent(cells []gridCell, width int) (int, int) {
	start := 0
	for start < len(cells) && (isSpaceCell(cells[start]) || isBorderGlyph(cells[start])) {
		start++
	}
	end := len(cells)
	for end > start && isSpaceCell(cells[end-1]) {
		end--
	}
	if start >= end {
		return width, 0
	}
	return cells[start].col, cells[end-1].col + cells[end-1].w
}

// visibleGrid is the stripped, cell-indexed form of the current viewport.
func (m *tuiModel) visibleGrid() []gridLine {
	lines := strings.Split(m.viewport.View(), "\n")
	grid := make([]gridLine, len(lines))
	for i, l := range lines {
		grid[i] = buildGridLine(l)
	}
	return grid
}

// selOrder returns the selection endpoints top-left first.
func selOrder(a, b selPos) (selPos, selPos) {
	if a.line > b.line || (a.line == b.line && a.col > b.col) {
		return b, a
	}
	return a, b
}

// selRowRange returns the half-open cell range [lo, hi) selected on a row.
func selRowRange(gl gridLine, row, firstRow, lastRow, firstCol, lastCol int) (int, int) {
	lo, hi := gl.cStart, gl.cEnd
	if row == firstRow {
		lo = max(lo, firstCol)
	}
	if row == lastRow {
		hi = min(hi, lastCol+1)
	}
	lo = max(lo, gl.cStart)
	hi = min(hi, gl.cEnd)
	return lo, hi
}

// selectedText is the plain text under the current selection. Leading
// border/padding and trailing width padding never enter the result, so a drag
// across a wide, padded transcript line copies just the logical text.
func (m *tuiModel) selectedText() string {
	if !m.sel.active {
		return ""
	}
	a, b := selOrder(m.sel.anchor, m.sel.cursor)
	grid := m.visibleGrid()
	if len(grid) == 0 {
		return ""
	}
	a.line = clampInt(a.line, 0, len(grid)-1)
	b.line = clampInt(b.line, 0, len(grid)-1)
	var out []string
	for row := a.line; row <= b.line; row++ {
		gl := grid[row]
		lo, hi := selRowRange(gl, row, a.line, b.line, a.col, b.col)
		var sb strings.Builder
		for _, c := range gl.cells {
			if c.col < hi && c.col+c.w > lo {
				sb.WriteString(c.text)
			}
		}
		out = append(out, strings.TrimRight(sb.String(), " \t"))
	}
	return strings.Join(out, "\n")
}

// copySelection copies the current selection (OSC 52) and reports the size in
// the status bar. An empty selection copies nothing and leaves the notice be.
func (m *tuiModel) copySelection() {
	text := m.selectedText()
	if text == "" {
		return
	}
	clipboardWrite(text)
	m.notice, m.noticeErr = fmt.Sprintf("Copied %d bytes to clipboard.", len(text)), false
}

// mapCell converts absolute screen cells to a viewport line/col. The header is
// one row and the centered column starts at pad, so the viewport origin is
// (pad, 1); columns and rows are clamped to the viewport.
func (m *tuiModel) mapCell(x, y int) (selPos, bool) {
	if m.width <= 0 || m.viewport.Height <= 0 {
		return selPos{}, false
	}
	pad := (m.width - m.columnWidth()) / 2
	col := x - pad
	row := y - 1
	if row < 0 || row >= m.viewport.Height {
		return selPos{}, false
	}
	col = clampInt(col, 0, max(0, m.viewport.Width-1))
	return selPos{line: row, col: col}, true
}

// handleMouseSelection handles transcript mouse events for in-app selection and
// link opening. It reports true when the event was fully handled; false lets
// the caller forward wheel (and other) events to the viewport. A non-nil cmd
// starts (or continues) the drag auto-scroll loop.
func (m *tuiModel) handleMouseSelection(msg tea.MouseMsg) (bool, tea.Cmd) {
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return false, nil
		}
		if msg.Ctrl {
			m.openLinkAt(msg.X, msg.Y)
			return true, nil
		}
		if msg.Shift {
			// The terminal's native bypass modifier: leave the drag alone.
			m.clearSelection()
			return true, nil
		}
		p, ok := m.mapCell(msg.X, msg.Y)
		if !ok {
			m.clearSelection()
			return true, nil
		}
		m.sel = selection{active: true, anchor: p, cursor: p}
		return true, nil
	case tea.MouseActionMotion:
		if !m.sel.active {
			return false, nil
		}
		if msg.Shift {
			return true, nil
		}
		if p, ok := m.mapCell(msg.X, msg.Y); ok {
			m.sel.cursor = p
		}
		return true, m.scheduleSelScroll()
	case tea.MouseActionRelease:
		if !m.sel.active {
			return false, nil
		}
		m.copySelection()
		m.clearSelection()
		return true, nil
	}
	return false, nil
}

// selScrollMsg advances the drag auto-scroll loop by one line.
type selScrollMsg struct{}

func (m *tuiModel) selScrollTick() tea.Cmd {
	return tea.Tick(tuiSelScrollInterval(m.cfg), func(time.Time) tea.Msg { return selScrollMsg{} })
}

// clearSelection drops the selection and stops the auto-scroll loop.
func (m *tuiModel) clearSelection() {
	m.sel.active = false
	m.selScrollTicking = false
}

// selAtScrollEdge reports whether the drag cursor sits on the viewport's top or
// bottom row, the trigger for auto-scroll.
func (m *tuiModel) selAtScrollEdge() bool {
	if !m.sel.active || m.viewport.Height <= 0 {
		return false
	}
	return m.sel.cursor.line <= 0 || m.sel.cursor.line >= m.viewport.Height-1
}

// scheduleSelScroll starts the auto-scroll loop when the cursor reaches an edge.
// It is a no-op while a loop is already running, so motion bursts don't spawn
// duplicate tickers.
func (m *tuiModel) scheduleSelScroll() tea.Cmd {
	if m.selScrollTicking || !m.selAtScrollEdge() {
		return nil
	}
	m.selScrollTicking = true
	return m.selScrollTick()
}

// selScrollStep scrolls the viewport one line toward the drag edge and extends
// the selection to the revealed row. The anchor is shifted with the viewport so
// it keeps pointing at the same content. It reports false at the scroll limit.
func (m *tuiModel) selScrollStep() bool {
	if !m.sel.active || m.viewport.Height <= 0 {
		return false
	}
	switch {
	case m.sel.cursor.line <= 0:
		if m.viewport.AtTop() {
			return false
		}
		m.viewport.ScrollUp(1)
		m.sel.anchor.line++
		m.sel.cursor.line = 0
	case m.sel.cursor.line >= m.viewport.Height-1:
		if m.viewport.AtBottom() {
			return false
		}
		m.viewport.ScrollDown(1)
		m.sel.anchor.line--
		m.sel.cursor.line = m.viewport.Height - 1
	default:
		return false
	}
	// Manual scrolling pauses autoscroll so a streaming turn can't yank the
	// viewport back to the bottom mid-drag.
	m.follow = false
	return true
}

// highlightSelection reverse-videos the selected cells of the rendered
// viewport without disturbing the surrounding styling: the escape sequences
// are injected at rune boundaries over the styled output.
func (m *tuiModel) highlightSelection(vp string) string {
	if !m.sel.active {
		return vp
	}
	lines := strings.Split(vp, "\n")
	if len(lines) == 0 {
		return vp
	}
	a, b := selOrder(m.sel.anchor, m.sel.cursor)
	a.line = clampInt(a.line, 0, len(lines)-1)
	b.line = clampInt(b.line, 0, len(lines)-1)
	for row := a.line; row <= b.line; row++ {
		gl := buildGridLine(lines[row])
		lo, hi := selRowRange(gl, row, a.line, b.line, a.col, b.col)
		if hi <= lo {
			continue
		}
		lines[row] = reverseRange(lines[row], lo, hi)
	}
	return strings.Join(lines, "\n")
}

// reverseRange reverse-videos the display cells [lo, hi) of a styled line. It
// re-asserts the attribute after every SGR sequence inside the range: the
// per-span resets that markdown/syntax highlighting emits would otherwise clear
// the reverse video after the first coloured segment.
func reverseRange(s string, lo, hi int) string {
	var b strings.Builder
	col := 0
	on := false
	var state byte
	for i := 0; i < len(s); {
		seq, w, n, ns := ansi.DecodeSequence(s[i:], state, nil)
		state = ns
		if n <= 0 {
			break
		}
		if w == 0 {
			b.WriteString(seq)
			if on && isSGRSeq(seq) {
				b.WriteString("\x1b[7m")
			}
			i += n
			continue
		}
		sel := col < hi && col+w > lo
		switch {
		case sel && !on:
			b.WriteString("\x1b[7m")
			on = true
		case !sel && on:
			b.WriteString("\x1b[27m")
			on = false
		}
		b.WriteString(seq)
		col += w
		i += n
	}
	if on {
		b.WriteString("\x1b[27m")
	}
	return b.String()
}

// isSGRSeq reports whether seq is a Select Graphic Rendition escape (`ESC [ … m`).
func isSGRSeq(seq string) bool {
	return len(seq) >= 3 && seq[0] == 0x1b && seq[1] == '[' && seq[len(seq)-1] == 'm'
}

var urlRe = regexp.MustCompile(`https?://[^\s]+`)

// openURL opens a URL in the platform browser. A var so tests stay off the
// real desktop. Only http(s) is ever passed.
var openURL = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

type linkSpan struct {
	start, end int // display cells, half-open
	url        string
}

// linkSpans finds URL spans in a stripped line, mapped to display cells.
func linkSpans(gl gridLine) []linkSpan {
	var plain strings.Builder
	for _, c := range gl.cells {
		plain.WriteString(c.text)
	}
	text := plain.String()
	var spans []linkSpan
	for _, loc := range urlRe.FindAllStringIndex(text, -1) {
		raw := text[loc[0]:loc[1]]
		url := strings.TrimRight(raw, ")]}.,;:!?\"'")
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		start := cellAtByte(gl, loc[0])
		spans = append(spans, linkSpan{start: start, end: start + ansi.StringWidth(url), url: url})
	}
	return spans
}

// cellAtByte maps a byte offset in the concatenated cell text to its cell.
func cellAtByte(gl gridLine, off int) int {
	b := 0
	for _, c := range gl.cells {
		if b >= off {
			return c.col
		}
		b += len(c.text)
	}
	return gl.width
}

// openLinkAt opens the link under a ctrl+click. Non-link clicks are a no-op.
func (m *tuiModel) openLinkAt(x, y int) {
	p, ok := m.mapCell(x, y)
	if !ok {
		return
	}
	grid := m.visibleGrid()
	if p.line < 0 || p.line >= len(grid) {
		return
	}
	for _, sp := range linkSpans(grid[p.line]) {
		if p.col >= sp.start && p.col < sp.end {
			if err := openURL(sp.url); err != nil {
				m.notice, m.noticeErr = "Could not open link: "+err.Error(), true
			} else {
				m.notice, m.noticeErr = "Opening link…", false
			}
			return
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
