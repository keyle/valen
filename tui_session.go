package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sashabaranov/go-openai"
)

// startFreshSession mints a brand-new blank session: a fresh ses-<nano> ID
// with its own JSONL, reseeded history, and all pending/live state dropped
// so memory and token history release. The old session file stays on disk,
// resumable via /sessions. Blocked while busy so an in-flight turn can't
// commit into (or be orphaned by) the fresh session.
func (m *tuiModel) startFreshSession() {
	if m.busy {
		m.notice = "Working… press esc twice to cancel."
		m.noticeErr = false
		return
	}
	m.cfg.SessionID = newSessionID()
	s, err := newSession(m.cfg)
	if err != nil {
		m.notice = "New session failed: " + err.Error()
		m.noticeErr = true
		return
	}
	m.session = s
	m.history = seedHistory(m.cfg)
	resetTodos()
	// SessionID is stamped per-request by headerTransport, so rebuild the
	// clients around the new ID; otherwise turns would keep attributing
	// to the old session server-side.
	m.httpClient = newHTTPClient(m.cfg)
	m.apiKey = effectiveOpenCodeKey(m.cfg, m.apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(m.cfg)
	m.client = newClient(m.cfg, m.apiKey)
	m.pendingPrompt = ""
	m.lastTurnDur = 0
	m.orMeta = nil
	m.orSpent, m.orSpentSet = 0, false
	m.pendingTools = nil
	m.pendingText = ""
	m.pendingThinking = ""
	m.pendingThinkIdx = 0
	m.toolStatus = ""
	m.cancel = nil
	m.chatCache, m.chatCacheTurns = nil, 0
	m.clearInput()
	m.notice = "New session " + m.session.ID + " — blank."
	m.noticeErr = false
	m.show(viewChat)
}

// compactCmd runs a manual compaction off the UI thread: prune older tool
// output locally, then summarize the head with the cheap [compaction] model.
// Send-side only — the session JSONL keeps every turn.
func (m *tuiModel) compactCmd() tea.Cmd {
	if m.busy {
		m.notice = "Working… press esc twice to cancel."
		m.noticeErr = false
		return nil
	}
	if len(m.history) < 3 {
		m.notice = "Nothing to compact — history already small."
		m.noticeErr = false
		return nil
	}
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.cancel = cancel
	m.busy = true
	m.toolStatus = "Compacting…"
	m.notice = ""
	m.pinBottom()

	cfg := m.cfg
	client := m.client
	apiKey := m.apiKey
	httpClient := m.httpClient
	h := append([]openai.ChatCompletionMessage(nil), m.history...)
	before := estimateTokens(h)
	return func() tea.Msg {
		pruned := pruneOldToolOutputs(h)
		summary, ok, err := compactHistory(ctx, cfg, client, apiKey, httpClient, &h)
		return compactDoneMsg{
			summary: summary, err: err, history: h,
			before: before, after: estimateTokens(h),
			pruned: pruned, compacted: ok, model: compactionModel(cfg),
		}
	}
}

func (m *tuiModel) exportTranscript() (string, bool) {
	if m.session == nil {
		return "No session to save.", true
	}
	path := fmt.Sprintf("valen-%s.md", m.session.ID)
	var b strings.Builder
	for _, t := range m.session.Turns {
		fmt.Fprintf(&b, "## You · %s\n\n%s\n\n", t.At.Format("15:04"), t.Prompt)
		if t.Thinking != "" {
			fmt.Fprintf(&b, "<details><summary>thinking%s</summary>\n\n%s\n\n</details>\n\n", thinkDurSuffix(t.DurMs), t.Thinking)
		}
		for _, tr := range t.Tools {
			fmt.Fprintf(&b, "> tool `%s` `%s`%s\n>\n> %s\n\n", tr.Name, truncatePlain(tr.Args, 120), toolDurSuffix(tr.DurMs), truncatePlain(tr.Output, 2000))
		}
		if t.Reply != "" {
			fmt.Fprintf(&b, "%s\n\n", t.Reply)
		}
		if t.Error != nil {
			fmt.Fprintf(&b, "> error: %s\n\n", t.Error.Title)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "Save failed: " + err.Error(), true
	}
	return "Saved " + path, false
}
