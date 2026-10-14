package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/sashabaranov/go-openai"
)

func sessionLabel(s *Session) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.Title)
}

// fk renders token counts in thousands for the status row: 999 stays raw,
// 275654 becomes 276k.
func fk(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

// parseRoundProgress reads "Round %d/%d" loop-event text for the status row.
func parseRoundProgress(s string) (cur, max int, ok bool) {
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "Round %d/%d", &cur, &max); err != nil || cur <= 0 || max <= 0 {
		return 0, 0, false
	}
	return cur, max, true
}
func (m *tuiModel) historyItems() []historyItem {
	var items []historyItem
	if m.session != nil {
		for i := range m.session.Turns {
			t := m.session.Turns[i]
			items = append(items, historyItem{turnIdx: i, label: truncatePlain(t.Prompt, 64), when: t.At, tokens: t.Usage.Total})
		}
		paths, _ := listSessions()
		for _, p := range paths {
			if p == m.session.Path {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			items = append(items, historyItem{isFile: true, path: p, label: "(saved) " + filepath.Base(p), when: info.ModTime()})
		}
	}
	return items
}
func (m *tuiModel) renderHistoryList() string {
	items := m.historyItems()
	if len(items) == 0 {
		return emptyStyle.Render("No turns yet — your conversation will appear here.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("History — enter opens · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, it := range items {
		line := fmt.Sprintf("%2d  %s  %s", i+1, it.when.Format("15:04:05"), it.label)
		if it.tokens > 0 {
			line += fmt.Sprintf("  (%d tok)", it.tokens)
		}
		line = truncateRunes(line, cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}
func (m *tuiModel) toolItems() []toolItem {
	var items []toolItem
	if m.session == nil {
		return items
	}
	for i := range m.session.Turns {
		for _, tr := range m.session.Turns[i].Tools {
			items = append(items, toolItem{turnIdx: i, record: tr})
		}
	}
	return items
}
func (m *tuiModel) renderToolsList() string {
	items := m.toolItems()
	if len(items) == 0 {
		return emptyStyle.Render("No tool calls yet.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("Tools — enter opens full output · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, it := range items {
		status := "ok"
		if strings.HasPrefix(strings.TrimSpace(it.record.Output), "error:") {
			status = "error"
		}
		line := fmt.Sprintf("%2d  %s  %s  [%s]", i+1, it.record.Name, truncateRunes(it.record.Args, 48), status)
		line = truncateRunes(line, cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}
func (m *tuiModel) sessionItems() []sessionItem {
	paths, _ := listSessions()
	var items []sessionItem
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		it := sessionItem{path: p, when: info.ModTime()}
		if meta, err := loadSessionMeta(metaPath(p)); err == nil {
			it.title, it.model, it.effort = meta.Title, meta.Model, meta.Effort
		}
		if turns, err := loadSession(p); err == nil && len(turns) > 0 {
			it.turns = len(turns)
			if it.title == "" {
				it.title = sessionTitle(turns[0].Prompt)
			}
			// Backfill titles on first listing so old JSONLs show names.
			if it.title != "" {
				if meta, err := loadSessionMeta(metaPath(p)); err != nil || meta.Title == "" {
					model, effort := it.model, it.effort
					if model == "" {
						model = turns[0].Model
					}
					if effort == "" {
						effort = turns[0].Effort
					}
					_ = saveSessionMeta(metaPath(p), SessionMeta{Title: it.title, Model: model, Effort: effort})
					it.model, it.effort = model, effort
				}
			}
		}
		if it.title == "" {
			it.title = strings.TrimSuffix(filepath.Base(p), ".jsonl")
		}
		items = append(items, it)
	}
	return items
}

// resumeSessionPath loads path into the current TUI state: turns become the
// transcript and history is rebuilt so the resumed conversation continues.
func (m *tuiModel) resumeSessionPath(path string) {
	if m.busy {
		m.notice = "Working... press esc twice to cancel before resuming."
		m.noticeErr = false
		return
	}
	turns, err := loadSession(path)
	if err != nil {
		m.notice = "Could not load session: " + err.Error()
		m.noticeErr = true
		return
	}
	m.session = &Session{Path: path, ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Turns: turns}
	m.cfg.SessionID = m.session.ID
	m.httpClient = newHTTPClient(m.cfg)
	m.apiKey = effectiveOpenCodeKey(m.cfg, m.apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(m.cfg)
	m.client = newClient(m.cfg, m.apiKey)
	for _, t := range turns {
		m.session.TotalUsage.Prompt += t.Usage.Prompt
		m.session.TotalUsage.Completion += t.Usage.Completion
		m.session.TotalUsage.Total += t.Usage.Total
	}
	m.history = seedHistory(m.cfg)
	start := 0
	for i := len(turns) - 1; i >= 0; i-- {
		if len(turns[i].History) > 0 {
			m.history = append([]openai.ChatCompletionMessage(nil), turns[i].History...)
			start = i + 1
			break
		}
	}
	for i := start; i < len(turns); i++ {
		t := turns[i]
		m.history = append(m.history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: t.Prompt})
		for j, tr := range t.Tools {
			id := tr.ID
			if id == "" {
				id = fmt.Sprintf("legacy-%d-%d", i, j)
			}
			m.history = append(m.history,
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: tr.Name, Arguments: tr.Args}}}},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: tr.Output})
		}
		if t.Reply != "" {
			m.history = append(m.history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: t.Reply})
		}
	}
	if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
		m.history[0].Content = effectiveSystem(m.cfg)
	}
	restoreTodosFromTurns(turns)
	if meta, err := loadSessionMeta(metaPath(path)); err == nil {
		m.session.Title, m.session.Model, m.session.Effort = meta.Title, meta.Model, meta.Effort
	}
	m.pendingPrompt, m.pendingText, m.pendingThinking, m.toolStatus = "", "", "", ""
	m.orMeta = nil
	m.orSpent, m.orSpentSet = 0, false
	m.pendingTools = nil
	m.cancel = nil
	m.chatCache, m.chatCacheTurns = nil, 0
	m.notice = "Resumed " + m.session.ID + " (" + itoa(len(turns)) + " turns)."
	m.noticeErr = false
	m.show(viewChat)
}
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// errEntry is one row of the errors view: a turn error or a retried failure.
type errEntry struct {
	at             time.Time
	title, details string
}

// errorEntries merges turn errors and retried provider failures by time.
func (m *tuiModel) errorEntries() []errEntry {
	es := append([]errEntry(nil), m.retryErrs...)
	if m.session != nil {
		for _, t := range m.session.Turns {
			if t.Error != nil {
				es = append(es, errEntry{t.At, t.Error.Title, t.Error.Details})
			}
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].at.Before(es[j].at) })
	return es
}
func (m *tuiModel) renderErrorsList() string {
	idx := m.errorEntries()
	if len(idx) == 0 {
		return emptyStyle.Render("No errors — friendly messages will land here, never kill the loop.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(idx) {
		m.cursor = len(idx) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("Errors — enter opens details · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, e := range idx {
		line := truncateRunes(fmt.Sprintf("%2d  %s  %s", i+1, e.at.Format("15:04:05"), singleLine(e.title, 200)), cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}
func (m *tuiModel) renderModelsList() string {
	cw := m.contentWidth()
	// Fixed full-width search box: the outer border is pinned to the content
	// width so typing never resizes it. textinput.View() renders one cell
	// wider once non-empty (the cursor), so the input width leaves one cell
	// of slack for the typed state; lipgloss pads the rest.
	boxW := cw - 2
	if boxW < 14 {
		boxW = 14
	}
	tiW := boxW - 3
	if tiW < 12 {
		tiW = 12
	}
	m.modelSearch.Width = tiW
	searchBox := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(mkBorder).Padding(0, 1).Width(boxW).Render(m.modelSearch.View())

	var b strings.Builder
	b.WriteString(searchBox + "\n\n")
	b.WriteString(metaStyle.Render("Models — ↑/↓/j/k select · enter switches · space favourites · ctrl+f filter · esc back") + "\n\n")
	if len(m.modelChoices) == 0 {
		query := strings.TrimSpace(m.modelFilter)
		if query == "" {
			b.WriteString(emptyStyle.Render("No models to show."))
		} else {
			b.WriteString(emptyStyle.Render(fmt.Sprintf("No models match %q.", query)))
		}
		return b.String()
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.modelChoices) {
		m.cursor = len(m.modelChoices) - 1
	}
	for i, c := range m.modelChoices {
		star := " "
		starCell := " "
		if m.isFavourite(c.id) {
			star = "★"
			starCell = favStyle.Render("★")
		}
		line := displayModelID(c.id)
		if c.current {
			line += "  ● current"
		}
		if c.transport != "chat" {
			line += fmt.Sprintf("  [/%s]", c.transport)
		}
		line = truncateRunes(line, cw-6)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(fmt.Sprintf(" %2d %s %s", i+1, star, line)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %2d %s %s\n", i+1, starCell, boldModelName(c.id, line)))
		}
	}
	b.WriteString("\n" + metaStyle.Render("★ favourites stay on top. Transports are marked: /chat, /responses and /messages all converse."))
	return b.String()
}

// boldModelName renders the model name inside a picker row bright and bold,
// leaving the provider prefix and trailing markers (● current, transport) in
// the terminal default. line must already be truncated: truncateRunes counts
// ANSI escapes, so the style is applied after the fact.
func boldModelName(id, line string) string {
	full := displayModelID(id)
	prefix := ""
	if i := strings.LastIndex(full, "/"); i >= 0 {
		prefix = full[:i+1]
	}
	if !strings.HasPrefix(line, prefix) {
		return line
	}
	rest := line[len(prefix):]
	end := strings.Index(rest, "  ")
	if end < 0 {
		end = len(rest)
	}
	if end == 0 {
		return line
	}
	return prefix + modelNameStyle.Render(rest[:end]) + rest[end:]
}
func effortLabel(level string) string {
	if level == "" {
		return "default (provider)"
	}
	return level
}
func (m *tuiModel) renderEffortList() string {
	if len(m.effortLevels) == 0 {
		return emptyStyle.Render("No thinking levels for this model.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.effortLevels) {
		m.cursor = len(m.effortLevels) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render(fmt.Sprintf("Thinking for %s — enter sets · esc back", m.cfg.Model)) + "\n\n")
	cw := m.contentWidth()
	for i, l := range m.effortLevels {
		line := truncateRunes(effortLabel(l), cw-4)
		if l == m.cfg.Thinking {
			line += "  ● current"
		}
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(fmt.Sprintf(" %2d  %s", i+1, line)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %2d  %s\n", i+1, line))
		}
	}
	return b.String()
}
func (m *tuiModel) renderHelp() string {
	return emptyStyle.Width(m.contentWidth()).Render(`Conversation first: your message shows instantly, the reply streams into the same thread.

Keys
  enter send · tab swaps work/plan mode (cyan box = plan) · shift+enter/alt+enter/ctrl+j newline · esc cancel/back · pgup/pgdn/wheel scroll
  wheel/pgup pauses autoscroll until you reach the bottom again
   shift+mouse drag selects text (opt+drag in iTerm/Terminal) and copies natively, wheel stays on · alt+m toggles plain-drag selection (wheel paused while off)
  ctrl+g help · ctrl+r history · ctrl+l sessions · ctrl+u tools · ctrl+x errors · ctrl+o thinking display
  ctrl+b details · ctrl+p/ctrl+m model · ctrl+n new session · ctrl+t thinking level · ctrl+y routing (cycles none→floor→nitro→exacto) · ctrl+s save · ctrl+q quit · ctrl+c/ctrl+d cancel · clear input · quit
  ↑ on an empty chatbox reloads your last prompt: edit it and enter resends from there
  favourite slots: alt+1…alt+9, alt+0 switch to the model + thinking + routing saved in that slot · alt+shift+1…0 saves the current triple there
  in lists: ↑↓ move · enter open · esc back
  /model picker: ↑↓/j/k select · enter switches · space stars/unstars favourites (kept on top) · ctrl+f filters (esc leaves the field, then backs)

Modes
   WORK (green chatbox) — default: the agent can read, edit, write and run commands.
   PLAN (cyan chatbox) — read-only: the agent investigates and replies with a plan; write/edit/shell/MCP are blocked, task is explore-only.
   Switch with tab or /plan (or start with -plan). Scrollback labels each message "You (PLAN)" in cyan or plain "You" in orange.

Commands
   /help /history /sessions (/resume) /tools /errors /think /save /copy /clear /new /plan /quit (or :q)
   /copy — copy the last reply to the clipboard (OSC 52); clicking the underlined "copy response" under it does the same
   /compact (/summarize) — prune + summarize older history with the cheap model; disk log untouched
   /plan (or tab) — toggle plan mode: read-only investigation, the reply is the plan (write/edit/shell/MCP blocked, task explore-only)
   /model [filter] — list models with provider prefixes (go/…, zen/…, openrouter/…) and switch (ctrl+p/ctrl+m does the same); missing keys report on pick/send, never block boot
  /effort [level] — thinking level for the current model (/thinking is an alias)
  /route [none|floor|nitro|exacto] — OpenRouter provider routing (ctrl+y cycles); a no-op on Go/Zen

When the model calls AskUserQuestion: ↑↓/jk move · enter select · space toggles multi-select · type your own when needed · enter confirms.

Go plan usage (5h rolling / 7d / 30d) shows in the footer, refreshed each
minute and after every turn: dim normally, yellow past 80%, pink at the cap.

Models persist to ~/.config/valen/state.json, so the last switch sticks across runs.
Favourites live beside it in ~/.config/valen/favourites.yaml (a plain list) and
show with a ★ at the top of the picker; space toggles them there.
Favourite slots (10, like weapon slots) live in ~/.config/valen/slots.yaml: pick a
model + thinking level, press alt+shift+<1-9,0> to store it, then alt+<1-9,0> jumps
back to it. Terminals need Option/Alt sent as Meta (macOS Terminal: "Use Option as
Meta key"; iTerm2: Left Option key = Esc+).
Thinking levels depend on the model (see /effort picker): muse-spark takes
minimal/low/medium/high/xhigh, gpt/grok take low…max, glm/deepseek take
low/high/max or high/max, kimi-k3 is max-only, toggle models take none/high.
Unset thinking means the provider default. ctrl+t cycles the list low→high
and wraps to low; unknown models learn valid levels from the provider 400
and cache them on disk.

Thinking is shown by default (ctrl+o hides). Tools render inline; ctrl+u shows full output.

Chatbox chrome lives in config.toml [input]: blink=false parks the block
cursor steady (no blink), height=N sets the box lines (default 3, blinking on).`) + "\n"
}
func (m *tuiModel) renderDetail() string {
	return m.wrapPlain("‹ "+m.detailTitle+" · esc back ›") + "\n\n" + m.wrapPlain(m.detailBody) + "\n"
}
func renderSessionFileText(m *tuiModel, path string) string {
	turns, err := loadSession(path)
	if err != nil {
		return "Could not load: " + err.Error()
	}
	var b strings.Builder
	for i := range turns {
		b.WriteString(m.renderTurnFull(&turns[i]))
		b.WriteString("\n---\n\n")
	}
	return b.String()
}
