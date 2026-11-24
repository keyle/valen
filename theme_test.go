package main

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/lipgloss"
)

func TestLoadConfigTUITheme(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiTheme(cfg); got != "terminal" {
		t.Fatalf("default theme = %q, want terminal", got)
	}
	cfg, err = loadConfig(writeConfig(t, "model = \"m\"\n[tui]\ntheme = \"monokai\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiTheme(cfg); got != "monokai" {
		t.Fatalf("theme = %q, want monokai", got)
	}
	if _, err := loadConfig(writeConfig(t, "model = \"m\"\n[tui]\ntheme = \"solarized\"\n")); err == nil {
		t.Fatal("unknown theme accepted, want error")
	}
}

func TestApplyThemeMonokai(t *testing.T) {
	defer applyTheme("terminal")
	applyTheme("monokai")
	if currentTheme != "monokai" {
		t.Fatalf("currentTheme = %q, want monokai", currentTheme)
	}
	if mkFg != lipgloss.Color("#F8F8F2") || mkPink != lipgloss.Color("#F92672") {
		t.Fatalf("monokai fg/pink = %v/%v, want historic hex", mkFg, mkPink)
	}
	if headerTitleStyle.GetForeground() != mkPink {
		t.Fatal("header title does not use mkPink")
	}
	if glamourStyle() != monokaiGlamour {
		t.Fatal("monokai glamour style changed")
	}
	if diffDelBg != lipgloss.Color("#3B2030") || diffAddBg != lipgloss.Color("#26361C") {
		t.Fatalf("monokai diff bg = %v/%v", diffDelBg, diffAddBg)
	}
	if selectedStyle.GetReverse() {
		t.Fatal("monokai selection should not be reverse")
	}
}

func TestApplyThemeTerminalUsesANSI(t *testing.T) {
	defer applyTheme("terminal")
	applyTheme("terminal")
	if currentTheme != "terminal" {
		t.Fatalf("currentTheme = %q, want terminal", currentTheme)
	}
	if mkFg != (lipgloss.NoColor{}) {
		t.Fatalf("terminal fg = %v, want terminal default (NoColor)", mkFg)
	}
	if mkPink != (lipgloss.AdaptiveColor{Light: "1", Dark: "9"}) {
		t.Fatalf("terminal pink = %v, want adaptive ANSI red", mkPink)
	}
	if mkGreen != (lipgloss.AdaptiveColor{Light: "2", Dark: "10"}) {
		t.Fatalf("terminal green = %v, want adaptive ANSI green", mkGreen)
	}
	if diffDelBg != (lipgloss.NoColor{}) || diffAddBg != (lipgloss.NoColor{}) {
		t.Fatal("terminal diff rows should not tint the background")
	}
	if !selectedStyle.GetReverse() {
		t.Fatal("terminal selection should reverse the terminal fg/bg")
	}
	want := terminalDarkGlamour
	if !lipgloss.HasDarkBackground() {
		want = terminalLightGlamour
	}
	if glamourStyle() != want {
		t.Fatal("terminal glamour style does not match background darkness")
	}
}

func TestTerminalTokenColor(t *testing.T) {
	defer applyTheme("terminal")
	applyTheme("terminal")
	for tt, want := range map[chroma.TokenType]lipgloss.TerminalColor{
		chroma.Keyword:       mkPink,
		chroma.NameFunction:  mkGreen,
		chroma.LiteralString: mkYellow,
		chroma.Comment:       mkComment,
		chroma.Text:          mkFg,
	} {
		if got := terminalTokenColor(tt); got != want {
			t.Fatalf("token %v = %v, want %v", tt, got, want)
		}
	}
}

func TestTerminalMarkdownRenders(t *testing.T) {
	defer applyTheme("terminal")
	cfg := Config{Model: "m"}
	cfg.TUI.Theme = "terminal"
	m := newTUIModel(cfg, nil, nil, nil)
	out := m.renderMarkdown("# Hi\n\nhello **world**\n\n```go\nfunc main() {}\n```\n")
	if out == "" {
		t.Fatal("terminal markdown rendered empty")
	}
	for _, hex := range []string{"#F8F8F2", "#F92672", "#A6E22E"} {
		if strings.Contains(out, hex) {
			t.Fatalf("terminal markdown still uses monokai hex %s", hex)
		}
	}
}

func TestNewTUIModelAppliesTheme(t *testing.T) {
	defer applyTheme("terminal")
	cfg := Config{Model: "m"}
	cfg.TUI.Theme = "monokai"
	_ = newTUIModel(cfg, nil, nil, nil)
	if currentTheme != "monokai" || mkFg != lipgloss.Color("#F8F8F2") {
		t.Fatalf("model did not install monokai palette: theme=%q fg=%v", currentTheme, mkFg)
	}
	_ = newTUIModel(Config{Model: "m"}, nil, nil, nil)
	if currentTheme != "terminal" || mkFg != (lipgloss.NoColor{}) {
		t.Fatalf("default model did not restore terminal: theme=%q fg=%v", currentTheme, mkFg)
	}
}

func TestTerminalGlamourCarriesNoHex(t *testing.T) {
	for _, style := range []string{terminalDarkGlamour, terminalLightGlamour} {
		if strings.Contains(style, "color\": \"#") {
			t.Fatalf("terminal glamour style carries a fixed hex colour: %s", style)
		}
	}
}

func TestExtractFencedBlocks(t *testing.T) {
	stripped, blocks := extractFencedBlocks("# Hi\n\n```go\nfunc main() {}\n```\n\ntail\n")
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if blocks[0].lang != "go" || blocks[0].code != "func main() {}" {
		t.Fatalf("block = %+v, want go/func main() {}", blocks[0])
	}
	if strings.Contains(stripped, "func main") {
		t.Fatal("stripped source still contains the code")
	}
	if !strings.Contains(stripped, fencePlaceholder(0)) || !strings.Contains(stripped, "tail") {
		t.Fatalf("stripped source = %q, want placeholder + tail", stripped)
	}

	stripped, blocks = extractFencedBlocks("plain text, no fences\n")
	if blocks != nil || stripped != "plain text, no fences\n" {
		t.Fatalf("no-fence input altered: %q %v", stripped, blocks)
	}

	stripped, blocks = extractFencedBlocks("x\n\n~~~python extra=1\nprint(1)\n~~~\n")
	if len(blocks) != 1 || blocks[0].lang != "python" || blocks[0].code != "print(1)" {
		t.Fatalf("tilde fence = %v (stripped %q), want python/print(1)", blocks, stripped)
	}

	// Unclosed fence consumes to end of input (trailing newline kept).
	_, blocks = extractFencedBlocks("x\n\n```js\nlet a = 1\n")
	if len(blocks) != 1 || blocks[0].lang != "js" || blocks[0].code != "let a = 1\n" {
		t.Fatalf("unclosed fence = %v, want js/let a = 1\\n", blocks)
	}

	// Placeholder-shaped user text disables extraction so the splice
	// cannot mistake it for a placeholder.
	src := "VALENCODEBLOCK0XQZ\n\n```go\nfunc main() {}\n```\n"
	stripped, blocks = extractFencedBlocks(src)
	if blocks != nil || stripped != src {
		t.Fatalf("collision guard altered input: %q %v", stripped, blocks)
	}
}

func TestHighlightLinesByLangUsesANSI(t *testing.T) {
	defer applyTheme("terminal")
	lines := highlightLinesByLang("go", "func main() { s := \"x\" }")
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	// Every rendered run must carry an ANSI palette colour, never a
	// fixed hex: the terminal theme supplies the hues.
	for _, line := range lines {
		for _, s := range line {
			if _, isHex := s.fg.(lipgloss.Color); isHex {
				t.Fatalf("token %q uses fixed hex %v, want ANSI", s.text, s.fg)
			}
		}
	}
	// Keywords map to the readable pink, functions to green.
	byText := map[string]lipgloss.TerminalColor{}
	for _, line := range lines {
		for _, s := range line {
			byText[s.text] = s.fg
		}
	}
	if byText["func"] != mkPink {
		t.Fatalf("keyword colour = %v, want terminal pink %v", byText["func"], mkPink)
	}
	if byText["main"] != mkGreen {
		t.Fatalf("function colour = %v, want terminal green %v", byText["main"], mkGreen)
	}
}

func TestTerminalMarkdownCodeUsesANSI(t *testing.T) {
	defer applyTheme("terminal")
	cfg := Config{Model: "m"}
	m := newTUIModel(cfg, nil, nil, nil)
	out := m.renderMarkdown("# Hi\n\n```go\nfunc main() { println(\"x\") }\n```\n")
	for _, want := range []string{"func", "main", "println"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered code missing %q: %q", want, out)
		}
	}
	for _, hex := range []string{"#ff0000", "#00ffff", "#00ff00", "#F92672"} {
		if strings.Contains(out, hex) {
			t.Fatalf("terminal code still uses fixed hex %s: %q", hex, out)
		}
	}
	if strings.Contains(out, fencePlaceholder(0)) {
		t.Fatal("placeholder leaked into output")
	}
}

func TestTerminalMarkdownCodeUnknownLangStaysPlain(t *testing.T) {
	defer applyTheme("terminal")
	cfg := Config{Model: "m"}
	m := newTUIModel(cfg, nil, nil, nil)
	out := m.renderMarkdown("```frobnicate\nplain text stays\n```\n")
	if !strings.Contains(out, "plain text stays") {
		t.Fatalf("unknown-language code lost: %q", out)
	}
}

func TestMonokaiMarkdownCodeStillGlamourHex(t *testing.T) {
	defer applyTheme("terminal")
	cfg := Config{Model: "m"}
	cfg.TUI.Theme = "monokai"
	m := newTUIModel(cfg, nil, nil, nil)
	out := m.renderMarkdown("```go\nfunc main() {}\n```\n")
	// Monokai keeps the glamour chroma path: hex colours render as
	// 24-bit SGR sequences, never as the ANSI placeholder token.
	if strings.Contains(out, fencePlaceholder(0)) {
		t.Fatal("monokai block was extracted as if it were terminal")
	}
	if !strings.Contains(out, "main") {
		t.Fatalf("monokai code lost: %q", out)
	}
}
