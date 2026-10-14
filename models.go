package main

import (
	"strings"

	"github.com/sashabaranov/go-openai"
)

// fallbackGoModels is the curated OpenCode Go catalog (from /models, live Oct 2
// 2026) used when the live list cannot be reached. Entries tagged non-chat
// need a different transport (/responses, /messages) and will not work on the
// chat endpoint — see modelTransport.
var fallbackGoModels = []string{
	"glm-5.3-flash",
	"glm-5.3",
	"glm-5.2",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"longcat-2.0",
	"longcat-2.5-preview-free",
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"deepseek-v4-flash-vision-exp",
	"mimo-v2.6-flash",
	"mimo-v2.6-pro",
	"mimo-v2.5",
	"mimo-v2.5-pro",
	"hy4-preview",
	"hy3",
	"space-bunny-free",
	"minimax-m3",
	"minimax-m2.7",
	"qwen3.8-max",
	"qwen3.8-flash",
	"qwen3.7-plus",
	"grok-4.7",
	"grok-4.6",
	"gpt-6-luna",
	"gpt-5.6-luna",
	"muse-spark-1.3-contributor",
	"muse-spark-1.2-contributor",
}

// Providers. Go models are stored bare ("glm-5.3-flash"); OpenRouter models
// carry an "openrouter/" prefix ("openrouter/moonshotai/kimi-k3") so the
// stored model ID is self-describing: requests, thinking levels and the
// picker all derive the provider from it, with no extra state to keep in
// sync. Bare IDs always mean Go, so existing configs load unchanged.
const (
	ProviderGo         = "go"
	ProviderOpenRouter = "openrouter"
	ProviderZen        = "zen"
)

// zenBaseURL is OpenCode Zen's gateway (same OpenCode key as Go). A var so
// offline tests can point it at a fake server. Zen models carry a "zen/"
// prefix ("zen/claude-haiku-5-5").
var zenBaseURL = "https://opencode.ai/zen/v1"

// fallbackZenModels is a curated Zen list (bare IDs, Oct 2026) used when the
// live /models catalog cannot be reached. Gemini models are omitted: Zen
// serves them on a native Gemini API valen does not speak.
var fallbackZenModels = []string{
	"claude-opus-5-5",
	"claude-sonnet-5-5",
	"claude-haiku-5-5",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-haiku-4-5",
	"gpt-6.1-sol",
	"gpt-6-astra",
	"gpt-6-luna",
	"gpt-5.6-sol",
	"gpt-5.5",
	"grok-4.7",
	"grok-4.6",
	"muse-spark-1.3",
	"mistral-large-4",
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"glm-5.3",
	"glm-5.3-flash",
	"minimax-m3",
	"kimi-k3",
	"kimi-k2.7-code",
	"qwen3.8-max",
	"qwen3.8-flash",
	"big-pickle",
}

// prefixedZenModels returns bare Zen IDs in stored (zen/-prefixed) form.
func prefixedZenModels(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, ProviderZen+"/"+id)
	}
	return out
}

// providerBaseURL is the API base for the model's provider: Zen has its own
// gateway; Go (and anything else non-OpenRouter) rides cfg.BaseURL.
func providerBaseURL(cfg Config) string {
	if modelProvider(cfg.Model) == ProviderZen {
		return zenBaseURL
	}
	return cfg.BaseURL
}

// openRouterBaseURL is a var (not const) so offline tests can point it at a
// fake server; production uses the OpenRouter v1 API.
var openRouterBaseURL = "https://openrouter.ai/api/v1"

// fallbackOpenRouterModels is a curated recent-flags list (from the live
// /models catalog, Oct 2026) used when the live list cannot be reached.
// Stored bare (no provider prefix); the picker prefixes them on merge.
var fallbackOpenRouterModels = []string{
	"anthropic/claude-opus-5.5",
	"anthropic/claude-sonnet-5.5",
	"openai/gpt-6.1-sol",
	"openai/gpt-6.1-sol-pro",
	"openai/gpt-6-luna",
	"openai/gpt-6-sol",
	"openai/gpt-6-astra",
	"x-ai/grok-4.7",
	"x-ai/grok-4.6",
	"google/gemini-3.8-flash",
	"google/gemini-3.7-flash",
	"deepseek/deepseek-v4.1-flash",
	"deepseek/deepseek-v4-pro-0813",
	"z-ai/glm-5.3",
	"z-ai/glm-5.3-flash",
	"qwen/qwen3.8-max",
	"qwen/qwen3.8-flash",
	"meta/muse-spark-1.3",
	"meta/muse-spark-1.2",
	"moonshotai/kimi-k3",
	"moonshotai/kimi-k2.7-code",
	"moonshotai/kimi-k2.6",
	"minimax/minimax-m3",
	"z-ai/glm-5.2",
	"meituan/longcat-2.0",
	"nvidia/nemotron-3.5-lightning",
	"arcee-ai/trinity-large-thinking",
}

// prefixedOpenRouterFallback returns the curated fallback list in stored
// (provider-prefixed) form for picker merges.
func prefixedOpenRouterFallback() []string {
	out := make([]string, 0, len(fallbackOpenRouterModels))
	for _, id := range fallbackOpenRouterModels {
		out = append(out, ProviderOpenRouter+"/"+id)
	}
	return out
}

// modelProvider reports which provider serves a stored model ID.
func modelProvider(id string) string {
	if strings.HasPrefix(normalizeModelID(id), ProviderOpenRouter+"/") {
		return ProviderOpenRouter
	}
	if strings.HasPrefix(normalizeModelID(id), ProviderZen+"/") {
		return ProviderZen
	}
	return ProviderGo
}

// wireModelID strips the provider prefix for the wire. OpenRouter expects the
// full vendor/model ID and, when routing is set, a `:floor`/`:nitro`/`:exacto`
// provider-sorting suffix; Go and Zen pass their bare ID through (routing is
// OpenRouter-only and ignored there). The routing argument is optional so all
// non-request callers (family/transport lookups) keep using the bare ID.
func wireModelID(id string, routing ...string) string {
	base := strings.TrimSpace(id)
	switch modelProvider(id) {
	case ProviderOpenRouter:
		base = base[len(ProviderOpenRouter)+1:]
		if len(routing) > 0 {
			if r := normalizeRouting(routing[0]); r != "" && validRouting(r) {
				base += ":" + r
			}
		}
	case ProviderZen:
		base = base[len(ProviderZen)+1:]
	}
	return base
}

// displayModelID prefixes bare Go IDs so every picker row names its
// provider; OpenRouter IDs already carry theirs.
func displayModelID(id string) string {
	if modelProvider(id) == ProviderGo {
		return ProviderGo + "/" + strings.TrimSpace(id)
	}
	return strings.TrimSpace(id)
}

// modelBaseID folds a stored ID down to the family lookup key: provider
// prefix off, then the vendor segment off ("openrouter/moonshotai/kimi-k3"
// → "kimi-k3"). Bare Go IDs pass through untouched.
func modelBaseID(id string) string {
	base := wireModelID(id)
	if i := strings.Index(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return normalizeModelID(base)
}

// modelTransport reports which API transport serves a model ID, matching the
// live Go endpoint table (Oct 2 2026): "chat" (/chat/completions, OpenAI
// chat), "responses" (/responses, OpenAI Responses) or "messages" (/messages,
// Anthropic Messages). OpenRouter models always ride OpenRouter's
// OpenAI-compatible chat endpoint, whatever the upstream vendor.
func modelTransport(id string) string {
	if modelProvider(id) == ProviderOpenRouter {
		return "chat"
	}
	if modelProvider(id) == ProviderZen {
		return zenTransport(wireModelID(id))
	}
	l := strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(l, "grok-"), strings.HasPrefix(l, "gpt-"), strings.HasPrefix(l, "muse-spark-"):
		return "responses"
	case strings.HasPrefix(l, "minimax-"), strings.HasPrefix(l, "qwen"), strings.HasPrefix(l, "claude-"):
		return "messages"
	default:
		return "chat"
	}
}

// zenTransport maps a bare Zen model ID to its endpoint (Oct 2026 docs):
// gpt/grok/muse → responses; claude and the qwen3.5–3.8 plus/flash/max-3.7
// family → messages; gemini → "gemini" (native API, unsupported); the rest
// (deepseek, glm, kimi, minimax, mistral, qwen3.8-max, big-pickle, free
// models) → chat.
func zenTransport(bare string) string {
	l := strings.ToLower(strings.TrimSpace(bare))
	switch {
	case strings.HasPrefix(l, "gemini-"):
		return "gemini"
	case strings.HasPrefix(l, "gpt-"), strings.HasPrefix(l, "grok-"), strings.HasPrefix(l, "muse-spark-"):
		return "responses"
	case strings.HasPrefix(l, "claude-"):
		return "messages"
	case l == "qwen3.8-max":
		return "chat"
	case strings.HasPrefix(l, "qwen"):
		return "messages"
	default:
		return "chat"
	}
}

// chatCompatible reports whether valen can talk to the model.
func chatCompatible(id string) bool { return modelTransport(id) == "chat" }

// knownContextWindows maps model families to input-token windows
// (best effort, Oct 2026). Explicit max_context_tokens always wins; families
// missing here resolve to defaultMaxContextTokens. Overestimates degrade
// gracefully: a provider 400/413 still triggers one compact-and-retry.
var knownContextWindows = []struct {
	prefix string
	tokens int
}{
	{"gemini", 1000000},
	{"claude", 500000},
	{"gpt-", 400000},
	{"grok-", 256000},
	{"qwen", 256000},
	{"kimi-", 256000},
	{"glm-", 200000},
	{"minimax-", 200000},
	{"deepseek", 128000},
}

// modelContextWindow reports the input-token window for a stored model ID,
// matching on the family key (provider and vendor segments stripped). 0
// means unknown — contextBudget falls back to defaultMaxContextTokens.
func modelContextWindow(id string) int {
	l := modelBaseID(id)
	for _, w := range knownContextWindows {
		if strings.HasPrefix(l, w.prefix) {
			return w.tokens
		}
	}
	return 0
}

// thinkingFamily groups models by how their thinking level is expressed
// outbound: "effort" models take reasoning_effort, "toggle" models take
// enable_thinking in chat template kwargs, "" means no thinking param.
func thinkingFamily(model string) string {
	l := modelBaseID(model)
	if levels, ok := lookupKnownLevels(l); ok {
		if len(levels) == 0 {
			return ""
		}
		if len(levels) == 2 && levels[0] == "none" && levels[1] == "high" {
			return "toggle"
		}
		return "effort"
	}
	for _, p := range []string{"kimi", "mimo", "qwen", "minimax", "muse-spark"} {
		if strings.HasPrefix(l, p) {
			return "toggle"
		}
	}
	return "effort"
}

// effortRank orders thinking levels low to high for cycling and display.
// "none" (off) sorts first; provider default ("") is never in the list.
var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// standardThinkingLevels is the full reasoning_effort vocabulary for models
// outside the known table. Unknown models fall back to this until a 400
// teaches us better (see maybeLearnThinkingLevels), so keep it ordered.
var standardThinkingLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// knownThinkingLevels is the well-known per-model table (mirrored from
// models.dev reasoning_options, Oct 2026). Keys are lowercased model IDs;
// empty means no adjustable thinking (provider default only). Toggle-only
// models use none/high via enable_thinking; effort models use
// reasoning_effort with the listed values.
var knownThinkingLevels = map[string][]string{
	"glm-5.3-flash":                {"low", "high", "max"},
	"glm-5.3":                      {"low", "high", "max"},
	"glm-5.2":                      {"high", "max"},
	"kimi-k3":                      {"max"},
	"kimi-k2.7-code":               {},
	"kimi-k2.6":                    {"none", "high"},
	"longcat-2.0":                  {"none", "high"},
	"longcat-2.0-free":             {"none", "high"},
	"longcat-2.5-preview-free":     {"none", "high"},
	"deepseek-v4.1-flash":          {"low", "high", "max"},
	"deepseek-v4-pro":              {"high", "max"},
	"deepseek-v4-flash":            {"low", "high", "max"},
	"deepseek-v4-flash-vision-exp": {"low", "high", "max"},
	"mimo-v2.6-flash":              {},
	"mimo-v2.6-flash-free":         {},
	"mimo-v2.6-pro":                {},
	"mimo-v2.5":                    {},
	"mimo-v2.5-free":               {},
	"mimo-v2.5-pro":                {},
	"hy4-preview":                  {"low", "medium", "high"},
	"hy3":                          {"low", "medium", "high"},
	"hy3-free":                     {"low", "medium", "high"},
	"space-bunny-free":             {"low", "medium", "high", "xhigh", "max"},
	"minimax-m3":                   {},
	"minimax-m3-free":              {},
	"minimax-m2.7":                 {},
	"qwen3.8-max":                  {"none", "high"},
	"qwen3.8-flash":                {"none", "low", "medium", "xhigh"},
	"qwen3.7-plus":                 {"none", "high"},
	"grok-4.7":                     {"low", "medium", "high", "xhigh"},
	"grok-4.6":                     {"low", "medium", "high", "xhigh"},
	"gpt-6-luna":                   {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-5.6-luna":                 {"none", "low", "medium", "high", "xhigh", "max"},
	// Muse Spark forces reasoning on (none 400s). Contributor tier has no
	// max; standard 1.3 adds max but the contributor IDs below are what
	// valen ships, so list without max. Probe/learn will add max if the
	// provider ever accepts it.
	"muse-spark-1.3-contributor":      {"minimal", "low", "medium", "high", "xhigh"},
	"muse-spark-1.3-contributor-free": {"minimal", "low", "medium", "high", "xhigh"},
	"muse-spark-1.3":                  {"minimal", "low", "medium", "high", "xhigh"},
	"muse-spark-1.2-contributor":      {"minimal", "low", "medium", "high", "xhigh"},
	"muse-spark-1.2-contributor-free": {"minimal", "low", "medium", "high", "xhigh"},
	"muse-spark-1.2":                  {"minimal", "low", "medium", "high", "xhigh"},
}

func normalizeModelID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// lookupKnownLevels checks the fixed table: exact match, then without a
// trailing -free, then family prefixes for forward compat (muse/gpt/grok).
func lookupKnownLevels(l string) ([]string, bool) {
	if lv, ok := knownThinkingLevels[l]; ok {
		return lv, true
	}
	if strings.HasSuffix(l, "-free") {
		if lv, ok := knownThinkingLevels[strings.TrimSuffix(l, "-free")]; ok {
			return lv, true
		}
	}
	// Family fallbacks for IDs not yet in the table.
	switch {
	case strings.HasPrefix(l, "muse-spark-"):
		return []string{"minimal", "low", "medium", "high", "xhigh"}, true
	case strings.HasPrefix(l, "gpt-"):
		return []string{"none", "low", "medium", "high", "xhigh", "max"}, true
	case strings.HasPrefix(l, "grok-"):
		return []string{"low", "medium", "high", "xhigh"}, true
	}
	return nil, false
}

// AvailableThinkingLevels lists the thinking levels a model accepts.
// Empty thinking (provider default) is always allowed on top of these.
// Order is low to high for ctrl+t cycling. Disk-cached probe results win,
// then the fixed well-known table, then the full standard vocabulary.
func AvailableThinkingLevels(model string) []string {
	if lv, ok := cachedThinkingLevels(model); ok {
		return append([]string(nil), lv...)
	}
	if lv, ok := lookupKnownLevels(modelBaseID(model)); ok {
		return append([]string(nil), lv...)
	}
	return append([]string(nil), standardThinkingLevels...)
}

// validThinkingLevel reports whether level is selectable for model.
// Empty means provider default and is always valid.
func validThinkingLevel(model, level string) bool {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return true
	}
	for _, l := range AvailableThinkingLevels(model) {
		if l == level {
			return true
		}
	}
	return false
}

// routingVariants are the OpenRouter provider-sorting modes in cycle order.
// "none" (the empty string) is OpenRouter's default load balancing, sent with
// no suffix.
var routingVariants = []string{"", "floor", "nitro", "exacto"}

// normalizeRouting lowercases and trims a routing value; "" and "none" both
// mean the default (no suffix).
func normalizeRouting(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	if r == "none" {
		return ""
	}
	return r
}

// validRouting reports whether r names a routing mode ("" = none).
func validRouting(r string) bool {
	switch normalizeRouting(r) {
	case "", "floor", "nitro", "exacto":
		return true
	}
	return false
}

// routingLabel is the user-facing name for a routing value: "none" when unset.
func routingLabel(r string) string {
	if r = normalizeRouting(r); r == "" {
		return "none"
	}
	return r
}

// isRoutingToken reports whether s is a slot routing token (the written
// vocabulary excludes "none", which absence represents; thinking "none" keeps
// its usual meaning).
func isRoutingToken(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "floor", "nitro", "exacto":
		return true
	}
	return false
}

// cycleRouting advances r through none → floor → nitro → exacto → none.
func cycleRouting(r string) string {
	r = normalizeRouting(r)
	for i, v := range routingVariants {
		if v == r {
			return routingVariants[(i+1)%len(routingVariants)]
		}
	}
	return "floor"
}

// applyThinkingToRequest stamps the thinking level onto an outbound request.
// No-op when level is empty (provider default), mirroring the temperature
// pointer semantics: unset omits the field.
func applyThinkingToRequest(req *openai.ChatCompletionRequest, model, level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return
	}
	if thinkingFamily(model) == "" {
		return
	}
	if thinkingFamily(model) == "toggle" {
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = level != "none"
		return
	}
	req.ReasoningEffort = level
}

// openRouterHeaders builds the auth headers for OpenRouter: bearer key plus
// an honest app title. No session header — that's Go routing, meaningless
// upstream of OpenRouter.
func openRouterHeaders(apiKey string) map[string]string {
	if apiKey == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + apiKey, "X-Title": "valen"}
}
