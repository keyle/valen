package main

import (
	"encoding/json"
	"strings"

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
	lx := lexers.Match(path)
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
	style := styles.Get("monokai")
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
