package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// TestORAllowedProvidersRefusalDoesNotPrompt: provider_only refusals are
// not privacy refusals even when phrased as "no allowed providers".
func TestORAllowedProvidersRefusalDoesNotPrompt(t *testing.T) {
	body := `{"error":{"message":"No allowed providers are available for the selected model.","code":404,"metadata":{"failed_routing_step":"Filter by Allowed Providers"}}}`
	if isORPrivacyRefusal(parseTransportError("openrouter", http.StatusNotFound, []byte(body))) {
		t.Fatal("allowlist refusals must not trigger the privacy prompt")
	}
}

func TestORPrivacyRefusalDetection(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		want   bool
	}{
		{http.StatusNotFound, "No endpoints found matching your data policy (Zero data retention). See https://openrouter.ai/docs", true},
		{http.StatusNotFound, "No endpoints available matching your guardrail restrictions and data policy", true},
		{http.StatusForbidden, "No endpoints found matching your data policy", true},
		{http.StatusNotFound, "No endpoints found for moonshotai/kimi-k3", false},
		{http.StatusNotFound, "No endpoints found that support tool use", false},
		{http.StatusBadRequest, "No endpoints found matching your data policy", false},
	}
	for _, c := range cases {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": c.msg}})
		err := parseTransportError("openrouter", c.status, body)
		if got := isORPrivacyRefusal(err); got != c.want {
			t.Errorf("status=%d msg=%q got=%v want=%v", c.status, c.msg, got, c.want)
		}
	}
	if isORPrivacyRefusal(parseTransportError("chat", 404, []byte(`{"error":{"message":"No endpoints matching data policy"}}`))) {
		t.Fatal("other transports must never match")
	}
}

// privacyRetryServer answers the first request with the data-policy 404 and
// every later one with a normal completion, recording the provider object.
func privacyRetryServer(t *testing.T, calls *int, providers *[]map[string]any) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Provider map[string]any `json:"provider"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*calls++
		n := *calls
		*providers = append(*providers, body.Provider)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"No endpoints found matching your data policy (Zero data retention)."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runPrivacyTurn(t *testing.T, cfg Config, answer string) (error, int, []map[string]any) {
	t.Helper()
	calls := 0
	var providers []map[string]any
	srv := privacyRetryServer(t, &calls, &providers)
	oldBase := openRouterBaseURL
	openRouterBaseURL = srv.URL
	t.Cleanup(func() { openRouterBaseURL = oldBase })

	asked := 0
	oldPrompt := askPrompter
	askPrompter = func(_ context.Context, qs []AskQuestion) ([]string, error) {
		asked++
		if len(qs) != 1 || qs[0].Header != "OpenRouter privacy" {
			t.Fatalf("unexpected prompt: %+v", qs)
		}
		return []string{answer}, nil
	}
	t.Cleanup(func() { askPrompter = oldPrompt })
	setORPrivacyRelaxed(false)
	t.Cleanup(func() { setORPrivacyRelaxed(false) })

	cfg.OpenRouterKey = "or-key"
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	_, _, err := runTurn(context.Background(), cfg, newClient(cfg, "key"), "key", newHTTPClient(cfg), NewRegistry(), &history, nil)
	if asked != 1 {
		t.Fatalf("prompt count=%d want 1", asked)
	}
	return err, calls, providers
}

func TestOpenRouterPrivacyRelaxRetriesTurn(t *testing.T) {
	cfg := newTestConfig("http://[IP_ADDRESS]:1")
	cfg.Model = "openrouter/moonshotai/kimi-k3"
	err, calls, providers := runPrivacyTurn(t, cfg, lblRelaxPrivacy)
	if err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
	if calls != 2 || len(providers) != 2 {
		t.Fatalf("requests=%d want 2", calls)
	}
	first, second := providers[0], providers[1]
	if first["zdr"] != true || first["data_collection"] != "deny" {
		t.Fatalf("first request must be strict: %v", first)
	}
	if second["zdr"] != false || second["data_collection"] != "allow" {
		t.Fatalf("retry must be relaxed: %v", second)
	}
	if !orPrivacyRelaxed() {
		t.Fatal("relaxation must stay for the session")
	}
}

func TestOpenRouterPrivacyKeepReturnsRefusal(t *testing.T) {
	cfg := newTestConfig("http://[IP_ADDRESS]:1")
	cfg.Model = "openrouter/moonshotai/kimi-k3"
	err, calls, _ := runPrivacyTurn(t, cfg, lblKeepPrivacy)
	if err == nil {
		t.Fatal("keeping the filters must keep the refusal")
	}
	if calls != 1 {
		t.Fatalf("requests=%d want 1 (no retry)", calls)
	}
	if orPrivacyRelaxed() {
		t.Fatal("keeping the filters must not relax the session")
	}
	fe := friendlyError(err)
	if !strings.Contains(fe.Title, "privacy filters") {
		t.Fatalf("title must name the privacy filters: %q", fe.Title)
	}
}

func TestOpenRouterPrivacyMetadataDetected(t *testing.T) {
	body := `{"error":{"message":"No endpoints found","metadata":{"failed_routing_step":"Filter by Data Policy"}}}`
	if !isORPrivacyRefusal(parseTransportError("openrouter", http.StatusNotFound, []byte(body))) {
		t.Fatal("routing metadata must identify a privacy refusal")
	}
	if isORPrivacyRefusal(parseTransportError("openrouter", http.StatusNotFound, []byte(`{"error":{"message":"No endpoints found"}}`))) {
		t.Fatal("a bare no-endpoints refusal must not prompt")
	}
}
