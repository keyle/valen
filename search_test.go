package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchParsesResults(t *testing.T) {
	body := `<html><body>
<a class="result__a" href="https://example.com/1">First &amp; Result</a>
<a class="result__snippet">Snippet one</a>
<a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2F2">Second Result</a>
<a class="result__snippet">Snippet <b>two</b></a>
</body></html>`
	restore := withSearchServer(t, body)
	defer restore()

	out, err := WebSearchTool().Fn(context.Background(), `{"query":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. First & Result", "https://example.com/1", "Snippet one", "2. Second Result", "https://example.com/2", "Snippet two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<b>") {
		t.Fatalf("tags not stripped:\n%s", out)
	}
}

func TestWebSearchCapsResults(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, `<a class="result__a" href="https://example.com/%d">R%d</a><a class="result__snippet">S%d</a>`, i, i, i)
	}
	restore := withSearchServer(t, b.String())
	defer restore()

	out, err := WebSearchTool().Fn(context.Background(), `{"query":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "https://example.com/"); got != maxSearchResults {
		t.Fatalf("results=%d want=%d\n%s", got, maxSearchResults, out)
	}
	if !strings.Contains(out, "R7") || strings.Contains(out, "R8\n") {
		t.Fatalf("cap not applied:\n%s", out)
	}
}

func TestWebSearchNoResults(t *testing.T) {
	restore := withSearchServer(t, "<html><body>nothing</body></html>")
	defer restore()

	out, err := WebSearchTool().Fn(context.Background(), `{"query":"test"}`)
	if err != nil || out != "no results" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestWebSearchRejectsEmptyQuery(t *testing.T) {
	if _, err := WebSearchTool().Fn(context.Background(), `{"query":"  "}`); err == nil {
		t.Fatal("expected error")
	}
}

func TestWebSearchReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	old := searchBaseURL
	searchBaseURL = srv.URL + "/"
	defer func() { searchBaseURL = old }()

	if _, err := WebSearchTool().Fn(context.Background(), `{"query":"test"}`); err == nil {
		t.Fatal("expected error")
	}
}

func TestWebFetchUsesBrowserUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, "<html><body>hi</body></html>")
	}))
	defer srv.Close()

	out, err := WebFetchTool().Fn(context.Background(), fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if out != "hi" {
		t.Fatalf("body=%q", out)
	}
	if gotUA != browserUserAgent || gotUA == userAgent {
		t.Fatalf("web fetch must send the browser UA, got %q", gotUA)
	}
}

func TestWebFetchStripsScriptsAndTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><style>p{color:red}</style><script>var x = "SECRET";</script></head>
<body><p>Hello <b>there</b> &amp; welcome</p></body></html>`)
	}))
	defer srv.Close()

	out, err := WebFetchTool().Fn(context.Background(), `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "color:red") {
		t.Fatalf("script/style leaked: %q", out)
	}
	if !strings.Contains(out, "Hello there & welcome") {
		t.Fatalf("got %q", out)
	}
}

func TestWebFetchTruncatesLongPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<p>"+strings.Repeat("word ", maxFetchRunes)+"</p>")
	}))
	defer srv.Close()

	out, err := WebFetchTool().Fn(context.Background(), `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxFetchRunes+64 || !strings.Contains(out, "truncated") {
		t.Fatalf("len=%d tail=%q", len(out), out[max(0, len(out)-40):])
	}
}

func TestWebFetchRejectsBadArgs(t *testing.T) {
	if _, err := WebFetchTool().Fn(context.Background(), `{`); err == nil {
		t.Fatal("expected error")
	}
}

func TestPlainText(t *testing.T) {
	cases := map[string]string{
		"plain":                  "plain",
		"<p>a</p>":               "a",
		"<script>x</script>y":    "y",
		"a&amp;b":                "a&b",
		"<p>a</p>\n\n  <p>b</p>": "a b",
	}
	for in, want := range cases {
		if got := plainText(in); got != want {
			t.Fatalf("plainText(%q) = %q want %q", in, got, want)
		}
	}
}

func TestResolveRedirect(t *testing.T) {
	if got := resolveRedirect("/l/?uddg=https%3A%2F%2Fx.test%2Fa"); got != "https://x.test/a" {
		t.Fatalf("got %q", got)
	}
	if got := resolveRedirect("https://x.test/a"); got != "https://x.test/a" {
		t.Fatalf("got %q", got)
	}
	if got := resolveRedirect("://bad"); got != "://bad" {
		t.Fatalf("got %q", got)
	}
}

func withSearchServer(t *testing.T, body string) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			t.Error("missing q parameter")
		}
		fmt.Fprint(w, body)
	}))
	old := searchBaseURL
	searchBaseURL = srv.URL + "/"
	return func() {
		searchBaseURL = old
		srv.Close()
	}
}
