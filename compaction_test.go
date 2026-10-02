package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
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
	if _, ok := readDescriptor("just some shell output\nline2"); ok {
		t.Fatal("non-read output must not match")
	}
}

func TestPruneKeepsNewestReads(t *testing.T) {
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}
	for i := 0; i < 4; i++ {
		h = append(h,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{openaiToolCall("id", "read")}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "id", Content: readOutput(fmt.Sprintf("f%d.go", i), 500, 1, 200)},
		)
	}
	if got := pruneOldToolOutputs(h); got <= 0 {
		t.Fatalf("expected reclaim, got %d", got)
	}
	// Newest two reads stay intact; older two collapse to descriptors.
	for i, wantIntact := range []bool{false, false, true, true} {
		content := h[2+i*2].Content
		if wantIntact && !strings.Contains(content, "some code line") {
			t.Fatalf("read %d must stay intact", i)
		}
		if !wantIntact && strings.Contains(content, "some code line") {
			t.Fatalf("read %d must collapse to a descriptor", i)
		}
		if !wantIntact && !strings.Contains(content, "re-read with read") {
			t.Fatalf("read %d descriptor missing re-read hint", i)
		}
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
	if cfg.Compaction.Buffer != cfg.MaxContextTokens/10 {
		t.Fatalf("buffer=%d budget=%d", cfg.Compaction.Buffer, cfg.MaxContextTokens)
	}
	if cfg.Compaction.Model != "" {
		t.Fatalf("model should default empty (session model): %q", cfg.Compaction.Model)
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
	}
	for _, err := range over {
		if !isContextOverflow(err) {
			t.Fatalf("must be overflow: %v", err)
		}
	}
	under := []error{
		&transportError{Transport: "chat", Status: 400, Message: "streaming is not supported"},
		&transportError{Transport: "chat", Status: 429, Message: "rate limited, slow down"},
	}
	for _, err := range under {
		if isContextOverflow(err) {
			t.Fatalf("must not be overflow: %v", err)
		}
	}
}
