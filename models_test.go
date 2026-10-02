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
	if got := AvailableThinkingLevels("glm-5.3-flash"); strings.Join(got, ",") != "high,max" {
		t.Fatalf("glm levels=%v", got)
	}
	if got := AvailableThinkingLevels("deepseek-v4-pro"); strings.Join(got, ",") != "high,max" {
		t.Fatalf("deepseek levels=%v", got)
	}
	if got := AvailableThinkingLevels("kimi-k3"); strings.Join(got, ",") != "none,high" {
		t.Fatalf("kimi levels=%v", got)
	}
	if got := AvailableThinkingLevels("mimo-v2.5"); strings.Join(got, ",") != "none,high" {
		t.Fatalf("mimo levels=%v", got)
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
	if validThinkingLevel("kimi-k3", "max") {
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
	applyThinkingToRequest(&toggle, "kimi-k3", "none")
	if toggle.ChatTemplateKwargs["enable_thinking"] != false {
		t.Fatalf("kwargs=%v", toggle.ChatTemplateKwargs)
	}
	applyThinkingToRequest(&toggle, "kimi-k3", "high")
	if toggle.ChatTemplateKwargs["enable_thinking"] != true {
		t.Fatalf("kwargs=%v", toggle.ChatTemplateKwargs)
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
	if err := persistModelSelection(path, "kimi-k3", "high"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, `model = "kimi-k3"`) || !strings.Contains(got, `thinking = "high"`) {
		t.Fatalf("persisted:\n%s", got)
	}
	if !strings.Contains(got, "# comment") || !strings.Contains(got, `base_url = "https://x"`) {
		t.Fatalf("comments/keys must survive:\n%s", got)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("persisted file must reload: %v", err)
	}
	if cfg.Model != "kimi-k3" || cfg.Thinking != "high" {
		t.Fatalf("reloaded=%+v", cfg)
	}
	if err := persistModelSelection(path, "kimi-k3", ""); err != nil {
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
	cfg.Model = "kimi-k3"
	cfg.Thinking = "none"
	captureBody(t, cfg, func(body map[string]any) {
		kwargs, ok := body["chat_template_kwargs"].(map[string]any)
		if !ok || kwargs["enable_thinking"] != false {
			t.Errorf("kwargs=%v", body["chat_template_kwargs"])
		}
	})
}
