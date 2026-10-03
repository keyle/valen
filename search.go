package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	maxSearchResults = 8
	maxSearchBytes   = 1 << 20
	maxFetchBytes    = 512 << 10
	maxFetchRunes    = 8000
	httpTimeout      = 30 * time.Second
)

// searchBaseURL is a package var so tests can point it at a local server.
var searchBaseURL = "https://html.duckduckgo.com/html/"

var httpClient = &http.Client{Transport: sharedTransport, Timeout: httpTimeout}

var (
	linkRe   = regexp.MustCompile(`class="result__a" href="([^"]+)"[^>]*>(.*?)</a>`)
	snipRe   = regexp.MustCompile(`class="result__snippet"[^>]*>(.*?)</a>`)
	spaceRe  = regexp.MustCompile(`\s+`)
	entities = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
		"&#x27;", "'", "&#39;", "'", "&nbsp;", " ", "&ndash;", "-", "&mdash;", "-",
	)
)

func WebSearchTool() Tool {
	return Tool{
		Name:        "web_search",
		Description: "Search the web and return up to 8 results as title, url and snippet",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"search query"}},"required":["query"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Query) == "" {
				return "", fmt.Errorf("query is required")
			}
			body, err := fetch(ctx, searchBaseURL+"?q="+url.QueryEscape(a.Query), maxSearchBytes)
			if err != nil {
				return "", err
			}
			results := parseSearchResults(string(body))
			if len(results) == 0 {
				return "no results", nil
			}
			return strings.Join(results, "\n"), nil
		},
	}
}

func parseSearchResults(body string) []string {
	links := linkRe.FindAllStringSubmatch(body, maxSearchResults)
	snips := snipRe.FindAllStringSubmatch(body, maxSearchResults)
	out := make([]string, 0, len(links))
	for i, l := range links {
		snippet := ""
		if i < len(snips) {
			snippet = plainText(snips[i][1])
		}
		out = append(out, fmt.Sprintf("%d. %s\n   %s\n   %s", i+1, plainText(l[2]), resolveRedirect(l[1]), snippet))
	}
	return out
}

// resolveRedirect unwraps DuckDuckGo's /l/?uddg=<encoded> tracker links.
func resolveRedirect(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
}

func WebFetchTool() Tool {
	return Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL and return its text content",
		Schema:      json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"absolute http(s) url"}},"required":["url"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			body, err := fetch(ctx, a.URL, maxFetchBytes)
			if err != nil {
				return "", err
			}
			return truncate(plainText(string(body)), maxFetchRunes), nil
		},
	}
}

func fetch(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", rawURL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// plainText strips tags, drops script/style content and collapses whitespace.
func plainText(s string) string {
	if !strings.Contains(s, "<") {
		return collapseSpace(entities.Replace(s))
	}
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return collapseSpace(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			if name, _ := z.TagName(); isSkippedTag(name) {
				skip++
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); isSkippedTag(name) && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}

func isSkippedTag(name []byte) bool {
	switch string(name) {
	case "script", "style", "noscript":
		return true
	}
	return false
}

func collapseSpace(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}
