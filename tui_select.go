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

// selPos is a point in the transcript: line is the absolute content line
// (0 = first line of the full rendered transcript, i.e. viewport.YOffset's
// coordinate space), col is a display cell (0-based). Absolute lines let a
// selection survive auto-scroll: rows scrolled out of the window stay part of
// the copy because the grid is built from the full content, not the visible
// slice.
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

// hasLeadingBorder reports whether a line's leading decoration carries a border
// glyph (a bordered body or a block quote). Such a marker is chrome and must
// never be copied, so the row starts at its content; un-bordered rows instead
// start at the selection's common base and keep their own indentation.
func hasLeadingBorder(gl gridLine) bool {
	for _, c := range gl.cells {
		if c.col >= gl.cStart {
			break
		}
		if isBorderGlyph(c) {
			return true
		}
	}
	return false
}

// gridBase is the leftmost content column across a run of grid lines: the
// shared leading decoration (document margin, block indent) that drag-copy
// strips. Lines indented deeper than the base — a code sample's body — keep
// their relative indentation instead of collapsing to their first glyph.
// Decoration-only lines (blank rows, borders) and bordered rows are ignored:
// a border is chrome stripped per row, so it must not drag the base leftward.
func gridBase(rows []gridLine) int {
	base := -1
	for _, gl := range rows {
		if gl.cStart >= gl.cEnd || hasLeadingBorder(gl) {
			continue
		}
		if base < 0 || gl.cStart < base {
			base = gl.cStart
		}
	}
	if base < 0 {
		return 0
	}
	return base
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

// contentGrid is the stripped, cell-indexed form of the full transcript (not
// just the visible window), so a selection can address lines that have
// auto-scrolled out of view.
func (m *tuiModel) contentGrid() []gridLine {
	lines := strings.Split(m.vpContent, "\n")
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
// base is the selection-wide left content column (from gridBase): un-bordered
// rows start there so indentation relative to base survives, while bordered
// rows start at their own content and the trailing edge is always clamped to
// the row's content end so width padding never enters the copy.
func selRowRange(gl gridLine, row, firstRow, lastRow, base, firstCol, lastCol int) (int, int) {
	lo, hi := base, gl.cEnd
	if hasLeadingBorder(gl) {
		lo = gl.cStart
	}
	if row == firstRow {
		lo = max(lo, firstCol)
	}
	if row == lastRow {
		hi = min(hi, lastCol+1)
	}
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
	grid := m.contentGrid()
	if len(grid) == 0 {
		return ""
	}
	a.line = clampInt(a.line, 0, len(grid)-1)
	b.line = clampInt(b.line, 0, len(grid)-1)
	base := gridBase(grid[a.line : b.line+1])
	var out []string
	for row := a.line; row <= b.line; row++ {
		gl := grid[row]
		lo, hi := selRowRange(gl, row, a.line, b.line, base, a.col, b.col)
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

// mapCell converts absolute screen cells to a transcript line/col. The header
// is one row and the centered column starts at pad, so the viewport origin is
// (pad, 1); columns and rows are clamped to the viewport, and the row is
// offset by the scroll position to yield an absolute content line.
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
	return selPos{line: m.viewport.YOffset + row, col: col}, true
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
// bottom row, the trigger for auto-scroll. The cursor is an absolute content
// line, so its visible row is line - YOffset.
func (m *tuiModel) selAtScrollEdge() bool {
	if !m.sel.active || m.viewport.Height <= 0 {
		return false
	}
	row := m.sel.cursor.line - m.viewport.YOffset
	return row <= 0 || row >= m.viewport.Height-1
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

// selScrollStep scrolls the viewport one line toward the drag edge and moves
// the cursor onto the revealed edge row. The anchor is an absolute content
// line, so it stays pinned to its content with no adjustment — the whole span
// (including rows now scrolled out of view) remains selectable. It reports
// false at the scroll limit.
func (m *tuiModel) selScrollStep() bool {
	if !m.sel.active || m.viewport.Height <= 0 {
		return false
	}
	row := m.sel.cursor.line - m.viewport.YOffset
	switch {
	case row <= 0:
		if m.viewport.AtTop() {
			return false
		}
		m.viewport.ScrollUp(1)
		m.sel.cursor.line = m.viewport.YOffset
	case row >= m.viewport.Height-1:
		if m.viewport.AtBottom() {
			return false
		}
		m.viewport.ScrollDown(1)
		m.sel.cursor.line = m.viewport.YOffset + m.viewport.Height - 1
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
// are injected at rune boundaries over the styled output. Selection endpoints
// are absolute content lines, so each is mapped back to a visible row via the
// current scroll offset; rows scrolled out of the window simply aren't drawn.
func (m *tuiModel) highlightSelection(vp string) string {
	if !m.sel.active {
		return vp
	}
	lines := strings.Split(vp, "\n")
	if len(lines) == 0 {
		return vp
	}
	off := m.viewport.YOffset
	a, b := selOrder(m.sel.anchor, m.sel.cursor)
	grid := m.contentGrid()
	if len(grid) == 0 {
		return vp
	}
	a.line = clampInt(a.line, 0, len(grid)-1)
	b.line = clampInt(b.line, 0, len(grid)-1)
	base := gridBase(grid[a.line : b.line+1])
	for row := a.line; row <= b.line; row++ {
		vr := row - off
		if vr < 0 || vr >= len(lines) {
			continue
		}
		gl := grid[row]
		lo, hi := selRowRange(gl, row, a.line, b.line, base, a.col, b.col)
		if hi <= lo {
			continue
		}
		lines[vr] = reverseRange(lines[vr], lo, hi)
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
	lines := strings.Split(m.vpContent, "\n")
	if p.line < 0 || p.line >= len(lines) {
		return
	}
	gl := buildGridLine(lines[p.line])
	for _, sp := range linkSpans(gl) {
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
