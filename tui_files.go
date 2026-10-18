package main

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
)

// Caps for the @ file picker: the walk stops at fileIndexLimit entries, and
// the dropdown renders at most maxFilePickerRows of the best matches.
const (
	fileIndexLimit      = 20000
	fileChoiceLimit     = 200
	maxFilePickerRows   = 8
	fileIndexTTL        = 10 * time.Second
	filePickerHintPlain = "↑↓ move · enter insert · esc close"
)

// fileEntry is one fuzzy target: a path relative to the workdir, slash
// separated. Dirs are kept so the folder itself can be inserted.
type fileEntry struct {
	path string
	dir  bool
}

func (e fileEntry) target() string {
	if e.dir {
		return e.path + "/"
	}
	return e.path
}

// fileEntries implements fuzzy.Source without allocating target strings.
type fileEntries []fileEntry

func (e fileEntries) String(i int) string { return e[i].target() }
func (e fileEntries) Len() int            { return len(e) }

// filePicker tracks the @token being edited and its current matches.
type filePicker struct {
	open    bool
	query   string
	start   int // token span in runes: "@" through the end of the token
	end     int
	choices fileEntries
	cursor  int
}

// fileIndexMsg carries the async workdir walk back to the UI thread.
type fileIndexMsg struct {
	root      string
	entries   fileEntries
	truncated bool
}

// buildFileIndex walks root once, skipping dependency/build dirs and secret
// .env files. It stops after limit entries and reports the truncation.
func buildFileIndex(root string, limit int) (fileEntries, bool) {
	var out fileEntries
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		if d.IsDir() && skippedDir(d.Name()) {
			return fs.SkipDir
		}
		if !d.IsDir() && isSecretFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, fileEntry{path: filepath.ToSlash(rel), dir: d.IsDir()})
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	// Shallow paths first: the empty-query list and equal fuzzy scores then
	// favour top-level files over a deep alphabetically-earlier subtree.
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := strings.Count(out[i].path, "/"), strings.Count(out[j].path, "/")
		if di != dj {
			return di < dj
		}
		return out[i].path < out[j].path
	})
	return out, len(out) >= limit
}

// inputCursorOffset converts the textarea's current logical line and column
// into a rune offset in the whole value.
func inputCursorOffset(value string, line, col int) int {
	r := []rune(value)
	if line <= 0 {
		return clampInt(col, 0, len(r))
	}
	off := 0
	for i, ln := range strings.Split(value, "\n") {
		if i == line {
			return clampInt(off+col, 0, len(r))
		}
		off += len([]rune(ln)) + 1
	}
	return len(r)
}

// escapedAt reports whether the rune at i is preceded by an odd number of
// backslashes, i.e. it is part of an escaped path token.
func escapedAt(r []rune, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && r[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// fileTokenAt finds the @token around cursor. It returns the token span and
// the unescaped query between "@" and the cursor. Tokens start at "@" after
// whitespace (or at the start) and run to the next unescaped whitespace.
func fileTokenAt(value string, cursor int) (start, end int, query string, ok bool) {
	r := []rune(value)
	if cursor < 0 || cursor > len(r) {
		return 0, 0, "", false
	}
	start = cursor
	for start > 0 {
		if unicode.IsSpace(r[start-1]) && !escapedAt(r, start-1) {
			break
		}
		start--
	}
	// start < cursor: a caret sitting on the "@" (e.g. after moving left
	// through an inserted path) is just before the token, not inside it.
	if start >= cursor || r[start] != '@' {
		return 0, 0, "", false
	}
	end = cursor
	for end < len(r) && (!unicode.IsSpace(r[end]) || escapedAt(r, end)) {
		end++
	}
	return start, end, unescapeFileToken(string(r[start+1 : cursor])), true
}

func escapeFileToken(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	return strings.ReplaceAll(p, " ", `\ `)
}

func unescapeFileToken(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	for i := 0; i < len(q); i++ {
		if q[i] == '\\' && i+1 < len(q) {
			i++
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// refreshFilePickerChoices re-derives the token from the current input and
// filters the index. It reports whether the picker stays open.
func (m *tuiModel) refreshFilePickerChoices() bool {
	if m.mode != viewChat || m.ask != nil {
		m.closeFilePicker()
		return false
	}
	value := m.input.Value()
	info := m.input.LineInfo()
	// StartColumn + ColumnOffset is the caret's rune offset in the logical
	// line: ColumnOffset alone is relative to the soft-wrapped visual row.
	col := info.StartColumn + info.ColumnOffset
	start, end, query, ok := fileTokenAt(value, inputCursorOffset(value, m.input.Line(), col))
	if !ok {
		m.closeFilePicker()
		return false
	}
	changed := !m.files.open || query != m.files.query
	m.files.open = true
	m.files.start, m.files.end, m.files.query = start, end, query
	if changed {
		m.files.cursor = 0
	}
	m.filterFileChoices()
	m.syncFilePickerHeight()
	return true
}

// syncFilePicker also starts the capped index walk when it is missing or
// stale. Call it after any edit or cursor move in the chatbox.
func (m *tuiModel) syncFilePicker() tea.Cmd {
	if !m.refreshFilePickerChoices() {
		return nil
	}
	if m.fileIndexReady && time.Since(m.fileIndexAt) <= fileIndexTTL {
		return nil
	}
	if m.fileIndexLoading {
		return nil
	}
	m.fileIndexLoading = true
	root := getFileRoot()
	return func() tea.Msg {
		entries, truncated := buildFileIndex(root, fileIndexLimit)
		return fileIndexMsg{root: root, entries: entries, truncated: truncated}
	}
}

func (m *tuiModel) closeFilePicker() {
	if !m.files.open {
		return
	}
	m.files = filePicker{}
	m.syncFilePickerHeight()
}

func (m *tuiModel) filterFileChoices() {
	if !m.files.open {
		return
	}
	m.files.choices = m.files.choices[:0]
	if !m.fileIndexReady {
		m.files.cursor = 0
		return
	}
	query := strings.TrimPrefix(m.files.query, "/")
	if strings.TrimSpace(query) == "" {
		n := min(len(m.fileIndex), fileChoiceLimit)
		m.files.choices = append(m.files.choices, m.fileIndex[:n]...)
	} else {
		matches := fuzzy.FindFrom(query, m.fileIndex)
		for i, mt := range matches {
			if i >= fileChoiceLimit {
				break
			}
			m.files.choices = append(m.files.choices, m.fileIndex[mt.Index])
		}
	}
	if m.files.cursor >= len(m.files.choices) {
		m.files.cursor = max(len(m.files.choices)-1, 0)
	}
}

// filePickerKey handles keys owned by the open dropdown. It reports whether
// the key was consumed, so typing still reaches the textarea.
func (m *tuiModel) filePickerKey(s string) (bool, tea.Cmd) {
	if !m.files.open || m.mode != viewChat || m.ask != nil {
		return false, nil
	}
	switch s {
	case "esc":
		m.closeFilePicker()
		return true, nil
	case "up":
		if n := len(m.files.choices); n > 0 {
			m.files.cursor = (m.files.cursor - 1 + n) % n
		}
		return true, nil
	case "down":
		if n := len(m.files.choices); n > 0 {
			m.files.cursor = (m.files.cursor + 1) % n
		}
		return true, nil
	case "enter":
		if len(m.files.choices) > 0 {
			m.acceptFileChoice()
			return true, nil
		}
		// No match: let enter send the prompt as usual.
		return false, nil
	}
	return false, nil
}

// acceptFileChoice replaces the @token with the selected path, leaving the
// cursor after it so typing continues naturally.
func (m *tuiModel) acceptFileChoice() {
	if !m.files.open || m.files.cursor < 0 || m.files.cursor >= len(m.files.choices) {
		return
	}
	e := m.files.choices[m.files.cursor]
	insert := "@" + escapeFileToken(e.target())
	if r := []rune(m.input.Value()); m.files.end >= len(r) || !unicode.IsSpace(r[m.files.end]) {
		insert += " "
	}
	m.replaceInputRange(m.files.start, m.files.end, insert)
	if abs, err := resolveFilePath(e.target()); err == nil {
		m.addMentionAttachment(abs)
	}
	m.closeFilePicker()
}

func (m *tuiModel) replaceInputRange(start, end int, insert string) {
	r := []rune(m.input.Value())
	start = clampInt(start, 0, len(r))
	end = clampInt(end, start, len(r))
	next := string(r[:start]) + insert + string(r[end:])
	m.setInputValueCaret(next, start+len([]rune(insert)))
	m.dismissTransientNotice()
}

// setInputValueCaret rewrites the chatbox and puts the caret at a rune offset,
// walking down the logical lines because textarea only moves to end on SetValue.
func (m *tuiModel) setInputValueCaret(value string, caret int) {
	m.input.SetValue(value)
	r := []rune(value)
	caret = clampInt(caret, 0, len(r))
	line, col := 0, 0
	for _, ch := range r[:caret] {
		if ch == '\n' {
			line, col = line+1, 0
			continue
		}
		col++
	}
	m.input.CursorStart()
	for i := 0; i < line; i++ {
		// End the logical line first: CursorDown otherwise walks soft-wrapped
		// visual rows of the same line.
		m.input.CursorEnd()
		m.input.CursorDown()
	}
	m.input.SetCursor(col)
}

// filePickerHeight is the rows the open dropdown adds, including its border
// and hint line. Chat layout subtracts it from the scrollback.
func (m *tuiModel) filePickerHeight() int {
	if !m.files.open || m.mode != viewChat || m.ask != nil {
		return 0
	}
	n := len(m.files.choices)
	if n == 0 {
		n = 1
	}
	if n > maxFilePickerRows {
		n = maxFilePickerRows
	}
	return n + 3
}

func (m *tuiModel) filePickerWindow() (start, end int) {
	n := len(m.files.choices)
	if n == 0 {
		return 0, 0
	}
	rows := min(n, maxFilePickerRows)
	start = m.files.cursor - rows + 1
	if start < 0 {
		start = 0
	}
	end = start + rows
	if end > n {
		end = n
		start = end - rows
	}
	return start, end
}

func (m *tuiModel) syncFilePickerHeight() {
	if m.height == 0 {
		return
	}
	h := m.viewportHeight()
	if h == m.viewport.Height {
		return
	}
	m.viewport.Height = h
	if m.follow && m.mode == viewChat {
		m.viewport.GotoBottom()
	}
}

func (m *tuiModel) filePickerView() string {
	if !m.files.open || m.mode != viewChat || m.ask != nil {
		return ""
	}
	cw := m.contentWidth()
	hint := filePickerHintPlain
	if !m.fileIndexReady {
		hint = "indexing files…"
	} else if m.fileIndexTrunc {
		hint += " · index capped"
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render(truncateRunes(hint, cw)))
	if !m.fileIndexReady || len(m.files.choices) == 0 {
		msg := "  no matches"
		if !m.fileIndexReady {
			msg = "  indexing…"
		}
		b.WriteString("\n" + metaStyle.Render(truncateRunes(msg, cw)))
	} else {
		start, end := m.filePickerWindow()
		for i := start; i < end; i++ {
			line := truncateRunes(m.files.choices[i].target(), cw-2)
			if i == m.files.cursor {
				b.WriteString("\n" + selectedStyle.Width(cw).Render(" "+line))
			} else {
				b.WriteString("\n  " + line)
			}
		}
	}
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(mkBorder).Padding(0, 1)
	if m.width > 0 {
		style = style.Width(m.columnWidth() - 2)
	}
	return style.Render(b.String())
}
