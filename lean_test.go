package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
	"valen/internal/guard"
)

// Per-round pruning only reclaims history outside the turn and token shields.
func TestLeanPrunesBetweenRounds(t *testing.T) {
	cfg := newTestConfig("http://127.0.0.1:1")
	cfg.MaxContextTokens = 100000
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 20000}, cfg.MaxContextTokens)
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "old request"},
	}
	for i := 0; i < 24; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: readOutput(fmt.Sprintf("f%d.go", i), 500, 1, 200)},
		)
	}
	lastOlderRead := h[len(h)-1].Content
	protectedStart := len(h)
	for _, request := range []string{"previous request", "current request"} {
		h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: request})
		for i := 0; i < 4; i++ {
			h = append(h,
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: readOutput(fmt.Sprintf("active%d.go", i), 500, 1, 200)},
			)
		}
	}
	protected := append([]openai.ChatCompletionMessage(nil), h[protectedStart:]...)
	leanHistoryForNextRound(cfg, &h)
	if strings.Contains(h[3].Content, "some code line") {
		t.Fatal("oldest read outside the shields should collapse between rounds")
	}
	if h[protectedStart-1].Content != lastOlderRead {
		t.Fatal("recent older reads inside the 40k shield must stay intact")
	}
	if !reflect.DeepEqual(h[protectedStart:], protected) {
		t.Fatal("all reads in the latest two user turns must stay intact")
	}
}

func TestLeanKeepsCurrentWorkingSetAcrossRounds(t *testing.T) {
	cfg := compactTestConfig("http://127.0.0.1:1")
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "implement this request"},
	}
	for round := 0; round < 10; round++ {
		calls := []openai.ToolCall{}
		var outputs []openai.ChatCompletionMessage
		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("r%d-%d", round, i)
			calls = append(calls, openaiToolCall(id, "read"))
			outputs = append(outputs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: readOutput(id+".go", 500, 1, 200)})
		}
		h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: calls})
		h = append(h, outputs...)
		if round == 3 {
			h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: guard.BatchNudgeText})
		}
		before := append([]openai.ChatCompletionMessage(nil), h...)
		leanHistoryForNextRound(cfg, &h)
		if !reflect.DeepEqual(h, before) {
			t.Fatalf("round %d: current working set must remain verbatim, including whole fresh batches", round)
		}
	}
	if estimateTokens(h) <= compactionPruneProtectTokens {
		t.Fatal("fixture must exceed the token shield to exercise turn protection")
	}
}

// TestLeanNeverSummarizesMidTurn: even over the soft threshold the per-round
// lean stays prune-only with no network call, so the hot tool loop never
// blocks on an extra LLM summary. Summaries fire between turns via
// maybeAutoCompact and on overflow retry.
func TestLeanNeverSummarizesMidTurn(t *testing.T) {
	cfg := compactTestConfig("http://127.0.0.1:1")
	h := longHistory()
	leanHistoryForNextRound(cfg, &h)
	for _, m := range h {
		if strings.HasPrefix(m.Content, compactionSummaryPrefix) {
			t.Fatal("per-round lean must not summarize mid-turn")
		}
	}
}

// TestLeanRespectsAutoOff: with auto disabled the per-round lean is a no-op.
func TestLeanRespectsAutoOff(t *testing.T) {
	cfg := newTestConfig("http://127.0.0.1:1")
	f := false
	cfg.Compaction.Auto = &f
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 4; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: readOutput(fmt.Sprintf("f%d.go", i), 500, 1, 200)},
		)
	}
	before := h[2].Content
	leanHistoryForNextRound(cfg, &h)
	if h[2].Content != before {
		t.Fatal("auto off must leave history untouched")
	}
}
