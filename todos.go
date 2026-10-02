package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// todos.go: session-scoped task breakdown (opencode-style `todowrite`).
//
// Chunky work slows down fast when it runs as one flat tool loop with no plan
// surface: the model re-explores, repeats reads, and every round resends the
// whole history. An explicit todo list breaks the work into small steps with
// one `in_progress` item at a time, so turns stay short and resumable.
//
// Store is in-memory only (no new files); the call records already fold into
// the session JSONL via the normal tool timeline, which is also how a resumed
// session restores them. `/clear` and `/new` reset the list.

// Todo is one step of the session plan. ID is optional (opencode omits it);
// content/status/priority follow the opencode schema so models already know it.
type Todo struct {
	ID       string `json:"id,omitempty"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

var (
	todoMu sync.Mutex
	todos  []Todo
)

// resetTodos clears the session plan. Used by tests, /clear, /new, and
// headless one-shot runs.
func resetTodos() {
	todoMu.Lock()
	defer todoMu.Unlock()
	todos = nil
}

// getTodos returns a copy of the current plan.
func getTodos() []Todo {
	todoMu.Lock()
	defer todoMu.Unlock()
	return append([]Todo(nil), todos...)
}

// setTodos replaces the plan after validation and returns the stored copy.
func setTodos(next []Todo) []Todo {
	todoMu.Lock()
	defer todoMu.Unlock()
	todos = append([]Todo(nil), next...)
	return append([]Todo(nil), todos...)
}

// restoreTodosFromTurns replays the last `todowrite` output found in loaded
// session turns, so /sessions resume continues the same plan. Parse failures
// (e.g. truncated output) keep the current list.
func restoreTodosFromTurns(turns []Turn) {
	var last []Todo
	for _, t := range turns {
		for _, tr := range t.Tools {
			if tr.Name != "todowrite" {
				continue
			}
			var list []Todo
			if err := json.Unmarshal([]byte(tr.Output), &list); err != nil {
				continue
			}
			last = list
		}
	}
	todoMu.Lock()
	defer todoMu.Unlock()
	todos = last
}

func validTodoStatus(s string) bool {
	switch s {
	case "pending", "in_progress", "completed", "cancelled":
		return true
	}
	return false
}

func validTodoPriority(s string) bool {
	switch s {
	case "high", "medium", "low":
		return true
	}
	return false
}

func normalizeTodos(in []Todo) ([]Todo, error) {
	out := make([]Todo, 0, len(in))
	for i, td := range in {
		content := strings.TrimSpace(td.Content)
		status := strings.ToLower(strings.TrimSpace(td.Status))
		priority := strings.ToLower(strings.TrimSpace(td.Priority))
		if content == "" {
			return nil, fmt.Errorf("todos[%d]: content is required", i)
		}
		if !validTodoStatus(status) {
			return nil, fmt.Errorf("todos[%d]: status must be pending, in_progress, completed or cancelled", i)
		}
		if !validTodoPriority(priority) {
			return nil, fmt.Errorf("todos[%d]: priority must be high, medium or low", i)
		}
		out = append(out, Todo{ID: strings.TrimSpace(td.ID), Content: content, Status: status, Priority: priority})
	}
	return out, nil
}

// todoWriteDesc mirrors opencode's todowrite guidance: when to plan, one
// in_progress at a time, update in real time, small actionable steps.
const todoWriteDesc = "Create and maintain a structured task list for the current coding session. Tracks progress, organizes multi-step work, and surfaces status to the user. Use proactively when the task needs 3+ steps, is non-trivial, or the user gave multiple tasks. Keep exactly one in_progress at a time; mark completed only after the work is actually done (including verification); if blocked, keep it in_progress and add a follow-up todo. Items must be specific and actionable — break large work into smaller steps. Skip for single straightforward tasks or pure questions."

// TodoWriteTool replaces the session plan. Output is the stored list as JSON.
func TodoWriteTool() Tool {
	return Tool{
		Name:        "todowrite",
		Description: todoWriteDesc,
		Schema:      json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","description":"The updated todo list","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string","description":"Brief description of the task"},"status":{"type":"string","description":"Current status of the task: pending, in_progress, completed, cancelled"},"priority":{"type":"string","description":"Priority level of the task: high, medium, low"}},"required":["content","status","priority"]}}},"required":["todos"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Todos []Todo `json:"todos"`
			}
			if err := json.Unmarshal([]byte(normalizeArgs(args)), &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			if a.Todos == nil {
				return "", fmt.Errorf("todos is required")
			}
			list, err := normalizeTodos(a.Todos)
			if err != nil {
				return "", err
			}
			stored := setTodos(list)
			b, err := json.MarshalIndent(stored, "", "  ")
			if err != nil {
				return "", err
			}
			if len(b) == 0 || string(b) == "null" {
				return "[]", nil
			}
			return string(b), nil
		},
	}
}

// TodoReadTool returns the current session plan as JSON.
func TodoReadTool() Tool {
	return Tool{
		Name:        "todoread",
		Description: "Read the current session todo list (written by todowrite). Returns [] when no plan exists.",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			list := getTodos()
			if len(list) == 0 {
				return "[]", nil
			}
			b, err := json.MarshalIndent(list, "", "  ")
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}
}
