package guard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func guardCalls(names ...string) []openai.ToolCall {
	out := make([]openai.ToolCall, len(names))
	for i, n := range names {
		out[i] = openai.ToolCall{ID: fmt.Sprintf("c%d", i), Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: n, Arguments: "{}"}}
	}
	return out
}

func TestLoopGuardsNudgeAfterStreak(t *testing.T) {
	g := New(100)
	for i := 0; i < 2; i++ {
		if nudge, _, stop := g.Note(guardCalls("read")); nudge || stop != nil {
			t.Fatalf("round %d: nudge=%v stop=%v", i+1, nudge, stop)
		}
	}
	if nudge, _, stop := g.Note(guardCalls("read")); !nudge || stop != nil {
		t.Fatalf("3rd single must nudge: nudge=%v stop=%v", nudge, stop)
	}
	// Streak resets after firing: next nudge comes after 3 more singles.
	for i := 0; i < 2; i++ {
		if nudge, _, _ := g.Note(guardCalls("read")); nudge {
			t.Fatalf("early re-nudge at +%d", i+1)
		}
	}
	if nudge, _, _ := g.Note(guardCalls("read")); !nudge {
		t.Fatal("6th single must re-nudge")
	}
}

func TestLoopGuardsMultiResetsStreak(t *testing.T) {
	g := New(100)
	g.Note(guardCalls("read"))
	g.Note(guardCalls("read"))
	if nudge, _, _ := g.Note(guardCalls("read", "grep")); nudge {
		t.Fatal("multi-call batch must reset the streak, not nudge")
	}
	g.Note(guardCalls("read"))
	g.Note(guardCalls("read"))
	if nudge, _, _ := g.Note(guardCalls("read")); !nudge {
		t.Fatal("streak restarts after multi-call batch")
	}
}

func TestLoopGuardsActingResets(t *testing.T) {
	g := New(4)
	g.Note(guardCalls("read"))
	g.Note(guardCalls("read"))
	if nudge, _, stop := g.Note(guardCalls("write")); nudge || stop != nil {
		t.Fatalf("acting call: nudge=%v stop=%v", nudge, stop)
	}
	// Counters restarted: 3 more non-acting rounds stay under the limit.
	for i := 0; i < 3; i++ {
		if _, _, stop := g.Note(guardCalls("read")); stop != nil {
			t.Fatalf("round +%d after acting call must not stop: %v", i+1, stop)
		}
	}
	if _, _, stop := g.Note(guardCalls("read")); stop == nil {
		t.Fatal("limit must trip 4 stagnant rounds after the reset")
	}
}

func TestLoopGuardsStagnantStops(t *testing.T) {
	g := New(3)
	g.Note(guardCalls("read"))
	g.Note(guardCalls("shell"))
	_, _, stop := g.Note(guardCalls("grep"))
	if stop == nil || !strings.Contains(stop.Error(), "3 rounds") || !strings.Contains(stop.Error(), "write/edit/task") {
		t.Fatalf("stop=%v", stop)
	}
}

func TestLoopGuardsAskPauses(t *testing.T) {
	g := New(100)
	for i := 0; i < 5; i++ {
		if nudge, _, stop := g.Note(guardCalls("AskUserQuestion")); nudge || stop != nil {
			t.Fatalf("question round must pause counters: nudge=%v stop=%v", nudge, stop)
		}
	}
	g.Note(guardCalls("read"))
	g.Note(guardCalls("read"))
	if nudge, _, _ := g.Note(guardCalls("read")); !nudge {
		t.Fatal("questions must not pollute the single-call streak")
	}
}

func TestLoopGuardsDefaultLimit(t *testing.T) {
	if g := New(0); g.Limit() != DefaultMaxStagnantRounds {
		t.Fatalf("limit=%d, want default %d", g.Limit(), DefaultMaxStagnantRounds)
	}
	if g := New(7); g.Limit() != 7 {
		t.Fatalf("limit=%d, want 7", g.Limit())
	}
}

func TestLoopGuardsActNudge(t *testing.T) {
	g := New(25)
	for i := 1; i < actNudgeStagnant; i++ {
		if _, act, stop := g.Note(guardCalls("read")); act || stop != nil {
			t.Fatalf("round %d: act=%v stop=%v", i, act, stop)
		}
	}
	if _, act, stop := g.Note(guardCalls("read")); !act || stop != nil {
		t.Fatalf("round %d must act-nudge: act=%v stop=%v", actNudgeStagnant, act, stop)
	}
	// Once per stagnant run: no repeat on the next round.
	if _, act, _ := g.Note(guardCalls("read")); act {
		t.Fatal("act nudge must fire once per stagnant run")
	}
	// Acting resets it: a fresh run nudges again at the same mark.
	g.Note(guardCalls("edit"))
	for i := 1; i < actNudgeStagnant; i++ {
		g.Note(guardCalls("read"))
	}
	if _, act, _ := g.Note(guardCalls("read")); !act {
		t.Fatal("act nudge must re-arm after an acting call")
	}
}

func TestLoopGuardsNoActNudgeWhenLimitLow(t *testing.T) {
	g := New(7)
	for i := 1; i <= 7; i++ {
		_, act, stop := g.Note(guardCalls("read"))
		if act {
			t.Fatalf("round %d: no room for an act nudge under a limit of 7", i)
		}
		if i < 7 && stop != nil {
			t.Fatalf("round %d: early stop %v", i, stop)
		}
		if i == 7 && stop == nil {
			t.Fatal("limit of 7 must stop on the 7th stagnant round")
		}
	}
}

func TestStripNudges(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "please batch these reads"},
		{Role: openai.ChatMessageRoleAssistant, Content: "on it"},
		{Role: openai.ChatMessageRoleUser, Content: BatchNudgeText},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: "out"},
		{Role: openai.ChatMessageRoleUser, Content: "  " + BatchNudgeText + "  "},
		{Role: openai.ChatMessageRoleUser, Content: ActNudgeText},
	}
	StripNudges(&h)
	if len(h) != 3 {
		t.Fatalf("msgs=%d: %+v", len(h), h)
	}
	if h[0].Content != "please batch these reads" || h[1].Content != "on it" || h[2].Role != openai.ChatMessageRoleTool {
		t.Fatalf("wrong survivors: %+v", h)
	}
}
