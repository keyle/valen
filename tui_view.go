package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
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
	vp := m.viewport.View()
	if m.mode == viewChat {
		vp = m.highlightSelection(vp)
	}
	parts := []string{header, vp, status}
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
		noun := "turns"
		if len(m.session.Turns) == 1 {
			noun = "turn"
		}
		mid = "  " + metaStyle.Render(fmt.Sprintf("%d %s · %s/%s · ↑%s ↓%s",
			len(m.session.Turns), noun, fk(estimateTokens(m.history)), fk(contextBudget(m.cfg)),
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
			parts = append(parts, fmt.Sprintf("%.0f TPS", m.orMeta.TPS))
		}
	}
	if p, ok := m.orPricing[m.cfg.Model]; ok {
		parts = append(parts, fmt.Sprintf("in $%.2f/out $%.2f/M", p.prompt*1e6, p.completion*1e6))
	}
	if m.orSpentSet {
		parts = append(parts, formatCost(m.orSpent)+" used")
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
