package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestFriendlyError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		wantIn string
	}{
		{"402", fmt.Errorf("api: %w", &openai.APIError{HTTPStatusCode: 402, Message: "insufficient account funds"}), "Out of credits"},
		{"401", &openai.APIError{HTTPStatusCode: 401, Message: "bad key"}, "rejected the key"},
		{"404", &openai.APIError{HTTPStatusCode: 404, Message: "model not found"}, "Model not available"},
		{"429", &openai.APIError{HTTPStatusCode: 429, Message: "slow down"}, "Rate limited"},
		{"500", &openai.APIError{HTTPStatusCode: 502, Message: "bad gateway"}, "hiccuped"},
		{"cancel", fmt.Errorf("api: %w", context.Canceled), "Cancelled"},
		{"timeout", errors.New("context deadline exceeded"), "timed out"},
		{"no key go", errors.New("OPENCODE_GO_API_KEY is not set"), "OpenCode Go API key not found"},
		{"no key openrouter", errors.New("OPENROUTER_API_KEY is not set (required by model \"openrouter/moonshotai/kimi-k3\")"), "OpenRouter key not found"},
		{"empty", errors.New("api: empty choices in response"), "empty reply"},
		{"rounds", errors.New("stopped after 8 rounds without a final answer"), "stopped after 8 rounds"},
		{"doom", doomLoopError("read"), "repeated the same tool call"},
		{"budget", errors.New("context too large: est. 999999 tokens exceeds budget 200000"), "context budget"},
		{"network", errors.New("dial tcp: connection refused"), "Couldn't reach"},
		{"mcp", errors.New("transport error: transport closed"), "MCP server"},
		{"messages 429", &transportError{Transport: "messages", Status: 429, Message: "slow down"}, "Rate limited"},
		{"responses 401", &transportError{Transport: "responses", Status: 401, Message: "bad key"}, "rejected the key"},
		{"openrouter 401", &transportError{Transport: "openrouter", Status: 401, Message: "No auth credentials found"}, "openrouter_api_key_env"},
		{"openrouter 402", &transportError{Transport: "openrouter", Status: 402, Message: "Insufficient credits"}, "OpenRouter account"},
		{"openrouter provisioning 401", &transportError{Transport: "openrouter", Status: 401, Message: "User not found."}, "provisioning/management"},
		{"openrouter 403 attestation", &transportError{Transport: "openrouter", Status: 403, Message: "This model requires you to complete the following before use: 18+ age confirmation. Confirm at https://openrouter.ai/settings/preferences."}, "18+ age confirmation"},
		{"other", errors.New("weird"), "Something went wrong"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := friendlyError(c.err)
			if !strings.Contains(fe.Title, c.wantIn) {
				t.Fatalf("title %q does not contain %q", fe.Title, c.wantIn)
			}
			if fe.Details == "" {
				t.Fatal("details must be preserved")
			}
		})
	}
}

func TestFriendlyErrorNil(t *testing.T) {
	if fe := friendlyError(nil); fe.Title != "" || fe.Details != "" {
		t.Fatalf("expected zero value, got %+v", fe)
	}
}

func TestOpenRouter400KeepsRawJSON(t *testing.T) {
	body := `{"error":{"message":"No endpoints found for deepseek/deepseek-v4.1-flash","code":400,"metadata":{"provider_name":"DeepSeek"}}}`
	fe := friendlyError(parseTransportError("openrouter", 400, []byte(body)))
	if !strings.Contains(fe.Title, "No endpoints found") {
		t.Fatalf("title must surface the provider message: %q", fe.Title)
	}
	if !strings.Contains(fe.Details, "openrouter HTTP 400") {
		t.Fatalf("details must name the transport: %q", fe.Details)
	}
	if !strings.Contains(fe.Details, `"provider_name":"DeepSeek"`) {
		t.Fatalf("details must keep the raw JSON: %q", fe.Details)
	}
}
