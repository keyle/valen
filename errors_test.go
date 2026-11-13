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
		{"no key go", errors.New("OPENCODE_API_KEY is not set"), "OpenCode API key not found"},
		{"no key openrouter", errors.New("OPENROUTER_API_KEY is not set (required by model \"openrouter/moonshotai/kimi-k3\")"), "OpenRouter key not found"},
		{"empty", errors.New("api: empty choices in response"), "empty reply"},
		{"output limit", fmt.Errorf("api: chat: incomplete response (length): %w", errOutputLimit), "ran out of output tokens"},
		{"rounds", errors.New("stopped after 8 rounds without a final answer"), "stopped after 8 rounds"},
		{"doom", doomLoopError("read"), "repeated the same tool call"},
		{"budget", errors.New("context too large: est. 999999 tokens exceeds budget 200000"), "context budget"},
		{"network", errors.New("dial tcp: connection refused"), "Couldn't reach"},
		{"mcp", errors.New("transport error: transport closed"), "MCP server"},
		{"messages 429", &transportError{Transport: "messages", Status: 429, Message: "slow down"}, "Rate limited"},
		{"responses 401", &transportError{Transport: "responses", Status: 401, Message: "bad key"}, "rejected the key"},
		{"openrouter 401", &transportError{Transport: "openrouter", Status: 401, Message: "No auth credentials found"}, "openrouter_key"},
		{"openrouter 402", &transportError{Transport: "openrouter", Status: 402, Message: "Insufficient credits"}, "OpenRouter account"},
		{"openrouter provisioning 401", &transportError{Transport: "openrouter", Status: 401, Message: "User not found."}, "provisioning/management"},
		{"openrouter 403 attestation", &transportError{Transport: "openrouter", Status: 403, Message: "This model requires you to complete the following before use: 18+ age confirmation. Confirm at https://openrouter.ai/settings/preferences."}, "18+ age confirmation"},
		{"openrouter 404", &transportError{Transport: "openrouter", Status: 404, Message: "No endpoints found for foo/bar"}, "OpenRouter refused the request (404)"},
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

// TestOpenRouterAllowedProvidersRefusal: a 404 from provider.only excluding
// every provider names both lists and the setting to change instead of
// claiming the model is unavailable on the plan.
func TestOpenRouterAllowedProvidersRefusal(t *testing.T) {
	body := `{"error":{"message":"No allowed providers are available for the selected model. Providers serving openai/gpt-6.1-terra-20261026: azure, openai, but your request's provider.only preference permits only: together, digitalocean, google-vertex, meta.","code":404,"metadata":{"available_providers":["azure","openai"],"requested_providers":["together","digitalocean","google-vertex","meta"],"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":7},{"step":"Filter by Allowed Providers","endpoint_count":0}],"failed_routing_step":"Filter by Allowed Providers"}}}`
	fe := friendlyError(parseTransportError("openrouter", 404, []byte(body)))
	for _, want := range []string{"provider_only", "azure, openai", "together, digitalocean, google-vertex, meta"} {
		if !strings.Contains(fe.Title, want) {
			t.Fatalf("title %q does not contain %q", fe.Title, want)
		}
	}
	if strings.Contains(fe.Title, "plan") {
		t.Fatalf("title must not blame the plan: %q", fe.Title)
	}
	if strings.Contains(fe.Title, "privacy") {
		t.Fatalf("allowlist title must not read as a privacy refusal: %q", fe.Title)
	}
	if !strings.Contains(fe.Details, "Filter by Allowed Providers") {
		t.Fatalf("details must keep the routing metadata: %q", fe.Details)
	}
}

// TestOpenRouterAllowedProvidersMetadata: the funnel step alone identifies the
// refusal even when the message is generic.
func TestOpenRouterAllowedProvidersMetadata(t *testing.T) {
	body := `{"error":{"message":"No endpoints found","metadata":{"failed_routing_step":"Filter by Allowed Providers"}}}`
	fe := friendlyError(parseTransportError("openrouter", 404, []byte(body)))
	if !strings.Contains(fe.Title, "provider_only") {
		t.Fatalf("metadata must identify the allowlist refusal: %q", fe.Title)
	}
}

// TestOpenRouterPrivacyRefusalTitle: a data-policy 404 keeps the privacy
// wording and never mentions provider_only, so ZDR refusals stay distinct
// from allowlist refusals.
func TestOpenRouterPrivacyRefusalTitle(t *testing.T) {
	body := `{"error":{"message":"No endpoints found matching your data policy (Zero data retention).","code":404,"metadata":{"failed_routing_step":"Filter by Data Policy"}}}`
	fe := friendlyError(parseTransportError("openrouter", 404, []byte(body)))
	if !strings.Contains(fe.Title, "privacy filters") {
		t.Fatalf("title must name the privacy filters: %q", fe.Title)
	}
	if strings.Contains(fe.Title, "provider_only") {
		t.Fatalf("privacy title must not mention the allowlist: %q", fe.Title)
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
