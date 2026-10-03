package main

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestPersistModelSelectionKeepsTopLevelScope(t *testing.T) {
	// Regression: appending model/thinking after a [table] header silently
	// scopes them as table keys (input.model), which strict loading rejects.
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "model = \"m\"\n[input]\nblink = false\nheight = 2\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistModelSelection(path, "kimi-k2.6", "high"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("persisted file must reload: %v", err)
	}
	if cfg.Model != "kimi-k2.6" || cfg.Thinking != "high" {
		t.Fatalf("reloaded=%+v", cfg)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(string(raw), "\n")
	headerAt, modelAt, thinkAt := -1, -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "[input]" {
			headerAt = i
		} else if isKeyLine(t, "model") {
			modelAt = i
		} else if isKeyLine(t, "thinking") {
			thinkAt = i
		}
	}
	if headerAt < 0 || modelAt < 0 || thinkAt < 0 || modelAt > headerAt || thinkAt > headerAt {
		t.Fatalf("top-level keys must precede [input]:\n%s", raw)
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

func TestModelProviderForms(t *testing.T) {
	for id, want := range map[string]string{
		"glm-5.3-flash":                    ProviderGo,
		"muse-spark-1.3-contributor":       ProviderGo,
		"openrouter/moonshotai/kimi-k3":    ProviderOpenRouter,
		"OpenRouter/anthropic/claude-x":    ProviderOpenRouter,
		"  openrouter/z-ai/glm-5.3-flash ": ProviderOpenRouter,
	} {
		if got := modelProvider(id); got != want {
			t.Fatalf("%q provider=%s want %s", id, got, want)
		}
	}
	if got := wireModelID("openrouter/moonshotai/kimi-k3"); got != "moonshotai/kimi-k3" {
		t.Fatalf("wire=%q", got)
	}
	if got := wireModelID("glm-5.3-flash"); got != "glm-5.3-flash" {
		t.Fatalf("go wire must pass through: %q", got)
	}
	if got := displayModelID("glm-5.3-flash"); got != "go/glm-5.3-flash" {
		t.Fatalf("go display must prefix: %q", got)
	}
	if got := displayModelID("openrouter/moonshotai/kimi-k3"); got != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("openrouter display must stay: %q", got)
	}
	for id, want := range map[string]string{
		"openrouter/moonshotai/kimi-k3": "kimi-k3",
		"openrouter/z-ai/glm-5.3":       "glm-5.3",
		"glm-5.3-flash":                 "glm-5.3-flash",
	} {
		if got := modelBaseID(id); got != want {
			t.Fatalf("%q base=%q want %q", id, got, want)
		}
	}
}

func TestAvailableThinkingLevelsOpenRouter(t *testing.T) {
	// Known families resolve through the vendor segment …
	if got := AvailableThinkingLevels("openrouter/moonshotai/kimi-k3"); strings.Join(got, ",") != "max" {
		t.Fatalf("kimi-k3 levels=%v", got)
	}
	if got := AvailableThinkingLevels("openrouter/moonshotai/kimi-k2.6"); strings.Join(got, ",") != "none,high" {
		t.Fatalf("kimi-k2.6 levels=%v", got)
	}
	// … unknown vendors fall back to the standard vocabulary until a 400
	// teaches the valid list.
	if got := AvailableThinkingLevels("openrouter/anthropic/claude-opus-5.5"); len(got) == 0 {
		t.Fatal("unknown openrouter models should get the standard set")
	}
	if got := modelTransport("openrouter/anthropic/claude-opus-5.5"); got != "chat" {
		t.Fatalf("openrouter transport=%s want chat", got)
	}
}

func TestLoadConfigProviderValidation(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, "model = \"openrouter/moonshotai/kimi-k3\"\nthinking = \"max\"\n")); err != nil {
		t.Fatalf("openrouter model must load: %v", err)
	}
	if _, err := loadConfig(writeConfig(t, "model = \"anthropic/claude-x\"\n")); err == nil {
		t.Fatal("bare vendor/model without the openrouter/ prefix must be rejected")
	}
}

func TestMatchGoModelProviderForms(t *testing.T) {
	ids := []string{"kimi-k3", "openrouter/moonshotai/kimi-k3", "glm-5.3-flash"}
	if got := matchGoModel(ids, "kimi-k3"); len(got) != 1 || got[0] != "kimi-k3" {
		t.Fatalf("bare exact must win: %v", got)
	}
	if got := matchGoModel(ids, "openrouter/moonshotai/kimi-k3"); len(got) != 1 || got[0] != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("prefixed exact must resolve stored: %v", got)
	}
	// Substring sees display and wire forms: "moonshotai" only matches the
	// OpenRouter row, "go/kimi" only the Go row via its display form.
	if got := matchGoModel(ids, "moonshotai"); len(got) != 1 || got[0] != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("vendor substring=%v", got)
	}
	if got := matchGoModel(ids, "go/kimi"); len(got) != 1 || got[0] != "kimi-k3" {
		t.Fatalf("display substring=%v", got)
	}
}

func TestFetchOpenRouterModels(t *testing.T) {
	var gotAuth, gotTitle, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotAuth, gotTitle, gotUA = r.Header.Get("Authorization"), r.Header.Get("X-Title"), r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[
			{"id":"moonshotai/kimi-k3"},
			{"id":"b-model"},
			{"id":"a-model"},
			{"id":"x/y:batch"},
			{"id":"~alias"},
			{"id":""},
			{"id":"keep/free"}
		]}`)
	}))
	defer srv.Close()
	ids, err := fetchOpenRouterModels(context.Background(), srv.Client(), srv.URL, "or-key")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "openrouter/a-model,openrouter/b-model,openrouter/keep/free,openrouter/moonshotai/kimi-k3" {
		t.Fatalf("ids=%v", ids)
	}
	if gotAuth != "Bearer or-key" || gotTitle != "valen" || gotUA != userAgent {
		t.Fatalf("headers auth=%q title=%q ua=%q", gotAuth, gotTitle, gotUA)
	}
	if len(prefixedOpenRouterFallback()) == 0 {
		t.Fatal("fallback list must not be empty")
	}
	for _, id := range prefixedOpenRouterFallback() {
		if modelProvider(id) != ProviderOpenRouter {
			t.Fatalf("fallback %q must be stored prefixed", id)
		}
	}
}

// TestOpenRouterAuthFailureBlamesRightKey: a 401 on OpenRouter traffic must
// name the OpenRouter env var (and the transport in details), never the Go
// api_key_env. Regression: the turn path once labeled everything "chat".
func TestOpenRouterAuthFailureBlamesRightKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"No auth credentials found","code":401}}`))
	}))
	defer srv.Close()

	oldBase := openRouterBaseURL
	openRouterBaseURL = srv.URL
	defer func() { openRouterBaseURL = oldBase }()

	cfg := newTestConfig(srv.URL)
	cfg.Model = "openrouter/moonshotai/kimi-k3"
	cfg.OpenRouterKey = "bad-key"
	err := chat(context.Background(), cfg, newClient(cfg, "go-key"), "go-key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected 401")
	}
	fe := friendlyError(err)
	if !strings.Contains(fe.Title, "openrouter_api_key_env") {
		t.Fatalf("title blames wrong key: %q", fe.Title)
	}
	if strings.Contains(fe.Title, "api_key_env.") && !strings.Contains(fe.Title, "openrouter_") {
		t.Fatalf("title still points at the Go env: %q", fe.Title)
	}
	if !strings.Contains(fe.Details, "openrouter") {
		t.Fatalf("details must name the transport: %q", fe.Details)
	}
}

// TestOpenRouterGateSurfacesProviderMessage: when OpenRouter refuses with a
// requirement (attestation, guardrail), the transcript title must carry the
// provider's message — it names the fix — instead of a generic line.
func TestOpenRouterGateSurfacesProviderMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"This model requires you to complete the following before use: 18+ age confirmation. Confirm at https://openrouter.ai/settings/preferences.","code":403}}`))
	}))
	defer srv.Close()

	oldBase := openRouterBaseURL
	openRouterBaseURL = srv.URL
	defer func() { openRouterBaseURL = oldBase }()

	cfg := newTestConfig(srv.URL)
	cfg.Model = "openrouter/meta/muse-spark-1.3-contributor"
	cfg.OpenRouterKey = "good-key"
	err := chat(context.Background(), cfg, newClient(cfg, "go-key"), "go-key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected 403")
	}
	fe := friendlyError(err)
	if !strings.Contains(fe.Title, "18+ age confirmation") || !strings.Contains(fe.Title, "openrouter.ai/settings/preferences") {
		t.Fatalf("transcript must show the provider message: %q", fe.Title)
	}
	if strings.Contains(fe.Title, "rejected the key") {
		t.Fatalf("must not blame the key: %q", fe.Title)
	}
}

// TestOpenRouterChatWire pins the OpenRouter request shape: stripped
// vendor/model on the wire, OpenRouter key (not the Go key), no Go session
// header, and the configured effort as reasoning_effort.
func TestOpenRouterChatWire(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotSession, gotTitle string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth, gotSession, gotTitle = r.Header.Get("Authorization"), r.Header.Get("x-opencode-session"), r.Header.Get("X-Title")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	oldBase := openRouterBaseURL
	openRouterBaseURL = srv.URL
	defer func() { openRouterBaseURL = oldBase }()

	cfg := newTestConfig(srv.URL)
	cfg.Model = "openrouter/moonshotai/kimi-k3"
	cfg.Thinking = "max"
	cfg.OpenRouterKey = "or-key"
	if err := chat(context.Background(), cfg, newClient(cfg, "go-key"), "go-key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path=%s", gotPath)
	}
	if gotBody["model"] != "moonshotai/kimi-k3" {
		t.Fatalf("wire model=%v", gotBody["model"])
	}
	if gotBody["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort=%v", gotBody["reasoning_effort"])
	}
	if gotAuth != "Bearer or-key" {
		t.Fatalf("auth=%q (Go key must never ride OpenRouter traffic)", gotAuth)
	}
	if gotSession != "" {
		t.Fatalf("session=%q (Go routing header must not leak to OpenRouter)", gotSession)
	}
	if gotTitle != "valen" {
		t.Fatalf("title=%q", gotTitle)
	}
}
