package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunTurnDispatchesPerTransport pins one representative model per
// transport to its endpoint path, so a modelTransport regression fails here
// instead of live. Each fake speaks just enough of its protocol for one
// no-tool turn; deeper coverage lives in main/messages/responses_test.go.
func TestRunTurnDispatchesPerTransport(t *testing.T) {
	cases := []struct{ model, wantPath string }{
		{"glm-5.3-flash", "/chat/completions"},
		{"minimax-m3", "/messages"},
		{"qwen3.8-flash", "/messages"},
		{"muse-spark-1.3-contributor", "/responses"},
		{"grok-4.7", "/responses"},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/chat/completions":
					w.Write(completion(nil, "ok"))
				case "/messages":
					w.Write(messagesTextResp("ok"))
				case "/responses":
					w.Write(responsesTextResp("ok"))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			cfg := newTestConfig(srv.URL)
			cfg.Model = c.model
			if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if gotPath != c.wantPath {
				t.Fatalf("model %s hit %q, want %q", c.model, gotPath, c.wantPath)
			}
		})
	}
}
