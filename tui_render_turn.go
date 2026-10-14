package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// committedTurns renders the immutable committed turns from the append-only
// per-turn cache. Delta flushes during streaming reuse the cache, so per-delta
// cost is the budgeted assembly (bounded by max_lines) plus the live pending
// block — not total history depth.
func (m *tuiModel) committedTurns(w int) string {
	if m.session == nil {
		return ""
	}
	n := len(m.session.Turns)
	if m.chatCacheW != w || m.chatCacheThink != m.showThink || m.chatCacheModel != m.cfg.Model || m.chatCacheTurns > n {
		m.chatCache, m.chatCacheTurns = nil, 0
	}
	for m.chatCacheTurns < n {
		full := m.renderTurn(&m.session.Turns[m.chatCacheTurns], w)
		m.chatCache = append(m.chatCache, chatTurnRender{full: full, fullN: lineCount(full)})
		m.chatCacheTurns++
	}
	m.chatCacheW, m.chatCacheThink, m.chatCacheModel = w, m.showThink, m.cfg.Model
	return m.assembleCommitted(tuiMaxLines(m.cfg))
}

const copyLabel = "copy response"

var copyStyle = lipgloss.NewStyle().Foreground(mkComment).Underline(true)

// clipboardWrite sends text to the system clipboard via OSC 52 (works over
// ssh; needs terminal support). A var so tests stay off stdout.
var clipboardWrite = func(s string) { fmt.Fprint(os.Stdout, ansi.SetSystemClipboard(s)) }

// lastReply is the newest turn's reply, "" when it has none.
func (m *tuiModel) lastReply() string {
	if m.session == nil || len(m.session.Turns) == 0 {
		return ""
	}
	return m.session.Turns[len(m.session.Turns)-1].Reply
}

// copyLastReply copies the newest reply (markdown source) and sets a notice.
func (m *tuiModel) copyLastReply() {
	r := m.lastReply()
	if r == "" {
		m.notice, m.noticeErr = "Nothing to copy yet.", true
		return
	}
	clipboardWrite(r)
	m.notice, m.noticeErr = fmt.Sprintf("Copied last reply (%d chars).", len(r)), false
}

// copyClicked reports whether a left press landed on the copy row, which is
// always the last transcript line (header is one row above the viewport).
func (m *tuiModel) copyClicked(msg tea.MouseMsg) bool {
	if m.mode != viewChat || m.pendingPrompt != "" || m.lastReply() == "" ||
		msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return false
	}
	row := msg.Y - 1
	lines := strings.Split(m.viewport.View(), "\n")
	if row < 0 || row >= len(lines) || m.viewport.YOffset+row != m.viewport.TotalLineCount()-1 {
		return false
	}
	return strings.TrimSpace(ansi.Strip(lines[row])) == copyLabel
}

// lineCount is the rendered line count of one turn, used to budget the
// scrollback without splitting the styled string back into lines.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// ensureCompact lazily builds (and caches) a turn's compact scrollback form.
func (m *tuiModel) ensureCompact(i int) {
	p := &m.chatCache[i]
	if p.compactReady {
		return
	}
	p.compact = m.renderTurnCompact(&m.session.Turns[i], m.chatCacheW)
	p.compactN = lineCount(p.compact)
	p.compactReady = true
}

// assembleCommitted joins cached turns under max_lines, newest first. The
// newest turn always survives (full when it fits, else compact); older turns
// stay full while they fit, then collapse to their compact form, then drop.
// A leading notice marks dropped turns. max_lines <= 0 means unlimited.
func (m *tuiModel) assembleCommitted(maxLines int) string {
	n := len(m.chatCache)
	if n == 0 {
		return ""
	}
	parts := m.chatCache
	// Unlimited budget: everything renders full.
	fullFrom, start, used := 0, 0, 0
	if maxLines > 0 {
		newest := n - 1
		if parts[newest].fullN <= maxLines {
			fullFrom = newest
			used = parts[newest].fullN
			for fullFrom > 0 && used+parts[fullFrom-1].fullN <= maxLines {
				fullFrom--
				used += parts[fullFrom].fullN
			}
			start = fullFrom
		} else {
			// A single huge turn: collapse it rather than hand the viewport
			// thousands of lines. fullFrom stays n, so every turn is compact.
			m.ensureCompact(newest)
			fullFrom = n
			start = newest
			used = parts[newest].compactN
		}
		// Older turns join compact while the budget allows.
		for start > 0 {
			m.ensureCompact(start - 1)
			l := parts[start-1].compactN
			if used+l > maxLines {
				break
			}
			start--
			used += l
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString(metaStyle.Render(fmt.Sprintf("… %d earlier turn(s) trimmed — ctrl+r for history", start)))
		b.WriteString("\n\n\n")
	}
	for i := start; i < n; i++ {
		if i > start {
			b.WriteString("\n\n\n")
		}
		if i >= fullFrom {
			b.WriteString(parts[i].full)
		} else {
			b.WriteString(parts[i].compact)
		}
	}
	return b.String()
}

// thinkDurSuffix renders the persisted thinking timestamp, e.g. " (1m3s)".
// Empty when unknown so pre-existing sessions render unchanged.
func thinkDurSuffix(durMs int64) string {
	if durMs <= 0 {
		return ""
	}
	return " (" + formatThinkDur(time.Duration(durMs)*time.Millisecond) + ")"
}

// toolDurSuffix renders per-tool execution time, e.g. " (342ms)", " (1.5s)".
// Empty when unknown so pre-existing sessions render unchanged.
func toolDurSuffix(durMs int64) string {
	if durMs <= 0 {
		return ""
	}
	return " (" + formatToolDur(time.Duration(durMs)*time.Millisecond) + ")"
}

// formatToolDur keeps ms precision under a second and one decimal under a
// minute so short tools still show useful times; longer ones match think.
func formatToolDur(d time.Duration) string {
	return formatThinkDur(d)
}

// formatThinkDur keeps ms precision under a second so short reasoning
// bursts and fast tools show useful times; longer ones round to seconds
// so the header reads 12s / 1m3s, matching the live status elapsed.
func formatThinkDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		s := float64(d) / float64(time.Second)
		if s >= 10 {
			return fmt.Sprintf("%.0fs", s)
		}
		return fmt.Sprintf("%.1fs", s)
	}
	return d.Round(time.Second).String()
}

// liveThinkSuffix is the in-flight elapsed for the pending thinking header;
// empty when the turn start is unknown.
func (m *tuiModel) liveThinkSuffix() string {
	if m.pendingSince.IsZero() {
		return ""
	}
	return " (" + formatThinkDur(m.turnElapsed()) + ")"
}
func (m *tuiModel) renderTurn(t *Turn, w int) string {
	return m.renderTurnMode(t, w, false)
}

// renderTurnCompact is the scrollback form used once the transcript is over
// the max_lines budget: reasoning collapses to a header. The user prompt, tool
// lines and the reply render unchanged, so trimming never cuts a response.
func (m *tuiModel) renderTurnCompact(t *Turn, w int) string {
	return m.renderTurnMode(t, w, true)
}

// userHead renders the "You" header for a turn, suffixed "(PLAN)" and tinted
// cyan when the prompt was sent in plan mode, so a mixed scrollback reads at
// a glance which mode each message belongs to.
func userHead(plan bool, when string) string {
	label, style := "You", userHeaderStyle
	if plan {
		label, style = "You (PLAN)", userHeaderPlanStyle
	}
	return style.Render(label) + "  " + metaStyle.Render(when)
}
func userBody(plan bool) lipgloss.Style {
	if plan {
		return userBodyPlanStyle
	}
	return userBodyStyle
}
func (m *tuiModel) renderTurnMode(t *Turn, w int, compact bool) string {
	var parts []string
	// bodyW is the style width; textW leaves room for the left border plus
	// padding so force-broken words still fit inside the chrome.
	bodyW := min(w-4, 120)
	textW := bodyW - 2

	head := userHead(t.Plan, t.At.Format("15:04"))
	body := userBody(t.Plan).Width(bodyW).Render(breakLongWords(t.Prompt, textW))
	parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, head, "", body))

	if t.Thinking != "" {
		switch {
		case !m.showThink:
			label := "✦ thinking" + thinkDurSuffix(t.DurMs)
			if t.DurMs > 0 {
				label = fmt.Sprintf("%s · %d chars — ctrl+o to expand", label, len(t.Thinking))
			} else {
				label = fmt.Sprintf("%s (%d chars) — ctrl+o to expand", label, len(t.Thinking))
			}
			parts = append(parts, thinkHeaderStyle.Render(label))
		case compact:
			label := fmt.Sprintf("✦ thinking%s · %d chars trimmed — ctrl+o to expand", thinkDurSuffix(t.DurMs), len(t.Thinking))
			parts = append(parts, thinkHeaderStyle.Render(label))
		default:
			thHead := thinkHeaderStyle.Render("✦ thinking" + thinkDurSuffix(t.DurMs))
			thBody := thinkBodyStyle.Width(bodyW).Render(breakLongWords(t.Thinking, textW))
			parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, thHead, thBody))
		}
	} else if m.showThink && t.DurMs > 0 && len(t.Tools) > 0 {
		// Tool-only turn: no thinking text, but the wall time still lands
		// on a bare thinking header so the elapsed persists in scrollback.
		// The "\n\n" join below keeps a line return after it.
		parts = append(parts, thinkHeaderStyle.Render("✦ thinking"+thinkDurSuffix(t.DurMs)))
	}

	for _, tr := range t.Tools {
		parts = append(parts, m.renderToolLine(tr))
	}

	if t.Reply != "" {
		aHead := asstHeadStyle.Render(" ✦ " + m.cfg.Model)
		aBody := asstBodyStyle.Width(bodyW).Render(breakLongWords(m.renderMarkdown(t.Reply), bodyW-1))
		parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, aHead, "", aBody))
	}

	if t.Error != nil {
		parts = append(parts, errorTitleSty.Width(min(w-4, 120)).Render(breakLongWords("✗ "+t.Error.Title, min(w-6, 118)))+"\n"+metaStyle.Render("  ctrl+x, enter for details"))
	}

	return strings.Join(parts, "\n\n")
}

// renderTurnFull always expands thinking and tool output for recall views.
// Content is wrapped to the transcript width so detail views never run
// off-screen.
func (m *tuiModel) renderTurnFull(t *Turn) string {
	var b strings.Builder
	who := "You"
	if t.Plan {
		who = "You (PLAN)"
	}
	b.WriteString(m.wrapPlain(who+" · "+t.At.Format("15:04:05")+"\n"+t.Prompt) + "\n\n")
	if t.Thinking != "" {
		b.WriteString(m.wrapPlain("✦ thinking"+thinkDurSuffix(t.DurMs)+"\n"+t.Thinking) + "\n\n")
	}
	for _, tr := range t.Tools {
		head := fmt.Sprintf("%s  %s%s", tr.Name, toolSummary(tr.Name, tr.Args, 300), toolDurSuffix(tr.DurMs))
		var body strings.Builder
		body.WriteString(head)
		if tr.Name == "edit" {
			if del, add, ok := editDiffLines(tr.Args, 300); ok {
				for _, ln := range del {
					body.WriteString("\n" + ln)
				}
				for _, ln := range add {
					body.WriteString("\n" + ln)
				}
				// The "edited …" confirmation adds nothing after the diff.
				if isEditConfirmation(tr.Output) {
					b.WriteString(m.wrapPlain(body.String()) + "\n\n")
					continue
				}
			}
		} else if tr.Name == "write" {
			if lines, ok := writeDiffLines(tr.Args, 300); ok {
				for _, ln := range lines {
					body.WriteString("\n" + ln)
				}
				if isEditConfirmation(tr.Output) {
					b.WriteString(m.wrapPlain(body.String()) + "\n\n")
					continue
				}
			}
		} else if tr.Name == "todowrite" || tr.Name == "todoread" {
			if lines, ok := todoChecklistLines(tr); ok {
				for _, ln := range lines {
					body.WriteString("\n" + ln)
				}
				b.WriteString(m.wrapPlain(body.String()) + "\n\n")
				continue
			}
		}
		if strings.TrimSpace(tr.Output) != "" {
			body.WriteString("\n" + tr.Output)
		}
		if tr.Err != "" {
			body.WriteString("\nerror: " + tr.Err)
		}
		b.WriteString(m.wrapPlain(body.String()) + "\n\n")
	}
	if t.Error != nil {
		b.WriteString(m.wrapPlain("✗ "+t.Error.Title+"\n"+t.Error.Details) + "\n\n")
	}
	if t.Reply != "" {
		b.WriteString(m.wrapPlain(t.Reply) + "\n")
	}
	return b.String()
}
