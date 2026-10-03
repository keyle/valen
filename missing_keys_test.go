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
	t.Setenv("TEST_GO_KEY_MISSING", "")
	t.Setenv("TEST_OR_KEY_MISSING", "")

	goCfg := Config{Model: "glm-5.3-flash", APIKeyEnv: "TEST_GO_KEY_MISSING", OpenRouterKeyEnv: "TEST_OR_KEY_MISSING", MaxRounds: 1}
	if err := missingKeyError(goCfg, ""); err == nil || !strings.Contains(err.Error(), "TEST_GO_KEY_MISSING") {
		t.Fatalf("go missing err=%v", err)
	} else {
		fe := friendlyError(err)
		if !strings.Contains(fe.Title, "OpenCode Go API key not found") {
			t.Fatalf("go title=%q", fe.Title)
		}
	}
	if err := missingKeyError(goCfg, "  live-key "); err != nil {
		t.Fatalf("go with key must pass: %v", err)
	}

	orCfg := Config{Model: "openrouter/moonshotai/kimi-k3", APIKeyEnv: "TEST_GO_KEY_MISSING", OpenRouterKeyEnv: "TEST_OR_KEY_MISSING", MaxRounds: 1}
	if err := missingKeyError(orCfg, "go-key"); err == nil || !strings.Contains(err.Error(), "TEST_OR_KEY_MISSING") {
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
	t.Setenv("TEST_GO_KEY_NOCALL", "")
	t.Setenv("TEST_OR_KEY_NOCALL", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, model := range []string{"glm-5.3-flash", "openrouter/moonshotai/kimi-k3"} {
		cfg := Config{Model: model, BaseURL: srv.URL, APIKeyEnv: "TEST_GO_KEY_NOCALL", OpenRouterKeyEnv: "TEST_OR_KEY_NOCALL", MaxRounds: 1, MaxContextTokens: 100000, RequestTimeoutSeconds: 5}
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
	t.Setenv("TEST_GO_KEY_LIVE", "live-go")
	cfg := Config{Model: "m", APIKeyEnv: "TEST_GO_KEY_LIVE"}
	if got := effectiveGoKey(cfg, ""); got != "live-go" {
		t.Fatalf("go live=%q", got)
	}
	t.Setenv("TEST_OR_KEY_LIVE", "live-or")
	ocfg := Config{Model: "openrouter/m/a", OpenRouterKeyEnv: "TEST_OR_KEY_LIVE"}
	if got := effectiveOpenRouterKey(ocfg); got != "live-or" {
		t.Fatalf("or live=%q", got)
	}
}

// TUI submit with a missing key stays idle with a provider-specific notice.
func TestTUISubmitWithoutKeyShowsNotice(t *testing.T) {
	t.Setenv("TEST_GO_KEY_SUBMIT", "")
	t.Setenv("TEST_OR_KEY_SUBMIT", "")
	cfg := Config{Model: "glm-5.3-flash", BaseURL: "https://unused", APIKeyEnv: "TEST_GO_KEY_SUBMIT", OpenRouterKeyEnv: "TEST_OR_KEY_SUBMIT", SessionID: "ses-tui", MaxRounds: 4}
	m := newTestModel(t, cfg)
	m.apiKey = ""
	m.cfg.OpenRouterKey = ""
	if cmd := m.submit("hello"); cmd != nil {
		t.Fatalf("missing go key must not start a turn")
	}
	if m.busy || !m.noticeErr || !strings.Contains(m.notice, "OpenCode Go API key not found") {
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
	t.Setenv("TEST_GO_KEY_SWITCH", "")
	t.Setenv("TEST_OR_KEY_SWITCH", "")
	cfg := Config{Model: "glm-5.3-flash", BaseURL: "https://unused", APIKeyEnv: "TEST_GO_KEY_SWITCH", OpenRouterKeyEnv: "TEST_OR_KEY_SWITCH", SessionID: "ses-tui", MaxRounds: 4}
	m := newTestModel(t, cfg)
	m.apiKey = ""
	m.cfg.OpenRouterKey = ""
	m.cfgPath = ""

	m.applyModelSwitch("openrouter/moonshotai/kimi-k3")
	if m.cfg.Model != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("switch must still land, model=%q", m.cfg.Model)
	}
	if !m.noticeErr || !strings.Contains(m.notice, "OpenRouter key not found") {
		t.Fatalf("or pick notice=%q", m.notice)
	}

	m.applyModelSwitch("glm-5.3-flash")
	if !m.noticeErr || !strings.Contains(m.notice, "OpenCode Go API key not found") {
		t.Fatalf("go pick notice=%q", m.notice)
	}
}

// Keyless picker still lists both providers from fallbacks (offline).
func TestTUIModelPickerListsBothWithoutKeys(t *testing.T) {
	t.Setenv("TEST_GO_KEY_PICKER", "")
	t.Setenv("TEST_OR_KEY_PICKER", "")
	oldBase := openRouterBaseURL
	openRouterBaseURL = "http://127.0.0.1:1"
	defer func() { openRouterBaseURL = oldBase }()

	cfg := Config{Model: "glm-5.3-flash", BaseURL: "http://127.0.0.1:1", APIKeyEnv: "TEST_GO_KEY_PICKER", OpenRouterKeyEnv: "TEST_OR_KEY_PICKER", SessionID: "ses-tui", MaxRounds: 4}
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
