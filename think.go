package main

import (
	"strings"
)

// Think-tag spellings seen in content streams: the short form and the "-ing"
// form.
const (
	thinkOpen   = "\x3cthink\x3e"
	thinkClose  = "\x3c\x2fthink\x3e"
	thinkOpenI  = "\x3cthinking\x3e"
	thinkCloseI = "\x3c\x2fthinking\x3e"
)

// splitThinking extracts think-tag blocks so the UI can render them collapsed
// instead of inline with the answer. A closing tag with no opening one marks
// implicit reasoning: some models stream their analysis at the start of
// content, then the closing tag, then the answer. Tags inside markdown code
// (inline spans and fenced blocks) are literal mentions, not reasoning: an
// answer discussing think tags writes them in backticks, and treating those
// as blocks would drop everything after the first mention.
func splitThinking(content string) (thinking, answer string) {
	content = strings.ReplaceAll(content, thinkOpenI, thinkOpen)
	content = strings.ReplaceAll(content, thinkCloseI, thinkClose)
	var thoughts []string
	var b strings.Builder
	rest := content
	for {
		i, tag, ok := indexThinkTag(rest)
		if !ok {
			b.WriteString(rest)
			break
		}
		if tag == thinkClose {
			if pre := strings.TrimSpace(rest[:i]); pre != "" {
				thoughts = append(thoughts, pre)
			}
			b.WriteString(rest[i+len(thinkClose):])
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(thinkOpen):]
		// Only a closing tag ends the block: skip any nested opening
		// mention inside the reasoning and keep looking for the close.
		j := -1
		for pos := 0; pos < len(rest); {
			k, tag, ok := indexThinkTag(rest[pos:])
			if !ok {
				break
			}
			if tag == thinkClose {
				j = pos + k
				break
			}
			pos += k + len(thinkOpen)
		}
		if j < 0 {
			thoughts = append(thoughts, rest)
			break
		}
		thoughts = append(thoughts, rest[:j])
		rest = rest[j+len(thinkClose):]
	}
	return strings.TrimSpace(strings.Join(thoughts, "\n")), strings.TrimSpace(b.String())
}

// indexThinkTag finds the first think open or close tag in s that lies
// outside markdown code, returning its offset and which tag it is. Inline
// code (a run of one or two backticks) ends at a matching run or a blank
// line; a fenced block (three or more backticks) ends at the next fence.
func indexThinkTag(s string) (int, string, bool) {
	inInline, inlineRun := false, 0
	inFence := false
	for i := 0; i < len(s); {
		if inInline && i+1 < len(s) && s[i] == '\n' && s[i+1] == '\n' {
			inInline, inlineRun = false, 0
		}
		if s[i] == '`' {
			run := 0
			for i+run < len(s) && s[i+run] == '`' {
				run++
			}
			switch {
			case run >= 3:
				inFence = !inFence
				inInline, inlineRun = false, 0
			case inFence:
			case inInline && run == inlineRun:
				inInline, inlineRun = false, 0
			case !inInline:
				inInline, inlineRun = true, run
			}
			i += run
			continue
		}
		if !inInline && !inFence {
			if strings.HasPrefix(s[i:], thinkOpen) {
				return i, thinkOpen, true
			}
			if strings.HasPrefix(s[i:], thinkClose) {
				return i, thinkClose, true
			}
		}
		i++
	}
	return 0, "", false
}

// hasThinkTag reports whether content may carry a reasoning block, including
// the "-ing" spelling.
func hasThinkTag(content string) bool {
	return strings.Contains(content, thinkOpen) || strings.Contains(content, thinkClose) ||
		strings.Contains(content, thinkOpenI) || strings.Contains(content, thinkCloseI)
}

// holdThinkPrefix trims a trailing fragment that may be the start of a think
// tag, so a stream never paints half a tag as answer text. The trimmed bytes
// return on the next call once the tag completes and the split drops them.
func holdThinkPrefix(s string) string {
	tags := []string{thinkOpen, thinkClose, thinkOpenI, thinkCloseI}
	max := 0
	for _, t := range tags {
		if len(t) > max {
			max = len(t)
		}
	}
	if max > len(s) {
		max = len(s)
	}
	for k := 1; k <= max; k++ {
		tail := s[len(s)-k:]
		for _, t := range tags {
			if strings.HasPrefix(t, tail) {
				return s[:len(s)-k]
			}
		}
	}
	return s
}
