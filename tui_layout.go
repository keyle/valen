package main

import (
	"fmt"
	"strings"
	"sync"
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

// inputTailRows is the number of blank rows kept below the text, so the box
// starts growing two lines before the bottom instead of at the last line.
const inputTailRows = 2

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
// height up to maxInputHeight, keeping inputTailRows blank rows below the
// text, and resizes the scrollback to match.
func (m *tuiModel) syncInputHeight() {
	h := m.inputRows() + inputTailRows
	if h > maxInputHeight {
		h = maxInputHeight
	}
	if base := inputHeight(m.cfg); h < base {
		h = base
	}
	if h != m.input.Height() {
		m.input.SetHeight(h)
		// The textarea only re-clamps its internal scroll on Update; a nil
		// message keeps the cursor row visible after the resize.
		m.input, _ = m.input.Update(nil)
	}
	if m.height == 0 {
		return
	}
	// Chips appearing or vanishing re-derive the viewport height even when
	// the chatbox itself did not resize.
	if vh := m.viewportHeight(); vh != m.viewport.Height {
		m.viewport.Height = vh
		if m.follow && m.mode == viewChat {
			m.viewport.GotoBottom()
		}
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
	h := m.height - 6 - m.input.Height() - m.filePickerHeight()
	if m.attachChips() != "" {
		h-- // the chips line sits above the chatbox
	}
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

// glamourStyle picks the markdown style for the active theme: monokai
// is fixed; terminal defers to the terminal's fg/bg and ANSI colours,
// bright on a dark background, normal on a light one.
func glamourStyle() string {
	if currentTheme != "terminal" {
		return monokaiGlamour
	}
	if lipgloss.HasDarkBackground() {
		return terminalDarkGlamour
	}
	return terminalLightGlamour
}

func (m *tuiModel) markdownRenderer() *glamour.TermRenderer {
	w := m.contentWidth() - 2
	if w < 40 {
		w = 40
	}
	theme := currentTheme
	if theme == "terminal" {
		theme += "/dark"
		if !lipgloss.HasDarkBackground() {
			theme = "terminal/light"
		}
	}
	if m.glam != nil && m.glamWidth == w && m.glamTheme == theme {
		return m.glam
	}
	r, err := glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(glamourStyle())), glamour.WithWordWrap(w))
	if err != nil {
		return nil
	}
	m.glam = r
	m.glamWidth = w
	m.glamTheme = theme
	return r
}

func (m *tuiModel) renderMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	if currentTheme == "terminal" {
		return m.renderMarkdownTerminal(s)
	}
	r := m.markdownRenderer()
	if r == nil {
		return s
	}
	out, err := r.Render(renderMath(normalizeLiteralBullets(s)))
	if err != nil {
		return s
	}
	out = strings.Trim(out, "\n")
	return indentListContinuations(out, m.glamWidth)
}

// fencedBlock is one ``` fenced code block extracted before glamour runs.
type fencedBlock struct {
	lang string
	code string
}

// fencePlaceholder is the paragraph token glamour renders in place of a
// fenced block so the highlighted code can be spliced back afterwards.
func fencePlaceholder(i int) string {
	return fmt.Sprintf("VALENCODEBLOCK%dXQZ", i)
}

// extractFencedBlocks pulls ``` / ~~~ fenced blocks out of markdown and
// returns the source with each replaced by a placeholder paragraph, so
// glamour never sees the code (its chroma style only accepts fixed hex
// colours, which would ignore the terminal theme). A source that already
// contains a placeholder token is returned untouched (blocks nil) so the
// splice cannot ever mistake user text for a placeholder.
func extractFencedBlocks(src string) (string, []fencedBlock) {
	if strings.Contains(src, "VALENCODEBLOCK") {
		return src, nil
	}
	lines := strings.Split(src, "\n")
	var out []string
	var blocks []fencedBlock
	for i := 0; i < len(lines); {
		line := lines[i]
		trim := strings.TrimSpace(line)
		fence := ""
		for _, f := range []string{"```", "~~~"} {
			if strings.HasPrefix(trim, f) {
				fence = f
				break
			}
		}
		if fence == "" {
			out = append(out, line)
			i++
			continue
		}
		lang := strings.TrimSpace(strings.TrimPrefix(trim, fence))
		// An info string with extra words (e.g. "go title=x") keeps
		// only its first word as the language.
		if fields := strings.Fields(lang); len(fields) > 0 {
			lang = fields[0]
		}
		var code []string
		i++
		for i < len(lines) {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
				i++
				break
			}
			code = append(code, lines[i])
			i++
		}
		out = append(out, "", fencePlaceholder(len(blocks)), "")
		blocks = append(blocks, fencedBlock{lang: lang, code: strings.Join(code, "\n")})
	}
	if len(blocks) == 0 {
		return src, nil
	}
	return strings.Join(out, "\n"), blocks
}

// codeBlockKey identifies one highlighted code block for the memo cache;
// the darkness flag is part of the key because the ANSI colours are
// adaptive (bright on dark, normal on light).
type codeBlockKey struct {
	lang  string
	code  string
	dark  bool
	theme string
}

// codeBlockCacheMax bounds the memo so long sessions cannot grow it
// without end. Entries are one block's rendered lines, evicted oldest-first.
const codeBlockCacheMax = 64

var (
	codeBlockMu    sync.Mutex
	codeBlocks     = map[codeBlockKey]string{}
	codeBlockOrder []codeBlockKey
)

// renderCodeBlock highlights one fenced block through the same ANSI token
// mapping as diffs, so syntax hues come from the terminal theme in
// terminal mode (no fixed hex like the glamour chroma style required).
func renderCodeBlock(lang, code string) string {
	key := codeBlockKey{lang: lang, code: code, dark: lipgloss.HasDarkBackground(), theme: currentTheme}
	codeBlockMu.Lock()
	if s, ok := codeBlocks[key]; ok {
		codeBlockMu.Unlock()
		return s
	}
	codeBlockMu.Unlock()

	segs := highlightLinesByLang(lang, code)
	var b strings.Builder
	for i, line := range segs {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, s := range line {
			text := strings.ReplaceAll(s.text, "\t", "    ")
			st := lipgloss.NewStyle().Foreground(s.fg).Bold(s.bold).Italic(s.italic)
			b.WriteString(st.Render(text))
		}
	}
	rendered := b.String()

	codeBlockMu.Lock()
	defer codeBlockMu.Unlock()
	if _, ok := codeBlocks[key]; !ok {
		codeBlocks[key] = rendered
		codeBlockOrder = append(codeBlockOrder, key)
		if len(codeBlockOrder) > codeBlockCacheMax {
			delete(codeBlocks, codeBlockOrder[0])
			codeBlockOrder = codeBlockOrder[1:]
		}
	}
	return rendered
}

// spliceCodeBlocks replaces each placeholder line in the glamour output
// with the highlighted code, keeping the placeholder's indent (and the
// blockquote marker, when present) on every code line.
func spliceCodeBlocks(rendered string, blocks []fencedBlock) string {
	lines := strings.Split(rendered, "\n")
	var out []string
	for _, line := range lines {
		idx := -1
		var block fencedBlock
		for i, b := range blocks {
			if j := strings.Index(line, fencePlaceholder(i)); j >= 0 {
				idx = j
				block = b
				break
			}
		}
		if idx < 0 {
			out = append(out, line)
			continue
		}
		prefix := ""
		for _, r := range line[:idx] {
			if r == ' ' || r == '\t' {
				prefix += string(r)
			} else {
				break
			}
		}
		if strings.Contains(line[:idx], "│") {
			prefix += "│ "
		}
		code := renderCodeBlock(block.lang, block.code)
		for _, cl := range strings.Split(code, "\n") {
			if cl == "" {
				out = append(out, "")
			} else {
				out = append(out, prefix+"  "+cl)
			}
		}
	}
	return strings.Join(out, "\n")
}

// renderMarkdownTerminal renders markdown for the terminal palette:
// fenced code blocks bypass glamour's fixed-hex chroma style entirely and
// are highlighted with real ANSI colours, so the terminal theme supplies
// every hue. Anything that is not a fenced block (including indented
// blocks, whose glamour style carries no colours) renders as before.
func (m *tuiModel) renderMarkdownTerminal(s string) string {
	stripped, blocks := extractFencedBlocks(s)
	stripped = renderMath(normalizeLiteralBullets(stripped))
	r := m.markdownRenderer()
	if r == nil {
		return s
	}
	out, err := r.Render(stripped)
	if err != nil {
		return s
	}
	out = strings.Trim(out, "\n")
	if len(blocks) > 0 {
		out = spliceCodeBlocks(out, blocks)
	}
	out = indentListContinuations(out, m.glamWidth)
	return strings.Trim(out, "\n")
}

// listItemCtx is one glamour list item still accepting wrapped continuation
// lines: its base indent (spaces before the marker) and marker display width.
type listItemCtx struct {
	indent int
	marker int
}

// parseListMarker reports whether a stripped glamour line starts a list item
// ("• ", "[✓] "/"[ ] ", or "N. ") and its base indent / marker width.
func parseListMarker(plain string) (listItemCtx, bool) {
	indent := 0
	for indent < len(plain) && plain[indent] == ' ' {
		indent++
	}
	rest := plain[indent:]
	switch {
	case strings.HasPrefix(rest, "• "):
		return listItemCtx{indent, 2}, true
	case strings.HasPrefix(rest, "[✓] ") || strings.HasPrefix(rest, "[ ] "):
		return listItemCtx{indent, 4}, true
	case len(rest) > 2 && rest[0] >= '0' && rest[0] <= '9':
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i+1 < len(rest) && rest[i] == '.' && rest[i+1] == ' ' {
			return listItemCtx{indent, i + 2}, true
		}
	}
	return listItemCtx{}, false
}

// indentListContinuations gives glamour list items a hanging indent: wrapped
// continuation lines get markerWidth extra spaces so they align under the
// item text instead of the bullet. Lines that would overflow width are
// re-wrapped within it. Blank lines end the item; code blocks (which glamour
// and spliceCodeBlocks set off with blank lines) are never inside a context.
func indentListContinuations(rendered string, width int) string {
	lines := strings.Split(rendered, "\n")
	out := make([]string, 0, len(lines))
	var cur *listItemCtx
	for _, line := range lines {
		plain := ansi.Strip(line)
		if strings.TrimSpace(plain) == "" {
			out = append(out, line)
			cur = nil
			continue
		}
		if ctx, ok := parseListMarker(plain); ok {
			c := ctx
			cur = &c
			out = append(out, line)
			continue
		}
		if cur == nil {
			out = append(out, line)
			continue
		}
		// Continuation belongs to cur: a block quote, rule or heading
		// interrupts the item.
		if strings.Contains(plain, "│") || strings.HasPrefix(strings.TrimSpace(plain), "--------") {
			cur = nil
			out = append(out, line)
			continue
		}
		gutter := strings.Repeat(" ", cur.marker)
		newLine := gutter + line
		if width > 0 && ansi.StringWidth(newLine) > width {
			limit := width - cur.indent - cur.marker
			content := strings.TrimLeft(newLine, " ")
			if limit > 0 && content != "" {
				wrapped := ansi.Wrap(content, limit, " ")
				parts := strings.Split(wrapped, "\n")
				for i, p := range parts {
					parts[i] = gutter + strings.Repeat(" ", cur.indent) + p
				}
				out = append(out, strings.Join(parts, "\n"))
				continue
			}
		}
		out = append(out, newLine)
	}
	return strings.Join(out, "\n")
}

// normalizeLiteralBullets rewrites line-leading literal bullet characters
// ("• ", "◦ ", "▪ ") to markdown list markers ("- "). Models often emit
// literal bullets, which goldmark reads as one flowing paragraph (the next
// bullet lands mid-line at the wrap point); a real list lets glamour give
// every item its own line and the hanging indent below its wrapped text.
// Fenced code and indented code (4+ spaces) are left untouched.
func normalizeLiteralBullets(src string) string {
	if !strings.ContainsAny(src, "•◦▪") {
		return src
	}
	lines := strings.Split(src, "\n")
	inFence := ""
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if inFence != "" {
			if strings.HasPrefix(trim, inFence) {
				inFence = ""
			}
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			inFence = trim[:3]
			continue
		}
		lines[i] = bulletLine(line)
	}
	return strings.Join(lines, "\n")
}

// bulletLine converts one line's leading literal bullet to "- ", preserving
// up to three columns of indent (CommonMark list indentation).
func bulletLine(line string) string {
	j := 0
	for j < len(line) && line[j] == ' ' {
		j++
	}
	if j > 3 {
		return line
	}
	rest := line[j:]
	for _, bullet := range []string{"• ", "◦ ", "▪ "} {
		if strings.HasPrefix(rest, bullet) {
			return line[:j] + "- " + rest[len(bullet):]
		}
	}
	return line
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
