package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
	"valen/internal/dvlog"
	"valen/internal/guard"
)

// TestChatStopsStagnantLoop: endless exploration without write/edit/task
// gets one final no-tools round; answering it with another (textless) tool
// call still aborts — no 4th request.
func TestChatStopsStagnantLoop(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("shell", `{"command":"echo 1"}`), ""),
		completion(toolCall("shell", `{"command":"echo 2"}`), ""),
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
	if *calls != 3 {
		t.Fatalf("calls=%d, want 3 (2 exploring + 1 failed recovery round)", *calls)
	}
}

// TestChatNudgesSingleCallStreak: three consecutive single-call rounds append
// the batching reminder (visible to the 4th request), and turn end strips it.
func countingProbeRegistry(count *int) *Registry {
	r := NewRegistry()
	_ = r.Add(Tool{Name: "probe", Description: "probe", Fn: func(ctx context.Context, args string) (string, error) {
		*count++
		return "probe:" + args, nil
	}})
	return r
}

func guardTestHistory() []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}
}

// completionWithText is completion plus assistant text alongside tool calls
// (completion blanks content whenever tool calls are present).
func completionWithText(toolCalls []map[string]any, content string) []byte {
	msg := map[string]any{"role": "assistant", "content": content, "tool_calls": toolCalls}
	b, _ := json.Marshal(map[string]any{
		"id": "1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	return b
}

func historyHas(history []openai.ChatCompletionMessage, text string) bool {
	for _, m := range history {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

// TestChatStallRecoveryFinishes: at the limit the turn runs one final round
// under the no-tools instruction; a text answer finishes the turn cleanly.
func TestChatStallRecoveryFinishes(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("probe", `{"n":1}`), ""),
		completion(toolCall("probe", `{"n":2}`), ""),
		completion(toolCall("probe", `{"n":3}`), ""),
		completion(nil, "final findings"),
	)
	execCount := 0
	registry := countingProbeRegistry(&execCount)
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 10
	cfg.MaxStagnantRounds = 3

	history := guardTestHistory()
	msg, _, err := runChatTurn(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "final findings" {
		t.Fatalf("reply=%q", msg.Content)
	}
	if *calls != 4 {
		t.Fatalf("calls=%d, want 4 (3 exploring + 1 recovery round)", *calls)
	}
	if historyHas(history, guard.StallRecoveryText) {
		t.Fatalf("recovery notice leaked past turn end: %+v", history)
	}
}

// TestChatStallRecoveryDeclinesTools: a recovery round answering with tools
// attached finishes on the text — the calls never execute.
func TestChatStallRecoveryDeclinesTools(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("probe", `{"n":1}`), ""),
		completion(toolCall("probe", `{"n":2}`), ""),
		completion(toolCall("probe", `{"n":3}`), ""),
		completionWithText(toolCall("probe", `{"n":4}`), "summary plus a call"),
	)
	execCount := 0
	registry := countingProbeRegistry(&execCount)
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 10
	cfg.MaxStagnantRounds = 3

	history := guardTestHistory()
	msg, _, err := runChatTurn(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "summary plus a call" || len(msg.ToolCalls) != 0 {
		t.Fatalf("reply=%+v", msg)
	}
	if *calls != 4 {
		t.Fatalf("calls=%d, want 4", *calls)
	}
	if execCount != 3 {
		t.Fatalf("execCount=%d, recovery-round call must not execute", execCount)
	}
	last := history[len(history)-1]
	if last.Role != openai.ChatMessageRoleAssistant || len(last.ToolCalls) != 0 {
		t.Fatalf("last history message must be a clean reply: %+v", last)
	}
}

func TestChatNudgesSingleCallStreak(t *testing.T) {
	var log strings.Builder
	prev := dvlog.SetOutput(&log)
	was := dvlog.Enabled()
	dvlog.Enable(true)
	defer func() {
		dvlog.Enable(was)
		dvlog.SetOutput(prev)
	}()

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
		if strings.Contains(m.Content, guard.BatchNudgeText) {
			t.Fatalf("nudge leaked past turn end: %+v", history)
		}
	}
}
