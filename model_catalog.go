package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// orPricing is one OpenRouter model's per-token input/output price from the
// catalog (`pricing.prompt` / `pricing.completion`), used for the footer's
// per-million rate metadata.
type orPricing struct {
	prompt     float64
	completion float64
}

// orPrice parses one OpenRouter catalog price (a string like "0.0000015",
// sometimes a bare JSON number); a malformed value yields 0.
func orPrice(raw string) float64 {
	f, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(raw), `"`), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// fetchOpenRouterCatalog returns the live OpenRouter model IDs in stored
// (provider-prefixed) form, sorted, plus each model's catalog pricing keyed by
// the same stored ID. Batch variants (":batch") and routing aliases ("~"
// prefix) are dropped: pricing/shape duplicates that would only clutter the
// picker.
func fetchOpenRouterCatalog(ctx context.Context, hc *http.Client, baseURL, apiKey string) ([]string, map[string]orPricing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, parseTransportError("openrouter", resp.StatusCode, raw)
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, fmt.Errorf("api: openrouter: decode models: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	prices := map[string]orPricing{}
	for _, m := range list.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || strings.HasPrefix(id, "~") || strings.Contains(id, ":batch") {
			continue
		}
		stored := ProviderOpenRouter + "/" + id
		ids = append(ids, stored)
		if p := (orPricing{prompt: orPrice(m.Pricing.Prompt), completion: orPrice(m.Pricing.Completion)}); p.prompt != 0 || p.completion != 0 {
			prices[stored] = p
		}
	}
	sort.Strings(ids)
	return ids, prices, nil
}

// fetchOpenRouterModels is the ID-only view of the catalog fetch.
func fetchOpenRouterModels(ctx context.Context, hc *http.Client, baseURL, apiKey string) ([]string, error) {
	ids, _, err := fetchOpenRouterCatalog(ctx, hc, baseURL, apiKey)
	return ids, err
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
