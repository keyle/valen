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
// content, then the closing tag, then the answer.
func splitThinking(content string) (thinking, answer string) {
	content = strings.ReplaceAll(content, thinkOpenI, thinkOpen)
	content = strings.ReplaceAll(content, thinkCloseI, thinkClose)
	var thoughts []string
	var b strings.Builder
	rest := content
	for {
		i := strings.Index(rest, thinkOpen)
		if i < 0 {
			if j := strings.Index(rest, thinkClose); j >= 0 {
				if pre := strings.TrimSpace(rest[:j]); pre != "" {
					thoughts = append(thoughts, pre)
				}
				b.WriteString(rest[j+len(thinkClose):])
			} else {
				b.WriteString(rest)
			}
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(thinkOpen):]
		j := strings.Index(rest, thinkClose)
		if j < 0 {
			thoughts = append(thoughts, rest)
			break
		}
		thoughts = append(thoughts, rest[:j])
		rest = rest[j+len(thinkClose):]
	}
	return strings.TrimSpace(strings.Join(thoughts, "\n")), strings.TrimSpace(b.String())
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
