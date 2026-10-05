package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/lipgloss"
)

// Row tints behind the syntax colors, close to Monokai's diff palette.
var (
	diffDelBg = lipgloss.Color("#3B2030")
	diffAddBg = lipgloss.Color("#26361C")
)

// diffStyle and the per-path lexer are hot on every live render, so resolve
// the style once and each lexer once instead of per frame.
var (
	diffStyle   = styles.Get("monokai")
	diffLexerMu sync.Mutex
	diffLexers  = map[string]chroma.Lexer{}
)

func diffLexer(path string) chroma.Lexer {
	diffLexerMu.Lock()
	defer diffLexerMu.Unlock()
	if lx, ok := diffLexers[path]; ok {
		return lx
	}
	lx := lexers.Match(path)
	diffLexers[path] = lx
	return lx
}

// diffPath pulls the file path out of edit/write args so the lexer can be
// picked from its extension.
func diffPath(args string) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	return a.Path
}

// highlightDiffRows renders "- x" / "+ x" lines (as built by editDiffLines /
// writeDiffLines) as chroma-highlighted rows: syntax-colored code over a
// red/green tinted background padded to width w. Lines without a sign
// prefix (the "… +N more" tail) render dim. The whole block is lexed at
// once so multi-line strings and comments keep their context.
func highlightDiffRows(path string, lines []string, w int) []string {
	bg := diffAddBg
	if len(lines) > 0 && strings.HasPrefix(lines[0], "- ") {
		bg = diffDelBg
	}
	signFg := mkGreen
	if bg == diffDelBg {
		signFg = mkPink
	}
	var code []string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "+ ") {
			code = append(code, ln[2:])
		}
	}
	segs := highlightLines(path, strings.Join(code, "\n"))

	out := make([]string, 0, len(lines))
	i := 0
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "- ") && !strings.HasPrefix(ln, "+ ") {
			out = append(out, "  "+lipgloss.NewStyle().Foreground(mkComment).Render(ln))
			continue
		}
		var segment []styledText
		if i < len(segs) {
			segment = segs[i]
		}
		i++
		sign := lipgloss.NewStyle().Foreground(signFg).Background(bg).Bold(true).Render(ln[:1] + " ")
		var b strings.Builder
		b.WriteString("  " + sign)
		col := 4
		for _, s := range segment {
			text := strings.ReplaceAll(s.text, "\t", "    ")
			if room := w - col; len([]rune(text)) > room {
				if room < 0 {
					room = 0
				}
				text = string([]rune(text)[:room])
			}
			if text == "" {
				continue
			}
			st := lipgloss.NewStyle().Background(bg).Foreground(s.fg).Bold(s.bold).Italic(s.italic)
			b.WriteString(st.Render(text))
			col += len([]rune(text))
		}
		if col < w {
			b.WriteString(lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", w-col)))
		}
		out = append(out, b.String())
	}
	return out
}

// diffRowKey identifies one highlighted diff block for the memo cache.
type diffRowKey struct {
	path string
	w    int
	body string
}

// diffRowCacheMax bounds the memo so a long session cannot grow it without
// end. Entries are small (one block's rendered rows) and evicted oldest-first.
const diffRowCacheMax = 128

var (
	diffRowMu    sync.Mutex
	diffRows     = map[diffRowKey][]string{}
	diffRowOrder []diffRowKey
)

// highlightDiffRowsCached memoizes highlightDiffRows by (path, width, content).
// The live transcript re-renders running edits/writes on every stream flush,
// so without this each frame re-lexes the same diff through chroma.
func highlightDiffRowsCached(path string, lines []string, w int) []string {
	key := diffRowKey{path: path, w: w, body: strings.Join(lines, "\n")}
	diffRowMu.Lock()
	if rows, ok := diffRows[key]; ok {
		diffRowMu.Unlock()
		return rows
	}
	diffRowMu.Unlock()

	rows := highlightDiffRows(path, lines, w)
	diffRowMu.Lock()
	defer diffRowMu.Unlock()
	if _, ok := diffRows[key]; !ok {
		diffRows[key] = rows
		diffRowOrder = append(diffRowOrder, key)
		if len(diffRowOrder) > diffRowCacheMax {
			delete(diffRows, diffRowOrder[0])
			diffRowOrder = diffRowOrder[1:]
		}
	}
	return rows
}

type styledText struct {
	text         string
	fg           lipgloss.Color
	bold, italic bool
}

// highlightLines lexes src by the language implied by path and returns the
// styled runs per source line. Unknown languages come back as plain runs.
func highlightLines(path, src string) [][]styledText {
	lines := [][]styledText{{}}
	add := func(text string, fg lipgloss.Color, bold, italic bool) {
		parts := strings.Split(text, "\n")
		for k, p := range parts {
			if k > 0 {
				lines = append(lines, nil)
			}
			if p != "" {
				n := len(lines) - 1
				lines[n] = append(lines[n], styledText{p, fg, bold, italic})
			}
		}
	}
	lx := diffLexer(path)
	if lx == nil {
		add(src, mkFg, false, false)
		return lines
	}
	it, err := chroma.Coalesce(lx).Tokenise(nil, src)
	if err != nil {
		lines = [][]styledText{{}}
		add(src, mkFg, false, false)
		return lines
	}
	style := diffStyle
	for tok := it(); tok != chroma.EOF; tok = it() {
		e := style.Get(tok.Type)
		fg := mkFg
		if e.Colour.IsSet() {
			fg = lipgloss.Color(e.Colour.String())
		}
		add(tok.Value, fg, e.Bold == chroma.Yes, e.Italic == chroma.Yes)
	}
	return lines
}
