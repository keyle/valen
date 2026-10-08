package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Missing keys must never block boot: dispatchTurn fails fast without any
// network, and friendlyError names the right provider env.
func TestMissingKeyErrorDistinguishesProviders(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openRouterKeyEnv, "")

	goCfg := Config{Model: "glm-5.3-flash", MaxRounds: 1}
	if err := missingKeyError(goCfg, ""); err == nil || !strings.Contains(err.Error(), openCodeKeyEnv) {
		t.Fatalf("go missing err=%v", err)
	} else {
		fe := friendlyError(err)
		if !strings.Contains(fe.Title, "OpenCode API key not found") {
			t.Fatalf("go title=%q", fe.Title)
		}
	}
	if err := missingKeyError(goCfg, "  live-key "); err != nil {
		t.Fatalf("go with key must pass: %v", err)
	}

	orCfg := Config{Model: "openrouter/moonshotai/kimi-k3", MaxRounds: 1}
	if err := missingKeyError(orCfg, "go-key"); err == nil || !strings.Contains(err.Error(), openRouterKeyEnv) {
		t.Fatalf("or missing err=%v", err)
	} else {
		fe := friendlyError(err)
		if !strings.Contains(fe.Title, "OpenRouter key not found") {
			t.Fatalf("or title=%q", fe.Title)
		}
		if strings.Contains(fe.Title, "OpenCode Go") {
			t.Fatalf("or title blames Go: %q", fe.Title)
		}
	}
	orCfg.OpenRouterKey = " or-live "
	if err := missingKeyError(orCfg, ""); err != nil {
		t.Fatalf("or with key must pass: %v", err)
	}
}

// dispatchTurn with missing keys must not touch the network.
func TestDispatchTurnMissingKeyMakesNoNetworkCall(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openRouterKeyEnv, "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, model := range []string{"glm-5.3-flash", "openrouter/moonshotai/kimi-k3"} {
		cfg := Config{Model: model, BaseURL: srv.URL, MaxRounds: 1, MaxContextTokens: 100000, RequestTimeoutSeconds: 5}
		h := seedHistory(cfg)
		_, _, err := dispatchTurn(context.Background(), cfg, newClient(cfg, ""), "", srv.Client(), NewRegistry(), &h, nil)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "is not set") {
			t.Fatalf("model %s err=%v", model, err)
		}
	}
	if calls != 0 {
		t.Fatalf("missing keys must not call network, calls=%d", calls)
	}
}

// Live env exports after boot must unblock dispatch without a restart.
func TestEffectiveKeysReadLiveEnv(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "live-go")
	cfg := Config{Model: "m"}
	if got := effectiveOpenCodeKey(cfg, ""); got != "live-go" {
		t.Fatalf("go live=%q", got)
	}
	t.Setenv(openRouterKeyEnv, "live-or")
	ocfg := Config{Model: "openrouter/m/a"}
	if got := effectiveOpenRouterKey(ocfg); got != "live-or" {
		t.Fatalf("or live=%q", got)
	}
	// Config values win over env.
	if got := effectiveOpenCodeKey(Config{OpenCodeGoKey: " cfg-go "}, ""); got != "cfg-go" {
		t.Fatalf("go cfg=%q", got)
	}
	if got := effectiveOpenRouterKey(Config{OpenRouterKey: "cfg-or"}); got != "cfg-or" {
		t.Fatalf("or cfg=%q", got)
	}
}

// TUI submit with a missing key stays idle with a provider-specific notice.
func TestTUISubmitWithoutKeyShowsNotice(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openRouterKeyEnv, "")
	cfg := Config{Model: "glm-5.3-flash", BaseURL: "https://unused", SessionID: "ses-tui", MaxRounds: 4}
	m := newTestModel(t, cfg)
	m.apiKey = ""
	m.cfg.OpenRouterKey = ""
	if cmd := m.submit("hello"); cmd != nil {
		t.Fatalf("missing go key must not start a turn")
	}
	if m.busy || !m.noticeErr || !strings.Contains(m.notice, "OpenCode API key not found") {
		t.Fatalf("go notice=%q err=%v busy=%v", m.notice, m.noticeErr, m.busy)
	}

	m.cfg.Model = "openrouter/moonshotai/kimi-k3"
	m.apiKey = ""
	m.cfg.OpenRouterKey = ""
	if cmd := m.submit("hello"); cmd != nil {
		t.Fatalf("missing or key must not start a turn")
	}
	if m.busy || !m.noticeErr || !strings.Contains(m.notice, "OpenRouter key not found") {
		t.Fatalf("or notice=%q err=%v busy=%v", m.notice, m.noticeErr, m.busy)
	}
}

// Picking a model without its key still switches but warns which key is missing.
func TestTUIApplyModelSwitchWarnsMissingKey(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openRouterKeyEnv, "")
	cfg := Config{Model: "glm-5.3-flash", BaseURL: "https://unused", SessionID: "ses-tui", MaxRounds: 4}
	m := newTestModel(t, cfg)
	m.apiKey = ""
	m.cfg.OpenRouterKey = ""
	m.statePath = ""

	m.applyModelSwitch("openrouter/moonshotai/kimi-k3")
	if m.cfg.Model != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("switch must still land, model=%q", m.cfg.Model)
	}
	if !m.noticeErr || !strings.Contains(m.notice, "OpenRouter key not found") {
		t.Fatalf("or pick notice=%q", m.notice)
	}

	m.applyModelSwitch("glm-5.3-flash")
	if !m.noticeErr || !strings.Contains(m.notice, "OpenCode API key not found") {
		t.Fatalf("go pick notice=%q", m.notice)
	}
}

// Keyless picker still lists both providers from fallbacks (offline).
func TestTUIModelPickerListsBothWithoutKeys(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openRouterKeyEnv, "")
	oldBase := openRouterBaseURL
	openRouterBaseURL = "http://127.0.0.1:1"
	defer func() { openRouterBaseURL = oldBase }()

	cfg := Config{Model: "glm-5.3-flash", BaseURL: "http://127.0.0.1:1", SessionID: "ses-tui", MaxRounds: 4}
	m := newTestModel(t, cfg)
	m.apiKey = ""
	m.cfg.OpenRouterKey = ""
	m.client = newClient(cfg, "")

	msg := m.openModelPicker("")()
	loaded, ok := msg.(modelsLoadedMsg)
	if !ok {
		t.Fatalf("msg=%T", msg)
	}
	var goFound, orFound bool
	for _, id := range loaded.ids {
		if modelProvider(id) == ProviderOpenRouter {
			orFound = true
		} else {
			goFound = true
		}
	}
	if !goFound || !orFound {
		t.Fatalf("keyless picker must list both providers: %v", loaded.ids)
	}
}

// One OpenCode key serves Go and Zen; legacy names still work as fallbacks.
func TestEffectiveOpenCodeKeyFallbacks(t *testing.T) {
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openCodeLegacyKeyEnv, "legacy-env")
	if got := effectiveOpenCodeKey(Config{}, ""); got != "legacy-env" {
		t.Fatalf("legacy env=%q", got)
	}
	t.Setenv(openCodeKeyEnv, "new-env")
	if got := effectiveOpenCodeKey(Config{}, ""); got != "new-env" {
		t.Fatalf("new env must win over legacy env: %q", got)
	}
	if got := effectiveOpenCodeKey(Config{OpenCodeGoKey: "legacy-cfg"}, ""); got != "legacy-cfg" {
		t.Fatalf("legacy cfg beats env: %q", got)
	}
	if got := effectiveOpenCodeKey(Config{OpenCodeKey: "new-cfg", OpenCodeGoKey: "legacy-cfg"}, ""); got != "new-cfg" {
		t.Fatalf("new cfg must win: %q", got)
	}
	if err := missingKeyError(Config{Model: "zen/gpt-6-luna"}, ""); err != nil {
		t.Fatalf("zen with env key must pass: %v", err)
	}
	t.Setenv(openCodeKeyEnv, "")
	t.Setenv(openCodeLegacyKeyEnv, "")
	if err := missingKeyError(Config{Model: "zen/gpt-6-luna"}, ""); err == nil {
		t.Fatal("zen without key must fail")
	}
}
