package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func guardTestHistory() []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}
}

func TestChatDistinctResearchWithoutNudges(t *testing.T) {
	var replies [][]byte
	for n := 0; n < 4; n++ {
		replies = append(replies, completion(toolCall("probe", fmt.Sprintf(`{"n":%d}`, n)), ""))
	}
	// Claims are not grounds for manufacturing another user instruction.
	replies = append(replies, completion(nil, "I implemented the requested change"))
	srv, requests := fakeServer(t, replies...)
	r := NewRegistry()
	executed := 0
	if err := r.Add(Tool{Name: "probe", Fn: func(context.Context, string) (string, error) {
		executed++
		return "finding", nil
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds, cfg.MaxStagnantRounds = 6, 1
	history := guardTestHistory()
	msg, usage, err := runChatTurn(context.Background(), cfg, nil, "", srv.Client(), r, &history, nil)
	if err != nil || msg.Content != "I implemented the requested change" || executed != 4 || *requests != 5 || usage.TotalTokens != 75 {
		t.Fatalf("reply=%+v usage=%+v executed=%d requests=%d err=%v", msg, usage, executed, *requests, err)
	}
	users := 0
	for _, msg := range history {
		if msg.Role == "user" {
			users++
			if msg.Content != "hi" {
				t.Fatalf("synthetic instruction: %q", msg.Content)
			}
		}
	}
	if users != 1 {
		t.Fatalf("user messages=%d", users)
	}
}

func TestModelTurnBoundsIdenticalBatches(t *testing.T) {
	r := NewRegistry()
	executed := 0
	_ = r.Add(Tool{Name: "probe", Fn: func(context.Context, string) (string, error) {
		executed++
		return "finding", nil
	}})
	history := guardTestHistory()
	requests := 0
	msg, usage, err := runModelTurn(context.Background(), Config{MaxRounds: 10, MaxStagnantRounds: 2}, r, &history, nil, func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		requests++
		args := `{"a":1,"b":2}`
		if requests%2 == 0 {
			args = ` { "b": 2, "a": 1 } `
		}
		calls := []openai.ToolCall{openaiToolCall(fmt.Sprintf("%d-a", requests), "probe"), openaiToolCall(fmt.Sprintf("%d-b", requests), "probe")}
		calls[0].Function.Arguments = args
		return openai.ChatCompletionMessage{Role: "assistant", Content: "summary plus calls", ToolCalls: calls}, openai.Usage{TotalTokens: 5}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "max_stagnant_rounds=2") || msg.Content != "" || requests != 3 || executed != 4 || usage.TotalTokens != 15 {
		t.Fatalf("msg=%+v usage=%+v requests=%d executed=%d err=%v", msg, usage, requests, executed, err)
	}
	if history[len(history)-1].Role != "tool" {
		t.Fatalf("rejected batch left an unanswered assistant call: %+v", history)
	}
}

func TestModelTurnFailedActionNotFakedSuccess(t *testing.T) {
	r := NewRegistry()
	executed := 0
	_ = r.Add(Tool{Name: "edit", Fn: func(context.Context, string) (string, error) {
		executed++
		return "", errors.New("edit failed")
	}})
	history := guardTestHistory()
	requests := 0
	msg, _, err := runModelTurn(context.Background(), Config{MaxRounds: 2}, r, &history, nil, func(_ context.Context, history []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		requests++
		if requests == 2 && history[len(history)-1].Content != "error: edit failed" {
			t.Fatalf("tool error not fed back: %+v", history)
		}
		call := openaiToolCall(fmt.Sprint(requests), "edit")
		call.Function.Arguments = fmt.Sprintf(`{"attempt":%d}`, requests)
		return openai.ChatCompletionMessage{Role: "assistant", Content: "summary with another action", ToolCalls: []openai.ToolCall{call}}, openai.Usage{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "2 rounds") || msg.Content != "" || executed != 2 || requests != 2 {
		t.Fatalf("reply=%+v executed=%d requests=%d err=%v", msg, executed, requests, err)
	}
	if history[len(history)-1].Content != "error: edit failed" {
		t.Fatalf("failed action missing from history: %+v", history)
	}
}

func TestDoomLoopScopedToUserTurn(t *testing.T) {
	call := openaiToolCall("1", "read")
	assistant := openai.ChatCompletionMessage{Role: "assistant", ToolCalls: []openai.ToolCall{call}}
	for _, boundary := range []openai.ChatCompletionMessage{{Role: "user", Content: "next"}, {Role: "assistant", Content: "final"}} {
		history := []openai.ChatCompletionMessage{assistant, assistant, boundary}
		if name := doomLoopTool(history, []openai.ToolCall{call}); name != "" {
			t.Fatalf("repeat crossed %s boundary: %q", boundary.Role, name)
		}
	}
	other := call
	call.Function.Arguments = `{"a":1,"b":2}`
	other.Function.Arguments = ` { "b":2, "a":1 } `
	if loopFingerprint(call) != loopFingerprint(other) {
		t.Fatal("fingerprints must canonicalize object keys and whitespace")
	}
}

func TestModelTurnRejectsThinkingOnlyFinal(t *testing.T) {
	for _, msg := range []openai.ChatCompletionMessage{
		{Role: "assistant", ReasoningContent: "private plan"},
		{Role: "assistant", Content: "<think>plan</think> \n"},
		{Role: "assistant", Content: " \n"},
	} {
		history := guardTestHistory()
		_, usage, err := runModelTurn(context.Background(), Config{MaxRounds: 2}, NewRegistry(), &history, nil, func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
			return msg, openai.Usage{TotalTokens: 7}, nil
		})
		if err == nil || usage.TotalTokens != 7 || len(history) != 2 {
			t.Fatalf("accepted thinking-only reply: %+v usage=%+v history=%+v err=%v", msg, usage, history, err)
		}
	}
}

func TestModelTurnDistinctBatchResetsRepeatCount(t *testing.T) {
	r := NewRegistry()
	_ = r.Add(Tool{Name: "probe", Fn: func(context.Context, string) (string, error) { return "ok", nil }})
	history := guardTestHistory()
	requests := 0
	_, _, err := runModelTurn(context.Background(), Config{MaxRounds: 7, MaxStagnantRounds: 2}, r, &history, nil, func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		requests++
		if requests == 7 {
			return openai.ChatCompletionMessage{Role: "assistant", Content: "done"}, openai.Usage{}, nil
		}
		calls := []openai.ToolCall{openaiToolCall(fmt.Sprintf("%d-a", requests), "probe"), openaiToolCall(fmt.Sprintf("%d-b", requests), "probe")}
		args, _ := json.Marshal(map[string]int{"group": (requests - 1) / 2})
		calls[0].Function.Arguments = string(args)
		return openai.ChatCompletionMessage{Role: "assistant", ToolCalls: calls}, openai.Usage{}, nil
	})
	if err != nil || requests != 7 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}
