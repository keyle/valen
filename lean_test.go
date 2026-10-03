package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// TestLeanPrunesBetweenRounds: the per-round lean collapses older reads
// without any network call when under the summary threshold — this is the
// cheap local win that keeps long turns from resending rounds × history.
func TestLeanPrunesBetweenRounds(t *testing.T) {
	cfg := newTestConfig("http://127.0.0.1:1")
	cfg.MaxContextTokens = 100000
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 20000}, cfg.MaxContextTokens)
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 4; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: readOutput(fmt.Sprintf("f%d.go", i), 500, 1, 200)},
		)
	}
	leanHistoryForNextRound(cfg, &h)
	for i, wantIntact := range []bool{false, false, true, true} {
		content := h[2+i*2].Content
		if wantIntact && !strings.Contains(content, "some code line") {
			t.Fatalf("read %d must stay intact", i)
		}
		if !wantIntact && strings.Contains(content, "some code line") {
			t.Fatalf("read %d must collapse between rounds", i)
		}
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
