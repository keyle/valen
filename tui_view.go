package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *tuiModel) renderAsk() string {
	if m.ask == nil {
		return ""
	}
	a := m.ask
	cw := m.contentWidth()
	if a.phase == "review" {
		var b strings.Builder
		b.WriteString(m.wrapAskHint("Confirm your answers — enter submits · esc cancels") + "\n\n")
		answers := m.askAnswers()
		for i, q := range a.qs {
			b.WriteString(m.wrapPlain(q.Question) + "\n" + m.wrapPlain("  → "+answers[i]) + "\n\n")
		}
		b.WriteString(m.wrapAskHint("enter = submit · esc = cancel"))
		return b.String()
	}
	q := a.qs[a.idx]
	var b strings.Builder
	fmt.Fprintf(&b, "Question %d of %d", a.idx+1, len(a.qs))
	if q.Header != "" {
		fmt.Fprintf(&b, " · %s", q.Header)
	}
	if q.MultiSelect {
		b.WriteString(" · multi-select (space toggles)")
	}
	b.WriteString("\n\n" + m.wrapPlain(q.Question) + "\n")
	for _, h := range q.Highlight {
		b.WriteString("\n" + accessSubjectStyle.Width(cw).Render(breakLongWords(h, cw)) + "\n")
	}
	b.WriteString("\n")
	if a.phase == "own" {
		b.WriteString(m.wrapPlain(fmt.Sprintf("Type your answer: %s▏\n\n  enter keeps it · esc back", a.own)))
		return b.String()
	}
	for j, o := range q.Options {
		row := fmt.Sprintf("%d. %s", j+1, o.Label)
		if o.Description != "" {
			row += " — " + o.Description
		}
		if q.MultiSelect {
			row = "[" + map[bool]string{true: "x", false: " "}[a.picks[a.idx][j]] + "] " + row
		}
		row = breakLongWords(row, cw-2)
		if j == a.cursor {
			rendered := selectedStyle.Width(cw).Render(" " + row)
			a.rowTop = strings.Count(b.String(), "\n")
			a.rowBottom = a.rowTop + strings.Count(rendered, "\n")
			b.WriteString(rendered + "\n")
		} else {
			b.WriteString(m.wrapPlain("  "+row) + "\n")
		}
	}
	if !q.NoFreeText {
		ownRow := breakLongWords(fmt.Sprintf("%d. Type your own answer", len(q.Options)+1), cw-2)
		if a.cursor == len(q.Options) {
			rendered := selectedStyle.Width(cw).Render(" " + ownRow)
			a.rowTop = strings.Count(b.String(), "\n")
			a.rowBottom = a.rowTop + strings.Count(rendered, "\n")
			b.WriteString(rendered + "\n")
		} else {
			b.WriteString(m.wrapPlain("  "+ownRow) + "\n")
		}
	}
	b.WriteString(m.wrapPlain("↑↓/jk move · enter select · esc cancel") + "\n")
	if q.MultiSelect {
		b.WriteString(m.wrapPlain("space toggles"))
	}
	return b.String()
}

// View renders header / viewport / status / input / footer.
func (m *tuiModel) View() string {
	if m.width == 0 && !m.ready {
		// Pre-window fallback used by tests.
		return m.renderViewportContent()
	}
	header := m.headerView()
	status := m.statusView()
	parts := []string{header, m.viewport.View(), status}
	// The picker views (sessions and models) are navigation surfaces: the
	// chatbox and quota footer don't apply there, but the ready/working
	// status stays so the session state remains visible.
	if !m.isPickerMode() {
		parts = append(parts, m.inputView(), m.footerView())
	}
	block := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// Never exceed the terminal height: an extra row scrolls the whole
	// screen up and shifts the status/chatbox off the bottom edge.
	if m.height > 0 {
		if lines := strings.Split(block, "\n"); len(lines) > m.height {
			block = strings.Join(lines[len(lines)-m.height:], "\n")
		}
	}
	// Center the capped column on wide terminals.
	if pad := (m.width - m.columnWidth()) / 2; pad > 0 {
		block = lipgloss.NewStyle().PaddingLeft(pad).Render(block)
	}
	return block
}
func (m *tuiModel) headerView() string {
	title := headerTitleStyle.Render(" ✦ valen")
	// Right edge: the bare model name (provider and vendor prefixes dropped)
	// and the thinking level. The logo carries the version on the left until
	// a session title replaces it.
	model := modelBaseID(m.cfg.Model)
	if m.cfg.Thinking != "" {
		model += " · " + m.cfg.Thinking
	}
	// Routing affinity rides only on OpenRouter models; Go and Zen ignore it.
	if r := normalizeRouting(m.cfg.Routing); r != "" && modelProvider(m.cfg.Model) == ProviderOpenRouter {
		model += " · " + r
	}
	right := headerDimStyle.Render(model)
	// Suffixes after the logo: plan marker and the overlay name. They are
	// reserved when truncating the label so the header never wraps onto a
	// second row (which pushes the whole screen down one line).
	var tail string
	if m.cfg.Plan {
		tail += "  ·  plan"
	}
	switch m.mode {
	case viewHistory:
		tail += "  ·  history"
	case viewSessions:
		tail += "  ·  sessions"
	case viewTools:
		tail += "  ·  tools"
	case viewErrors:
		tail += "  ·  errors"
	case viewHelp:
		tail += "  ·  help"
	case viewModels:
		tail += "  ·  models"
	case viewEffort:
		tail += "  ·  thinking"
	case viewDetail:
		tail += "  ·  " + m.detailTitle
	case viewAsk:
		tail += "  ·  question"
	}
	label := sessionLabel(m.session)
	left := title
	if label != "" {
		if m.width > 0 {
			// Reserve the logo, separator, tail, the model at the right, a
			// one-cell gap and the two-cell right margin.
			reserved := lipgloss.Width(" ✦ valen") + lipgloss.Width("  ·  ") + lipgloss.Width(tail) + lipgloss.Width(right) + 3
			// truncateRunes appends "…" past n, so leave one cell for it.
			label = truncateRunes(label, m.columnWidth()-reserved-1)
		}
		left += headerDimStyle.Render("  ·  " + label)
	} else {
		left += headerDimStyle.Render(" " + version)
	}
	line := left + headerDimStyle.Render(tail)
	if m.width > 0 {
		// One row, always: clip instead of wrapping. The model sits flush
		// right, two cells in, to line up with the footer's right text.
		w := m.columnWidth()
		line += strings.Repeat(" ", max(1, w-lipgloss.Width(line)-lipgloss.Width(right)-2)) + right + "  "
		return lipgloss.NewStyle().Inline(true).MaxWidth(w).Render(line)
	}
	return line + headerDimStyle.Render("  ·  ") + right
}
func (m *tuiModel) statusView() string {
	var left string
	if m.busy {
		elapsed := m.turnElapsed().Round(time.Second)
		tool := m.toolStatus
		if tool == "" {
			tool = "Working…"
		}
		prog := ""
		if m.roundCur > 0 && m.roundMax > 0 {
			prog = fmt.Sprintf(" · round %d/%d", m.roundCur, m.roundMax)
		}
		left = statusBusyStyle.Render(fmt.Sprintf("%s working  %s%s  %s", m.spinner.View(), tool, prog, elapsed))
	} else {
		left = "● ready"
		if m.lastTurnDur > 0 {
			left += " · " + formatThinkDur(m.lastTurnDur)
		}
		left = statusIdleStyle.Render(left)
	}
	var mid string
	if m.notice != "" {
		if m.noticeErr {
			mid = "  " + errNoticeStyle.Render(m.notice)
		} else {
			mid = "  " + noticeStyle.Render(m.notice)
		}
	} else if m.session != nil {
		mid = "  " + metaStyle.Render(fmt.Sprintf("%d turns · %s/%s · ↑%s ↓%s",
			len(m.session.Turns), fk(estimateTokens(m.history)), fk(contextBudget(m.cfg)),
			fk(m.session.TotalUsage.Prompt), fk(m.session.TotalUsage.Completion)))
	}
	right := ""
	if m.showMeta {
		right = metaStyle.Render(fmt.Sprintf("  ·  %s %s", m.cfg.Model, providerBaseURL(m.cfg)))
	}
	if m.busy {
		// Cancel hint lives here, pinned to the right edge directly above
		// the chatbox. The scrollback no longer repeats the status line,
		// so the spinner row above is the single source of live state.
		esc := metaStyle.Render("esc to stop")
		if m.width > 0 {
			avail := m.columnWidth() - 4
			used := lipgloss.Width(left) + lipgloss.Width(mid) + lipgloss.Width(right) + lipgloss.Width(esc)
			if used+2 <= avail {
				esc = strings.Repeat(" ", avail-used) + esc
			} else {
				esc = "  " + esc
			}
		} else {
			esc = "  " + esc
		}
		right += esc
	}
	row := lipgloss.NewStyle().Padding(0, 2).Render(left + mid + right)
	if m.width > 0 {
		// One row, always: a long notice (e.g. the plan-mode switch text)
		// must clip rather than wrap and push the layout down a line.
		row = lipgloss.NewStyle().Inline(true).MaxWidth(m.columnWidth()).Render(row)
	}
	return row
}

// inputBorderColor: orange while working, green when ready (cyan in plan
// mode), dim outside the chat view.
func (m *tuiModel) inputBorderColor() lipgloss.Color {
	switch {
	case m.busy:
		return mkOrange
	case m.mode != viewChat:
		return mkBorder
	case m.cfg.Plan:
		return mkCyan
	default:
		return mkGreen
	}
}
func (m *tuiModel) inputView() string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(m.inputBorderColor()).Padding(0, 1)
	if m.width > 0 {
		style = style.Width(m.columnWidth() - 2)
	}
	return style.Render(m.input.View())
}

// footerDir is the workdir shown in the footer, with $HOME shortened to ~.
func footerDir() string {
	dir := getFileRoot()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+string(filepath.Separator)) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}
func (m *tuiModel) footerView() string {
	hint := footerDir()
	// The quota sits at the right, named by its plan. openrouter/… shows the
	// OpenRouter balance once the credits fetch lands (plain API keys 403, so
	// the footer stays clean rather than erroring). zen/… is pay-as-you-go
	// with no quota, so it shows just its name. Go models show the Go plan
	// quota.
	switch modelProvider(m.cfg.Model) {
	case ProviderOpenRouter:
		var text string
		if m.orCredits != nil {
			text = m.orCredits.footerText()
		}
		text += m.orMetaSuffix()
		if text == "" {
			return footerStyle.Padding(0, 2).Render(hint)
		}
		return m.footerLine(hint, text, footerStyle)
	case ProviderZen:
		return m.footerLine(hint, "Zen", footerStyle)
	}
	if m.goUsage == nil {
		return footerStyle.Padding(0, 2).Render(hint)
	}
	text := "Go · " + m.goUsage.footerText(time.Now())
	if m.width > 0 && lipgloss.Width(hint)+lipgloss.Width(text)+5 > m.columnWidth()-4 {
		// Narrow terminals fall back to the rolling window only.
		text = fmt.Sprintf("Go · 5h %d%%", m.goUsage.Rolling.Percent)
	}
	uStyle := footerStyle
	switch m.goUsage.usageSev() {
	case "warn":
		uStyle = noticeStyle
	case "bad":
		uStyle = errNoticeStyle
	}
	return m.footerLine(hint, text, uStyle)
}

// orMetaSuffix is the best-effort OpenRouter metadata appended after the
// balance: the serving provider, throughput, per-million input/output rates
// and the cumulative billed cost of the session. Each segment is independently
// optional; an empty result adds nothing, and the footer's width rule drops the
// whole text on narrow terminals. It renders even without a balance, since the
// session cost is measured locally from usage, not from the management API.
func (m *tuiModel) orMetaSuffix() string {
	var parts []string
	if m.orMeta != nil {
		if p := strings.TrimSpace(m.orMeta.Provider); p != "" {
			parts = append(parts, p)
		}
		if m.orMeta.TPS > 0 {
			parts = append(parts, fmt.Sprintf("%.0f t/s", m.orMeta.TPS))
		}
	}
	if p, ok := m.orPricing[m.cfg.Model]; ok {
		parts = append(parts, fmt.Sprintf("in $%.2f/out $%.2f/M", p.prompt*1e6, p.completion*1e6))
	}
	if m.orSpentSet {
		parts = append(parts, "sess "+formatCost(m.orSpent))
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

// formatCost renders a billed dollar amount, keeping four decimals below a cent
// so sub-cent turns do not collapse to $0.00 and switching to cents once the
// running total is legible at that scale.
func formatCost(c float64) string {
	if c < 0.01 {
		return fmt.Sprintf("$%.4f", c)
	}
	return fmt.Sprintf("$%.2f", c)
}

// footerLine puts text at the right edge of the column, opposite the hint.
// When the pair won't fit, only the hint shows.
func (m *tuiModel) footerLine(hint, text string, st lipgloss.Style) string {
	if m.width == 0 {
		return footerStyle.Padding(0, 2).Render(hint + "  " + st.Render(text))
	}
	avail := m.columnWidth() - 4
	if lipgloss.Width(hint)+lipgloss.Width(text)+5 > avail {
		return footerStyle.Padding(0, 2).Render(hint)
	}
	line := hint + strings.Repeat(" ", avail-lipgloss.Width(hint)-lipgloss.Width(text)) + st.Render(text)
	return footerStyle.Padding(0, 2).Render(line)
}
func (m *tuiModel) refreshViewport() {
	m.viewport.SetContent(m.renderViewportContent())
}
func (m *tuiModel) renderViewportContent() string {
	switch m.mode {
	case viewHistory:
		return m.renderHistoryList()
	case viewSessions:
		return m.renderSessionsList()
	case viewTools:
		return m.renderToolsList()
	case viewErrors:
		return m.renderErrorsList()
	case viewHelp:
		return m.renderHelp()
	case viewModels:
		return m.renderModelsList()
	case viewEffort:
		return m.renderEffortList()
	case viewDetail:
		return m.renderDetail()
	case viewAsk:
		return m.renderAsk()
	default:
		return m.renderChat()
	}
}
func (m *tuiModel) renderChat() string {
	w := m.viewWidth()
	if m.session == nil || len(m.session.Turns) == 0 {
		m.chatCache, m.chatCacheTurns = nil, 0
		if m.pendingPrompt == "" {
			return ""
		}
	}
	committed := m.committedTurns(w)
	if m.pendingPrompt == "" {
		if m.lastReply() != "" {
			committed += "\n\n" + strings.Repeat(" ", max(m.contentWidth()-len(copyLabel), 0)) + copyStyle.Render(copyLabel)
		}
		return committed
	}
	if committed == "" {
		return m.renderPending(w)
	}
	// Generous vertical padding between turns reads like a conversation.
	return committed + "\n\n\n" + m.renderPending(w)
}

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
		b.WriteString(metaStyle.Render(fmt.Sprintf("… %d earlier turn(s) trimmed — ctrl+h for history", start)))
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
