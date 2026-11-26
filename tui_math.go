package main

import (
	"strings"
	"unicode"
)

// renderMath converts LaTeX math spans ($...$, $$...$$, \(...\), \[...\])
// to plain Unicode text so glamour (goldmark) never sees raw LaTeX. The
// committed reply keeps the original source (copyLastReply copies
// Turn.Reply); this is display-only. Fenced code blocks and inline code
// spans are copied verbatim, and currency/shell dollars ($5, $10 to $20,
// $HOME, ${VAR}) are left untouched by the delimiter guards in
// isMathClose / the opening check.
func renderMath(src string) string {
	r := []rune(src)
	var b strings.Builder
	for i := 0; i < len(r); {
		// Fenced block: copy the whole block verbatim. Detected at a line
		// start like extractFencedBlocks (trimmed ``` / ~~~ prefix).
		if isLineStart(r, i) {
			if fence, ok := fenceAt(r, i); ok {
				end := fenceBlockEnd(r, i, fence)
				b.WriteString(string(r[i:end]))
				i = end
				continue
			}
		}
		switch r[i] {
		case '`':
			n := 0
			for i+n < len(r) && r[i+n] == '`' {
				n++
			}
			end := inlineCodeEnd(r, i, n)
			b.WriteString(string(r[i:end]))
			i = end
		case '$':
			if i+1 < len(r) && r[i+1] == '$' {
				if close := findDisplayClose(r, i+2, '$', '$'); close >= 0 {
					b.WriteString(convertMath(string(r[i+2 : close])))
					i = close + 2
				} else {
					b.WriteString("$$")
					i += 2
				}
				continue
			}
			if isInlineOpen(r, i) {
				if close := findInlineClose(r, i+1); close >= 0 {
					b.WriteString(convertMath(string(r[i+1 : close])))
					i = close + 1
					continue
				}
			}
			b.WriteRune('$')
			i++
		case '\\':
			if i+1 < len(r) && r[i+1] == '[' {
				if close := findEscClose(r, i+2, ']'); close >= 0 {
					b.WriteString(convertMath(string(r[i+2 : close])))
					i = close + 2
				} else {
					b.WriteString("\\[")
					i += 2
				}
				continue
			}
			if i+1 < len(r) && r[i+1] == '(' {
				if close := findEscClose(r, i+2, ')'); close >= 0 {
					b.WriteString(convertMath(string(r[i+2 : close])))
					i = close + 2
				} else {
					b.WriteString("\\(")
					i += 2
				}
				continue
			}
			// An escaped character (\$, \`, \...) is copied as a pair so
			// the second char can never be mistaken for a delimiter.
			if i+1 < len(r) {
				b.WriteRune(r[i])
				b.WriteRune(r[i+1])
				i += 2
			} else {
				b.WriteRune(r[i])
				i++
			}
		default:
			b.WriteRune(r[i])
			i++
		}
	}
	return b.String()
}

// isLineStart reports whether i is the first rune of a line.
func isLineStart(r []rune, i int) bool { return i == 0 || r[i-1] == '\n' }

// fenceAt reports whether a fenced-block opener starts at i and returns the
// fence marker (``` or ~~~).
func fenceAt(r []rune, i int) (string, bool) {
	j := i
	for j < len(r) && (r[j] == ' ' || r[j] == '\t') {
		j++
	}
	rest := string(r[j:])
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(rest, f) {
			return f, true
		}
	}
	return "", false
}

// fenceBlockEnd returns the index just past the closing fence line (or the
// end of input for an unclosed fence).
func fenceBlockEnd(r []rune, start int, fence string) int {
	i := start
	// Skip the opening line.
	for i < len(r) && r[i] != '\n' {
		i++
	}
	if i < len(r) {
		i++
	}
	for i < len(r) {
		lineStart := i
		j := i
		for j < len(r) && (r[j] == ' ' || r[j] == '\t') {
			j++
		}
		if strings.HasPrefix(string(r[j:]), fence) {
			for i < len(r) && r[i] != '\n' {
				i++
			}
			if i < len(r) {
				i++
			}
			return i
		}
		_ = lineStart
		for i < len(r) && r[i] != '\n' {
			i++
		}
		if i < len(r) {
			i++
		}
	}
	return len(r)
}

// inlineCodeEnd returns the index just past the closing backtick run of the
// same length (same line only), or past the opening run when unclosed.
func inlineCodeEnd(r []rune, start, n int) int {
	for j := start + n; j < len(r); j++ {
		if r[j] == '\n' {
			break
		}
		if r[j] == '`' {
			m := 0
			for j+m < len(r) && r[j+m] == '`' {
				m++
			}
			if m == n {
				return j + m
			}
			j += m - 1
		}
	}
	return start + n
}

// isInlineOpen applies the opening-$ guards: the next char must exist and
// not be whitespace (so "$ x$" and "$5 ..." style mismatches never open).
func isInlineOpen(r []rune, i int) bool {
	if i+1 >= len(r) {
		return false
	}
	next := r[i+1]
	return !unicode.IsSpace(next)
}

// findInlineClose finds the closing $ for an inline span: not preceded by
// whitespace, not immediately followed by a digit or word char (so "$10 to
// $20", "$HOME and $USER" and "$5-$10" never pair up), and not across a
// blank line. A $-candidate preceded by whitespace is the opener of the
// next span, so the current one is unclosed and the search aborts.
func findInlineClose(r []rune, from int) int {
	for j := from; j < len(r); j++ {
		if r[j] == '\n' {
			k := j + 1
			for k < len(r) && (r[k] == ' ' || r[k] == '\t' || r[k] == '\r') {
				k++
			}
			if k < len(r) && r[k] == '\n' {
				return -1
			}
			continue
		}
		if r[j] != '$' {
			continue
		}
		if j == from {
			continue
		}
		if unicode.IsSpace(r[j-1]) {
			return -1
		}
		if j+1 < len(r) && isWordChar(r[j+1]) {
			continue
		}
		return j
	}
	return -1
}

// findDisplayClose finds a two-rune display delimiter ($$ or \]) starting
// at from; display math may span lines.
func findDisplayClose(r []rune, from int, a, b rune) int {
	for j := from; j+1 < len(r); j++ {
		if r[j] == a && r[j+1] == b {
			return j
		}
	}
	return -1
}

// findEscClose finds "\x" (e.g. "\]" or "\)") starting at from.
func findEscClose(r []rune, from int, closer rune) int {
	for j := from; j+1 < len(r); j++ {
		if r[j] == '\\' && r[j+1] == closer {
			return j
		}
	}
	return -1
}

func isWordChar(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_'
}

// convertMath converts one math span's inner text to Unicode. It collapses
// whitespace runs to single spaces and escapes markdown-significant
// characters so glamour can't re-read them as inline markup.
func convertMath(inner string) string {
	out := convertLatex([]rune(inner))
	out = strings.Join(strings.Fields(out), " ")
	if out == "" {
		return ""
	}
	return escapeMathMarkdown(out)
}

// escapeMathMarkdown backslash-escapes characters goldmark would turn into
// emphasis, links or code inside the converted math.
func escapeMathMarkdown(s string) string {
	return strings.NewReplacer(
		"_", `\_`,
		"*", `\*`,
		"`", "\\`",
		"[", `\[`,
		"]", `\]`,
		"~", `\~`,
		"<", `\<`,
		">", `\>`,
		"&", `\&`,
		"|", `\|`,
	).Replace(s)
}

// convertLatex is the recursive LaTeX-to-Unicode converter for one span.
func convertLatex(r []rune) string {
	var b strings.Builder
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '\\' && i+1 < len(r) && !unicode.IsLetter(r[i+1]):
			b.WriteString(convertEscaped(r[i+1]))
			i += 2
		case c == '\\':
			j := i + 1
			for j < len(r) && unicode.IsLetter(r[j]) {
				j++
			}
			name := string(r[i+1 : j])
			i = convertCommand(&b, r, j, name)
		case c == '^' || c == '_':
			i = convertScript(&b, r, i)
		case c == '&':
			b.WriteRune(' ')
			i++
		case unicode.IsSpace(c):
			b.WriteRune(' ')
			i++
		default:
			b.WriteRune(c)
			i++
		}
	}
	return b.String()
}

// convertEscaped maps a non-letter escape (\{, \$, \,, \\, ...).
func convertEscaped(next rune) string {
	switch next {
	case '{':
		return "{"
	case '}':
		return "}"
	case '$':
		return "$"
	case '%':
		return "%"
	case '#':
		return "#"
	case '&':
		return "&"
	case '_':
		return "_"
	case '|':
		return "∥"
	case '\\':
		return " "
	case '/', ',', ';', ':', '!', ' ':
		return "" // spacing commands: \, \; \: \! \  \/
	default:
		return string(next)
	}
}

// convertCommand handles "\" + letters; r[j:] is the rest after the name.
// It returns the index to continue from.
func convertCommand(b *strings.Builder, r []rune, j int, name string) int {
	if latexTextCommands[name] {
		arg, next := parseLatexArg(r, j)
		b.WriteString(convertLatex([]rune(arg)))
		return next
	}
	switch name {
	case "frac", "dfrac", "tfrac":
		return convertFrac(b, r, j, false)
	case "binom", "dbinom", "tbinom":
		return convertFrac(b, r, j, true)
	case "sqrt":
		return convertSqrt(b, r, j)
	case "left", "right", "displaystyle", "limits", "nolimits":
		return j // drop the command; the delimiter that follows stays
	case "begin", "end":
		_, next := parseLatexArg(r, j)
		return next
	case "quad", "qquad", "thinspace", "medspace", "thickspace", "enspace", "nobreakspace":
		b.WriteRune(' ')
		return j
	case "hspace", "vspace":
		_, next := parseLatexArg(r, j)
		return next
	}
	if comb, ok := latexAccents[name]; ok {
		arg, next := parseLatexArg(r, j)
		conv := convertLatex([]rune(arg))
		if rr := []rune(conv); len(rr) > 0 {
			b.WriteString(string(rr[:len(rr)-1]))
			b.WriteRune(rr[len(rr)-1])
			b.WriteString(comb)
		}
		return next
	}
	if sym, ok := latexSymbols[name]; ok {
		b.WriteString(sym)
		return j
	}
	// Unknown command: keep the name without the backslash.
	b.WriteString(name)
	return j
}

// convertFrac renders \frac{a}{b} as a/b, parenthesizing compound parts,
// and \binom{n}{k} as (n,k).
func convertFrac(b *strings.Builder, r []rune, j int, binom bool) int {
	a, next := parseLatexArg(r, j)
	bb, next := parseLatexArg(r, next)
	ca, cb := convertLatex([]rune(a)), convertLatex([]rune(bb))
	if binom {
		b.WriteString("(" + ca + "," + cb + ")")
		return next
	}
	b.WriteString(fracPart(ca) + "/" + fracPart(cb))
	return next
}

// fracPart parenthesizes a multi-rune part that contains an operator.
func fracPart(s string) string {
	rs := []rune(s)
	if len(rs) <= 1 {
		return s
	}
	for _, c := range rs {
		switch c {
		case '+', '-', '*', '/', '=', '<', '>', ' ', '≤', '≥', '≠', '×', '·', '→', '⇒':
			return "(" + s + ")"
		}
	}
	return s
}

// convertSqrt renders \sqrt{x} as √x (parenthesized when compound) and
// \sqrt[n]{x} as √[n]x.
func convertSqrt(b *strings.Builder, r []rune, j int) int {
	k := skipLatexSpaces(r, j)
	var nth string
	hasNth := false
	if k < len(r) && r[k] == '[' {
		end := k + 1
		for end < len(r) && r[end] != ']' {
			end++
		}
		if end < len(r) {
			nth = convertLatex(r[k+1 : end])
			hasNth = true
			k = end + 1
		}
	}
	arg, next := parseLatexArg(r, k)
	ca := convertLatex([]rune(arg))
	if hasNth {
		b.WriteString("√[" + nth + "]" + ca)
		return next
	}
	if len([]rune(ca)) == 1 {
		b.WriteString("√" + ca)
	} else {
		b.WriteString("√(" + ca + ")")
	}
	return next
}

// convertScript renders ^... and _... with Unicode super/subscripts when
// every character maps, else falls back to ^(...)/_(...).
func convertScript(b *strings.Builder, r []rune, i int) int {
	arg, next := parseLatexArg(r, i+1)
	conv := convertLatex([]rune(arg))
	mapping := latexSuperscripts
	if r[i] == '_' {
		mapping = latexSubscripts
	}
	rs := []rune(conv)
	mapped := make([]rune, 0, len(rs))
	ok := len(rs) > 0
	for _, c := range rs {
		m, found := mapping[c]
		if !found {
			ok = false
			break
		}
		mapped = append(mapped, m)
	}
	if ok {
		b.WriteString(string(mapped))
	} else {
		b.WriteString(string(r[i]) + "(" + conv + ")")
	}
	return next
}

// parseLatexArg reads one argument: a braced group, a single command, or a
// single rune. It returns the raw argument text and the index after it.
func parseLatexArg(r []rune, pos int) (string, int) {
	pos = skipLatexSpaces(r, pos)
	if pos >= len(r) {
		return "", pos
	}
	if r[pos] == '{' {
		depth := 1
		var inner []rune
		for j := pos + 1; j < len(r); j++ {
			if r[j] == '\\' && j+1 < len(r) && (r[j+1] == '{' || r[j+1] == '}') {
				inner = append(inner, r[j], r[j+1])
				j++
				continue
			}
			if r[j] == '{' {
				depth++
			} else if r[j] == '}' {
				depth--
				if depth == 0 {
					return string(inner), j + 1
				}
			}
			inner = append(inner, r[j])
		}
		return string(inner), len(r)
	}
	if r[pos] == '\\' && pos+1 < len(r) && unicode.IsLetter(r[pos+1]) {
		j := pos + 1
		for j < len(r) && unicode.IsLetter(r[j]) {
			j++
		}
		// Include a following braced group so \hat{x}-style args stay whole.
		if k := skipLatexSpaces(r, j); k < len(r) && r[k] == '{' {
			arg, end := parseLatexArg(r, k)
			_ = arg
			return string(r[pos:end]), end
		}
		return string(r[pos:j]), j
	}
	return string(r[pos]), pos + 1
}

func skipLatexSpaces(r []rune, pos int) int {
	for pos < len(r) && unicode.IsSpace(r[pos]) {
		pos++
	}
	return pos
}

// latexTextCommands are font/text commands whose braced argument is unwrapped.
var latexTextCommands = map[string]bool{
	"text": true, "mathrm": true, "mathbf": true, "mathsf": true,
	"mathtt": true, "mathit": true, "mathcal": true, "mathbb": true,
	"mathfrak": true, "mathnormal": true, "operatorname": true,
	"textbf": true, "textit": true, "textsc": true, "textrm": true,
	"boldsymbol": true, "bm": true,
}

// latexAccents maps accent commands to their combining character.
var latexAccents = map[string]string{
	"hat": "\u0302", "tilde": "\u0303", "bar": "\u0304", "dot": "\u0307",
	"ddot": "\u0308", "vec": "\u20d7", "grave": "\u0300", "acute": "\u0301",
	"check": "\u030c", "breve": "\u0306", "dddot": "\u030b",
}

// latexSuperscripts maps ASCII chars to Unicode super/subscripts.
var latexSuperscripts = map[rune]rune{
	'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶',
	'7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽',
	')': '⁾', 'a': 'ᵃ', 'b': 'ᵇ', 'c': 'ᶜ', 'd': 'ᵈ', 'e': 'ᵉ', 'g': 'ᵍ',
	'h': 'ʰ', 'j': 'ʲ', 'k': 'ᵏ', 'l': 'ˡ', 'm': 'ᵐ', 'n': 'ⁿ', 'o': 'ᵒ',
	'p': 'ᵖ', 'r': 'ʳ', 's': 'ˢ', 't': 'ᵗ', 'u': 'ᵘ', 'v': 'ⱽ', 'w': 'ʷ',
	'x': 'ˣ', 'y': 'ʸ', 'z': 'ᶻ', 'A': 'ᴬ', 'B': 'ᴮ', 'D': 'ᴰ', 'E': 'ᴱ',
	'G': 'ᴳ', 'H': 'ᴴ', 'I': 'ᴵ', 'J': 'ᴶ', 'K': 'ᴷ', 'L': 'ᴸ', 'M': 'ᴹ',
	'N': 'ᴺ', 'O': 'ᴼ', 'P': 'ᴾ', 'R': 'ᴿ', 'S': 'ˢ', 'T': 'ᵀ', 'U': 'ᵁ',
	'V': 'ⱽ', 'W': 'ᵂ',
}

// latexSubscripts maps ASCII chars to Unicode subscripts.
var latexSubscripts = map[rune]rune{
	'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆',
	'7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍',
	')': '₎', 'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'k': 'ₖ', 'l': 'ₗ', 'm': 'ₘ',
	'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 's': 'ₛ', 't': 'ₜ', 'x': 'ₓ',
	'β': 'ᵦ', 'γ': 'ᵧ', 'ρ': 'ᵨ', 'φ': 'ᵩ', 'χ': 'ᵪ',
}

// latexSymbols maps LaTeX commands to their Unicode symbol.
var latexSymbols = map[string]string{
	// Sets, relations, logic.
	"subseteq": "⊆", "subseteqq": "⊆", "supseteq": "⊇", "supseteqq": "⊇",
	"subset": "⊂", "supset": "⊃", "subsetneq": "⊊", "supsetneq": "⊋",
	"nsubset": "⊄", "nsupset": "⊅", "in": "∈", "notin": "∉", "ni": "∋",
	"cup": "∪", "cap": "∩", "setminus": "∖", "emptyset": "∅", "varnothing": "∅",
	"leq": "≤", "le": "≤", "geq": "≥", "ge": "≥", "neq": "≠", "ne": "≠",
	"approx": "≈", "cong": "≅", "equiv": "≡", "sim": "∼", "simeq": "≃",
	"propto": "∝", "ll": "≪", "gg": "≫", "prec": "≺", "succ": "≻",
	"preceq": "⪯", "succeq": "⪰", "models": "⊨", "vdash": "⊢",
	"forall": "∀", "exists": "∃", "nexists": "∄", "neg": "¬", "lnot": "¬",
	"land": "∧", "wedge": "∧", "lor": "∨", "vee": "∨",
	"oplus": "⊕", "otimes": "⊗", "odot": "⊙", "perp": "⊥",
	"mid": "∣", "parallel": "∥", "therefore": "∴", "because": "∵",
	// Arrows.
	"to": "→", "rightarrow": "→", "leftarrow": "←", "Rightarrow": "⇒",
	"Leftarrow": "⇐", "iff": "⇔", "Leftrightarrow": "⇔", "mapsto": "↦",
	"uparrow": "↑", "downarrow": "↓", "nearrow": "↗", "searrow": "↘",
	"swarrow": "↙", "nwarrow": "↖", "Longrightarrow": "⟹",
	"Longleftarrow": "⟸", "longrightarrow": "⟶", "longleftrightarrow": "⟷",
	"hookrightarrow": "↪",
	// Operators and calculus.
	"times": "×", "cdot": "·", "div": "÷", "pm": "±", "mp": "∓",
	"ast": "∗", "star": "⋆", "dagger": "†", "ddagger": "‡", "circ": "∘",
	"bullet": "•", "infty": "∞", "partial": "∂", "nabla": "∇", "hbar": "ħ",
	"sum": "∑", "prod": "∏", "int": "∫", "iint": "∬", "oint": "∮",
	"lim": "lim", "max": "max", "min": "min", "sup": "sup", "inf": "inf",
	"det": "det", "exp": "exp", "log": "log", "ln": "ln", "sin": "sin",
	"cos": "cos", "tan": "tan", "sinh": "sinh", "cosh": "cosh", "tanh": "tanh",
	"arcsin": "arcsin", "arccos": "arccos", "arctan": "arctan",
	// Dots, punctuation, misc.
	"ldots": "…", "dots": "…", "cdots": "⋯", "vdots": "⋮", "ddots": "⋱",
	"prime": "′", "angle": "∠", "triangle": "△", "square": "□",
	"diamond": "◇", "aleph": "ℵ", "ell": "ℓ", "Re": "ℜ", "Im": "ℑ",
	// Greek, lowercase.
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ε",
	"varepsilon": "ϵ", "zeta": "ζ", "eta": "η", "theta": "θ", "vartheta": "ϑ",
	"iota": "ι", "kappa": "κ", "lambda": "λ", "mu": "μ", "nu": "ν",
	"xi": "ξ", "pi": "π", "rho": "ρ", "sigma": "σ", "varsigma": "ς",
	"tau": "τ", "upsilon": "υ", "phi": "φ", "varphi": "ϕ", "chi": "χ",
	"psi": "ψ", "omega": "ω",
	// Greek, capital.
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ",
	"Pi": "Π", "Sigma": "Σ", "Upsilon": "Υ", "Phi": "Φ", "Psi": "Ψ",
	"Omega": "Ω", "Alpha": "Α", "Beta": "Β", "Epsilon": "Ε", "Zeta": "Ζ",
	"Eta": "Η", "Iota": "Ι", "Kappa": "Κ", "Mu": "Μ", "Nu": "Ν",
	"Omicron": "Ο", "Rho": "Ρ", "Tau": "Τ", "Chi": "Χ",
}
