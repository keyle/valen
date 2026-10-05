package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testGoUsage() GoUsage {
	return GoUsage{
		Rolling: UsageWindow{Status: "ok", Percent: 90, ResetsAt: time.Now().Add(38 * time.Minute)},
		Weekly:  UsageWindow{Status: "ok", Percent: 36, ResetsAt: time.Now().Add(3 * 24 * time.Hour)},
		Monthly: UsageWindow{Status: "ok", Percent: 18, ResetsAt: time.Now().Add(28 * 24 * time.Hour)},
	}
}

func TestFetchGoUsage(t *testing.T) {
	var gotAuth, gotSession, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"rolling":{"status":"ok","percent":90,"resetsAt":"2026-10-02T06:53:55.532Z"},`+
			`"weekly":{"status":"ok","percent":36,"resetsAt":"2026-10-05T00:00:00.000Z"},`+
			`"monthly":{"status":"rate-limited","percent":100,"resetsAt":"2026-10-30T11:52:02.000Z"}}}`)
	}))
	defer srv.Close()

	hc := &http.Client{Transport: &headerTransport{base: http.DefaultTransport, sessionID: "ses-usage"}}
	u, err := fetchGoUsage(context.Background(), hc, srv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/usage" || gotAuth != "Bearer key" || gotSession != "ses-usage" || gotUA != userAgent {
		t.Fatalf("request: path=%q auth=%q session=%q ua=%q", gotPath, gotAuth, gotSession, gotUA)
	}
	if u.Rolling.Percent != 90 || u.Weekly.Percent != 36 || u.Monthly.Status != "rate-limited" {
		t.Fatalf("usage: %+v", u)
	}
	if u.Rolling.ResetsAt.IsZero() {
		t.Fatal("resetsAt not parsed")
	}
}

func TestFetchGoUsageHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	if _, err := fetchGoUsage(context.Background(), srv.Client(), srv.URL, "key"); err == nil {
		t.Fatal("404 must be an error so the footer stays hidden")
	}
}

func TestUsageFooterText(t *testing.T) {
	now := time.Now()
	text := testGoUsage().footerText(now)
	for _, want := range []string{"5h 90%", "7d 36%", "30d 18%", "↻"} {
		if !strings.Contains(text, want) {
			t.Fatalf("footer text %q missing %q", text, want)
		}
	}
	cool := GoUsage{Rolling: UsageWindow{Status: "ok", Percent: 12}, Weekly: UsageWindow{Status: "ok", Percent: 3}, Monthly: UsageWindow{Status: "ok", Percent: 1}}
	if strings.Contains(cool.footerText(now), "↻") {
		t.Fatalf("cold windows should not show reset hints: %q", cool.footerText(now))
	}
}

func TestUsageSeverity(t *testing.T) {
	if got := testGoUsage().usageSev(); got != "warn" {
		t.Fatalf("90%% should warn, got %q", got)
	}
	bad := testGoUsage()
	bad.Rolling.Percent = 100
	if got := bad.usageSev(); got != "bad" {
		t.Fatalf("100%% should be bad, got %q", got)
	}
	ok := GoUsage{Rolling: UsageWindow{Percent: 10}, Weekly: UsageWindow{Percent: 5}, Monthly: UsageWindow{Percent: 1}}
	if got := ok.usageSev(); got != "" {
		t.Fatalf("low usage should be normal, got %q", got)
	}
}

func TestTUIUsageMsgAndFooter(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	m.width = 200
	if strings.Contains(m.footerView(), "5h") {
		t.Fatal("footer must stay clean until usage arrives")
	}
	_, _ = m.Update(usageMsg{usage: testGoUsage()})
	footer := m.footerView()
	for _, want := range []string{"5h 90%", "7d 36%", "30d 18%"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer %q missing %q", footer, want)
		}
	}
	// A failed refresh keeps the last good snapshot.
	_, _ = m.Update(usageMsg{err: fmt.Errorf("boom")})
	if m.goUsage == nil || !strings.Contains(m.footerView(), "5h 90%") {
		t.Fatal("usage should survive a failed refresh")
	}
	// Narrow terminals fall back to the rolling window only.
	m.width = 60
	if footer := m.footerView(); strings.Contains(footer, "30d") {
		t.Fatalf("narrow footer should drop weekly/monthly:\n%s", footer)
	}
}

func TestTUIUsageFetchCmd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"rolling":{"status":"ok","percent":7,"resetsAt":"2026-10-02T06:53:55Z"}}}`)
	}))
	defer srv.Close()

	m := newTestModel(t, testConfig(srv.URL))
	if cmd := m.fetchUsageCmd(); cmd != nil {
		t.Fatal("no key/client yet — usage fetch should be a no-op")
	}
	m.apiKey = "key"
	m.httpClient = srv.Client()
	msg, ok := m.fetchUsageCmd()().(usageMsg)
	if !ok || msg.err != nil || msg.usage.Rolling.Percent != 7 {
		t.Fatalf("usage msg: %+v ok=%v", msg, ok)
	}
	// A second fetch while one is in flight is a no-op (tick + post-turn
	// refresh must not stack on slow networks); after the reply lands, a
	// new fetch dispatches again.
	m2 := newTestModel(t, testConfig(srv.URL))
	m2.apiKey = "key"
	m2.httpClient = srv.Client()
	if m2.fetchUsageCmd() == nil {
		t.Fatal("first fetch should dispatch")
	}
	if m2.fetchUsageCmd() != nil {
		t.Fatal("overlapping fetch should be a no-op")
	}
	_, _ = m2.Update(usageMsg{usage: testGoUsage()})
	if m2.fetchUsageCmd() == nil {
		t.Fatal("fetch should dispatch again after the reply")
	}
}

func TestFetchOpenRouterCredits(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"total_credits":20,"total_usage":7.5}}`)
	}))
	defer srv.Close()

	c, err := fetchOpenRouterCredits(context.Background(), srv.Client(), srv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/credits" || gotAuth != "Bearer key" {
		t.Fatalf("request: path=%q auth=%q", gotPath, gotAuth)
	}
	if c.Remaining() != 12.5 {
		t.Fatalf("remaining: %+v", c)
	}
}

func TestFetchOpenRouterCreditsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Plain API keys get a 403 here; the footer must stay hidden rather
		// than surface an error.
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := fetchOpenRouterCredits(context.Background(), srv.Client(), srv.URL, "key"); err == nil {
		t.Fatal("403 must be an error so the footer stays hidden")
	}
}

func TestORCreditsRemainingAndFooter(t *testing.T) {
	c := ORCredits{TotalCredits: 20, TotalUsage: 7.555}
	if got := c.footerText(); got != "openrouter $12.45 left" {
		t.Fatalf("footer text %q", got)
	}
	over := ORCredits{TotalCredits: 1, TotalUsage: 2}
	if over.Remaining() != 0 {
		t.Fatalf("over-spend must floor at zero: %v", over.Remaining())
	}
	if got := over.footerText(); got != "openrouter $0.00 left" {
		t.Fatalf("footer text %q", got)
	}
}

func orTestConfig(url string) Config {
	cfg := testConfig(url)
	cfg.Model = "openrouter/anthropic/claude-test"
	cfg.OpenRouterKey = "or-key"
	return cfg
}

func TestTUIFooterHidesGoUsageOnOpenRouter(t *testing.T) {
	m := newTestModel(t, orTestConfig("https://unused"))
	m.width = 200
	// Even with a Go usage snapshot around, an OpenRouter model never shows
	// the Go plan quota.
	m.goUsage = &GoUsage{Rolling: UsageWindow{Status: "ok", Percent: 90}}
	if footer := m.footerView(); strings.Contains(footer, "5h") || strings.Contains(footer, "90%") {
		t.Fatalf("OpenRouter footer must hide Go usage:\n%s", footer)
	}
	if footer := m.footerView(); !strings.Contains(footer, "/help for keys and commands") {
		t.Fatalf("hint must remain:\n%s", footer)
	}
	// A failed credits refresh keeps the footer clean (or keeps the last
	// good balance).
	_, _ = m.Update(orCreditsMsg{err: fmt.Errorf("boom")})
	if m.orCredits != nil {
		t.Fatal("failed refresh must not set credits")
	}
	_, _ = m.Update(orCreditsMsg{credits: ORCredits{TotalCredits: 20, TotalUsage: 7.5}})
	footer := m.footerView()
	if !strings.Contains(footer, "openrouter $12.50 left") {
		t.Fatalf("footer must show the balance:\n%s", footer)
	}
	if strings.Contains(footer, "5h") {
		t.Fatalf("balance footer must not leak Go usage:\n%s", footer)
	}
	_, _ = m.Update(orCreditsMsg{err: fmt.Errorf("boom")})
	if !strings.Contains(m.footerView(), "openrouter $12.50 left") {
		t.Fatal("balance should survive a failed refresh")
	}
}

func TestFetchQuotaCmdDispatchesByProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/credits":
			fmt.Fprint(w, `{"data":{"total_credits":10,"total_usage":3}}`)
		case "/usage":
			fmt.Fprint(w, `{"usage":{"rolling":{"status":"ok","percent":7,"resetsAt":"2026-10-02T06:53:55Z"}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldBase := openRouterBaseURL
	openRouterBaseURL = srv.URL
	defer func() { openRouterBaseURL = oldBase }()

	// OpenRouter model -> credits fetch, never the Go usage fetch.
	om := newTestModel(t, orTestConfig("https://unused"))
	om.httpClient = srv.Client()
	cmd := om.fetchQuotaCmd()
	if cmd == nil {
		t.Fatal("OpenRouter model should dispatch a credits fetch")
	}
	msg, ok := cmd().(orCreditsMsg)
	if !ok || msg.err != nil || msg.credits.Remaining() != 7 {
		t.Fatalf("credits msg: %+v ok=%v", msg, ok)
	}

	// Go model -> usage fetch, never the credits fetch.
	gm := newTestModel(t, testConfig(srv.URL))
	gm.httpClient = srv.Client()
	gcmd := gm.fetchQuotaCmd()
	if gcmd == nil {
		t.Fatal("Go model should dispatch a usage fetch")
	}
	gmsg := gcmd()
	if _, ok := gmsg.(usageMsg); !ok {
		t.Fatalf("Go model must fetch usage, got %T", gmsg)
	}
}
