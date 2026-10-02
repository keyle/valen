package main

import (
	"strings"
)

// splitThinking extracts <think>...</think> blocks so the UI can render them
// collapsed instead of inline with the answer.
func splitThinking(content string) (thinking, answer string) {
	var thoughts []string
	var b strings.Builder
	rest := content
	for {
		i := strings.Index(rest, "<think>")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i+len("<think>"):]
		j := strings.Index(rest, "</think>")
		if j < 0 {
			thoughts = append(thoughts, rest)
			break
		}
		thoughts = append(thoughts, rest[:j])
		rest = rest[j+len("</think>"):]
	}
	return strings.TrimSpace(strings.Join(thoughts, "\n")), strings.TrimSpace(b.String())
}
