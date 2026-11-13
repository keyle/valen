package main

// privacy.go: OpenRouter's per-request privacy filters (provider.zdr and
// provider.data_collection). Both default to the strict side: zero data
// retention on, provider data collection denied. When no endpoint matches,
// the turn asks once whether to relax them for the rest of the session;
// nothing is written to config, so the next run is strict again.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const (
	lblKeepPrivacy  = "Keep privacy filters"
	lblRelaxPrivacy = "Relax for this session"
)

var (
	privacyMu      sync.Mutex
	privacyRelaxed bool
	// privacyAskMu serializes the decision across parallel task subagents:
	// the first refusal prompts, the rest see the relaxed state and retry.
	privacyAskMu sync.Mutex
)

// zdrEnabled reports the configured ZDR filter: nil means on.
func zdrEnabled(cfg Config) bool {
	return cfg.ZDR == nil || *cfg.ZDR
}

// dataCollectionAllowed reports whether providers may collect/train: nil
// means no.
func dataCollectionAllowed(cfg Config) bool {
	return cfg.DataCollection != nil && *cfg.DataCollection
}

// orPrivacyRelaxed reports the session-wide relaxation set after a refusal.
func orPrivacyRelaxed() bool {
	privacyMu.Lock()
	defer privacyMu.Unlock()
	return privacyRelaxed
}

func setORPrivacyRelaxed(on bool) {
	privacyMu.Lock()
	privacyRelaxed = on
	privacyMu.Unlock()
}

// privacyFilters names the filters still constraining cfg, for the prompt.
func privacyFilters(cfg Config) []string {
	var out []string
	if zdrEnabled(cfg) {
		out = append(out, "zdr = true (zero data retention)")
	}
	if !dataCollectionAllowed(cfg) {
		out = append(out, "data_collection = false (no provider data collection)")
	}
	return out
}

// isORPrivacyMessage matches OpenRouter's refusal to route because no
// endpoint satisfies the request's data policy. The message is usually
// "No endpoints found matching your data policy"; guardrail variants name
// "guardrail restrictions and data policy". The routing metadata may carry
// the reason when the message itself is generic.
func isORPrivacyMessage(text string) bool {
	m := strings.ToLower(text)
	if !strings.Contains(m, "no endpoints") && !strings.Contains(m, "no provider") {
		return false
	}
	for _, k := range []string{"data policy", "zero data retention", "zdr", "data collection", "guardrail"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// isORPrivacyRefusal reports an OpenRouter 403/404 caused by the privacy
// filters. Other 404s (unknown model, missing tool support) stay untouched.
func isORPrivacyRefusal(err error) bool {
	var te *transportError
	if !errors.As(err, &te) || te.Transport != "openrouter" {
		return false
	}
	if te.Status != http.StatusForbidden && te.Status != http.StatusNotFound {
		return false
	}
	// An allowlist refusal is fixed by provider_only, not by relaxing
	// privacy: never offer the privacy prompt for it, even when its body
	// also mentions data policy wording.
	if isORAllowedProvidersRefusal(te) {
		return false
	}
	return isORPrivacyMessage(te.Message + "\n" + te.Raw)
}

// promptORPrivacyRelax asks whether to relax both filters for the rest of
// the session. It returns true when a retry should use the relaxed settings;
// a dismissal, a missing prompter or "keep" all fail closed.
func promptORPrivacyRelax(ctx context.Context, cfg Config) bool {
	privacyAskMu.Lock()
	defer privacyAskMu.Unlock()
	if orPrivacyRelaxed() {
		return true
	}
	filters := privacyFilters(cfg)
	if len(filters) == 0 {
		return false
	}
	answers, err := promptUser(ctx, []AskQuestion{{
		Question:   fmt.Sprintf("OpenRouter found no endpoint for %s under your privacy filters. Relax them for the rest of this session?", displayModelID(cfg.Model)),
		Header:     "OpenRouter privacy",
		Highlight:  filters,
		NoFreeText: true,
		Options: []AskOption{
			{Label: lblKeepPrivacy, Description: "the request stays blocked; pick another model or change zdr/data_collection in config"},
			{Label: lblRelaxPrivacy, Description: "the provider may retain your prompts and replies and may train on them; not saved to config"},
		},
	}})
	if err != nil || len(answers) == 0 || answers[0] != lblRelaxPrivacy {
		return false
	}
	setORPrivacyRelaxed(true)
	return true
}
