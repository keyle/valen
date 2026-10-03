package main

import (
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// Loop guards against two failure modes the doom-loop check misses:
//
//   - Stall: the model explores for dozens of rounds (reads, greps, shells)
//     without ever acting (write/edit/task) — tokens and minutes burn with
//     no code change. Stops the turn at max_stagnant_rounds (default 25).
//   - Dribble: the model emits one tool call per round instead of batching
//     independent calls. After batchNudgeStreak consecutive single-call
//     rounds, one ephemeral user reminder joins history; it is stripped at
//     turn end so later turns stay clean (prompt-only batching, the way
//     opencode's batch tool works, is already in the system prompt — this is
//     the backstop for models that ignore it).
const (
	defaultMaxStagnantRounds = 25
	batchNudgeStreak         = 3
	batchNudgeText           = "[ephemeral nudge — not part of the task] Batch all independent tool calls in the same block instead of one per round; keep dependent calls sequential."
)

// isActingTool reports calls that change code or delegate work: the only
// things that reset the stall counter. Shell stays out — probing heredocs
// and module-cache greps look busy while changing nothing.
func isActingTool(name string) bool {
	switch name {
	case "write", "edit", "task":
		return true
	}
	return false
}

type loopGuards struct {
	stagnantLimit int
	singleStreak  int
	stagnant      int
}

func newLoopGuards(cfg Config) loopGuards {
	limit := cfg.MaxStagnantRounds
	if limit <= 0 {
		limit = defaultMaxStagnantRounds
	}
	return loopGuards{stagnantLimit: limit}
}

// note consumes one round's tool calls. nudge asks the caller to append the
// batching reminder; stop aborts the turn when the stall limit trips.
func (g *loopGuards) note(calls []openai.ToolCall) (nudge bool, stop error) {
	if len(calls) == 0 {
		return false, nil
	}
	for _, tc := range calls {
		// Interactive questions pause both counters: waiting on the human
		// is neither batching failure nor stalling.
		if tc.Function.Name == "AskUserQuestion" {
			return false, nil
		}
		if isActingTool(tc.Function.Name) {
			g.singleStreak, g.stagnant = 0, 0
			return false, nil
		}
	}
	g.stagnant++
	if g.stagnantLimit > 0 && g.stagnant >= g.stagnantLimit {
		return false, stagnantError(g.stagnant)
	}
	if len(calls) == 1 {
		g.singleStreak++
		if g.singleStreak >= batchNudgeStreak {
			g.singleStreak = 0
			return true, nil
		}
	} else {
		g.singleStreak = 0
	}
	return false, nil
}

func stagnantError(rounds int) error {
	return fmt.Errorf("stopped: %d rounds with tool calls but no write/edit/task — the loop is exploring without acting; narrow the task, delegate exploration to task, or /clear (raise max_stagnant_rounds to allow longer research)", rounds)
}

// stripNudges removes ephemeral batching reminders at turn end so they never
// leak into later turns, the session log, or token estimates. Exact match
// only — user text mentioning batching is untouched.
func stripNudges(history *[]openai.ChatCompletionMessage) {
	h := *history
	out := h[:0]
	for _, m := range h {
		if m.Role == openai.ChatMessageRoleUser && strings.TrimSpace(m.Content) == batchNudgeText {
			continue
		}
		out = append(out, m)
	}
	for i := len(out); i < len(h); i++ {
		h[i] = openai.ChatCompletionMessage{}
	}
	*history = out
}
