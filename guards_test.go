package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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
	g := newLoopGuards(Config{MaxStagnantRounds: 100})
	for i := 0; i < 2; i++ {
		if nudge, _, stop := g.note(guardCalls("read")); nudge || stop != nil {
			t.Fatalf("round %d: nudge=%v stop=%v", i+1, nudge, stop)
		}
	}
	if nudge, _, stop := g.note(guardCalls("read")); !nudge || stop != nil {
		t.Fatalf("3rd single must nudge: nudge=%v stop=%v", nudge, stop)
	}
	// Streak resets after firing: next nudge comes after 3 more singles.
	for i := 0; i < 2; i++ {
		if nudge, _, _ := g.note(guardCalls("read")); nudge {
			t.Fatalf("early re-nudge at +%d", i+1)
		}
	}
	if nudge, _, _ := g.note(guardCalls("read")); !nudge {
		t.Fatal("6th single must re-nudge")
	}
}

func TestLoopGuardsMultiResetsStreak(t *testing.T) {
	g := newLoopGuards(Config{MaxStagnantRounds: 100})
	g.note(guardCalls("read"))
	g.note(guardCalls("read"))
	if nudge, _, _ := g.note(guardCalls("read", "grep")); nudge {
		t.Fatal("multi-call batch must reset the streak, not nudge")
	}
	g.note(guardCalls("read"))
	g.note(guardCalls("read"))
	if nudge, _, _ := g.note(guardCalls("read")); !nudge {
		t.Fatal("streak restarts after multi-call batch")
	}
}

func TestLoopGuardsActingResets(t *testing.T) {
	g := newLoopGuards(Config{MaxStagnantRounds: 4})
	g.note(guardCalls("read"))
	g.note(guardCalls("read"))
	if nudge, _, stop := g.note(guardCalls("write")); nudge || stop != nil {
		t.Fatalf("acting call: nudge=%v stop=%v", nudge, stop)
	}
	// Counters restarted: 3 more non-acting rounds stay under the limit.
	for i := 0; i < 3; i++ {
		if _, _, stop := g.note(guardCalls("read")); stop != nil {
			t.Fatalf("round +%d after acting call must not stop: %v", i+1, stop)
		}
	}
	if _, _, stop := g.note(guardCalls("read")); stop == nil {
		t.Fatal("limit must trip 4 stagnant rounds after the reset")
	}
}

func TestLoopGuardsStagnantStops(t *testing.T) {
	g := newLoopGuards(Config{MaxStagnantRounds: 3})
	g.note(guardCalls("read"))
	g.note(guardCalls("shell"))
	_, _, stop := g.note(guardCalls("grep"))
	if stop == nil || !strings.Contains(stop.Error(), "3 rounds") || !strings.Contains(stop.Error(), "write/edit/task") {
		t.Fatalf("stop=%v", stop)
	}
}

func TestLoopGuardsAskPauses(t *testing.T) {
	g := newLoopGuards(Config{MaxStagnantRounds: 100})
	for i := 0; i < 5; i++ {
		if nudge, _, stop := g.note(guardCalls("AskUserQuestion")); nudge || stop != nil {
			t.Fatalf("question round must pause counters: nudge=%v stop=%v", nudge, stop)
		}
	}
	g.note(guardCalls("read"))
	g.note(guardCalls("read"))
	if nudge, _, _ := g.note(guardCalls("read")); !nudge {
		t.Fatal("questions must not pollute the single-call streak")
	}
}

func TestLoopGuardsDefaultLimit(t *testing.T) {
	if g := newLoopGuards(Config{}); g.stagnantLimit != defaultMaxStagnantRounds {
		t.Fatalf("limit=%d, want default %d", g.stagnantLimit, defaultMaxStagnantRounds)
	}
	if g := newLoopGuards(Config{MaxStagnantRounds: 7}); g.stagnantLimit != 7 {
		t.Fatalf("limit=%d, want 7", g.stagnantLimit)
	}
}

func TestLoopGuardsActNudge(t *testing.T) {
	g := newLoopGuards(Config{MaxStagnantRounds: 25})
	for i := 1; i < actNudgeStagnant; i++ {
		if _, act, stop := g.note(guardCalls("read")); act || stop != nil {
			t.Fatalf("round %d: act=%v stop=%v", i, act, stop)
		}
	}
	if _, act, stop := g.note(guardCalls("read")); !act || stop != nil {
		t.Fatalf("round %d must act-nudge: act=%v stop=%v", actNudgeStagnant, act, stop)
	}
	// Once per stagnant run: no repeat on the next round.
	if _, act, _ := g.note(guardCalls("read")); act {
		t.Fatal("act nudge must fire once per stagnant run")
	}
	// Acting resets it: a fresh run nudges again at the same mark.
	g.note(guardCalls("edit"))
	for i := 1; i < actNudgeStagnant; i++ {
		g.note(guardCalls("read"))
	}
	if _, act, _ := g.note(guardCalls("read")); !act {
		t.Fatal("act nudge must re-arm after an acting call")
	}
}

func TestLoopGuardsNoActNudgeWhenLimitLow(t *testing.T) {
	g := newLoopGuards(Config{MaxStagnantRounds: 7})
	for i := 1; i <= 7; i++ {
		_, act, stop := g.note(guardCalls("read"))
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
		{Role: openai.ChatMessageRoleUser, Content: batchNudgeText},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: "out"},
		{Role: openai.ChatMessageRoleUser, Content: "  " + batchNudgeText + "  "},
		{Role: openai.ChatMessageRoleUser, Content: actNudgeText},
	}
	stripNudges(&h)
	if len(h) != 3 {
		t.Fatalf("msgs=%d: %+v", len(h), h)
	}
	if h[0].Content != "please batch these reads" || h[1].Content != "on it" || h[2].Role != openai.ChatMessageRoleTool {
		t.Fatalf("wrong survivors: %+v", h)
	}
}

// TestChatStopsStagnantLoop: endless exploration without write/edit/task
// aborts at the stagnant limit — no 3rd request.
func TestChatStopsStagnantLoop(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("shell", `{"command":"echo 1"}`), ""),
		completion(toolCall("shell", `{"command":"echo 2"}`), ""),
		completion(nil, "done"),
	)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 10
	cfg.MaxStagnantRounds = 2

	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "2 rounds") || !strings.Contains(err.Error(), "write/edit/task") {
		t.Fatalf("err=%v", err)
	}
	if *calls != 2 {
		t.Fatalf("calls=%d, want 2 (abort before a 3rd request)", *calls)
	}
}

// TestChatNudgesSingleCallStreak: three consecutive single-call rounds append
// the batching reminder (visible to the 4th request), and turn end strips it.
func TestChatNudgesSingleCallStreak(t *testing.T) {
	v, w, f, s := saveDebugGlobals()
	defer restoreDebugGlobals(v, w, f, s)
	debugVerbose = true
	debugStart = time.Now()
	var log strings.Builder
	debugOut = &log

	srv, calls := fakeServer(t,
		completion(toolCall("shell", `{"command":"echo 1"}`), ""),
		completion(toolCall("shell", `{"command":"echo 2"}`), ""),
		completion(toolCall("shell", `{"command":"echo 3"}`), ""),
		completion(nil, "done"),
	)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 10

	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}
	msg, _, err := runChatTurn(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "done" {
		t.Fatalf("reply=%q", msg.Content)
	}
	if *calls != 4 {
		t.Fatalf("calls=%d, want 4", *calls)
	}
	if !strings.Contains(log.String(), "batch nudge appended") {
		t.Fatalf("no nudge in dv log:\n%s", log.String())
	}
	for _, m := range history {
		if strings.Contains(m.Content, batchNudgeText) {
			t.Fatalf("nudge leaked past turn end: %+v", history)
		}
	}
}
