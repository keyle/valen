package main

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// maxColumnWidth caps the whole UI column (header, scrollback, status, input,
// footer). It is contentWidth's 120 plus the 8 columns of chrome, so
// scrollback text keeps its readable width and the rest lines up with it.
const maxColumnWidth = 128

// columnWidth is the width of the centered UI column: the terminal width,
// capped at maxColumnWidth.
func (m *tuiModel) columnWidth() int {
	if m.width > maxColumnWidth {
		return maxColumnWidth
	}
	return m.width
}

// isPickerMode reports whether the active view is a list picker (or the
// question overlay) that hides the chatbox and footer, leaving the whole
// height to the list so a question stays readable on short terminals.
func (m *tuiModel) isPickerMode() bool {
	return m.mode == viewSessions || m.mode == viewModels || m.mode == viewSlots || m.mode == viewAsk
}

// maxInputHeight caps how far the chatbox grows with its content.
const maxInputHeight = 10

// inputRows counts the visual rows the chatbox text occupies at the current
// textarea width, soft wraps included (minimum 1).
func (m *tuiModel) inputRows() int {
	w := m.input.Width()
	if w <= 0 {
		return 1
	}
	rows := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		rows += len(wrapInputLine([]rune(ln), w))
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// wrapInputLine mirrors the bubbles textarea word wrap so the chatbox can
// size itself to the rows the textarea will actually draw.
func wrapInputLine(runes []rune, width int) [][]rune {
	var (
		lines  = [][]rune{{}}
		word   []rune
		row    int
		spaces int
	)
	strw := func(r []rune) int { return lipgloss.Width(string(r)) }
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			if strw(lines[row])+strw(word)+spaces > width {
				row++
				lines = append(lines, []rune{})
			}
			lines[row] = append(lines[row], word...)
			lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
			spaces = 0
			word = nil
		} else if strw(word)+lipgloss.Width(string(word[len(word)-1])) > width {
			if len(lines[row]) > 0 {
				row++
				lines = append(lines, []rune{})
			}
			lines[row] = append(lines[row], word...)
			word = nil
		}
	}
	if strw(lines[row])+strw(word)+spaces >= width {
		lines = append(lines, append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces+1))...))
	}
	return lines
}

// syncInputHeight grows the chatbox with its content, from the configured
// height up to maxInputHeight, and resizes the scrollback to match.
func (m *tuiModel) syncInputHeight() {
	h := m.inputRows()
	if h > maxInputHeight {
		h = maxInputHeight
	}
	if base := inputHeight(m.cfg); h < base {
		h = base
	}
	if h == m.input.Height() {
		return
	}
	m.input.SetHeight(h)
	// The textarea only re-clamps its internal scroll on Update; a nil
	// message keeps the cursor row visible after the resize.
	m.input, _ = m.input.Update(nil)
	if m.height == 0 {
		return
	}
	m.viewport.Height = m.viewportHeight()
	if m.follow && m.mode == viewChat {
		m.viewport.GotoBottom()
	}
}

// viewportHeight is the scrollback height for the current mode and terminal
// size. Chat reserves room for the chatbox, status and footer; pickers drop
// the chatbox and footer, so the viewport grows to keep the status row pinned
// to the bottom edge instead of leaving dead space beneath it.
func (m *tuiModel) viewportHeight() int {
	if m.height == 0 {
		return m.viewport.Height
	}
	h := m.height - 6 - m.input.Height()
	if m.isPickerMode() {
		h = m.height - 2 // header + status
	}
	if h < 5 {
		h = 5
	}
	return h
}

func (m *tuiModel) viewWidth() int {
	if m.viewport.Width > 0 {
		return m.viewport.Width
	}
	if m.width > 0 {
		return m.width - 4
	}
	return 80
}

func (m *tuiModel) markdownRenderer() *glamour.TermRenderer {
	w := m.viewWidth() - 6
	if w < 40 {
		w = 40
	}
	if m.glam != nil && m.glamWidth == w {
		return m.glam
	}
	r, err := glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(monokaiGlamour)), glamour.WithWordWrap(w))
	if err != nil {
		return nil
	}
	m.glam = r
	m.glamWidth = w
	return r
}

func (m *tuiModel) renderMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	r := m.markdownRenderer()
	if r == nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimSpace(out)
}

func truncatePlain(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// truncateRunes is truncatePlain but rune-safe, for wrapping display lines
// that may contain multibyte text.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return "…"
	}
	return string(r[:n]) + "…"
}

// contentWidth is the usable text width for transcript and overlay blocks:
// the viewport width minus chrome, clamped to a readable range.
func (m *tuiModel) contentWidth() int {
	w := m.viewWidth() - 4
	if w > 120 {
		w = 120
	}
	if w < 20 {
		w = 20
	}
	return w
}

// breakLongWords force-breaks single words (URLs, hashes, long tokens) that
// exceed width, so they cannot push a line off-screen. Normal word wrapping
// is left to the lipgloss Width styles applied at render time.
func breakLongWords(s string, width int) string {
	if width < 10 {
		width = 10
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		// Measure display width, not runes: s may carry ANSI styling
		// (rendered markdown), and escape bytes must neither count toward
		// the width nor ever be split.
		if ansi.StringWidth(line) <= width {
			continue
		}
		words := strings.Split(line, " ")
		for j, w := range words {
			if ansi.StringWidth(w) > width {
				words[j] = ansi.Hardwrap(w, width, true)
			}
		}
		lines[i] = strings.Join(words, " ")
	}
	return strings.Join(lines, "\n")
}

// wrapPlain wraps preformatted/plain text to the transcript width, for
// scrollback, question and detail views that render without markdown.
func (m *tuiModel) wrapPlain(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return lipgloss.NewStyle().Width(m.contentWidth()).Render(breakLongWords(s, m.contentWidth()))
}

// wrapAskHint renders the question-overlay waiting hints (confirmation
// submit line) in bold green like the ready status, so it reads as waiting
// on the user rather than idle helper text.
func (m *tuiModel) wrapAskHint(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return confirmHintStyle.Width(m.contentWidth()).Render(breakLongWords(s, m.contentWidth()))
}

// pinBottom re-renders and snaps the transcript to the newest line,
// re-enabling autoscroll. Use for explicit jumps (submit, commit,
// returning to chat); streaming ticks honor follow instead so a user
// reading history is not yanked down.
func (m *tuiModel) pinBottom() {
	m.follow = true
	m.refreshViewport()
	m.viewport.GotoBottom()
}
