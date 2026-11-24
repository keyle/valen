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
// Installed from the active palette by applyTheme (terminal: no tint,
// the terminal background shows through).
var (
	diffDelBg lipgloss.TerminalColor = lipgloss.Color("#3B2030")
	diffAddBg lipgloss.TerminalColor = lipgloss.Color("#26361C")
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
	isDel := len(lines) > 0 && strings.HasPrefix(lines[0], "- ")
	bg := diffAddBg
	if isDel {
		bg = diffDelBg
	}
	signFg := mkGreen
	if isDel {
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
	path  string
	w     int
	body  string
	theme string
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
	key := diffRowKey{path: path, w: w, body: strings.Join(lines, "\n"), theme: currentTheme}
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
	fg           lipgloss.TerminalColor
	bold, italic bool
}

// terminalTokenColor maps a chroma token to the terminal palette's ANSI
// colours (via the mk* palette globals, which are ANSI in terminal mode),
// so syntax hues come from the terminal theme.
func terminalTokenColor(tt chroma.TokenType) lipgloss.TerminalColor {
	switch {
	case tt.InCategory(chroma.Comment):
		return mkComment
	case tt.InSubCategory(chroma.NameFunction), tt.InSubCategory(chroma.NameClass), tt.InSubCategory(chroma.NameAttribute):
		return mkGreen
	case tt.InSubCategory(chroma.NameBuiltin), tt == chroma.KeywordType, tt.InSubCategory(chroma.NameConstant):
		return mkCyan
	case tt.InSubCategory(chroma.NameTag), tt == chroma.GenericDeleted:
		return mkPink
	case tt.InCategory(chroma.Keyword):
		return mkPink
	case tt.InSubCategory(chroma.LiteralString):
		return mkYellow
	case tt.InCategory(chroma.Literal):
		return mkPurple
	case tt.InCategory(chroma.Operator):
		return mkPink
	case tt == chroma.GenericInserted:
		return mkGreen
	case tt.InCategory(chroma.Generic):
		return mkComment
	}
	return mkFg
}

// highlightWithLexer lexes src with lx and returns the styled runs per
// source line. A nil lexer comes back as plain runs.
func highlightWithLexer(lx chroma.Lexer, src string) [][]styledText {
	lines := [][]styledText{{}}
	add := func(text string, fg lipgloss.TerminalColor, bold, italic bool) {
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
		if currentTheme == "terminal" {
			fg = terminalTokenColor(tok.Type)
		} else if e.Colour.IsSet() {
			fg = lipgloss.Color(e.Colour.String())
		}
		add(tok.Value, fg, e.Bold == chroma.Yes, e.Italic == chroma.Yes)
	}
	return lines
}

// highlightLines lexes src by the language implied by path and returns the
// styled runs per source line. Unknown languages come back as plain runs.
func highlightLines(path, src string) [][]styledText {
	return highlightWithLexer(diffLexer(path), src)
}

// langLexer resolves a markdown fence language (go, py, js, ...) to a chroma
// lexer. Unknown and empty languages return nil (plain rendering).
var (
	langLexerMu sync.Mutex
	langLexers  = map[string]chroma.Lexer{}
)

func langLexer(lang string) chroma.Lexer {
	if fields := strings.Fields(lang); len(fields) > 0 {
		lang = fields[0]
	} else {
		lang = ""
	}
	lang, _, _ = strings.Cut(lang, ",")
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" || lang == "text" {
		return nil
	}
	langLexerMu.Lock()
	defer langLexerMu.Unlock()
	if lx, ok := langLexers[lang]; ok {
		return lx
	}
	lx := lexers.Get(lang)
	langLexers[lang] = lx
	return lx
}

// highlightLinesByLang lexes src as the markdown fence language lang.
func highlightLinesByLang(lang, src string) [][]styledText {
	return highlightWithLexer(langLexer(lang), src)
}
