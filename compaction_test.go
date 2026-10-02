package main

import (
	"context"
	"encoding/json"
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
func TestCompactAutoRetry(t *testing.T) {
	var calls int
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
		calls++
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "cheap-flash" {
			w.Write(completion(nil, "CHEAP SUMMARY"))
			return
		}
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"This model's maximum context length is exceeded","type":"invalid_request"}}`))
			return
		}
		w.Write(completion(nil, "recovered"))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	cfg.MaxContextTokens = 100000
	cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 200, Model: "cheap-flash"}, cfg.MaxContextTokens)
	h := longHistory()
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
