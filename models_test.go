package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestAvailableThinkingLevels(t *testing.T) {
	if got := AvailableThinkingLevels("glm-5.3-flash"); strings.Join(got, ",") != "low,high,max" {
		t.Fatalf("glm levels=%v", got)
	}
	if got := AvailableThinkingLevels("deepseek-v4-pro"); strings.Join(got, ",") != "high,max" {
		t.Fatalf("deepseek levels=%v", got)
	}
	if got := AvailableThinkingLevels("kimi-k3"); strings.Join(got, ",") != "max" {
		t.Fatalf("kimi-k3 levels=%v", got)
	}
	if got := AvailableThinkingLevels("kimi-k2.6"); strings.Join(got, ",") != "none,high" {
		t.Fatalf("kimi toggle levels=%v", got)
	}
	if got := AvailableThinkingLevels("mimo-v2.5"); len(got) != 0 {
		t.Fatalf("mimo has no adjustable thinking, got=%v", got)
	}
	if got := AvailableThinkingLevels("muse-spark-1.3-contributor"); strings.Join(got, ",") != "minimal,low,medium,high,xhigh" {
		t.Fatalf("muse levels=%v", got)
	}
	if got := AvailableThinkingLevels("gpt-5.6-luna"); strings.Join(got, ",") != "none,low,medium,high,xhigh,max" {
		t.Fatalf("gpt levels=%v", got)
	}
	if got := AvailableThinkingLevels("custom-thing"); len(got) == 0 {
		t.Fatal("unknown models should get the standard set")
	}
}

func TestValidThinkingLevel(t *testing.T) {
	if !validThinkingLevel("glm-5.3", "") || !validThinkingLevel("glm-5.3", "HIGH") {
		t.Fatal("empty and case-insensitive levels should validate")
	}
	if validThinkingLevel("glm-5.3", "none") {
		t.Fatal("none is not offered on effort models")
	}
	if !validThinkingLevel("kimi-k3", "max") {
		t.Fatal("max-only kimi-k3 should accept max")
	}
	if validThinkingLevel("kimi-k3", "high") {
		t.Fatal("high is not offered on max-only kimi-k3")
	}
	if validThinkingLevel("kimi-k2.6", "max") {
		t.Fatal("max is not offered on toggle models")
	}
}

func TestApplyThinkingToRequest(t *testing.T) {
	var req openai.ChatCompletionRequest
	applyThinkingToRequest(&req, "glm-5.3", "")
	if req.ReasoningEffort != "" || req.ChatTemplateKwargs != nil {
		t.Fatal("empty thinking must leave the request untouched")
	}
	applyThinkingToRequest(&req, "glm-5.3", "max")
	if req.ReasoningEffort != "max" {
		t.Fatalf("reasoning_effort=%q", req.ReasoningEffort)
	}
	var toggle openai.ChatCompletionRequest
	applyThinkingToRequest(&toggle, "kimi-k2.6", "none")
	if toggle.ChatTemplateKwargs["enable_thinking"] != false {
		t.Fatalf("kwargs=%v", toggle.ChatTemplateKwargs)
	}
	applyThinkingToRequest(&toggle, "kimi-k2.6", "high")
	if toggle.ChatTemplateKwargs["enable_thinking"] != true {
		t.Fatalf("kwargs=%v", toggle.ChatTemplateKwargs)
	}
	var fixed openai.ChatCompletionRequest
	applyThinkingToRequest(&fixed, "mimo-v2.5", "high")
	if fixed.ReasoningEffort != "" || fixed.ChatTemplateKwargs != nil {
		t.Fatalf("fixed no-control models must omit thinking, got %+v", fixed)
	}
}

func TestModelTransport(t *testing.T) {
	cases := map[string]string{
		"glm-5.3-flash": "chat", "kimi-k3": "chat", "hy3": "chat",
		"space-bunny-free": "chat", "mimo-v2.5": "chat",
		"gpt-5.6-luna": "responses", "gpt-6-luna": "responses", "grok-4.7": "responses",
		"grok-4.6":                   "responses",
		"muse-spark-1.3-contributor": "responses", "muse-spark-1.2-contributor": "responses",
		"minimax-m3": "messages", "minimax-m2.7": "messages",
		"qwen3.8-max": "messages", "qwen3.8-flash": "messages",
		"qwen3.7-plus": "messages",
	}
	for id, want := range cases {
		if got := modelTransport(id); got != want {
			t.Fatalf("%s transport=%s want %s", id, got, want)
		}
	}
	if !chatCompatible("glm-5.3") || chatCompatible("gpt-5.6-luna") || chatCompatible("qwen3.8-flash") {
		t.Fatal("chat compatibility mismatch")
	}
}

func TestMatchGoModel(t *testing.T) {
	ids := []string{"glm-5.3", "glm-5.3-flash", "kimi-k3"}
	if got := matchGoModel(ids, "KIMI-K3"); len(got) != 1 || got[0] != "kimi-k3" {
		t.Fatalf("exact=%v", got)
	}
	if got := matchGoModel(ids, "glm"); len(got) != 2 {
		t.Fatalf("substring=%v", got)
	}
	if got := matchGoModel(ids, "nope"); len(got) != 0 {
		t.Fatalf("nomatch=%v", got)
	}
}

func TestPersistModelSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "# comment\nmodel = \"glm-5.3-flash\"\n# thinking = \"high\"\nbase_url = \"https://x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistModelSelection(path, "kimi-k2.6", "high"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, `model = "kimi-k2.6"`) || !strings.Contains(got, `thinking = "high"`) {
		t.Fatalf("persisted:\n%s", got)
	}
	if !strings.Contains(got, "# comment") || !strings.Contains(got, `base_url = "https://x"`) {
		t.Fatalf("comments/keys must survive:\n%s", got)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("persisted file must reload: %v", err)
	}
	if cfg.Model != "kimi-k2.6" || cfg.Thinking != "high" {
		t.Fatalf("reloaded=%+v", cfg)
	}
	if err := persistModelSelection(path, "kimi-k2.6", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "\nthinking =") {
		t.Fatalf("empty thinking must remove the line:\n%s", raw)
	}
}

func TestLoadConfigThinkingValidation(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"glm-5.3\"\nthinking = \"MAX\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thinking != "max" {
		t.Fatalf("thinking=%q", cfg.Thinking)
	}
	if _, err := loadConfig(writeConfig(t, "model = \"glm-5.3\"\nthinking = \"none\"\n")); err == nil {
		t.Fatal("expected rejection of none on glm")
	}
}

func captureBody(t *testing.T, cfg Config, fn func(map[string]any)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		fn(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()
	cfg.BaseURL = srv.URL
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestRunTurnSendsReasoningEffort(t *testing.T) {
	cfg := newTestConfig("https://unused")
	cfg.Model = "glm-5.3"
	cfg.Thinking = "max"
	captureBody(t, cfg, func(body map[string]any) {
		if body["reasoning_effort"] != "max" {
			t.Errorf("reasoning_effort=%v", body["reasoning_effort"])
		}
	})
}

func TestRunTurnOmitsThinkingByDefault(t *testing.T) {
	cfg := newTestConfig("https://unused")
	cfg.Model = "glm-5.3"
	captureBody(t, cfg, func(body map[string]any) {
		if _, ok := body["reasoning_effort"]; ok {
			t.Errorf("reasoning_effort should be omitted, got %v", body["reasoning_effort"])
		}
		if _, ok := body["chat_template_kwargs"]; ok {
			t.Errorf("chat_template_kwargs should be omitted, got %v", body["chat_template_kwargs"])
		}
	})
}

func TestRunTurnSendsEnableThinking(t *testing.T) {
	cfg := newTestConfig("https://unused")
	cfg.Model = "kimi-k2.6"
	cfg.Thinking = "none"
	captureBody(t, cfg, func(body map[string]any) {
		kwargs, ok := body["chat_template_kwargs"].(map[string]any)
		if !ok || kwargs["enable_thinking"] != false {
			t.Errorf("kwargs=%v", body["chat_template_kwargs"])
		}
	})
}

func TestParseThinkingLevelsFromError(t *testing.T) {
	got := parseThinkingLevelsFromError("Upstream request failed: [400] validation failed: effort must be one of: minimal low medium high xhigh max")
	if strings.Join(got, ",") != "minimal,low,medium,high,xhigh,max" {
		t.Fatalf("got=%v", got)
	}
	got = parseThinkingLevelsFromError(`error: pick one of these: "low", "high", "max"`)
	if strings.Join(got, ",") != "low,high,max" {
		t.Fatalf("quoted got=%v", got)
	}
	if got := parseThinkingLevelsFromError("connection refused"); got != nil {
		t.Fatalf("non-validation should be nil, got=%v", got)
	}
	if got := parseThinkingLevelsFromError("effort must be one of: high, max"); strings.Join(got, ",") != "high,max" {
		t.Fatalf("subset got=%v", got)
	}
}

func TestThinkingLevelsCacheRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, ok := cachedThinkingLevels("custom-xyz"); ok {
		t.Fatal("empty cache should miss")
	}
	if !maybeLearnThinkingLevels("custom-xyz", "effort must be one of: low medium high") {
		t.Fatal("should learn from 400")
	}
	lv, ok := cachedThinkingLevels("custom-xyz")
	if !ok || strings.Join(lv, ",") != "low,medium,high" {
		t.Fatalf("cached=%v ok=%v", lv, ok)
	}
	if got := AvailableThinkingLevels("custom-xyz"); strings.Join(got, ",") != "low,medium,high" {
		t.Fatalf("available should use cache, got=%v", got)
	}
}

func TestProbeThinkingLevelsUsesCacheAndTable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Known models never hit the network (bad URL would fail if called).
	cfg := newTestConfig("https://unused.invalid")
	cfg.Model = "muse-spark-1.3-contributor"
	lv, err := probeThinkingLevels(context.Background(), cfg, "key", nil)
	if err != nil || strings.Join(lv, ",") != "minimal,low,medium,high,xhigh" {
		t.Fatalf("muse probe=%v err=%v", lv, err)
	}
	// Unknown model with a 400 listing levels learns and caches.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"effort must be one of: low high max"}}`))
	}))
	defer srv.Close()
	cfg2 := newTestConfig(srv.URL)
	cfg2.Model = "future-thing-9"
	lv, err = probeThinkingLevels(context.Background(), cfg2, "key", srv.Client())
	if err != nil || strings.Join(lv, ",") != "low,high,max" {
		t.Fatalf("probe=%v err=%v", lv, err)
	}
	if cached, ok := cachedThinkingLevels("future-thing-9"); !ok || strings.Join(cached, ",") != "low,high,max" {
		t.Fatalf("not cached: %v %v", cached, ok)
	}
}

func TestMessagesThinkingBudgets(t *testing.T) {
	if got := messagesThinking("none"); got["type"] != "disabled" {
		t.Fatalf("none=%v", got)
	}
	if got := messagesThinking("low"); got["type"] != "enabled" {
		t.Fatalf("low=%v", got)
	}
	if got := messagesThinking("bogus"); got != nil {
		t.Fatalf("unknown should omit, got=%v", got)
	}
}
