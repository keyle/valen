package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderMathBasics(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{`$\text{PH}$`, "PH"},
		{`$\text{P} = \text{NP} = \text{coNP} = \text{PH}$`, "P = NP = coNP = PH"},
		{`$\text{BPP} \subseteq \text{PH}$`, "BPP ⊆ PH"},
		{`$\text{P} \subsetneq \text{EXPTIME}$`, "P ⊊ EXPTIME"},
		{`$\text{P} = \text{PSPACE}$`, "P = PSPACE"},
		{`$\text{P} =
\text{PSPACE}$`, "P = PSPACE"},
		{`$x^2$`, "x²"},
		{`$x_{n+1}$`, "xₙ₊₁"},
		{`$\frac{a}{b}$`, "a/b"},
		{`$\frac{x+1}{y}$`, "(x+1)/y"},
		{`$\sqrt{x}$`, "√x"},
		{`$\sum_{i=0}^{n} i$`, `∑\_(i=0)ⁿ i`},
		{`$$P = NP$$`, "P = NP"},
		{`\(a \le b\)`, "a ≤ b"},
		{`\[a \neq b\]`, "a ≠ b"},
		{`$O(n \log n)$`, "O(n log n)"},
		{`$a < b$`, `a \< b`},
		{`$[0,1]$`, `\[0,1\]`},
	} {
		if got := renderMath(tt.in); got != tt.want {
			t.Errorf("renderMath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRenderMathSkipsCodeAndCurrency(t *testing.T) {
	for _, in := range []string{"$5", "$10 to $20", "$HOME and $USER", "${FOO}", "cost is $x and $y"} {
		if got := renderMath(in); got != in {
			t.Errorf("renderMath(%q) = %q, want untouched", in, got)
		}
	}
	in := "```\n$x$\n```\n\n`$x$` and $y$"
	got := renderMath(in)
	if strings.Count(got, "$x$") != 2 {
		t.Fatalf("fenced/inline code must keep $x$ twice: %q", got)
	}
	if !strings.Contains(got, " y") || strings.Contains(got, "$y$") {
		t.Fatalf("trailing math not converted: %q", got)
	}
}

// Underscores that survive conversion (unmappable subscript) must be
// backslash-escaped so goldmark cannot start emphasis inside math.
func TestRenderMathUnderscoreEscaped(t *testing.T) {
	got := renderMath(`$a_i$`)
	if !strings.Contains(got, `\_`) {
		t.Fatalf("bare underscore leaked: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("unexpected newline: %q", got)
	}
}

const mathSnippetMD = `### 2. Theoretical Computer Science collapses (partially)

• The Polynomial Hierarchy ($\text{PH}$) collapses to $\text{P}$: $\text{P} = \text{NP} = \text{coNP} = \text{PH}$.
• Randomized polynomial time ($\text{BPP}$) collapses to $\text{P}$ (since $\text{BPP} \subseteq \text{PH}$).
• Cryptographic pseudorandomness dies: No cryptographically secure PRGs, PRFs, or obfuscation.
• Hardness conjectures fall: The Exponential Time Hypothesis (ETH), Strong ETH, and NP-hardness-based
inapproximability barriers all fail.

What does not collapse in theory:

• The Time Hierarchy Theorem still holds: $\text{P} \subsetneq \text{EXPTIME}$ is unconditionally proved, so
genuinely exponential-time problems remain hard.
• $\text{PSPACE}$ doesn't automatically collapse: $\text{P} = \text{NP}$ does not immediately imply $\text{P} =
\text{PSPACE}$ (games like generalized chess/Go stay hard unless further breakthroughs happen).
• Undecidable problems (like the Halting Problem) remain impossible.`

func TestRenderMarkdownMathSnippet(t *testing.T) {
	defer applyTheme("terminal")
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	out := m.renderMarkdown(mathSnippetMD)
	plain := ansi.Strip(out)
	for _, bad := range []string{"### ", "## ", `$\text`, `\subseteq`, `\subsetneq`, `\text{`, "$\\"} {
		if strings.Contains(plain, bad) {
			t.Fatalf("rendered markdown still contains %q:\n%s", bad, plain)
		}
	}
	for _, want := range []string{"P = NP = coNP = PH", "BPP ⊆ PH", "P ⊊ EXPTIME", "P = PSPACE", "2. Theoretical Computer Science collapses"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, plain)
		}
	}
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		if w := ansi.StringWidth(line); w > m.glamWidth {
			t.Fatalf("line %d width %d exceeds glam width %d: %q\n%s", i, w, m.glamWidth, line, plain)
		}
	}
	// The hardness bullet wraps: its continuation must hang-indent under
	// the bullet text (two spaces) and contain the wrapped tail.
	idx := -1
	for i, line := range lines {
		if strings.Contains(line, "Hardness conjectures fall") {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("hardness bullet missing:\n%s", plain)
	}
	if !strings.HasPrefix(lines[idx], "• ") {
		t.Fatalf("bullet line %d not bullet-led: %q", idx, lines[idx])
	}
	if idx+1 >= len(lines) || !strings.HasPrefix(lines[idx+1], "  ") {
		t.Fatalf("continuation not hanging-indented: %q", lines[idx+1])
	}
	if !strings.Contains(plain, "inapproximability barriers all fail") {
		t.Fatalf("wrapped tail missing:\n%s", plain)
	}
}

func TestRenderMarkdownOrderedListHangingIndent(t *testing.T) {
	defer applyTheme("terminal")
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 60
	out := m.renderMarkdown("1. first ordered item with quite a lot of text that will surely wrap onto another line here\n2. second item\n")
	plain := ansi.Strip(out)
	lines := strings.Split(plain, "\n")
	idx := -1
	for i, line := range lines {
		if strings.Contains(line, "first ordered item") {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("ordered item missing:\n%s", plain)
	}
	if idx+1 < len(lines) && strings.Contains(lines[idx+1], "wrap onto another line") {
		if !strings.HasPrefix(lines[idx+1], "   ") {
			t.Fatalf("ordered continuation not hanging-indented: %q", lines[idx+1])
		}
	}
}

func TestIndentListContinuationsDirect(t *testing.T) {
	in := "• short\nlong continuation without bullet\n• second item\n  nested style line"
	got := indentListContinuations(in, 80)
	if !strings.Contains(got, "\n  long continuation without bullet") {
		t.Fatalf("continuation not indented:\n%s", got)
	}
}
