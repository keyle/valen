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
)

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
