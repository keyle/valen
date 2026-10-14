package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// maxEditDiffLines caps the edit/write diff preview so one change never
// floods the transcript; the tail counts the remainder. 16 = 8 del + 8 add.
const maxEditDiffLines = 16

// displayPath renders a tool-call path for compact headers, preferring the
// workdir-local form over a full path when the model passed an absolute path
// inside the root (`read /work/foo.go` reads as `read foo.go`). Paths outside
// the root, and already-relative paths, pass through unchanged.
func displayPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return p
	}
	return relToRoot(p)
}

// toolSummary renders tool args in plain English for scrollback headers
// instead of raw JSON: paths, patterns and commands read as words.
// Unknown tools and unparseable args fall back to truncated raw args.
func toolSummary(name, args string, maxText int) string {
	short := func(s string) string { return truncateRunes(strings.TrimSpace(s), maxText) }
	switch name {
	case "read":
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Path) == "" {
			return short(args)
		}
		p := displayPath(a.Path)
		if a.Offset <= 1 && a.Limit <= 0 {
			return short(p)
		}
		start := a.Offset
		if start < 1 {
			start = 1
		}
		if a.Limit > 0 {
			return short(fmt.Sprintf("%s:%d-%d", p, start, start+a.Limit-1))
		}
		return short(fmt.Sprintf("%s:%d+", p, start))
	case "edit", "write":
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Path) == "" {
			return short(args)
		}
		return short(displayPath(a.Path))
	case "grep":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Include string `json:"include"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Pattern) == "" {
			return short(args)
		}
		var b strings.Builder
		b.WriteString(short(`"` + strings.TrimSpace(a.Pattern) + `"`))
		if p := displayPath(a.Path); p != "" && p != "." {
			b.WriteString(" in " + short(p))
		}
		if inc := strings.TrimSpace(a.Include); inc != "" {
			b.WriteString(" · " + short(inc))
		}
		return truncateRunes(b.String(), maxText)
	case "glob":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Pattern) == "" {
			return short(args)
		}
		s := strings.TrimSpace(a.Pattern)
		if p := displayPath(a.Path); p != "" && p != "." {
			s += " in " + p
		}
		return short(s)
	case "shell":
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Command) == "" {
			return short(args)
		}
		return short(a.Command)
	case "web_search":
		var a struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Query) == "" {
			return short(args)
		}
		return short(a.Query)
	case "web_fetch":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.URL) == "" {
			return short(args)
		}
		return short(a.URL)
	case "task":
		var a struct {
			Description string `json:"description"`
			Prompt      string `json:"prompt"`
		}
		if json.Unmarshal([]byte(args), &a) != nil {
			return short(args)
		}
		if d := strings.TrimSpace(a.Description); d != "" {
			return short(d)
		}
		if p := strings.TrimSpace(a.Prompt); p != "" {
			if i := strings.Index(p, "\n"); i >= 0 {
				p = p[:i]
			}
			return short(p)
		}
		return short(args)
	case "AskUserQuestion":
		var a struct {
			Questions []struct {
				Question string `json:"question"`
				Header   string `json:"header"`
			} `json:"questions"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || len(a.Questions) == 0 {
			return short(args)
		}
		q := strings.TrimSpace(a.Questions[0].Question)
		if q == "" {
			q = strings.TrimSpace(a.Questions[0].Header)
		}
		if q == "" {
			return short(args)
		}
		return short(q)
	default:
		return short(args)
	}
}

// isEditConfirmation reports the stock edit/write success line, which the
// recall view drops once the diff itself is shown.
func isEditConfirmation(out string) bool {
	s := strings.TrimSpace(out)
	return strings.HasPrefix(s, "edited ") || strings.HasPrefix(s, "replaced ") || strings.HasPrefix(s, "wrote ")
}

// editDiffLines renders the oldString/newString pair from an edit call as
// -/+ diff lines, capped at maxEditDiffLines with a +N more tail.
func editDiffLines(args string, maxText int) (del, add []string, ok bool) {
	var a struct {
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || a.OldString == "" && a.NewString == "" {
		return nil, nil, false
	}
	w := maxText - 2
	if w < 10 {
		w = 10
	}
	for _, ln := range strings.Split(a.OldString, "\n") {
		del = append(del, "- "+truncateRunes(ln, w))
	}
	for _, ln := range strings.Split(a.NewString, "\n") {
		add = append(add, "+ "+truncateRunes(ln, w))
	}
	total := len(del) + len(add)
	if total <= maxEditDiffLines {
		return del, add, true
	}
	// Keep the head of each side; the tail counts what was cut.
	keepDel := len(del)
	keepAdd := len(add)
	for keepDel+keepAdd > maxEditDiffLines && (keepDel > 1 || keepAdd > 1) {
		if keepAdd >= keepDel && keepAdd > 1 {
			keepAdd--
		} else if keepDel > 1 {
			keepDel--
		} else {
			break
		}
	}
	del = del[:keepDel]
	add = add[:keepAdd]
	tail := fmt.Sprintf("… +%d more", total-keepDel-keepAdd)
	add = append(add, tail)
	return del, add, true
}

// writeDiffLines renders a write call's content as + diff lines, capped.
func writeDiffLines(args string, maxText int) ([]string, bool) {
	var a struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Content) == "" {
		return nil, false
	}
	w := maxText - 2
	if w < 10 {
		w = 10
	}
	var out []string
	lines := strings.Split(a.Content, "\n")
	for _, ln := range lines {
		out = append(out, "+ "+truncateRunes(ln, w))
		if len(out) >= maxEditDiffLines {
			if len(lines) > len(out) {
				out = append(out, fmt.Sprintf("… +%d more", len(lines)-len(out)))
			}
			break
		}
	}
	return out, true
}
func (m *tuiModel) renderToolLine(tr ToolRecord) string {
	cw := m.contentWidth()
	maxText := cw - 4
	if maxText < 20 {
		maxText = 20
	}
	// Session-plan calls render as a compact checklist instead of raw JSON
	// so todo progress reads like opencode's in the scrollback, both live
	// and committed. Anything unparseable falls through to the default row.
	if tr.Name == "todowrite" || tr.Name == "todoread" {
		if lines, ok := todoChecklistLines(tr); ok {
			out := toolLineStyle.Width(cw).Render(breakLongWords("TODO", cw-2))
			for _, ln := range lines {
				out += "\n" + toolDimStyle.Width(cw).Render(breakLongWords(ln, cw-2))
			}
			return out
		}
	}
	summary := toolSummary(tr.Name, tr.Args, maxText)
	label := fmt.Sprintf("%s  %s%s", tr.Name, summary, toolDurSuffix(tr.DurMs))
	// Tool styles carry PaddingLeft(2), so break two short of cw.
	if tr.Err != "" || strings.HasPrefix(strings.TrimSpace(tr.Output), "error:") {
		return toolErrStyle.Width(cw).Render(breakLongWords("✗ "+label, cw-2))
	}
	// Edits render the old/new pair as a -/+ diff instead of raw JSON.
	if tr.Name == "edit" {
		if del, add, ok := editDiffLines(tr.Args, maxText); ok {
			out := toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(tr.Args), del, cw) {
				out += "\n" + ln
			}
			for _, ln := range highlightDiffRowsCached(diffPath(tr.Args), add, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	if tr.Name == "write" {
		if lines, ok := writeDiffLines(tr.Args, maxText); ok {
			out := toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(tr.Args), lines, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	out := strings.TrimSpace(tr.Output)
	if out != "" {
		first := strings.SplitN(out, "\n", 2)[0]
		first = truncateRunes(first, maxText)
		return toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2)) + "\n" + toolDimStyle.Width(cw).Render(breakLongWords("→ "+first, cw-2))
	}
	return toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
}

// renderPendingTool renders one live tool row. In-flight tools (no result
// yet) read "now: <action>" in yellow; the scrollback no longer redraws on
// every spinner tick, so an animated glyph there would just freeze mid-frame.
// Finished tools render the usual line plus preview.
func (m *tuiModel) renderPendingTool(t pendingTool) string {
	if t.done {
		return m.renderToolLine(ToolRecord{Name: t.name, Args: t.args, Output: t.output, DurMs: t.durMs})
	}
	cw := m.contentWidth()
	maxText := cw - 4
	if maxText < 20 {
		maxText = 20
	}
	// A running todowrite has no result yet, but its args already carry the
	// planned list — show the checklist under the running header.
	if t.name == "todowrite" || t.name == "todoread" {
		if lines, ok := formatTodoChecklist(t.args); ok {
			out := toolRunStyle.Width(cw).Render(breakLongWords("TODO", cw-2))
			for _, ln := range lines {
				out += "\n" + toolDimStyle.Width(cw).Render(breakLongWords(ln, cw-2))
			}
			return out
		}
	}
	summary := toolSummary(t.name, t.args, maxText)
	// A running edit already carries old/new strings — show the diff live.
	if t.name == "edit" {
		if del, add, ok := editDiffLines(t.args, maxText); ok {
			out := toolRunStyle.Width(cw).Render(breakLongWords(fmt.Sprintf("now: %s  %s", t.name, summary), cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(t.args), del, cw) {
				out += "\n" + ln
			}
			for _, ln := range highlightDiffRowsCached(diffPath(t.args), add, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	if t.name == "write" {
		if lines, ok := writeDiffLines(t.args, maxText); ok {
			out := toolRunStyle.Width(cw).Render(breakLongWords(fmt.Sprintf("now: %s  %s", t.name, summary), cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(t.args), lines, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	label := fmt.Sprintf("now: %s  %s", t.name, summary)
	// Tool styles carry PaddingLeft(2), so break two short of cw.
	return toolRunStyle.Width(cw).Render(breakLongWords(label, cw-2))
}
func (m *tuiModel) renderPending(w int) string {
	bodyW := min(w-4, 120)
	textW := bodyW - 2
	head := userHead(m.pendingPlan, "now")
	body := userBody(m.pendingPlan).Width(bodyW).Render(breakLongWords(m.pendingPrompt, textW))
	parts := []string{head, "", body, ""}

	// The thinking header renders from turn start (body follows once text
	// streams in) so the live elapsed is always visible while busy, even
	// across long tool-only stretches with no thinking deltas yet. The
	// trailing blank keeps a line return after the thinking state.
	// Models that stream <think> inside content would otherwise show their
	// reasoning as plain (white) answer text; route it to the thinking style.
	live, answer := m.pendingThinking, m.pendingText
	if strings.Contains(answer, "<think>") {
		t, a := splitThinking(answer)
		answer = a
		if t != "" {
			if live != "" {
				live += "\n"
			}
			live += t
		}
	}
	if m.showThink {
		parts = append(parts, thinkHeaderStyle.Render("✦ thinking"+m.liveThinkSuffix()))
		if live != "" {
			parts = append(parts, thinkBodyStyle.Width(bodyW).Render(breakLongWords(live, textW)))
		}
		parts = append(parts, "")
	}
	for _, t := range m.pendingTools {
		parts = append(parts, m.renderPendingTool(t))
	}
	if len(m.pendingTools) > 0 {
		parts = append(parts, "")
	}
	if answer != "" {
		parts = append(parts, asstHeadStyle.Render(" ✦ "+m.cfg.Model))
		parts = append(parts, "")
		parts = append(parts, asstBodyStyle.Width(bodyW).Render(breakLongWords(streamTail(answer), bodyW-1)))
		parts = append(parts, "")
	}

	// No trailing status line here: the spinner row above the chatbox already
	// carries the live tool/reasoning state, and the cancel hint sits at its
	// right edge. Repeating either in the scrollback doubles every update.
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}
