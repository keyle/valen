package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func pickerTestModel(t *testing.T) (*tuiModel, string) {
	t.Helper()
	root := t.TempDir()
	oldRoot := getFileRoot()
	setFileRoot(root)
	t.Cleanup(func() { setFileRoot(oldRoot) })
	return newTestModel(t, testConfig("https://unused")), root
}

func writeTestFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadPickerIndex(t *testing.T, m *tuiModel, root string) {
	t.Helper()
	entries, truncated := buildFileIndex(root, fileIndexLimit)
	m.fileIndex, m.fileIndexTrunc = entries, truncated
	m.fileIndexReady = true
	m.fileIndexLoading = false
	m.fileIndexAt = time.Now()
}

func typePickerKeys(t *testing.T, m *tuiModel, s string) {
	t.Helper()
	for _, r := range s {
		mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = mm.(*tuiModel)
	}
}

func pickerTargets(m *tuiModel) []string {
	out := make([]string, len(m.files.choices))
	for i, c := range m.files.choices {
		out[i] = c.target()
	}
	return out
}

func TestTUIFileTokenAt(t *testing.T) {
	cases := []struct {
		name, value string
		cursor      int
		start, end  int
		query       string
		ok          bool
	}{
		{"start", "@src", 4, 0, 4, "src", true},
		{"after text", "look @src/ma", 12, 5, 12, "src/ma", true},
		{"before rest", "look @src/ma rest", 12, 5, 12, "src/ma", true},
		{"empty", "@", 1, 0, 1, "", true},
		{"escaped space", `@My\ Dir/`, 9, 0, 9, "My Dir/", true},
		{"email", "a@b", 3, 0, 0, "", false},
		{"space after", "@src ", 5, 0, 0, "", false},
		{"newline", "x\n@go", 5, 2, 5, "go", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, query, ok := fileTokenAt(c.value, c.cursor)
			if ok != c.ok || (ok && (start != c.start || end != c.end || query != c.query)) {
				t.Fatalf("got start=%d end=%d query=%q ok=%v", start, end, query, ok)
			}
		})
	}
}

func TestTUIFileTokenAtCaretOnAtSign(t *testing.T) {
	// Moving left through an inserted @path lands the caret on the "@".
	// That is before the token, not inside it — and must not slice past it.
	if _, _, _, ok := fileTokenAt("read this @a/b", 10); ok {
		t.Fatal("caret on '@' is not inside the token")
	}
	if _, _, _, ok := fileTokenAt("@a", 0); ok {
		t.Fatal("caret at the start of the input is not inside the token")
	}
	if start, _, query, ok := fileTokenAt("@a", 1); !ok || start != 0 || query != "" {
		t.Fatalf("caret right after '@': start=%d query=%q ok=%v", start, query, ok)
	}
}

func TestTUIFilePickerCursorWalksInsertedPath(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "path/path/filename", "x")
	loadPickerIndex(t, m, root)

	typePickerKeys(t, m, "read this @file")
	mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if got, want := m.input.Value(), "read this @path/path/filename "; got != want {
		t.Fatalf("input=%q want %q", got, want)
	}
	// Caret inside the inserted path: the picker reopens on the prefix.
	for i := 0; i < len("filename"); i++ {
		mm, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
		m = mm.(*tuiModel)
	}
	if !m.files.open {
		t.Fatal("picker must reopen while the caret is inside the token")
	}
	// Walking left over the "@" and out of the token closes it (no panic).
	for i := 0; i < 40; i++ {
		mm, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
		m = mm.(*tuiModel)
	}
	if m.files.open {
		t.Fatal("picker must close once the caret leaves the token")
	}
	if m.input.LineInfo().ColumnOffset != 0 {
		t.Fatalf("caret=%d", m.input.LineInfo().ColumnOffset)
	}
	_ = m.View()
	if got := m.input.Value(); got != "read this @path/path/filename " {
		t.Fatalf("left must not edit the input: %q", got)
	}
}

func TestTUIFilePickerFiltersAndInsertsFile(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "main.go", "package main")
	writeTestFile(t, root, "src/main.go", "package src")
	writeTestFile(t, root, "src/deep/notes.txt", "notes")
	writeTestFile(t, root, "node_modules/pkg/index.js", "dep")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	loadPickerIndex(t, m, root)

	typePickerKeys(t, m, "@notes")
	if !m.files.open {
		t.Fatal("@ must open the picker")
	}
	targets := pickerTargets(m)
	if len(targets) != 1 || targets[0] != "src/deep/notes.txt" {
		t.Fatalf("fuzzy match over the subtree: %v", targets)
	}
	if _, _, _, ok := fileTokenAt(m.input.Value(), len([]rune(m.input.Value()))); !ok {
		t.Fatal("picker lost its token")
	}
	mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if m.files.open {
		t.Fatal("enter must close the picker")
	}
	if got, want := m.input.Value(), "@src/deep/notes.txt "; got != want {
		t.Fatalf("input=%q want %q", got, want)
	}
	if m.input.LineInfo().ColumnOffset != len([]rune(m.input.Value())) {
		t.Fatal("caret not placed after the inserted token")
	}

	// Editing mid-token replaces the whole token without adding a space
	// before the existing one, and leaves the caret after the path.
	m.input.SetValue("look @notes please")
	m.setInputValueCaret(m.input.Value(), 10) // after "@note"
	m.refreshFilePickerChoices()
	if !m.files.open || m.files.query != "note" {
		t.Fatalf("mid-token picker open=%v query=%q", m.files.open, m.files.query)
	}
	mm, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if got, want := m.input.Value(), "look @src/deep/notes.txt please"; got != want {
		t.Fatalf("mid-token insert=%q want %q", got, want)
	}
	if m.input.LineInfo().ColumnOffset != 24 {
		t.Fatalf("caret=%d", m.input.LineInfo().ColumnOffset)
	}
}

func TestTUIFilePickerSelectsFolderAndEscKeepsToken(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "docs/readme.md", "hi")
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	loadPickerIndex(t, m, root)

	typePickerKeys(t, m, "@doc")
	if len(m.files.choices) == 0 || m.files.choices[0].target() != "docs/" && m.files.choices[0].target() != "docs/readme.md" {
		t.Fatalf("choices=%v", pickerTargets(m))
	}
	m.files.cursor = 0
	for i, c := range m.files.choices {
		if c.target() == "docs/" {
			m.files.cursor = i
		}
	}
	mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if got, want := m.input.Value(), "@docs/ "; got != want {
		t.Fatalf("folder insert=%q want %q", got, want)
	}

	m.input.SetValue("@doc")
	m.refreshFilePickerChoices()
	mm, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*tuiModel)
	if m.files.open {
		t.Fatal("esc must close the picker")
	}
	if got := m.input.Value(); got != "@doc" {
		t.Fatalf("esc must keep the token, input=%q", got)
	}
}

func TestTUIFilePickerEscapesSpaces(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "My Notes.md", "todo")
	loadPickerIndex(t, m, root)

	typePickerKeys(t, m, `@My\ N`)
	if len(m.files.choices) != 1 || m.files.choices[0].target() != "My Notes.md" {
		t.Fatalf("choices=%v", pickerTargets(m))
	}
	mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if got, want := m.input.Value(), `@My\ Notes.md `; got != want {
		t.Fatalf("input=%q want %q", got, want)
	}
}

func TestTUIFilePickerIndexSkipsAndCaps(t *testing.T) {
	m, root := pickerTestModel(t)
	_ = m
	writeTestFile(t, root, "a.go", "a")
	writeTestFile(t, root, "b.go", "b")
	writeTestFile(t, root, "c.go", "c")
	writeTestFile(t, root, "vendor/v.go", "v")
	entries, truncated := buildFileIndex(root, 2)
	if !truncated || len(entries) != 2 {
		t.Fatalf("cap: entries=%d truncated=%v", len(entries), truncated)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.path, "vendor/") {
			t.Fatalf("skipped path leaked into the index: %s", e.path)
		}
	}
	all, _ := buildFileIndex(root, fileIndexLimit)
	if len(all) != 3 {
		t.Fatalf("want 3 visible entries, got %v", all)
	}
}

func TestTUIFilePickerIndexShallowFirst(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "b.go", "b")
	writeTestFile(t, root, "z.go", "z")
	writeTestFile(t, root, "a/m.go", "m")
	writeTestFile(t, root, "a/deep/x.go", "x")
	entries, _ := buildFileIndex(root, fileIndexLimit)
	var got []string
	for _, e := range entries {
		got = append(got, e.target())
	}
	want := []string{"a/", "b.go", "z.go", "a/deep/", "a/m.go", "a/deep/x.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", got, want)
	}

	m.fileIndex, m.fileIndexReady = entries, true
	m.input.SetValue("@")
	m.refreshFilePickerChoices()
	if targets := pickerTargets(m); len(targets) < 2 || targets[0] != "a/" || targets[1] != "b.go" {
		t.Fatalf("empty query must list shallow paths first: %v", targets)
	}
}

func TestTUIFilePickerEnterWithoutMatchesSubmits(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "main.go", "package main")
	loadPickerIndex(t, m, root)

	typePickerKeys(t, m, "@zzz")
	if !m.files.open || len(m.files.choices) != 0 {
		t.Fatalf("open=%v choices=%v", m.files.open, pickerTargets(m))
	}
	mm, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if m.files.open {
		t.Fatal("enter must close the picker")
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("enter must send the prompt, input=%q", got)
	}
	if !m.busy || cmd == nil {
		t.Fatalf("prompt not sent: busy=%v cmd=%v", m.busy, cmd != nil)
	}
}

func TestTUIFilePickerUpdatesAfterIndexMessage(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "cmd/main.go", "package main")
	m.input.SetValue("@cmd")
	m.refreshFilePickerChoices()
	if len(m.files.choices) != 0 {
		t.Fatal("picker must wait for the index")
	}
	_, _ = m.Update(fileIndexMsg{root: root, entries: fileEntries{{path: "cmd/", dir: true}, {path: "cmd/main.go"}}})
	if len(m.files.choices) != 2 {
		t.Fatalf("index message did not refresh choices: %v", pickerTargets(m))
	}
}

func TestTUIInputCaretRestored(t *testing.T) {
	m, _ := pickerTestModel(t)
	m.setInputValueCaret("one\ntwo", 5)
	if m.input.Line() != 1 || m.input.LineInfo().ColumnOffset != 1 {
		t.Fatalf("line=%d col=%d", m.input.Line(), m.input.LineInfo().ColumnOffset)
	}
	// A soft-wrapped first line must not be mistaken for extra logical lines.
	long := strings.Repeat("word ", 40) + "\ntwo"
	m.setInputValueCaret(long, len([]rune(long))-1)
	if m.input.Line() != 1 || m.input.LineInfo().ColumnOffset != 2 {
		t.Fatalf("wrapped line=%d col=%d", m.input.Line(), m.input.LineInfo().ColumnOffset)
	}
}

func TestTUIFilePickerShrinksViewport(t *testing.T) {
	m, root := pickerTestModel(t)
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		writeTestFile(t, root, name, "x")
	}
	loadPickerIndex(t, m, root)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := m.viewport.Height
	m.input.SetValue("@")
	m.refreshFilePickerChoices()
	m.syncFilePickerHeight()
	if m.filePickerHeight() == 0 {
		t.Fatal("open picker must reserve rows")
	}
	if m.viewport.Height != before-m.filePickerHeight() {
		t.Fatalf("viewport=%d before=%d picker=%d", m.viewport.Height, before, m.filePickerHeight())
	}
	if v := m.filePickerView(); !strings.Contains(v, "a.go") || !strings.Contains(v, "enter insert") {
		t.Fatalf("picker view missing rows/hint:\n%s", v)
	}
}
