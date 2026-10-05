package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
	"valen/internal/guard"
)

func compactTestConfig(srvURL string) Config {
	cfg := newTestConfig(srvURL)
	cfg.MaxContextTokens = 100000
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 200}, cfg.MaxContextTokens)
	return cfg
}

func longHistory() []openai.ChatCompletionMessage {
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 6; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "question " + strings.Repeat("q", 400)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "answer " + strings.Repeat("a", 400)},
		)
	}
	h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "recent question"})
	return h
}

func readOutput(path string, total, start, end int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "File %s (%d lines, showing %d-%d):\n", path, total, start, end)
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d: some code line number %d with identifiers\n", i, i)
	}
	fmt.Fprintf(&b, "...[%d more lines, read with offset %d]", total-end, end+1)
	return b.String()
}

// Fresh results retain everything allowed by the registry cap.
func TestFitSendBudget(t *testing.T) {
	if got := fitSendBudget("short"); got != "short" {
		t.Fatalf("short must pass: %q", got)
	}
	decision := "User has answered your questions: color=blue"
	if got := fitSendBudget(decision + strings.Repeat("x", 5000)); got != decision+strings.Repeat("x", 5000) {
		t.Fatal("decisions must stay verbatim")
	}
	for _, content := range []string{strings.Repeat("y", 16000), strings.Repeat("\u754c", 5000)} {
		if got := fitSendBudget(content); got != content {
			t.Fatalf("fresh result must stay verbatim: %d runes", len([]rune(got)))
		}
	}
}

// TestFitSendBudgetKeepsReads: file reads pass through untouched even past
// 2k old send cap, like other fresh outputs.
func TestFitSendBudgetKeepsReads(t *testing.T) {
	read := readOutput("TODO.md", 140, 1, 140)
	if got := fitSendBudget(read); got != read {
		t.Fatalf("reads must stay verbatim: %d vs %d runes", len([]rune(got)), len([]rune(read)))
	}
}

func TestReadDescriptorKeepsIdentity(t *testing.T) {
	out := readOutput("a.go", 100, 1, 50)
	d, ok := readDescriptor(out)
	if !ok {
		t.Fatalf("read output not recognized:\n%s", out[:120])
	}
	if !strings.Contains(d, "File a.go (100 lines, showing 1-50)") {
		t.Fatalf("identity must stay verbatim: %q", d)
	}
	if !strings.Contains(d, "offset 51") {
		t.Fatalf("next-offset hint must survive: %q", d)
	}
	if strings.Contains(d, "some code line") {
		t.Fatalf("code content must be dropped: %q", d[:200])
	}
	if again, ok := readDescriptor(d); !ok || again != d {
		t.Fatal("re-pruning a read descriptor must not drop its offset hint")
	}
	if _, ok := readDescriptor("just some shell output\nline2"); ok {
		t.Fatal("non-read output must not match")
	}
}

func TestPruneProtectsLatestTwoUserTurns(t *testing.T) {
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
	protectedStart := len(h)
	for _, request := range []string{"previous request", "current request"} {
		h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: request})
		for i := 0; i < 24; i++ {
			h = append(h,
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: readOutput(fmt.Sprintf("active%d.go", i), 500, 1, 200)},
			)
			if i == 12 {
				for _, nudge := range []string{guard.BatchNudgeText, guard.ActNudgeText, guard.StallRecoveryText, guard.ClaimChallengeText} {
					h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: nudge})
				}
			}
		}
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("shell", "shell")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "shell", Content: strings.Repeat("shell output\n", 1000)},
		)
	}
	protected := append([]openai.ChatCompletionMessage(nil), h[protectedStart:]...)
	if got := pruneOldToolOutputs(h); got <= 0 {
		t.Fatalf("expected reclaim, got %d", got)
	}
	if !reflect.DeepEqual(h[protectedStart:], protected) {
		t.Fatal("latest two genuine user turns must stay fully intact, including before nudges")
	}
	if strings.Contains(h[3].Content, "some code line") || !strings.Contains(h[3].Content, "re-read with read") {
		t.Fatal("oldest read should collapse to a descriptor")
	}
}

func TestPruneSharesProtectedBudgetForOlderReadsAndTools(t *testing.T) {
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "old request"}}
	for i := 0; i < 16; i++ {
		content := strings.Repeat("shell log\n", 1600)
		name := "shell"
		if i%2 == 0 {
			content = readOutput(fmt.Sprintf("f%d.go", i), 500, 1, 300)
			name = "read"
		}
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", name)}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: content},
		)
	}
	olderEnd := len(h)
	h = append(h,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "previous request"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "done"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current request"},
	)
	before := append([]openai.ChatCompletionMessage(nil), h...)
	pruneOldToolOutputs(h)
	kept := 0
	for i := olderEnd - 1; i > 0; i-- {
		if before[i].Role != openai.ChatMessageRoleTool {
			if !reflect.DeepEqual(h[i], before[i]) {
				t.Fatal("assistant calls must stay unchanged")
			}
			continue
		}
		kept += estimateTokens(before[i : i+1])
		if kept <= compactionPruneProtectTokens {
			if h[i].Content != before[i].Content {
				t.Fatalf("older output %d inside shared 40k shield changed", i)
			}
		} else if h[i].Content == before[i].Content {
			t.Fatalf("older output %d outside shared 40k shield should shrink", i)
		}
	}
	pruned := append([]openai.ChatCompletionMessage(nil), h...)
	if reclaimed := pruneOldToolOutputs(h); reclaimed != 0 || !reflect.DeepEqual(h, pruned) {
		t.Fatal("repeated pruning without new history must leave descriptors and outputs stable")
	}
}

func TestPruneKeepsWholeFreshBatchWithoutUserBoundary(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("old log\n", 30000)},
		{Role: openai.ChatMessageRoleAssistant},
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("fresh%d", i)
		h[1].ToolCalls = append(h[1].ToolCalls, openaiToolCall(id, "shell"))
		h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: strings.Repeat("fresh log\n", 1600)})
	}
	fresh := append([]openai.ChatCompletionMessage(nil), h[1:]...)
	if estimateTokens(fresh) <= compactionPruneProtectTokens {
		t.Fatal("fresh batch fixture must exceed the 40k shield")
	}
	if pruneOldToolOutputs(h) <= 0 {
		t.Fatal("older output should shrink")
	}
	if !reflect.DeepEqual(h[1:], fresh) {
		t.Fatal("entire fresh batch must remain available, even without user boundaries")
	}
}

func TestPruneProtectsDecisions(t *testing.T) {
	decision := "User has answered your questions: color=blue"
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 10; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "shell")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: strings.Repeat("log line output ", 2000)},
		)
	}
	h = append(h,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("q", "AskUserQuestion")}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "q", Content: decision},
	)
	// Push the decision far from the tip so phase-2 would eat it if unprotected.
	for i := 0; i < 10; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "shell")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: strings.Repeat("more logs ", 2000)},
		)
	}
	pruneOldToolOutputs(h)
	for _, m := range h {
		if strings.Contains(m.Content, "color=blue") && m.Content != decision {
			t.Fatalf("decision must stay verbatim: %q", m.Content)
		}
	}
	found := false
	for _, m := range h {
		if m.Content == decision {
			found = true
		}
	}
	if !found {
		t.Fatal("decision output went missing")
	}
}

func TestCompactionThresholdOften(t *testing.T) {
	cfg := Config{MaxContextTokens: 300000}
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{}, cfg.MaxContextTokens)
	if got := compactionThreshold(cfg); got != defaultCompactionKeepTokens*compactionSoftThresholdFactor {
		t.Fatalf("huge budgets must compact at keep*3 (%d), got %d", defaultCompactionKeepTokens*compactionSoftThresholdFactor, got)
	}
	small := Config{MaxContextTokens: 20000}
	small.Compaction = normalizeCompactionConfig(CompactionConfig{}, small.MaxContextTokens)
	if got := compactionThreshold(small); got != 20000-2000 {
		t.Fatalf("small budgets stay near the fail-fast limit, got %d", got)
	}
}

// TestAutoCompactsOften: history past the soft threshold shrinks via the
// cheap summarizer without any provider overflow.
func TestAutoCompactsOften(t *testing.T) {
	srv, _ := fakeServer(t, completion(nil, "EARLY SUMMARY"))
	cfg := compactTestConfig(srv.URL)
	h := longHistory()
	if !maybeAutoCompact(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h) {
		t.Fatal("history over threshold must compact")
	}
	found := false
	for _, m := range h {
		if strings.HasPrefix(m.Content, compactionSummaryPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("summary missing after auto-compact")
	}
}

func TestCompactionConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"kimi-k3\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Compaction.Auto == nil || !*cfg.Compaction.Auto {
		t.Fatalf("auto should default on: %+v", cfg.Compaction)
	}
	if cfg.Compaction.KeepTokens != defaultCompactionKeepTokens {
		t.Fatalf("keep=%d", cfg.Compaction.KeepTokens)
	}
	if cfg.Compaction.Buffer != contextBudget(cfg)/10 {
		t.Fatalf("buffer=%d budget=%d", cfg.Compaction.Buffer, contextBudget(cfg))
	}
	if cfg.Compaction.Model != "" {
		t.Fatalf("model should default empty (session model): %q", cfg.Compaction.Model)
	}
	if cfg.Compaction.Thinking != defaultCompactionThinking {
		t.Fatalf("thinking should default %q, got %q", defaultCompactionThinking, cfg.Compaction.Thinking)
	}
}

func TestCompactionThinkingExplicitAndFallback(t *testing.T) {
	// Explicit medium on a model that accepts it resolves verbatim.
	cfg, err := loadConfig(writeConfig(t, "model = \"muse-spark-1.3-contributor\"\n[compaction]\nthinking = \"medium\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := compactionThinking(cfg); got != "medium" {
		t.Fatalf("thinking=%q", got)
	}
	// Explicit bogus level fails config load like top-level thinking.
	if _, err := loadConfig(writeConfig(t, "model = \"muse-spark-1.3-contributor\"\n[compaction]\nthinking = \"ultra\"\n")); err == nil {
		t.Fatal("bad compaction thinking must fail")
	}
	// Default medium on a model without medium (glm flash) falls back to the
	// cheapest accepted level instead of failing.
	cfg2, err := loadConfig(writeConfig(t, "model = \"muse-spark-1.3-contributor\"\n[compaction]\nmodel = \"glm-5.3-flash\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := compactionThinking(cfg2); got != "low" {
		t.Fatalf("flash fallback thinking=%q, want low", got)
	}
}

func TestCompactionModelOverride(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"muse-spark-1.3-contributor\"\n[compaction]\nmodel = \"glm-5.3-flash\"\nkeep_tokens = 5000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := compactionModel(cfg); got != "glm-5.3-flash" {
		t.Fatalf("model=%q", got)
	}
	if cfg.Compaction.KeepTokens != 5000 {
		t.Fatalf("keep=%d", cfg.Compaction.KeepTokens)
	}
	if cheapestThinking("muse-spark-1.3-contributor") != "minimal" {
		t.Fatalf("want minimal cheapest, got %q", cheapestThinking("muse-spark-1.3-contributor"))
	}
}

func TestPruneOldToolOutputs(t *testing.T) {
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 8; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: strings.Repeat("x", 30000)},
		)
	}
	before := estimateTokens(h)
	reclaimed := pruneOldToolOutputs(h)
	if reclaimed <= 0 {
		t.Fatalf("expected reclaim, got %d", reclaimed)
	}
	last := h[len(h)-1].Content
	if !strings.Contains(last, strings.Repeat("x", 100)) {
		t.Fatalf("newest tool output must stay intact: %q", last[:100])
	}
	if strings.Contains(h[2].Content, strings.Repeat("x", 5000)) {
		t.Fatalf("oldest tool output must be shortened")
	}
	if !strings.Contains(h[2].Content, "pruned") {
		t.Fatalf("pruned marker missing: %q", h[2].Content)
	}
	if estimateTokens(h) >= before {
		t.Fatalf("history must shrink after prune")
	}
}

func TestSplitKeepsRecentTail(t *testing.T) {
	h := longHistory()
	head, tail := splitForCompaction(h, 200)
	if len(head) == 0 || len(tail) == 0 {
		t.Fatalf("head=%d tail=%d", len(head), len(tail))
	}
	if estimateTokens(tail) > 200 {
		t.Fatalf("tail est=%d over keep", estimateTokens(tail))
	}
	last := tail[len(tail)-1].Content
	if last != "recent question" {
		t.Fatalf("tail must end with recent: %q", last)
	}
}

func TestSplitPreservesOversizedCurrentTurn(t *testing.T) {
	request := "  Implement this exact request:\n" + strings.Repeat("specific constraint\n", 2000) + "  "
	older := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "old request"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("old", "shell")}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "old", Content: "old output"},
	}
	current := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: request},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("a", "read"), openaiToolCall("b", "shell")}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "a", Content: readOutput("active.go", 500, 1, 200)},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "b", Content: strings.Repeat("fresh output\n", 1000)},
	}
	for _, nudge := range []string{guard.BatchNudgeText, guard.ActNudgeText, guard.StallRecoveryText, guard.ClaimChallengeText} {
		current = append(current, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: nudge})
	}
	current = append(current,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("c", "shell")}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "c", Content: strings.Repeat("latest log\n", 1000)},
	)
	h := append(append([]openai.ChatCompletionMessage(nil), older...), current...)
	for _, keep := range []int{1, 200, estimateTokens(current) - 1} {
		head, tail := splitForCompaction(h, keep)
		if !reflect.DeepEqual(head, older[1:]) || !reflect.DeepEqual(tail, current) {
			t.Fatalf("keep=%d: current request and complete transactions must stay verbatim; head=%d tail=%d", keep, len(head), len(tail))
		}
	}
	if head, tail := splitForCompaction(current, 1); len(head) != 0 || !reflect.DeepEqual(tail, current) {
		t.Fatal("a lone oversized active turn has nothing safe to compact")
	}
}

func TestSplitPrefersWholeUserTurns(t *testing.T) {
	h := longHistory()
	start := len(h) - 3
	head, tail := splitForCompaction(h, estimateTokens(h[start:])+10)
	if !reflect.DeepEqual(head, h[1:start]) || !reflect.DeepEqual(tail, h[start:]) {
		t.Fatal("keep budget should retain the previous whole turn when it fits")
	}
}

func TestCompactNoOpWhenHistoryFitsOrOnlyCurrentTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no-op compaction must not call a summarizer")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name string
		h    []openai.ChatCompletionMessage
		keep int
	}{
		{"entire history fits", longHistory(), estimateTokens(longHistory())},
		{"only current turn", []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "sys"},
			{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("current request\n", 1000)},
			{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("a", "shell")}},
			{Role: openai.ChatMessageRoleTool, ToolCallID: "a", Content: "fresh result"},
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := compactTestConfig(srv.URL)
			cfg.Compaction.KeepTokens = tc.keep
			h := append([]openai.ChatCompletionMessage(nil), tc.h...)
			summary, ok, err := compactHistory(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h)
			if err != nil || ok || summary != "" || !reflect.DeepEqual(h, tc.h) {
				t.Fatalf("must be a lossless no-op: ok=%v summary=%q err=%v", ok, summary, err)
			}
		})
	}
}

func TestSerializeForSummaryExcludesThinking(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, Content: "<think>PRIVATE ANALYSIS</think>Visible decision", ToolCalls: []openai.ToolCall{openaiToolCall("a", "read")}},
		{Role: openai.ChatMessageRoleAssistant, Content: "<think>PRIVATE ONLY</think>"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Visible partial<think>PRIVATE DANGLING"},
	}
	text := serializeForSummary(h)
	if strings.Contains(text, "PRIVATE") || strings.Contains(text, "<think>") {
		t.Fatalf("thinking must not enter summary input: %s", text)
	}
	if strings.Count(text, "[Assistant]:") != 2 || !strings.Contains(text, "Visible decision") || !strings.Contains(text, "Visible partial") || !strings.Contains(text, "[Assistant tool call]: read(") {
		t.Fatalf("visible text and calls must survive: %s", text)
	}
}

func TestCompactUsesOnlyVisibleSummary(t *testing.T) {
	for _, response := range []string{"<think>private</think>VISIBLE SUMMARY", "<think>private only</think>", "<think>unfinished private"} {
		t.Run(response, func(t *testing.T) {
			srv, _ := fakeServer(t, completion(nil, response))
			cfg := compactTestConfig(srv.URL)
			h := longHistory()
			before := append([]openai.ChatCompletionMessage(nil), h...)
			summary, ok, err := compactHistory(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h)
			if strings.Contains(response, "VISIBLE SUMMARY") {
				if err != nil || !ok || summary != "VISIBLE SUMMARY" || h[1].Content != compactionSummaryPrefix+summary {
					t.Fatalf("only visible summary should be stored: summary=%q ok=%v err=%v", summary, ok, err)
				}
			} else if err == nil || ok || !reflect.DeepEqual(h, before) {
				t.Fatalf("thinking-only summary must fail without rewriting history: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestCompactTrimsOldestSummaryInput(t *testing.T) {
	for _, largeHead := range []bool{false, true} {
		t.Run(fmt.Sprintf("largeHead=%v", largeHead), func(t *testing.T) {
			var prompt string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []openai.ChatCompletionMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				prompt = body.Messages[len(body.Messages)-1].Content
				w.Header().Set("Content-Type", "application/json")
				w.Write(completion(nil, "RECENT SUMMARY"))
			}))
			defer srv.Close()
			cfg := compactTestConfig(srv.URL)
			cfg.MaxContextTokens = 10000
			cfg.Compaction.Buffer = 1000
			oldWork := strings.Repeat("old completed work\n", 80)
			if largeHead {
				oldWork = "OLDEST HEAD MARKER\n" + strings.Repeat("obsolete detail\n", 4000)
			}
			current := "  CURRENT REQUEST must remain exact\n  "
			h := []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: "sys"},
				{Role: openai.ChatMessageRoleAssistant, Content: compactionSummaryPrefix + "OLDEST SUMMARY MARKER\n" + strings.Repeat("past detail\n", 6000) + "<think>PRIVATE PREVIOUS SUMMARY</think>RECENT PREVIOUS SUMMARY"},
				{Role: openai.ChatMessageRoleUser, Content: oldWork},
				{Role: openai.ChatMessageRoleAssistant, Content: "RECENT COMPLETED WORK"},
				{Role: openai.ChatMessageRoleUser, Content: current},
			}
			_, ok, err := compactHistory(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h)
			if err != nil || !ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if strings.Contains(prompt, "OLDEST") || strings.Contains(prompt, "PRIVATE") || !strings.Contains(prompt, "RECENT COMPLETED WORK") || !strings.Contains(prompt, "older head truncated") {
				t.Fatal("oversized summary input must drop its oldest prefix, not recent work")
			}
			if !largeHead && !strings.Contains(prompt, "RECENT PREVIOUS SUMMARY") {
				t.Fatal("the recent part of a large previous summary should survive when there is room")
			}
			if len([]rune(prompt))/4 > cfg.MaxContextTokens-cfg.Compaction.Buffer {
				t.Fatalf("summary input should be bounded: %d runes", len([]rune(prompt)))
			}
			if h[len(h)-1].Content != current {
				t.Fatal("current request must stay verbatim outside summary input")
			}
		})
	}
}

// TestCompactKeepsRecent: long fixture compacts to system + one summary +
// verbatim tail; the old head text is gone from the send-side history.
func TestCompactKeepsRecent(t *testing.T) {
	srv, _ := fakeServer(t, completion(nil, "SUMMARY TEXT"))
	cfg := compactTestConfig(srv.URL)
	h := longHistory()
	summary, ok, err := compactHistory(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if summary != "SUMMARY TEXT" {
		t.Fatalf("summary=%q", summary)
	}
	if h[0].Role != openai.ChatMessageRoleSystem || h[0].Content != "sys" {
		t.Fatalf("system must be preserved: %+v", h[0])
	}
	if !strings.HasPrefix(h[1].Content, compactionSummaryPrefix) {
		t.Fatalf("summary must follow system: %q", h[1].Content[:60])
	}
	joined := ""
	for _, m := range h {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "recent question") {
		t.Fatalf("tail must be kept verbatim")
	}
	if strings.Contains(joined, "question qqq") {
		t.Fatalf("old head must be replaced by the summary")
	}
}

// TestCompactUpdatesInPlace: a second compaction folds into the existing
// summary instead of stacking two summary blocks.
func TestCompactUpdatesInPlace(t *testing.T) {
	srv, _ := fakeServer(t, completion(nil, "FIRST"), completion(nil, "SECOND"))
	cfg := compactTestConfig(srv.URL)
	h := longHistory()
	if _, ok, err := compactHistory(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h); err != nil || !ok {
		t.Fatalf("first ok=%v err=%v", ok, err)
	}
	h = append(h, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "more " + strings.Repeat("m", 2000)})
	if _, ok, err := compactHistory(context.Background(), cfg, newClient(cfg, "key"), "key", nil, &h); err != nil || !ok {
		t.Fatalf("second ok=%v err=%v", ok, err)
	}
	n := 0
	for _, m := range h {
		if strings.HasPrefix(m.Content, compactionSummaryPrefix) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 summary block, got %d", n)
	}
}

// TestCompactAutoRetry: provider 400s the turn as too-long, the cheap model
// summarizes, the retried turn succeeds — offline, one model per role.
// History sits between keep_tokens (head exists) and the soft threshold (no
// pre-send compact), so only the 400-recovery path fires.
func TestCompactAutoRetry(t *testing.T) {
	var mainCalls int
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		models = append(models, body.Model)
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "cheap-flash" {
			w.Write(completion(nil, "CHEAP SUMMARY"))
			return
		}
		mainCalls++
		if mainCalls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"This model's maximum context length is exceeded","type":"invalid_request"}}`))
			return
		}
		w.Write(completion(nil, "recovered"))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	cfg.MaxContextTokens = 100000
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 2000, Model: "cheap-flash"}, cfg.MaxContextTokens)
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 20; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "question " + strings.Repeat("q", 400)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "answer " + strings.Repeat("a", 400)},
		)
	}
	if est := estimateTokens(h); est <= 2000 || est >= 6000 {
		t.Fatalf("fixture must sit between keep and threshold, est=%d", est)
	}
	reply, _, err := runTurn(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), &h, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if reply.Content != "recovered" {
		t.Fatalf("reply=%q", reply.Content)
	}
	foundCheap := false
	for _, m := range models {
		if m == "cheap-flash" {
			foundCheap = true
		}
	}
	if !foundCheap {
		t.Fatalf("cheap summarizer never called: %v", models)
	}
	foundSummary := false
	for _, m := range h {
		if strings.HasPrefix(m.Content, compactionSummaryPrefix) {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatalf("history must carry the summary after recovery")
	}
}

func TestIsContextOverflow(t *testing.T) {
	over := []error{
		&transportError{Transport: "chat", Status: 400, Message: "maximum context length exceeded"},
		&transportError{Transport: "chat", Status: 413, Message: "payload"},
		&transportError{Transport: "responses", Status: 400, Message: "context too long"},
		&openai.APIError{HTTPStatusCode: 400, Message: "context_length_exceeded"},
		&openai.RequestError{HTTPStatusCode: 400, Err: errors.New("prompt is too long: 250000 tokens > 200000 maximum")},
		fmt.Errorf("wrapped: %w", &transportError{Status: 422, Message: "input tokens exceed the token limit"}),
		errors.New("request exceeds the context window"),
	}
	for _, err := range over {
		if !isContextOverflow(err) {
			t.Fatalf("must be overflow: %v", err)
		}
	}
	under := []error{
		&transportError{Transport: "chat", Status: 400, Message: "streaming is not supported"},
		&transportError{Transport: "chat", Status: 429, Message: "rate limited, slow down"},
		context.Canceled,
		fmt.Errorf("wrapped: %w", context.Canceled),
		context.DeadlineExceeded,
		&transportError{Status: 400, Message: "max_tokens is an invalid parameter"},
		&openai.APIError{HTTPStatusCode: 400, Message: "max_tokens exceeds the maximum allowed output tokens"},
		&transportError{Status: 400, Message: "max_tokens exceeds the model's token limit"},
		&transportError{Status: 400, Message: "unsupported context parameter"},
		&transportError{Status: 400, Message: "tool name too long"},
		&transportError{Status: 400, Message: "temperature exceeds the allowed range"},
		&transportError{Status: 429, Message: "input tokens exceed rate limit"},
		&transportError{Status: 500, Message: "failed to read context length: upstream timeout"},
	}
	for _, err := range under {
		if isContextOverflow(err) {
			t.Fatalf("must not be overflow: %v", err)
		}
	}
}
