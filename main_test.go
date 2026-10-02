package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"kimi-k3\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != defaultBaseURL || cfg.APIKeyEnv != defaultAPIKeyEnv || cfg.MaxRounds != defaultMaxRounds {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if !strings.HasPrefix(cfg.SessionID, "ses-") {
		t.Fatalf("bad session id %q", cfg.SessionID)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `model = "glm-5.3"
base_url = "https://example.test/v1"
api_key_env = "MY_KEY"
session_id = "ses-fixed"
max_rounds = 3
temperature = 0.0
tools = ["shell"]

[[mcp]]
name = "fs"
cmd = "npx"
args = ["-y", "server"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://example.test/v1" || cfg.SessionID != "ses-fixed" || cfg.MaxRounds != 3 {
		t.Fatalf("bad overrides: %+v", cfg)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0 {
		t.Fatalf("temperature 0 must stay explicit: %+v", cfg.Temperature)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "fs" || cfg.MCPServers[0].Args[1] != "server" {
		t.Fatalf("bad mcp: %+v", cfg.MCPServers)
	}
}

func TestLoadConfigRejectsMissingModel(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, "base_url = \"x\"\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsIncompleteMCP(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, "model = \"m\"\n[[mcp]]\nname = \"fs\"\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadConfigRejectsMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestHeaderTransportStampsIdentity(t *testing.T) {
	var gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := (&headerTransport{base: http.DefaultTransport, sessionID: "ses-test"}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotSession != "ses-test" || gotUA != userAgent {
		t.Fatalf("session=%q ua=%q", gotSession, gotUA)
	}
}

func completion(toolCalls []map[string]any, content string) []byte {
	msg := map[string]any{"role": "assistant", "content": content}
	if toolCalls != nil {
		msg["content"] = ""
		msg["tool_calls"] = toolCalls
	}
	b, _ := json.Marshal(map[string]any{
		"id": "1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	return b
}

func toolCall(name, args string) []map[string]any {
	return []map[string]any{{"id": "call-1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
}

func fakeServer(t *testing.T, responses ...[]byte) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		i := min(calls, len(responses)-1)
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(responses[i])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestConfig(baseURL string) Config {
	return Config{Model: "m", BaseURL: baseURL, SessionID: "ses-test", MaxRounds: 4}
}

func TestChatRunsToolLoop(t *testing.T) {
	srv, calls := fakeServer(t,
		completion(toolCall("shell", `{"command":"echo hi"}`), ""),
		completion(nil, "all done"),
	)
	registry := NewRegistry()
	if unknown := registry.LoadBuiltins([]string{"shell"}); len(unknown) != 0 {
		t.Fatalf("unknown tools: %v", unknown)
	}
	cfg := newTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "all done") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[tool shell]") || !strings.Contains(stderr.String(), "tokens in=20 out=10") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if *calls != 2 {
		t.Fatalf("calls=%d", *calls)
	}
}

func TestChatFeedsToolFailureBackToModel(t *testing.T) {
	srv, _ := fakeServer(t,
		completion(toolCall("shell", `{"command":"exit 3"}`), ""),
		completion(nil, "reported"),
	)
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "reported") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestChatStopsAtMaxRounds(t *testing.T) {
	srv, _ := fakeServer(t, completion(toolCall("shell", `{"command":"true"}`), ""))
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := newTestConfig(srv.URL)
	cfg.MaxRounds = 2

	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "2 rounds") {
		t.Fatalf("err=%v", err)
	}
}

func TestChatRejectsEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[]}`))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("err=%v", err)
	}
}

func TestChatSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"message":"Insufficient account funds","type":"billing"}}`))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "api:") {
		t.Fatalf("err=%v", err)
	}
}

func TestChatSendsSystemPromptAndTools(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell", "web_search"})
	cfg := newTestConfig(srv.URL)
	cfg.System = "be brief"

	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, registry, "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Fatalf("first message=%v", first)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%v", tools)
	}
}

func TestChatOmitsTemperatureWhenUnset(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, present := gotBody["temperature"]; present {
		t.Fatalf("temperature should be omitted, got %v", gotBody["temperature"])
	}
}
