package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// wireModelID strips the provider prefix for the wire: OpenRouter expects
// the full vendor/model ID, Go passes its bare ID through.
func wireModelID(id string) string {
	switch modelProvider(id) {
	case ProviderOpenRouter:
		return strings.TrimSpace(id)[len(ProviderOpenRouter)+1:]
	case ProviderZen:
		return strings.TrimSpace(id)[len(ProviderZen)+1:]
	}
	return strings.TrimSpace(id)
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

// thinkingLevelsCachePath is $XDG_DATA_HOME/valen/thinking_levels.json
// (or ~/.local/share/valen/...), a map of lowercase model ID to levels.
func thinkingLevelsCachePath() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "valen", "thinking_levels.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "valen", "thinking_levels.json"), nil
}

func loadThinkingLevelsCache() map[string][]string {
	out := map[string][]string{}
	p, err := thinkingLevelsCachePath()
	if err != nil {
		return out
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		out[normalizeModelID(k)] = v
	}
	return out
}

func cachedThinkingLevels(model string) ([]string, bool) {
	l := normalizeModelID(model)
	m := loadThinkingLevelsCache()
	lv, ok := m[l]
	if !ok {
		return nil, false
	}
	return lv, true
}

func saveThinkingLevelsCache(model string, levels []string) error {
	p, err := thinkingLevelsCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	m := loadThinkingLevelsCache()
	m[normalizeModelID(model)] = append([]string(nil), levels...)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(raw, '\n'), 0o600)
}

// parseThinkingLevelsFromError extracts valid levels from a provider 400
// like "effort must be one of: minimal low medium high xhigh max".
// It returns nil when the message is not a level-validation error.
func parseThinkingLevelsFromError(msg string) []string {
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "one of") && !strings.Contains(lower, "valid") && !strings.Contains(lower, "must be") && !strings.Contains(lower, "pick") {
		return nil
	}
	hasTopic := strings.Contains(lower, "effort") || strings.Contains(lower, "thinking") || strings.Contains(lower, "reasoning")
	known := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	var found []string
	for _, k := range known {
		// Word-boundary search so "low" doesn't match inside other words.
		// Pad with non-letter sentinels and look for the token.
		idx := strings.Index(lower, k)
		for idx >= 0 {
			before, after := true, true
			if idx > 0 {
				c := lower[idx-1]
				if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
					before = false
				}
			}
			if e := idx + len(k); e < len(lower) {
				c := lower[e]
				if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
					after = false
				}
			}
			if before && after {
				found = append(found, k)
				break
			}
			nxt := strings.Index(lower[idx+1:], k)
			if nxt < 0 {
				break
			}
			idx = idx + 1 + nxt
		}
	}
	if len(found) == 0 {
		return nil
	}
	// Without an effort/thinking topic word, require at least two levels so
	// ordinary prose mentioning "low" doesn't false-positive.
	if !hasTopic && len(found) < 2 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool { return effortRank[found[i]] < effortRank[found[j]] })
	return found
}

// maybeLearnThinkingLevels parses a 400 validation message and persists the
// valid levels for model. It reports whether anything was learned.
func maybeLearnThinkingLevels(model, errMsg string) bool {
	lv := parseThinkingLevelsFromError(errMsg)
	if len(lv) == 0 {
		return false
	}
	if cur, ok := cachedThinkingLevels(model); ok && strings.Join(cur, ",") == strings.Join(lv, ",") {
		return true
	}
	if err := saveThinkingLevelsCache(model, lv); err != nil {
		return false
	}
	return true
}

// probeThinkingLevels asks the provider what it accepts by sending a bogus
// effort ("__probe__"). A 400 like "effort must be one of: ..." is parsed,
// cached to disk, and returned. Known models return the table without any
// network. It returns an error when the provider ignores the probe (200) or
// the message carries no level list.
func probeThinkingLevels(ctx context.Context, cfg Config, apiKey string, hc *http.Client) ([]string, error) {
	l := normalizeModelID(cfg.Model)
	if lv, ok := cachedThinkingLevels(l); ok {
		return lv, nil
	}
	if lv, ok := lookupKnownLevels(modelBaseID(cfg.Model)); ok {
		return append([]string(nil), lv...), nil
	}
	const probe = "__probe__"
	var url string
	var body any
	sendCfg := cfg
	// Resolve live keys so a post-boot export still probes correctly.
	apiKey = effectiveOpenCodeKey(cfg, apiKey)
	sendCfg.OpenRouterKey = effectiveOpenRouterKey(cfg)
	headers := chatHeaders(apiKey)
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		// No Go session affinity on OpenRouter: the session header
		// would be a meaningless foreign header upstream.
		sendCfg.SessionID = ""
		url = openRouterBaseURL + "/chat/completions"
		body = map[string]any{"model": wireModelID(cfg.Model), "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "reasoning_effort": probe}
		headers = openRouterHeaders(cfg.OpenRouterKey)
	} else {
		switch modelTransport(cfg.Model) {
		case "responses":
			url = responsesEndpoint(providerBaseURL(cfg))
			body = map[string]any{"model": wireModelID(cfg.Model), "input": []any{map[string]any{"role": "user", "content": "hi"}}, "reasoning": map[string]any{"effort": probe}, "store": false}
		case "messages":
			url = messagesEndpoint(providerBaseURL(cfg))
			body = map[string]any{"model": wireModelID(cfg.Model), "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "reasoning_effort": probe}
		default:
			url = chatEndpoint(providerBaseURL(cfg))
			body = map[string]any{"model": wireModelID(cfg.Model), "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "reasoning_effort": probe}
		}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*1000000000)
		defer cancel()
	}
	resp, err := postAPI(ctx, hc, sendCfg, url, body, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 200 {
		return nil, fmt.Errorf("probe ignored (HTTP 200)")
	}
	lv := parseThinkingLevelsFromError(string(raw))
	if len(lv) == 0 {
		return nil, fmt.Errorf("probe gave no levels (HTTP %d)", resp.StatusCode)
	}
	_ = saveThinkingLevelsCache(l, lv)
	return lv, nil
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

// fetchOpenRouterModels returns the live OpenRouter model IDs in stored
// (provider-prefixed) form, sorted. Batch variants (":batch") and routing
// aliases ("~" prefix) are dropped: pricing/shape duplicates that would only
// clutter the picker.
func fetchOpenRouterModels(ctx context.Context, hc *http.Client, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range openRouterHeaders(apiKey) {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", userAgent)
	client := hc
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := clientDoWithRetryTimeout(ctx, client, req, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseTransportError("openrouter", resp.StatusCode, raw)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("api: openrouter: decode models: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || strings.HasPrefix(id, "~") || strings.Contains(id, ":batch") {
			continue
		}
		ids = append(ids, ProviderOpenRouter+"/"+id)
	}
	sort.Strings(ids)
	return ids, nil
}

// fetchZenModels returns the live OpenCode Zen model IDs in stored
// (zen/-prefixed) form, sorted. Models on transports valen cannot speak
// (native Gemini) are dropped. The catalog is public; the key is sent when
// present.
func fetchZenModels(ctx context.Context, hc *http.Client, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("User-Agent", userAgent)
	client := hc
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := clientDoWithRetryTimeout(ctx, client, req, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseTransportError("zen", resp.StatusCode, raw)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("api: zen: decode models: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || zenTransport(id) == "gemini" {
			continue
		}
		ids = append(ids, ProviderZen+"/"+id)
	}
	sort.Strings(ids)
	return ids, nil
}

// fetchGoModels returns the live OpenCode Go model IDs, sorted.
func fetchGoModels(ctx context.Context, client *openai.Client) ([]string, error) {
	list, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Models))
	for _, m := range list.Models {
		if strings.TrimSpace(m.ID) != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// knownGoModel reports whether id is in the curated catalog (case-insensitive).
func knownGoModel(id string) bool {
	for _, m := range fallbackGoModels {
		if strings.EqualFold(m, strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

// matchGoModel resolves arg against stored IDs: exact (case-insensitive)
// match wins, otherwise a case-insensitive substring search. Matching also
// sees the display ("go/…") and wire ("vendor/…") forms, so "kimi-k3" finds
// both the Go model and "openrouter/moonshotai/kimi-k3". It returns stored IDs.
func matchGoModel(ids []string, arg string) []string {
	arg = strings.TrimSpace(arg)
	for _, id := range ids {
		if strings.EqualFold(id, arg) || strings.EqualFold(displayModelID(id), arg) {
			return []string{id}
		}
	}
	var out []string
	lower := strings.ToLower(arg)
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), lower) ||
			strings.Contains(strings.ToLower(displayModelID(id)), lower) ||
			strings.Contains(strings.ToLower(wireModelID(id)), lower) {
			out = append(out, id)
		}
	}
	return out
}
