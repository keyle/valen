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
