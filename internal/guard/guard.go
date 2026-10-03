// Package guard owns the tool-loop guards against two failure modes the
// doom-loop check misses:
//
//   - Stall: the model explores for dozens of rounds (reads, greps, shells)
//     without ever acting (write/edit/task) — tokens and minutes burn with
//     no code change. Stops the turn at the stagnant limit (default 25).
//   - Dribble: the model emits one tool call per round instead of batching
//     independent calls. After BatchStreak consecutive single-call rounds,
//     one ephemeral user reminder joins history; it is stripped at turn end
//     so later turns stay clean.
package guard

import (
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

const (
	// DefaultMaxStagnantRounds stops a turn with tool calls but no
	// write/edit/task. Shell probing doesn't reset it — it looks busy while
	// changing nothing.
	DefaultMaxStagnantRounds = 25
	// BatchStreak fires the batching nudge after this many consecutive
	// single-call rounds.
	BatchStreak = 3
	// BatchNudgeText asks for small purposeful batches; stripped at turn end.
	BatchNudgeText = "[ephemeral nudge — not part of the task] Batch independent tool calls in the same block — small purposeful batches of 2–4 related calls; keep dependent calls sequential."
	// actNudgeStagnant fires one act reminder this many stagnant rounds in,
	// inviting the model to advance before the stall guard aborts the turn.
	// Limits at or under it skip straight to the stop.
	actNudgeStagnant = 15
	// ActNudgeText invites the model to advance; stripped at turn end.
	ActNudgeText = "[ephemeral nudge — not part of the task] You have explored for many rounds without calling write/edit/task. You likely already hold what you need: make the change now, or call AskUserQuestion to state what blocks you."
	// StallRecoveryText is the last-chance instruction appended when the
	// stall limit trips: instead of dying, the turn runs one final round
	// and finishes with whatever text the model produces. Like the nudges
	// it strips at turn end, so the next turn starts with a clean slate.
	StallRecoveryText = "[ephemeral — not part of the task] Stop calling tools now. Based on everything gathered so far, write your final answer: summarize findings and state exactly what edits remain. Do not emit any more tool calls."
)

// IsActingTool reports calls that change code or delegate work: the only
// things that reset the stall counter.
func IsActingTool(name string) bool {
	switch name {
	case "write", "edit", "task":
		return true
	}
	return false
}

// Guards tracks one turn's loop health across rounds.
type Guards struct {
	limit        int
	singleStreak int
	stagnant     int
	actNudged    bool
}

// New starts guards with the given stagnant limit (<=0 means the default).
func New(limit int) Guards {
	if limit <= 0 {
		limit = DefaultMaxStagnantRounds
	}
	return Guards{limit: limit}
}

// Limit reports the stagnant limit.
func (g *Guards) Limit() int { return g.limit }

// Stagnant reports rounds with tool calls but no acting call so far.
func (g *Guards) Stagnant() int { return g.stagnant }

// Note consumes one round's tool calls. nudge asks the caller to append the
// batching reminder, act asks for the act-now reminder; stop aborts the turn
// when the stall limit trips.
func (g *Guards) Note(calls []openai.ToolCall) (nudge, act bool, stop error) {
	if len(calls) == 0 {
		return false, false, nil
	}
	for _, tc := range calls {
		// Interactive questions pause all counters: waiting on the human
		// is neither batching failure, stalling, nor progress.
		if tc.Function.Name == "AskUserQuestion" {
			return false, false, nil
		}
		if IsActingTool(tc.Function.Name) {
			g.singleStreak, g.stagnant, g.actNudged = 0, 0, false
			return false, false, nil
		}
	}
	g.stagnant++
	if g.limit > 0 && g.stagnant >= g.limit {
		return false, false, StagnantError(g.stagnant)
	}
	if !g.actNudged && g.limit > actNudgeStagnant && g.stagnant == actNudgeStagnant {
		g.actNudged = true
		act = true
	}
	if len(calls) == 1 {
		g.singleStreak++
		if g.singleStreak >= BatchStreak {
			g.singleStreak = 0
			return true, act, nil
		}
	} else {
		g.singleStreak = 0
	}
	return false, act, nil
}

// StagnantError aborts a turn that explores without acting.
func StagnantError(rounds int) error {
	return fmt.Errorf("stopped: %d rounds with tool calls but no write/edit/task — the loop is exploring without acting; narrow the task, delegate exploration to task, or /clear (raise max_stagnant_rounds to allow longer research)", rounds)
}

// StripNudges removes ephemeral reminders at turn end so they never leak
// into later turns, the session log, or token estimates. Exact match only —
// user text mentioning batching is untouched.
func StripNudges(history *[]openai.ChatCompletionMessage) {
	h := *history
	out := h[:0]
	for _, m := range h {
		if m.Role == openai.ChatMessageRoleUser {
			if c := strings.TrimSpace(m.Content); c == BatchNudgeText || c == ActNudgeText || c == StallRecoveryText {
				continue
			}
		}
		out = append(out, m)
	}
	for i := len(out); i < len(h); i++ {
		h[i] = openai.ChatCompletionMessage{}
	}
	*history = out
}
