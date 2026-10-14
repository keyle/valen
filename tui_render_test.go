package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestChatCacheIncrementalMatchesFull(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	mkTurn := func(prompt string) Turn {
		return Turn{At: time.Now(), Prompt: prompt, Thinking: "plan", Reply: "reply " + prompt, Usage: Usage{Total: 1}}
	}
	m.session.Turns = []Turn{mkTurn("one"), mkTurn("two")}
	_ = m.renderChat()
	if m.chatCacheTurns != 2 {
		t.Fatalf("cache should hold 2 turns, holds %d", m.chatCacheTurns)
	}
	m.session.Turns = append(m.session.Turns, mkTurn("three"))
	extended := m.renderChat()
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(extended, want) {
			t.Fatalf("extended chat missing %q", want)
		}
	}
	// Toggling thinking rebuilds (cache key change) and round-trips exactly.
	m.showThink = false
	hidden := m.renderChat()
	if strings.Contains(hidden, "plan") {
		t.Fatalf("hidden thinking should collapse:\n%s", hidden)
	}
	m.showThink = true
	reshown := m.renderChat()
	if reshown != extended {
		t.Fatalf("toggle round-trip should rebuild identical output")
	}
	m.viewport.Width = 100
	wide := m.renderChat()
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide chat missing %q", want)
		}
	}
}

// TestTUICommittedTrimCollapsesReasoningKeepsPromptReplyAndTools pins the
// scrollback budget contract: once a turn alone exceeds max_lines, its
// reasoning collapses while the user prompt, tool rows and the full reply
// survive. Replies are never cut, even past the budget.
func TestTUICommittedTrimCollapsesReasoningKeepsPromptReplyAndTools(t *testing.T) {
	limit := 30
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.TUI.MaxLines = &limit
	m.viewport.Width = 80
	var thinking strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&thinking, "reasoning line %d\n", i)
	}
	var reply strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&reply, "reply line %d\n", i)
	}
	m.session.Turns = []Turn{{
		At:       time.Now(),
		Prompt:   "the pristine user request",
		Thinking: thinking.String(),
		Reply:    reply.String(),
		Tools:    []ToolRecord{{Name: "read", Args: `{"path":"foo.go"}`, Output: "package main"}},
	}}

	out := m.renderChat()
	if strings.Contains(out, "reasoning line 199") {
		t.Fatalf("reasoning body must be trimmed past the budget:\n%s", out)
	}
	// Glamour wraps and styles the reply, so compare on whitespace-collapsed plain text.
	plain := strings.Join(strings.Fields(ansi.Strip(out)), " ")
	for _, want := range []string{"pristine user request", "read", "foo.go", "trimmed", "reply line 0 ", "reply line 39"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("priority content %q missing from compact transcript:\n%s", want, out)
		}
	}
	if strings.Contains(plain, "more lines") {
		t.Fatalf("reply must never be cut in the scrollback:\n%s", out)
	}
}

// TestTUICommittedTrimBudgetAcrossTurns verifies the budget drops the oldest
// turns from scrollback with a notice while keeping the newest prompt, and
// that toggling the cap back to unlimited restores every turn (disk history
// stays lossless; the view is what shrinks).
func TestTUICommittedTrimBudgetAcrossTurns(t *testing.T) {
	limit := 20
	m := newTestModel(t, testConfig("https://unused"))
	m.cfg.TUI.MaxLines = &limit
	m.viewport.Width = 80
	for i := 0; i < 6; i++ {
		m.session.Turns = append(m.session.Turns, Turn{
			At: time.Now(), Prompt: fmt.Sprintf("prompt-%d", i), Reply: "ok",
		})
	}
	out := m.renderChat()
	if !strings.Contains(out, "earlier turn(s) trimmed") {
		t.Fatalf("dropped turns need a notice:\n%s", out)
	}
	if !strings.Contains(out, "prompt-5") {
		t.Fatalf("newest turn must survive the budget:\n%s", out)
	}
	if strings.Contains(out, "prompt-0") {
		t.Fatalf("oldest turn should be trimmed at budget %d:\n%s", limit, out)
	}
	unlimited := 0
	m.cfg.TUI.MaxLines = &unlimited
	full := m.renderChat()
	if !strings.Contains(full, "prompt-0") || strings.Contains(full, "trimmed") {
		t.Fatalf("unlimited budget must restore the full transcript:\n%s", full)
	}
}

func TestTUICommittedTrimDefaultsTo1000(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	if got := tuiMaxLines(m.cfg); got != 1000 {
		t.Fatalf("default max_lines=%d, want 1000", got)
	}
	zero := 0
	m.cfg.TUI.MaxLines = &zero
	if got := tuiMaxLines(m.cfg); got != 0 {
		t.Fatalf("explicit 0 should disable trimming, got %d", got)
	}
}

func TestTUIPendingRunningToolsShowNowMarker(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "do things"

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_search", args: `{"query":"go"}`})
	if m.toolStatus != "web_search…" {
		t.Fatalf("single running status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "web_fetch", args: `{"url":"https://x"}`})
	if m.toolStatus != "web_fetch… · 2 running" {
		t.Fatalf("parallel running status=%q", m.toolStatus)
	}

	// The scrollback no longer redraws on spinner ticks, so live rows read
	// "now: <action>" instead of a frozen spinner frame.
	out := m.renderPending(80)
	if strings.Count(out, "now:") != 2 {
		t.Fatalf("both open threads should carry the now marker:\n%s", out)
	}
	if strings.Contains(out, "· running") {
		t.Fatalf("open threads must not use the old running label:\n%s", out)
	}

	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_search", output: "1. release notes"})
	if m.toolStatus != "✓ web_search · 1 still running" {
		t.Fatalf("partial completion status=%q", m.toolStatus)
	}
	out3 := m.renderPending(80)
	if strings.Count(out3, "now:") != 1 {
		t.Fatalf("finished thread must lose its now marker:\n%s", out3)
	}
	if !strings.Contains(out3, "1. release notes") {
		t.Fatalf("finished thread keeps its preview:\n%s", out3)
	}

	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "web_fetch", output: "ok"})
	if m.toolStatus != "✓ web_fetch" {
		t.Fatalf("all-done status=%q", m.toolStatus)
	}
}

// TestTUIStatusShowsPhase: model-side deltas flip the footer from tool state
// to thinking/writing, so a long reasoning round never looks like stuck
// local work.
func TestTUIStatusShowsPhase(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.busy = true

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "shell", args: `{"command":"ls"}`})
	if m.toolStatus != "shell…" {
		t.Fatalf("tool status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "shell", output: "x"})
	if m.toolStatus != "✓ shell" {
		t.Fatalf("done status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "hmm"})
	if m.toolStatus != "Reasoning…" {
		t.Fatalf("thinking status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "text_delta", text: "answer"})
	if m.toolStatus != "writing…" {
		t.Fatalf("writing status=%q", m.toolStatus)
	}
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "read", args: `{"path":"a"}`})
	if m.toolStatus != "read…" {
		t.Fatalf("back-to-tool status=%q", m.toolStatus)
	}
}

func TestAppendThinkingDeltaBoundaries(t *testing.T) {
	// Fragments within one part concatenate exactly (no shredding).
	s, cur := appendThinkingDelta("", "Hel", 0, 0)
	s, cur = appendThinkingDelta(s, "lo", 0, cur)
	if s != "Hello" || cur != 0 {
		t.Fatalf("fragments glued wrong: %q/%d", s, cur)
	}
	// A new part starts on its own line.
	s, cur = appendThinkingDelta(s, "Reading config", 1, cur)
	if s != "Hello\nReading config" || cur != 1 {
		t.Fatalf("part boundary missing newline: %q/%d", s, cur)
	}
	// Same part continues on the same line without another break.
	s, cur = appendThinkingDelta(s, " more", 1, cur)
	if s != "Hello\nReading config more" {
		t.Fatalf("continuation broke: %q", s)
	}
	// No doubled newlines when either side already breaks.
	s, cur = appendThinkingDelta("a\n", "b", 1, 0)
	if s != "a\nb" {
		t.Fatalf("doubled head newline: %q", s)
	}
	s, _ = appendThinkingDelta("a", "\nb", 1, 0)
	if s != "a\nb" {
		t.Fatalf("doubled tail newline: %q", s)
	}
	// Empty deltas are no-ops and don't advance the part.
	s, cur = appendThinkingDelta("a", "", 1, 0)
	if s != "a" || cur != 0 {
		t.Fatalf("empty delta changed state: %q/%d", s, cur)
	}
}

// TestTUIThinkingPartsSeparateLiveLines: the first update of a new reasoning
// part lands on its own line in the live view, matching the committed join of
// final summary parts; fragments that share a part concatenate exactly.
func TestTUIThinkingPartsSeparateLiveLines(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "Checking the repo layout", idx: 0})
	_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: "Reading the config file", idx: 1})
	if m.pendingThinking != "Checking the repo layout\nReading the config file" {
		t.Fatalf("live thinking glued across parts: %q", m.pendingThinking)
	}
	// Same-part fragments concatenate exactly; only a part boundary breaks.
	m2 := newTestModel(t, testConfig("https://unused"))
	_, _ = m2.Update(toolEventMsg{kind: "thinking_delta", text: "Checking the repo layout", idx: 0})
	_, _ = m2.Update(toolEventMsg{kind: "thinking_delta", text: " and more", idx: 0})
	if m2.pendingThinking != "Checking the repo layout and more" {
		t.Fatalf("same-part fragments shredded: %q", m2.pendingThinking)
	}
}

// TestThinkingFragmentsJoinIntoProse: reasoning arrives as short multi-word
// fragments (DeepSeek-style). They must concatenate into one flowing thought,
// not one line per delta.
func TestThinkingFragmentsJoinIntoProse(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	frags := []string{
		"passing", " network=true triggers", " the ask", ". Avoiding",
		" silly", " prompts means", ": don't invoke", " network", "=true",
	}
	for _, frag := range frags {
		_, _ = m.Update(toolEventMsg{kind: "thinking_delta", text: frag, idx: 0})
	}
	want := "passing network=true triggers the ask. Avoiding silly prompts means: don't invoke network=true"
	if m.pendingThinking != want {
		t.Fatalf("reasoning shredded:\n got %q\nwant %q", m.pendingThinking, want)
	}
}

// TestTUICommittedThinkingKeepsStreamedText: the success-path commit must
// keep the streamed thinking buffer for display; replay history keeps the
// exactly-concatenated authoritative reasoning.
func TestTUICommittedThinkingKeepsStreamedText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Checking the repo layout\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" and reading the config\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	if len(m.session.Turns) != 1 {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	turn := m.session.Turns[0]
	if turn.Thinking != "Checking the repo layout and reading the config" {
		t.Fatalf("committed thinking altered: %q", turn.Thinking)
	}
	if turn.Reply != "done" {
		t.Fatalf("reply: %q", turn.Reply)
	}
	// History keeps the exact concatenation for provider replay.
	last := m.history[len(m.history)-1]
	if last.ReasoningContent != "Checking the repo layout and reading the config" {
		t.Fatalf("replay reasoning altered: %q", last.ReasoningContent)
	}
	if !strings.Contains(m.renderChat(), "Checking the repo layout and reading the config") {
		t.Fatalf("scrollback missing thinking:\n%s", m.renderChat())
	}
}

// TestThinkingFragmentsJoinWithinRound: fragments in one round join into a
// single flowing thought instead of one line per delta.
func TestThinkingFragmentsJoinWithinRound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"first round\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" second part\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" third part\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	runSubmit(t, m, "work")
	if len(m.session.Turns) != 1 {
		t.Fatalf("turns: %+v", m.session.Turns)
	}
	if thinking := m.session.Turns[0].Thinking; thinking != "first round second part third part" {
		t.Fatalf("thinking fragments not joined: %q", thinking)
	}
}

func TestThinkDurShowsSubSecond(t *testing.T) {
	for durMs, want := range map[int64]string{
		0:     "",
		1:     " (1ms)",
		342:   " (342ms)",
		1500:  " (1.5s)",
		12000: " (12s)",
		63000: " (1m3s)",
	} {
		if got := thinkDurSuffix(durMs); got != want {
			t.Fatalf("thinkDurSuffix(%d) = %q, want %q", durMs, got, want)
		}
		if durMs > 0 {
			if got := toolDurSuffix(durMs); got != want {
				t.Fatalf("toolDurSuffix(%d) = %q, want %q", durMs, got, want)
			}
		}
	}
}

// TestPendingThinkingHeaderBeforeText: the live thinking header (with
// elapsed) renders from turn start, before any thinking delta arrives, so a
// long tool-only stretch never hides the elapsed. The scrollback carries no
// duplicate status line — live state lives once in the spinner row above the
// chatbox, with the cancel hint pinned to its right edge.
func TestPendingThinkingHeaderBeforeText(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.width = 200
	m.busy = true
	m.pendingPrompt = "do the thing"
	m.pendingSince = time.Now().Add(-63 * time.Second)
	out := m.renderPending(80)
	if !strings.Contains(out, "✦ thinking (1m3s)") {
		t.Fatalf("live header must show elapsed before thinking text:\n%s", out)
	}
	if strings.Contains(out, "Working…") || strings.Contains(out, "esc esc") {
		t.Fatalf("scrollback must not repeat the live status or cancel hint:\n%s", out)
	}
	status := m.statusView()
	if !strings.Contains(status, "Working…") {
		t.Fatalf("spinner row must carry the live status:\n%s", status)
	}
	if !strings.Contains(status, "esc to stop") {
		t.Fatalf("cancel hint must sit in the spinner row:\n%s", status)
	}
	// Line return after the thinking state: header must not share a visual
	// line with body text.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "✦ thinking") && len(strings.TrimSpace(line)) > len("✦ thinking (1m3s)")+4 && strings.Contains(line, "do the thing") {
			t.Fatalf("thinking header glued to body text:\n%s", out)
		}
	}
}

// TestPendingThinkingHeaderHiddenStaysHidden: /think off keeps the live
// thinking state (and its elapsed) out of the pending block.
func TestPendingThinkingHeaderHiddenStaysHidden(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.showThink = false
	m.pendingPrompt = "do the thing"
	m.pendingSince = time.Now().Add(-63 * time.Second)
	m.pendingThinking = "secret plan"
	if out := m.renderPending(80); strings.Contains(out, "✦ thinking") {
		t.Fatalf("hidden thinking must stay out of live view:\n%s", out)
	}
}

// TestCommittedToolOnlyShowsDuration: a turn with tools but no thinking text
// still persists the wall time on a bare thinking header.
func TestCommittedToolOnlyShowsDuration(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.session.Turns = []Turn{{
		At: time.Now(), Prompt: "list it", DurMs: 63000,
		Tools: []ToolRecord{{Name: "shell", Args: "ls", Output: "x"}},
	}}
	out := m.renderChat()
	if !strings.Contains(out, "✦ thinking (1m3s)") {
		t.Fatalf("tool-only turn must persist elapsed:\n%s", out)
	}
}

// TestCommittedPlainTextNoBareHeader: a plain Q&A turn (no thinking, no
// tools) must not gain a bare thinking header just for the timestamp.
func TestCommittedPlainTextNoBareHeader(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.session.Turns = []Turn{{At: time.Now(), Prompt: "hi", Reply: "hello", DurMs: 2500}}
	if out := m.renderChat(); strings.Contains(out, "✦ thinking") {
		t.Fatalf("plain turn must not grow a bare thinking header:\n%s", out)
	}
}

// TestTurnDoneFloorsDuration: even an instant turn commits DurMs >= 1 so the
// persisted thinking header never drops its elapsed suffix.
func TestTurnDoneFloorsDuration(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.pendingSince = time.Now()
	m.session.Turns = nil
	_, _ = m.Update(turnDoneMsg{prompt: "hi", thinking: "plan", answer: "ok"})
	if len(m.session.Turns) != 1 {
		t.Fatalf("turn not committed: %+v", m.session.Turns)
	}
	if m.session.Turns[0].DurMs < 1 {
		t.Fatalf("DurMs must floor at 1, got %d", m.session.Turns[0].DurMs)
	}
	if got := thinkDurSuffix(m.session.Turns[0].DurMs); got == "" {
		t.Fatal("floored duration must render a suffix")
	}
}

// TestPendingToolMatchesByID: two parallel same-name calls keep their own
// outputs — the first-started finishing first must not credit the later row.
func TestPendingToolMatchesByID(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "read two files"

	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "read", args: `{"path":"a"}`, id: "call-a"})
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "read", args: `{"path":"b"}`, id: "call-b"})
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "read", output: "A-content", id: "call-a"})
	if !m.pendingTools[0].done || m.pendingTools[0].output != "A-content" {
		t.Fatalf("call-a row must close with its own output: %+v", m.pendingTools)
	}
	if m.pendingTools[1].done {
		t.Fatalf("call-b must still be open: %+v", m.pendingTools)
	}
}

// TestTUITodoChecklistInScrollback: todowrite rows render the plan as a
// glyph checklist instead of raw JSON — live while running (from args) and
// committed (from the stored result).
func TestTUITodoChecklistInScrollback(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80
	m.busy = true
	m.pendingPrompt = "slice it"

	args := `{"todos":[{"content":"Slice it","status":"completed","priority":"high"},{"content":"Wire UI","status":"in_progress","priority":"medium"}]}`
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "todowrite", args: args})
	live := m.renderPending(80)
	for _, want := range []string{"TODO", "[x] Slice it", "[.] Wire UI"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live todowrite must checklist %q:\n%s", want, live)
		}
	}
	if strings.Contains(live, `"todos"`) {
		t.Fatalf("live todowrite must not dump raw JSON:\n%s", live)
	}

	out := `[{"content":"Slice it","status":"completed","priority":"high"},{"content":"Wire UI","status":"in_progress","priority":"medium"}]`
	_, _ = m.Update(toolEventMsg{kind: "tool_result", text: "todowrite", output: out})
	done := m.renderPending(80)
	for _, want := range []string{"[x] Slice it", "[.] Wire UI"} {
		if !strings.Contains(done, want) {
			t.Fatalf("finished todowrite must checklist %q:\n%s", want, done)
		}
	}

	row := m.renderToolLine(ToolRecord{Name: "todowrite", Args: args, Output: out})
	for _, want := range []string{"TODO", "[x] Slice it", "[.] Wire UI"} {
		if !strings.Contains(row, want) {
			t.Fatalf("committed todowrite must checklist %q:\n%s", want, row)
		}
	}
	if strings.Contains(row, `"todos"`) {
		t.Fatalf("committed todowrite must not dump raw JSON:\n%s", row)
	}

	// Non-todo tools keep the default row.
	plain := m.renderToolLine(ToolRecord{Name: "shell", Args: `{"command":"ls"}`, Output: "main.go"})
	if !strings.Contains(plain, "main.go") {
		t.Fatalf("other tools must keep default preview:\n%s", plain)
	}
}

// TestTUIToolSummariesInScrollback: file tools render plain-English headers
// (no raw JSON); edits render the old/new pair as a -/+ diff.
func TestTUIToolSummariesInScrollback(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.viewport.Width = 80

	editArgs := `{"path":"foo.go","oldString":"a := 1\nb := 2","newString":"a := 1\nb := 3"}`
	// Strip styling: chroma/lipgloss split diff text with ANSI runs when
	// the terminal reports color support, so assert on visible text.
	row := ansi.Strip(m.renderToolLine(ToolRecord{Name: "edit", Args: editArgs, Output: "edited foo.go"}))
	for _, want := range []string{"edit", "foo.go", "- b := 2", "+ b := 3"} {
		if !strings.Contains(row, want) {
			t.Fatalf("edit row must show diff %q:\n%s", want, row)
		}
	}
	if strings.Contains(row, "oldString") {
		t.Fatalf("edit row must not dump raw JSON:\n%s", row)
	}

	readRow := ansi.Strip(m.renderToolLine(ToolRecord{Name: "read", Args: `{"path":"foo.go","offset":10,"limit":5}`, Output: "File foo.go (20 lines, showing 10-14):\n10: x"}))
	if !strings.Contains(readRow, "foo.go") || strings.Contains(readRow, `"path"`) {
		t.Fatalf("read row must summarize path, not JSON:\n%s", readRow)
	}

	grepRow := ansi.Strip(m.renderToolLine(ToolRecord{Name: "grep", Args: `{"pattern":"foo.*","path":"."}`, Output: "foo.go:1: foo bar"}))
	if !strings.Contains(grepRow, "foo.*") || strings.Contains(grepRow, `"pattern"`) {
		t.Fatalf("grep row must show the pattern, not JSON:\n%s", grepRow)
	}

	// Live running edit shows the same diff under the spinner header.
	m.busy = true
	m.pendingPrompt = "fix it"
	_, _ = m.Update(toolEventMsg{kind: "tool_start", text: "edit", args: editArgs})
	live := ansi.Strip(m.renderPending(80))
	for _, want := range []string{"edit", "now:", "- b := 2", "+ b := 3"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live edit must diff %q:\n%s", want, live)
		}
	}
	if strings.Contains(live, "oldString") {
		t.Fatalf("live edit must not dump raw JSON:\n%s", live)
	}
}

// TestToolSummaryPrefersLocalPaths: an absolute path inside the workdir
// renders workdir-relative in tool headers; outside paths keep the full path.
func TestToolSummaryPrefersLocalPaths(t *testing.T) {
	root := useTempRoot(t)
	inside := filepath.Join(root, "sub", "foo.go")
	for _, name := range []string{"read", "write", "edit"} {
		args := fmt.Sprintf(`{"path":%q}`, inside)
		if got := toolSummary(name, args, 300); got != "sub/foo.go" {
			t.Fatalf("%s inside root = %q, want sub/foo.go", name, got)
		}
	}
	if got := toolSummary("read", `{"path":"foo.go"}`, 300); got != "foo.go" {
		t.Fatalf("relative path = %q, want foo.go", got)
	}
	outside := filepath.Join(filepath.Dir(root), "elsewhere.go")
	if got := toolSummary("read", fmt.Sprintf(`{"path":%q}`, outside), 300); got != outside {
		t.Fatalf("outside root = %q, want %q", got, outside)
	}
	grepArgs := fmt.Sprintf(`{"pattern":"foo","path":%q}`, inside)
	if got := toolSummary("grep", grepArgs, 300); !strings.Contains(got, "in sub/foo.go") {
		t.Fatalf("grep must localize path, got %q", got)
	}
}

func TestTUIUnknownCommandNotices(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.handleCommand("/nope")
	if !m.noticeErr || !strings.Contains(m.notice, "/nope") {
		t.Fatalf("notice=%q err=%v", m.notice, m.noticeErr)
	}
}

func newTestModelWithConfig(t *testing.T, body string) (*tuiModel, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := newSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg, newClient(cfg, "key"), NewRegistry(), session)
	m.ctx = context.Background()
	m.statePath = filepath.Join(dir, "state.json")
	return m, m.statePath
}

func TestTUIModelDirectSwitchPersists(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "test-key")
	m, path := newTestModelWithConfig(t, "model = \"glm-5.3-flash\"\nthinking = \"max\"\nbase_url = \"https://unused\"\n")
	m.handleCommand("/model kimi-k2.6")
	if m.cfg.Model != "kimi-k2.6" {
		t.Fatalf("model=%q notice=%q", m.cfg.Model, m.notice)
	}
	if m.cfg.Thinking != "" {
		t.Fatalf("max is invalid on kimi toggle and must reset, got %q", m.cfg.Thinking)
	}
	st := loadUserState(path)
	if st.Model != "kimi-k2.6" || st.Thinking != "" {
		t.Fatalf("state not persisted: %+v", st)
	}
	if !strings.Contains(m.notice, "Switched") || !strings.Contains(m.notice, "Saved") {
		t.Fatalf("notice=%q", m.notice)
	}
}

func TestTUIModelSwitchSessionOnlyWithoutPath(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.handleCommand("/model glm-5.3")
	if m.cfg.Model != "glm-5.3" {
		t.Fatalf("model=%q", m.cfg.Model)
	}
	if !strings.Contains(m.notice, "session only") {
		t.Fatalf("notice=%q", m.notice)
	}
}

func TestTUIModelPickerListsAndSelects(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"kimi-k3", "glm-5.3", "gpt-5.6-luna"}, filter: ""})
	if m.mode != viewModels || len(m.modelChoices) != 3 {
		t.Fatalf("mode=%v choices=%v", m.mode, m.modelChoices)
	}
	if m.modelChoices[0].id != "glm-5.3" {
		t.Fatalf("choices should be sorted: %v", m.modelChoices)
	}
	text := m.renderViewportContent()
	if !strings.Contains(text, "gpt-5.6-luna") || !strings.Contains(text, "/responses") {
		t.Fatalf("incompatible transport must be marked:\n%s", text)
	}
	m.openSelected() // cursor 0 → glm-5.3
	if m.cfg.Model != "glm-5.3" || m.mode != viewChat {
		t.Fatalf("model=%q mode=%v notice=%q", m.cfg.Model, m.mode, m.notice)
	}
}

func TestTUIModelPickerFavouritesFloatAndToggle(t *testing.T) {
	m, statePath := newTestModelWithConfig(t, "model = \"m\"\nbase_url = \"https://unused\"\n")
	favPath := filepath.Join(filepath.Dir(statePath), userFavouritesName)
	if err := os.WriteFile(favPath, []byte("- gpt-5.6-luna\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"kimi-k3", "glm-5.3", "gpt-5.6-luna"}})
	if m.mode != viewModels || len(m.modelChoices) != 3 {
		t.Fatalf("mode=%v choices=%v", m.mode, m.modelChoices)
	}
	if m.modelChoices[0].id != "gpt-5.6-luna" {
		t.Fatalf("favourite must sort to the top: %v", m.modelChoices)
	}
	if text := ansi.Strip(m.renderModelsList()); !strings.Contains(text, "★") {
		t.Fatalf("favourite must render a star:\n%s", text)
	}

	// Space on the highlighted favourite unstars it and persists.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if m.isFavourite("gpt-5.6-luna") {
		t.Fatal("space must unstar the highlighted model")
	}
	if m.modelChoices[m.cursor].id != "gpt-5.6-luna" {
		t.Fatalf("cursor should stay on the toggled model: cursor=%d choices=%v", m.cursor, m.modelChoices)
	}
	if raw, _ := os.ReadFile(favPath); strings.Contains(string(raw), "gpt-5.6-luna") {
		t.Fatalf("unstar must persist: %q", raw)
	}

	// Space on a plain row stars it, floats it to the top and writes the file.
	m.cursor = 0
	for i, c := range m.modelChoices {
		if c.id == "glm-5.3" {
			m.cursor = i
		}
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if !m.isFavourite("glm-5.3") {
		t.Fatal("space must star glm-5.3")
	}
	if m.modelChoices[0].id != "glm-5.3" {
		t.Fatalf("new favourite must float to top: %v", m.modelChoices)
	}
	if raw, _ := os.ReadFile(favPath); !strings.Contains(string(raw), "- glm-5.3") {
		t.Fatalf("star must persist: %q", raw)
	}

	// Enter still selects the highlighted model.
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.cfg.Model != "glm-5.3" || m.mode != viewChat {
		t.Fatalf("enter must select: model=%q mode=%v", m.cfg.Model, m.mode)
	}
}

func TestTUIModelPickerSearchBoxKeepsFullWidth(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.width = 200
	m.modelChoices = []modelChoice{{id: "alpha"}, {id: "beta"}}
	boxWidth := func() int {
		out := m.renderModelsList()
		line, _, _ := strings.Cut(out, "\n")
		return lipgloss.Width(line)
	}
	m.modelSearch.SetValue("")
	empty := boxWidth()
	m.modelSearch.SetValue("anthropic/claude")
	typed := boxWidth()
	if empty != typed {
		t.Fatalf("search box resized while typing: empty=%d typed=%d", empty, typed)
	}
	// Pinned to the content width (plus the two border cells), like the
	// chatbox, rather than shrinking to the query.
	if want := m.contentWidth(); empty != want && empty != want+2 {
		t.Fatalf("search box width=%d, want content width %d (or +2 border)", empty, want)
	}
}

func TestTUIModelPickerSearchAndEscape(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	_, _ = m.Update(modelsLoadedMsg{ids: []string{"alpha", "j-model", "k-model"}})
	if m.mode != viewModels {
		t.Fatalf("mode=%v", m.mode)
	}
	if text := ansi.Strip(m.renderViewportContent()); !strings.Contains(text, "ctrl+f to filter models") {
		t.Fatalf("picker should render its search box:\n%s", text)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.cursor != 1 || m.modelSearch.Value() != "" {
		t.Fatalf("unfocused j should move down: cursor=%d query=%q", m.cursor, m.modelSearch.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.cursor != 0 {
		t.Fatalf("unfocused k should move up: cursor=%d", m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlF})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.modelSearch.Value() != "j" || len(m.modelChoices) != 1 || m.modelChoices[0].id != "j-model" {
		t.Fatalf("j should filter instead of moving the cursor: query=%q choices=%v cursor=%d", m.modelSearch.Value(), m.modelChoices, m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 0 || m.modelSearch.Value() != "j" {
		t.Fatalf("arrow navigation changed the query or moved past the only result: cursor=%d query=%q", m.cursor, m.modelSearch.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.modelSearch.Value() != "k" || len(m.modelChoices) != 1 || m.modelChoices[0].id != "k-model" {
		t.Fatalf("k should filter instead of moving the cursor: query=%q choices=%v cursor=%d", m.modelSearch.Value(), m.modelChoices, m.cursor)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewModels || m.modelSearch.Focused() || m.modelSearch.Value() != "k" {
		t.Fatalf("first esc should only unfocus the field: mode=%v query=%q", m.mode, m.modelSearch.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.modelSearch.Value() != "k" || m.cursor != 0 {
		t.Fatalf("unfocused j must navigate, not type: query=%q cursor=%d", m.modelSearch.Value(), m.cursor)
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != viewChat {
		t.Fatalf("esc without field focus should close the picker: mode=%v", m.mode)
	}
}

// TestTUICtrlMCSIParses: the disambiguated ctrl+m letter sequences open the
// model picker; the bare mod-less form must keep typing (it is a plain "m").
func TestTUICtrlMCSIParses(t *testing.T) {
	if !ctrlMCSI([]byte("\x1b[109;5u")) || !ctrlMCSI([]byte("\x1b[109;5~")) {
		t.Fatal("ctrl+m letter sequences must parse")
	}
	for _, seq := range [][]byte{
		[]byte("\x1b[109u"),   // no modifier: plain "m"
		[]byte("\x1b[109;2u"), // shift+m, not ours
		[]byte("\x1b[13;5u"),  // ctrl+enter, handled by the enter path
		[]byte("m"),
	} {
		if ctrlMCSI(seq) {
			t.Fatalf("must not parse as ctrl+m: %q", seq)
		}
	}
}

func TestTUICtrlMOpensModelPicker(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	mm, cmd := m.Update([]byte("\x1b[109;5u"))
	m = mm.(*tuiModel)
	if cmd == nil {
		t.Fatal("ctrl+m must start the model fetch")
	}
	if !strings.Contains(m.notice, "Loading models") {
		t.Fatalf("notice=%q", m.notice)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("ctrl+m leaked into input: %q", got)
	}
	// While a question overlay waits, ctrl+m stays swallowed like enter.
	m2 := newTestModel(t, testConfig("https://unused"))
	m2.ask = &askState{phase: "pick"}
	mm, cmd = m2.Update([]byte("\x1b[109;5u"))
	m2 = mm.(*tuiModel)
	if cmd != nil || strings.Contains(m2.notice, "Loading models") {
		t.Fatalf("ctrl+m must not hijack the ask overlay: cmd=%v notice=%q", cmd, m2.notice)
	}
}

// TestTUIOverlayCursorScrollsViewport: cursoring past the visible page must
// scroll the list with it, both directions, instead of losing the cursor.
