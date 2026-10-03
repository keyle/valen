package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// FriendlyError pairs a one-line message safe for the transcript with the raw
// cause kept for the ctrl+x errors view.
type FriendlyError struct {
	Title   string `json:"title"`
	Details string `json:"details"`
}

func friendlyError(err error) FriendlyError {
	if err == nil {
		return FriendlyError{}
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case errors.Is(err, context.Canceled):
		return FriendlyError{"Cancelled. Your message is kept; resend when ready.", msg}
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return FriendlyError{"The request timed out. Check your network and resend.", msg}
	case strings.Contains(lower, "no such host"), strings.Contains(lower, "connection refused"), strings.Contains(lower, "dial tcp"), strings.Contains(lower, "network is unreachable"):
		return FriendlyError{"Couldn't reach the provider. Check network/VPN and resend.", msg}
	case strings.Contains(lower, "is not set"):
		return FriendlyError{"No API key found — set the env var named by api_key_env.", msg}
	case strings.Contains(lower, "empty choices"):
		return FriendlyError{"The model returned an empty reply. Try rephrasing or resending.", msg}
	case strings.Contains(lower, "stopped after"):
		return FriendlyError{msg + " — ask the model to continue or simplify.", msg}
	case strings.Contains(lower, "identical input"):
		return FriendlyError{"Stopped: the model repeated the same tool call 3× — resend with a narrower ask or /clear.", msg}
	case strings.Contains(lower, "context too large"):
		return FriendlyError{"History exceeded the context budget — /compact to summarize or /clear for a fresh start.", msg}
	case strings.Contains(lower, "transport error"), strings.Contains(lower, "initialize:"), strings.Contains(lower, "list tools:"):
		return FriendlyError{"An MCP server hiccuped; the conversation kept going without its tools.", msg}
	}

	var ae *openai.APIError
	if errors.As(err, &ae) {
		return friendlyStatus(ae.HTTPStatusCode, ae.Message)
	}
	var re *openai.RequestError
	if errors.As(err, &re) {
		return friendlyStatus(re.HTTPStatusCode, err.Error())
	}
	var te *transportError
	if errors.As(err, &te) {
		fe := friendlyStatus(te.Status, te.Message)
		// Details always name the transport: with two providers a bare
		// "HTTP 401" no longer says whose key was rejected.
		fe.Details = fmt.Sprintf("%s HTTP %d: %s", te.Transport, te.Status, te.Message)
		if te.Transport == "openrouter" {
			switch te.Status {
			case 401:
				fe.Title = "OpenRouter rejected the key. Check the env var named by openrouter_api_key_env (restart the TUI after exporting it)."
				// Provisioning/management keys administer keys and
				// guardrails but can't run inference: OpenRouter answers
				// them on /chat/completions with 401 "User not found"
				// while they look valid in the dashboard.
				if strings.Contains(strings.ToLower(te.Message), "user not found") {
					fe.Title = "That OpenRouter key can't run inference — provisioning/management keys only manage keys/guardrails. Create a regular API key at openrouter.ai/settings/keys and export it."
				}
			case 403:
				// 403 on OpenRouter is authorization, never authentication:
				// guardrail blocks, attestation gates (age confirmation),
				// content filters. The provider message names the exact
				// requirement — often with the URL that resolves it — so
				// surface it instead of blaming the key.
				if msg := singleLine(te.Message, 200); msg != "" {
					fe.Title = "OpenRouter refused the request: " + msg
				} else {
					fe.Title = "OpenRouter refused the request (403)."
				}
			case 402:
				fe.Title = "Out of credits on the OpenRouter account. Top up at openrouter.ai, then resend."
			}
		}
		return fe
	}
	return FriendlyError{"Something went wrong.", msg}
}

// singleLine collapses a provider message to one rune-capped line for use
// in a title. Provider messages sometimes carry newlines or long tails;
// the full text stays in Details.
func singleLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return "…"
	}
	return string(r[:n]) + "…"
}

func friendlyStatus(code int, detail string) FriendlyError {
	switch code {
	case 401, 403:
		return FriendlyError{"The API rejected the key. Check the env var named by api_key_env.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 402:
		return FriendlyError{"Out of credits on the OpenCode Go account. Add balance or switch model.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 404:
		return FriendlyError{"Model not available on this plan. Pick another in config.toml.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 429:
		return FriendlyError{"Rate limited. Wait a moment and resend — your prompt is kept.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 500, 502, 503, 504:
		return FriendlyError{"The provider hiccuped (5xx). Sending again usually works.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 0:
		return FriendlyError{"No response from the provider.", detail}
	default:
		return FriendlyError{fmt.Sprintf("Provider error (HTTP %d).", code), detail}
	}
}
