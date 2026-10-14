package main

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fetchUsageCmd reloads the Go plan quota off the UI thread. It is a no-op
// (nil) when no HTTP client/key is configured, e.g. in model-only tests, or
// when a fetch is already in flight (the minute tick and post-turn refresh
// would otherwise overlap on slow networks).
func (m *tuiModel) fetchUsageCmd() tea.Cmd {
	if m.httpClient == nil || m.usageInflight {
		return nil
	}
	key := effectiveOpenCodeKey(m.cfg, m.apiKey)
	if key == "" {
		return nil
	}
	m.apiKey = key
	m.usageInflight = true
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	hc, url, key := m.httpClient, m.cfg.BaseURL, m.apiKey
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 10*time.Second)
		defer cancel()
		u, err := fetchGoUsage(ctx, hc, url, key)
		return usageMsg{usage: u, err: err}
	}
}

// fetchORCreditsCmd reloads the OpenRouter balance off the UI thread. Like
// fetchUsageCmd it is a no-op (nil) without a client/key or while a fetch is
// already in flight. Non-management keys 403 and the footer stays hidden.
func (m *tuiModel) fetchORCreditsCmd() tea.Cmd {
	if m.httpClient == nil || m.orCreditsInflight {
		return nil
	}
	key := effectiveOpenRouterKey(m.cfg)
	if key == "" {
		return nil
	}
	m.orCreditsInflight = true
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	hc, url := m.httpClient, openRouterBaseURL
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 10*time.Second)
		defer cancel()
		c, err := fetchOpenRouterCredits(ctx, hc, url, key)
		return orCreditsMsg{credits: c, err: err}
	}
}

// fetchQuotaCmd reloads whichever quota the footer shows for the current
// model: the OpenRouter balance on openrouter/… models, the Go plan quota
// otherwise. Callers (init, tick, post-turn, model switch) stay on this so
// the hidden provider never refreshes while selected.
func (m *tuiModel) fetchQuotaCmd() tea.Cmd {
	switch modelProvider(m.cfg.Model) {
	case ProviderOpenRouter:
		return m.fetchORCreditsCmd()
	case ProviderZen:
		return nil // Zen is pay-as-you-go: no plan quota to show
	}
	return m.fetchUsageCmd()
}

func usageTick() tea.Cmd {
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return usageTickMsg{} })
}
