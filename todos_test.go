package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTodoRoundTrip: todowrite stores an opencode-schema list, todoread
// returns it, and invalid states are rejected.
func TestTodoRoundTrip(t *testing.T) {
	resetTodos()
	ctx := context.Background()

	w := TodoWriteTool()
	out, err := w.Fn(ctx, `{"todos":[{"content":"Slice it","status":"in_progress","priority":"high"},{"content":"Tests","status":"pending","priority":"medium"}]}`)
	if err != nil {
		t.Fatalf("write err=%v", err)
	}
	var stored []Todo
	if err := json.Unmarshal([]byte(out), &stored); err != nil {
		t.Fatalf("write output is not JSON: %v\n%s", err, out)
	}
	if len(stored) != 2 || stored[0].Status != "in_progress" || stored[1].Priority != "medium" {
		t.Fatalf("stored=%v", stored)
	}

	r := TodoReadTool()
	rout, err := r.Fn(ctx, `{}`)
	if err != nil {
		t.Fatalf("read err=%v", err)
	}
	if rout != out {
		t.Fatalf("read must return the written list:\nwrite=%s\nread=%s", out, rout)
	}

	for _, bad := range []string{
		`{"todos":[{"content":"x","status":"doing","priority":"high"}]}`,
		`{"todos":[{"content":"x","status":"pending","priority":"urgent"}]}`,
		`{"todos":[{"content":"  ","status":"pending","priority":"low"}]}`,
		`{}`,
	} {
		if _, err := w.Fn(ctx, bad); err == nil {
			t.Fatalf("bad write must fail: %s", bad)
		}
	}
	// Failed writes leave the plan untouched.
	rout2, _ := r.Fn(ctx, `{}`)
	if rout2 != out {
		t.Fatalf("failed writes must not mutate: %s", rout2)
	}
}

func TestTodoEmptyAndReset(t *testing.T) {
	resetTodos()
	ctx := context.Background()
	if out, _ := TodoReadTool().Fn(ctx, `{}`); out != "[]" {
		t.Fatalf("empty plan must read as [], got %q", out)
	}
	if _, err := TodoWriteTool().Fn(ctx, `{"todos":[]}`); err != nil {
		t.Fatalf("clearing via [] must work: %v", err)
	}
	if _, err := TodoWriteTool().Fn(ctx, `{"todos":[{"content":"a","status":"pending","priority":"low"}]}`); err != nil {
		t.Fatal(err)
	}
	resetTodos()
	if out, _ := TodoReadTool().Fn(ctx, `{}`); out != "[]" {
		t.Fatalf("reset must clear, got %q", out)
	}
}

func TestTodoRestoreFromTurns(t *testing.T) {
	resetTodos()
	first, _ := json.Marshal([]Todo{{Content: "old", Status: "completed", Priority: "low"}})
	last, _ := json.Marshal([]Todo{{Content: "new", Status: "in_progress", Priority: "high"}})
	restoreTodosFromTurns([]Turn{
		{Tools: []ToolRecord{{Name: "shell", Output: "hi"}}},
		{Tools: []ToolRecord{{Name: "todowrite", Output: string(first)}}},
		{Tools: []ToolRecord{{Name: "todowrite", Output: string(last)}}},
	})
	got := getTodos()
	if len(got) != 1 || got[0].Content != "new" || got[0].Status != "in_progress" {
		t.Fatalf("must restore the last plan, got %+v", got)
	}
	// Turns without todowrite clear nothing when empty... but a resume with no
	// todowrite records means no plan.
	resetTodos()
	restoreTodosFromTurns([]Turn{{Tools: []ToolRecord{{Name: "shell", Output: "hi"}}}})
	if out := strings.TrimSpace(getTodosJSON()); out != "[]" {
		t.Fatalf("no todowrite records means empty plan, got %s", out)
	}
}

func getTodosJSON() string {
	b, _ := json.Marshal(getTodos())
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}
