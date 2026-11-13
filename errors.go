package main

import (
	"context"
	"encoding/json"
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
	case errors.Is(err, errOutputLimit):
		return FriendlyError{"The model ran out of output tokens before finishing (reasoning counts toward max_tokens). Raise max_tokens or lower thinking, then resend.", msg}
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return FriendlyError{"The request timed out. Check your network and resend.", msg}
	case strings.Contains(lower, "no such host"), strings.Contains(lower, "connection refused"), strings.Contains(lower, "dial tcp"), strings.Contains(lower, "network is unreachable"):
		return FriendlyError{"Couldn't reach the provider. Check network/VPN and resend.", msg}
	case strings.Contains(lower, "is not set"):
		switch {
		case strings.Contains(lower, "openrouter"):
			return FriendlyError{"OpenRouter key not found — set openrouter_key in config or export OPENROUTER_API_KEY.", msg}
		case strings.Contains(lower, "openai"):
			return FriendlyError{"OpenAI key not found — set openai_key in config or export OPENAI_API_KEY.", msg}
		}
		return FriendlyError{"OpenCode API key not found — set opencode_key in config or export OPENCODE_API_KEY.", msg}
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

	var stopErr *chatStopError
	if errors.As(err, &stopErr) {
		return friendlyChatStop(stopErr)
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
		// "HTTP 401" no longer says whose key was rejected. The raw
		// provider body follows so ctrl+x shows the full JSON, not just
		// the extracted message.
		fe.Details = fmt.Sprintf("%s HTTP %d: %s", te.Transport, te.Status, te.Message)
		if raw := strings.TrimSpace(te.Raw); raw != "" {
			fe.Details += "\n\nRaw response:\n" + raw
		}
		if te.Transport == "openrouter" {
			switch te.Status {
			case 401:
				fe.Title = "OpenRouter rejected the key. Check openrouter_key or OPENROUTER_API_KEY."
				// Provisioning/management keys administer keys and
				// guardrails but can't run inference: OpenRouter answers
				// them on /chat/completions with 401 "User not found"
				// while they look valid in the dashboard.
				if strings.Contains(strings.ToLower(te.Message), "user not found") {
					fe.Title = "That OpenRouter key can't run inference — provisioning/management keys only manage keys/guardrails. Create a regular API key at openrouter.ai/settings/keys and export it."
				}
			case 400:
				// 400 on OpenRouter is a rejected request (bad model
				// ID, unsupported param like reasoning_effort,
				// provider downtime routing). The message names the
				// exact cause, so surface it in the transcript title;
				// the full JSON stays in Details.
				if msg := singleLine(te.Message, 200); msg != "" {
					fe.Title = "OpenRouter refused the request (400): " + msg
				} else {
					fe.Title = "OpenRouter refused the request (400)."
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
			case 404:
				switch {
				// The allowlist check comes first: its routing step is
				// authoritative, so a ZDR-sounding phrase in the body can't
				// turn an allowlist refusal into a privacy one.
				case isORAllowedProvidersRefusal(te):
					fe.Title = orAllowedProvidersTitle(te)
				case isORPrivacyMessage(te.Message + "\n" + te.Raw):
					fe.Title = "No OpenRouter endpoint matches your privacy filters (zdr/data_collection or the account's privacy settings). Pick another model or relax them."
				default:
					// Unknown model, unsupported tool use, no endpoints:
					// surface the provider message like 400/403 instead of
					// the OpenCode-flavored "plan" fallback.
					if msg := singleLine(te.Message, 200); msg != "" {
						fe.Title = "OpenRouter refused the request (404): " + msg
					} else {
						fe.Title = "OpenRouter refused the request (404)."
					}
				}
			default:
				// Other OpenRouter 4xx/5xx: keep friendlyStatus wording
				// but prefix the transport so the transcript never reads
				// as a Go failure.
				if te.Status >= 400 && te.Status < 600 {
					if msg := singleLine(te.Message, 200); msg != "" && !strings.Contains(fe.Title, msg) {
						fe.Title += " " + msg
					}
				}
			}
		}
		return fe
	}
	return FriendlyError{"Something went wrong.", msg}
}

// rawBodyCap bounds the provider body kept for ctrl+x details: large
// enough to hold the full error JSON (message + code + metadata), small
// enough to stay readable in the detail view.
const rawBodyCap = 4000

// capRawBody keeps the trimmed provider body for Details. Empty bodies
// yield "" so callers can skip the raw section.
func capRawBody(body []byte) string {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return ""
	}
	if len(msg) > rawBodyCap {
		msg = msg[:rawBodyCap] + "…"
	}
	return msg
}

// rawDiff returns raw unless it is identical to msg, in which case ""
// avoids duplicating the same line in Details. When the body was JSON the
// raw block is always kept — even though it contains the extracted message
// — because code/metadata/provider fields are what diagnose a 400.
func rawDiff(raw, msg string) string {
	raw = strings.TrimSpace(raw)
	msg = strings.TrimSpace(msg)
	if raw == "" || raw == msg {
		return ""
	}
	return raw
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

// orRoutingRefusal is the routing metadata OpenRouter attaches to a 404 that
// explains why no endpoint was eligible. Available lists the providers serving
// the model; Allowed is the provider.only allowlist that filtered them out.
type orRoutingRefusal struct {
	Available []string
	Allowed   []string
	Step      string
}

func parseORRoutingRefusal(raw string) orRoutingRefusal {
	var body struct {
		Error struct {
			Metadata struct {
				AvailableProviders []string `json:"available_providers"`
				RequestedProviders []string `json:"requested_providers"`
				FailedRoutingStep  string   `json:"failed_routing_step"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &body) != nil {
		return orRoutingRefusal{}
	}
	return orRoutingRefusal{
		Available: body.Error.Metadata.AvailableProviders,
		Allowed:   body.Error.Metadata.RequestedProviders,
		Step:      body.Error.Metadata.FailedRoutingStep,
	}
}

// isORAllowedProvidersRefusal reports OpenRouter's 404 when provider.only
// excludes every provider that serves the model. The message names the
// allowlist; the routing funnel names the failing step when it does not.
func isORAllowedProvidersRefusal(te *transportError) bool {
	if strings.Contains(strings.ToLower(te.Message), "no allowed providers") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(parseORRoutingRefusal(te.Raw).Step), "Filter by Allowed Providers")
}

// orAllowedProvidersTitle names both provider lists and the setting to change,
// so the transcript points at the allowlist instead of blaming the plan.
func orAllowedProvidersTitle(te *transportError) string {
	r := parseORRoutingRefusal(te.Raw)
	detail := ""
	switch {
	case len(r.Available) > 0 && len(r.Allowed) > 0:
		detail = fmt.Sprintf(": it is served by %s, but provider_only permits %s", strings.Join(r.Available, ", "), strings.Join(r.Allowed, ", "))
	case len(r.Allowed) > 0:
		detail = fmt.Sprintf(": provider_only permits %s", strings.Join(r.Allowed, ", "))
	case len(r.Available) > 0:
		detail = fmt.Sprintf(": it is served by %s", strings.Join(r.Available, ", "))
	}
	return singleLine("OpenRouter has no allowed provider for this model"+detail+". Update provider_only in valen.toml (or your saved OpenRouter provider preferences) or pick another model.", 300)
}

// friendlyChatStop maps a non-normal chat stop. Native reasons are the
// provider's precise cause; MALFORMED_FUNCTION_CALL is Gemini 3 rejecting a
// tool continuation whose signed reasoning state was not replayed.
func friendlyChatStop(e *chatStopError) FriendlyError {
	native := strings.ToUpper(e.Native)
	details := chatStopDetails(e)
	switch {
	case strings.Contains(native, "MALFORMED_FUNCTION_CALL"), strings.Contains(native, "MALFORMED_TOOL_CALL"), strings.Contains(native, "UNEXPECTED_TOOL_CALL"):
		return FriendlyError{
			Title:   "The provider rejected the model's tool-call state (" + e.Native + ") — signed reasoning from an earlier round was lost. Resend for a fresh round; if it repeats, /clear or switch model.",
			Details: details,
		}
	case errors.Is(e, errContentFilter):
		return FriendlyError{
			Title:   "The provider's content filter stopped the response. Rephrase or resend, or switch model.",
			Details: details,
		}
	}
	title := "The provider stopped the response"
	switch {
	case e.Native != "":
		title += " (" + e.Native + ")"
	case e.Reason != "":
		title += " (" + e.Reason + ")"
	}
	if d := singleLine(e.Detail, 200); d != "" {
		title += ": " + d
	} else {
		title += "."
	}
	return FriendlyError{Title: title, Details: details}
}

// chatStopDetails keeps the stop's reason, native reason, provider message and
// raw body so ctrl+x diagnoses the failure without re-running the turn.
func chatStopDetails(e *chatStopError) string {
	transport := e.Transport
	if transport == "" {
		transport = "chat"
	}
	parts := []string{fmt.Sprintf("%s stop: reason=%q native=%q", transport, e.Reason, e.Native)}
	if d := strings.TrimSpace(e.Detail); d != "" {
		parts = append(parts, d)
	}
	if raw := strings.TrimSpace(e.Raw); raw != "" {
		parts = append(parts, "Raw response:\n"+raw)
	}
	return strings.Join(parts, "\n\n")
}

func friendlyStatus(code int, detail string) FriendlyError {
	switch code {
	case 401, 403:
		return FriendlyError{"The API rejected the key. Check opencode_key or OPENCODE_API_KEY.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 402:
		return FriendlyError{"Out of credits on the OpenCode account. Add balance or switch model.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 404:
		return FriendlyError{"Model not available on this plan. Pick another in valen.toml.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 429:
		return FriendlyError{"Rate limited. Wait a moment and resend — your prompt is kept.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 500, 502, 503, 504:
		return FriendlyError{"The provider hiccuped (5xx). Sending again usually works.", fmt.Sprintf("HTTP %d: %s", code, detail)}
	case 0:
		// No status means the failure arrived inside a 200 stream; that is a
		// provider stop, not a silent empty response.
		if msg := singleLine(detail, 200); msg != "" {
			return FriendlyError{"The provider stopped the response: " + msg, detail}
		}
		return FriendlyError{"The provider stopped the response before finishing.", detail}
	default:
		// Generic codes (notably 400): surface the provider message in
		// the transcript title so the failure is actionable without
		// opening ctrl+x; the full text stays in Details.
		if msg := singleLine(detail, 200); msg != "" {
			return FriendlyError{fmt.Sprintf("Provider error (HTTP %d): %s", code, msg), fmt.Sprintf("HTTP %d: %s", code, detail)}
		}
		return FriendlyError{fmt.Sprintf("Provider error (HTTP %d).", code), detail}
	}
}
