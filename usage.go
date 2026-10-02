package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GoUsage is the OpenCode Go plan quota snapshot from GET {base_url}/usage.
// Each window carries a 0-100 percent against its limit and, when known,
// when it resets. status is "ok" or "rate-limited".
type GoUsage struct {
	Rolling UsageWindow `json:"rolling"`
	Weekly  UsageWindow `json:"weekly"`
	Monthly UsageWindow `json:"monthly"`
}

// UsageWindow is one quota window (rolling 5h, weekly, monthly).
type UsageWindow struct {
	Status   string    `json:"status"`
	Percent  int       `json:"percent"`
	ResetsAt time.Time `json:"resetsAt"`
}

type usageResponse struct {
	Usage GoUsage `json:"usage"`
}

// fetchGoUsage reads the plan usage endpoint. Non-Go keys and non-OpenCode
// base URLs will simply error (usually 404) and the footer stays hidden.
func fetchGoUsage(ctx context.Context, hc *http.Client, baseURL, apiKey string) (GoUsage, error) {
	var u GoUsage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/usage", nil)
	if err != nil {
		return u, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := hc.Do(req)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return u, fmt.Errorf("usage: %s", resp.Status)
	}
	var body usageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return u, err
	}
	return body.Usage, nil
}

// usageSev classifies how alarming the snapshot is: "" normal, "warn" at
// 80%+ in any window, "bad" at 100%+ or when the window is rate-limited.
func (u GoUsage) usageSev() string {
	sev := ""
	for _, w := range []UsageWindow{u.Rolling, u.Weekly, u.Monthly} {
		if (w.Status != "" && w.Status != "ok") || w.Percent >= 100 {
			return "bad"
		}
		if w.Percent >= 80 {
			sev = "warn"
		}
	}
	return sev
}

// footerText renders the compact quota summary, e.g.
// "5h 90% ↻38m · 7d 36% · 30d 18%". Reset hints only appear on windows
// that are running hot, where the countdown is actionable.
func (u GoUsage) footerText(now time.Time) string {
	windows := []struct {
		label string
		win   UsageWindow
	}{
		{"5h", u.Rolling},
		{"7d", u.Weekly},
		{"30d", u.Monthly},
	}
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		part := fmt.Sprintf("%s %d%%", w.label, w.win.Percent)
		if w.win.Percent >= 80 || (w.win.Status != "" && w.win.Status != "ok") {
			if left := w.win.ResetsAt.Sub(now); left > 0 {
				part += " ↻" + shortReset(left)
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

func shortReset(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
