package main

import (
	"strings"
)

// appendStreamText accumulates live deltas, capped so a very long stream
// stays cheap. The committed history message always keeps the full text.
func appendStreamText(s, delta string) string {
	const maxChars = 32768
	if len(delta) >= maxChars {
		s = strings.Clone(delta[len(delta)-maxChars:])
	} else {
		if len(s)+len(delta) > maxChars {
			s = s[len(s)+len(delta)-maxChars:]
		}
		s += delta
	}
	// A byte cap must not leave a partial UTF-8 rune at the start of the tail.
	for len(s) > 0 && s[0]&0xc0 == 0x80 {
		s = s[1:]
	}
	return s
}

// appendThinkingDelta accumulates one reasoning update into the live view.
// Deltas are arbitrary fragments, so fragments that share a part index
// concatenate exactly and the thought stays whole; only the first delta of
// a new part starts on its own line, mirroring the committed newline join
// of separate summary parts. cur tracks the part index of the tail.
func appendThinkingDelta(s, delta string, idx, cur int) (string, int) {
	if delta == "" {
		return s, cur
	}
	if s != "" && idx != cur && !strings.HasSuffix(s, "\n") && !strings.HasPrefix(delta, "\n") {
		s += "\n"
	}
	return appendStreamText(s, delta), idx
}

// roundBreak ends the text streamed so far with a blank line, so the next
// model round starts its own paragraph instead of gluing onto the previous
// round's last sentence. Empty text and text already ending in a blank
// line are left alone.
func roundBreak(s string) string {
	switch {
	case s == "", strings.HasSuffix(s, "\n\n"):
		return s
	case strings.HasSuffix(s, "\n"):
		return s + "\n"
	}
	return s + "\n\n"
}

// Stream chunks are arbitrary token fragments, not lines. Preserve whitespace
// and concatenate exactly; only the capped rolling tail is discarded.
func appendStreamLine(s, delta string) string {
	return appendStreamText(s, delta)
}

// streamTail keeps live transcript renders cheap: only the tail of a long
// streamed reply is shown while it arrives; the full text lands on commit.
// It cuts on a line boundary so the live view stays a list of whole lines.
func streamTail(s string) string {
	const maxRunes = 1500
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	tail := r[len(r)-maxRunes:]
	if i := indexRune(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return "…\n" + string(tail)
}
func indexRune(r []rune, c rune) int {
	for i, v := range r {
		if v == c {
			return i
		}
	}
	return -1
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
