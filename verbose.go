package main

import (
	"fmt"
	"io"
	"strings"
)

// headlessVerbose streams a one-shot (-p) run's activity to stderr while the
// final answer still lands on stdout (or in done.reply with --stream). The
// default (level 0) shows tool calls and the live answer; level 1 (-v) adds
// round markers and thinking summaries; level 2 (-vv) adds raw
// chain-of-thought, full tool arguments and tool results.
type headlessVerbose struct {
	w    io.Writer
	more bool
	full bool

	open  bool
	kind  string
	index int
	raw   bool

	sawThinking bool
	sawRaw      bool

	answer strings.Builder
	sent   string
}

func newHeadlessVerbose(w io.Writer, level int) *headlessVerbose {
	return &headlessVerbose{w: w, more: level >= 1, full: level >= 2}
}

func (v *headlessVerbose) event(ev Event) {
	if v == nil {
		return
	}
	switch ev.Kind {
	case "round_start":
		if !v.more {
			return
		}
		v.closeLine()
		fmt.Fprintf(v.w, "[%s]\n", ev.Text)
	case "thinking_delta":
		if !v.more {
			return
		}
		if ev.Raw {
			v.sawRaw = true
		} else {
			v.sawThinking = true
		}
		if ev.Raw && !v.full {
			return
		}
		v.stream("thinking", ev.Text, ev.Index, ev.Raw)
	case "text_delta":
		v.answerDelta(ev.Text)
	case "tool_start":
		v.closeLine()
		if v.full {
			fmt.Fprintf(v.w, "[tool] %s %s\n", ev.Text, ev.Args)
		} else {
			fmt.Fprintf(v.w, "[tool] %s\n", summarizeToolCall(ev.Text, ev.Args))
		}
	case "tool_result":
		if !v.full {
			return
		}
		v.closeLine()
		fmt.Fprintf(v.w, "[result] %s %dms\n", ev.Text, ev.DurMs)
		if ev.Output != "" {
			io.WriteString(v.w, ev.Output)
			if !strings.HasSuffix(ev.Output, "\n") {
				fmt.Fprintln(v.w)
			}
		}
	}
}

// answerDelta streams the visible answer, splitting out content-inlined think
// blocks and holding back a trailing partial tag so reasoning never reaches
// stderr as answer text.
func (v *headlessVerbose) answerDelta(text string) {
	v.answer.WriteString(text)
	_, answer := splitThinking(v.answer.String())
	answer = holdThinkPrefix(answer)
	if len(answer) > len(v.sent) && strings.HasPrefix(answer, v.sent) {
		v.stream("answer", answer[len(v.sent):], 0, false)
		v.sent = answer
	}
}

// stream writes one delta as it arrives, opening a labeled line when the
// segment (kind, part index, raw/summary) changes.
func (v *headlessVerbose) stream(kind, text string, index int, raw bool) {
	if text == "" {
		return
	}
	if v.open && (v.kind != kind || v.index != index || v.raw != raw) {
		v.closeLine()
	}
	if !v.open {
		fmt.Fprintf(v.w, "[%s] ", kind)
		v.kind, v.index, v.raw, v.open = kind, index, raw, true
	}
	io.WriteString(v.w, text)
}

func (v *headlessVerbose) closeLine() {
	if v == nil || !v.open {
		return
	}
	fmt.Fprintln(v.w)
	v.open = false
}

// fallbackThinking prints the committed reasoning when the provider ignored
// streaming and no deltas arrived. Raw-only streams stay hidden at -v.
func (v *headlessVerbose) fallbackThinking(thinking string) {
	if v == nil || !v.more || thinking == "" || v.sawThinking || v.sawRaw {
		return
	}
	v.closeLine()
	fmt.Fprintf(v.w, "[thinking] %s\n", thinking)
}
