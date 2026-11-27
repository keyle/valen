package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keyle/valen/internal/dvlog"
)

// Shared streaming layer for the three transports.
//
// Every round first tries a streaming request (`stream: true`). The response
// body is peeked: a `{` means the provider answered with a complete unary
// JSON object (or ignores streaming), which folds through the existing unary
// parsers; otherwise the body is server-sent events parsed incrementally so
// text/thinking deltas reach the UI live. A 400 that complains about
// streaming falls back to one unary retry per round.
//
// Transient 429/502/503/504 responses and network errors retry with backoff
// (up to 3 attempts) so overloads back off instead of failing the turn.
const retryMaxAttempts = 3

// retryBackoff is a var (not const) so offline tests can shrink the waits;
// production uses 300ms then 800ms.
var retryBackoff = []time.Duration{300 * time.Millisecond, 800 * time.Millisecond}

func retryDelay(attempt int) time.Duration {
	if attempt < len(retryBackoff) {
		return retryBackoff[attempt]
	}
	return retryBackoff[len(retryBackoff)-1]
}

// retryDelayFor is the backoff honouring a provider's wait hint: the
// larger of the fixed backoff and the hint shown.
func retryDelayFor(attempt int, hint time.Duration) time.Duration {
	if d := retryDelay(attempt); hint > d {
		return hint
	}
	return retryDelay(attempt)
}

// parseRetryAfterHeader reads Retry-After: seconds or HTTP date.
func parseRetryAfterHeader(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// parseRateLimitHeaders reads OpenRouter's X-RateLimit-* platform headers.
func parseRateLimitHeaders(h http.Header) (limit, remaining int, resetMs int64) {
	if v := strings.TrimSpace(h.Get("X-RateLimit-Limit")); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	if v := strings.TrimSpace(h.Get("X-RateLimit-Remaining")); v != "" {
		remaining, _ = strconv.Atoi(v)
	}
	if v := strings.TrimSpace(h.Get("X-RateLimit-Reset")); v != "" {
		resetMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return limit, remaining, resetMs
}

// rateResetDelay converts X-RateLimit-Reset (OpenRouter: epoch ms) to a wait.
// Some proxies send epoch seconds; an absurd result falls back to seconds.
func rateResetDelay(resetMs int64) time.Duration {
	if resetMs <= 0 {
		return 0
	}
	reset := time.UnixMilli(resetMs)
	if d := time.Until(reset); d > 0 && d < 24*time.Hour {
		return d
	}
	reset = time.Unix(resetMs, 0)
	if d := time.Until(reset); d > 0 && d < 24*time.Hour {
		return d
	}
	return 0
}

// rateHint is the wait the response asks for: Retry-After, else the
// X-RateLimit reset window when the quota is exhausted.
func rateHint(h http.Header) time.Duration {
	if d := parseRetryAfterHeader(h); d > 0 {
		return d
	}
	limit, remaining, resetMs := parseRateLimitHeaders(h)
	if remaining == 0 && (limit == 0 || remaining <= limit) {
		return rateResetDelay(resetMs)
	}
	return 0
}

// rateState tracks the last seen quota per host so a fast provider is not
// hammered: after a 429 (or a 200 reporting remaining=0) the next request
// waits until the hinted reset instead of firing immediately.
var (
	rateMu     sync.Mutex
	rateStates = map[string]rateState{}
)

type rateState struct {
	blockedUntil time.Time
	remaining    int
	resetMs      int64
}

func rateHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Host
}

// noteRateLimit records quota headers from any response (200 included) so the
// next call can pace itself.
func noteRateLimit(h http.Header, host string) {
	if host == "" || h == nil {
		return
	}
	_, remaining, resetMs := parseRateLimitHeaders(h)
	hint := rateHint(h)
	if remaining == 0 && resetMs == 0 && hint == 0 && h.Get("X-RateLimit-Remaining") == "" {
		return
	}
	rateMu.Lock()
	defer rateMu.Unlock()
	st := rateStates[host]
	if h.Get("X-RateLimit-Remaining") != "" {
		st.remaining = remaining
	}
	if resetMs != 0 {
		st.resetMs = resetMs
	}
	if hint > 0 {
		if until := time.Now().Add(hint); until.After(st.blockedUntil) {
			st.blockedUntil = until
		}
	}
	rateStates[host] = st
}

// rateWait sleeps until the recorded reset when the quota is spent.
// It is a no-op for hosts that never sent quota headers.
func rateWait(ctx context.Context, host string) error {
	rateMu.Lock()
	st := rateStates[host]
	rateMu.Unlock()
	if st.blockedUntil.IsZero() {
		return nil
	}
	d := time.Until(st.blockedUntil)
	if d <= 0 {
		return nil
	}
	dvlog.Logf("rate limit pacing host=%s sleep=%s", host, dvlog.Dur(d))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// withRateLimit attaches quota headers (and the wait hint they encode) to a
// parsed transport error.
func withRateLimit(te *transportError, h http.Header) *transportError {
	if te == nil || h == nil {
		return te
	}
	te.RateLimitLimit, te.RateLimitRemaining, te.RateLimitResetMs = parseRateLimitHeaders(h)
	if d := rateHint(h); d > te.RetryAfter {
		te.RetryAfter = d
	}
	return te
}

// fillRateLimitFromBody reads OpenRouter's error.metadata hints (an upstream
// 429 like BaseTen's shared pool): provider, source and retry_after_seconds.
func fillRateLimitFromBody(te *transportError, body []byte) {
	if te == nil || len(body) == 0 {
		return
	}
	var v struct {
		Error struct {
			Metadata struct {
				ProviderName       string `json:"provider_name"`
				LimitSource        string `json:"limit_source"`
				RetryAfterSeconds  int    `json:"retry_after_seconds"`
				RetryAfterSecondsR int    `json:"retry_after_seconds_raw"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return
	}
	if te.Provider == "" {
		te.Provider = v.Error.Metadata.ProviderName
	}
	if te.Upstream == "" {
		te.Upstream = v.Error.Metadata.LimitSource
	}
	secs := v.Error.Metadata.RetryAfterSeconds
	if secs == 0 {
		secs = v.Error.Metadata.RetryAfterSecondsR
	}
	if secs > 0 {
		if d := time.Duration(secs) * time.Second; d > te.RetryAfter {
			te.RetryAfter = d
		}
	}
}

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// clientDoWithRetry executes req, retrying transient statuses and network
// errors with backoff. Each attempt's deadline lasts through body consumption,
// not just receipt of headers. Closing the body releases its context.
func clientDoWithRetry(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	return clientDoWithRetryTimeout(ctx, client, req, requestTimeout(currentTurnConfig(ctx, Config{})))
}

type deadlineBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *deadlineBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

func clientDoWithRetryTimeout(ctx context.Context, client *http.Client, req *http.Request, timeout time.Duration) (*http.Response, error) {
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if err := rateWait(ctx, rateHost(req)); err != nil {
			return nil, err
		}
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		t0 := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := client.Do(req.Clone(attemptCtx))
		if err != nil {
			cancel()
			if ctx.Err() != nil || attempt+1 >= retryMaxAttempts {
				dvlog.Logf("http POST %s attempt=%d err=%v dur=%s terminal", path, attempt+1, err, dvlog.Dur(time.Since(t0)))
				return nil, err
			}
			dvlog.Logf("http POST %s attempt=%d err=%v dur=%s sleep=%s", path, attempt+1, err, dvlog.Dur(time.Since(t0)), retryDelay(attempt))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay(attempt)):
			}
			continue
		}
		noteRateLimit(resp.Header, rateHost(req))
		if !isRetryableStatus(resp.StatusCode) || attempt+1 >= retryMaxAttempts {
			dvlog.Logf("http POST %s attempt=%d status=%d dur=%s", path, attempt+1, resp.StatusCode, dvlog.Dur(time.Since(t0)))
			resp.Body = &deadlineBody{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		hint := rateHint(resp.Header)
		dvlog.Logf("http POST %s attempt=%d status=%d dur=%s sleep=%s", path, attempt+1, resp.StatusCode, dvlog.Dur(time.Since(t0)), retryDelayFor(attempt, hint))
		// An error body may never finish. Sacrifice connection reuse rather than
		// letting a retry stall while draining even a byte-bounded body.
		cancel()
		resp.Body.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelayFor(attempt, hint)):
		}
	}
	return nil, errors.New("api: retry exhausted")
}

func postAPI(ctx context.Context, hc *http.Client, cfg Config, url string, body any, headers map[string]string) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	dvlog.Logf("http POST %s reqbytes=%d", url, len(raw))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cfg.SessionID != "" {
		req.Header.Set("x-opencode-session", cfg.SessionID)
	}
	req.Header.Set("User-Agent", userAgent)
	client := hc
	if client == nil {
		client = http.DefaultClient
	}
	return clientDoWithRetryTimeout(ctx, client, req, requestTimeout(cfg))
}

// parseTransportError maps a non-200 body the same way across transports,
// trying the OpenAI error shape, then the Anthropic shape, then raw text.
// The capped raw body is kept on Raw so ctrl+x details can show the full
// JSON even when only the message is extracted here.
func parseTransportError(transport string, status int, body []byte) *transportError {
	raw := capRawBody(body)
	var oa struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &oa); err == nil && oa.Error.Message != "" {
		te := &transportError{Transport: transport, Status: status, Message: oa.Error.Message, Raw: rawDiff(raw, oa.Error.Message)}
		fillRateLimitFromBody(te, body)
		return te
	}
	var anth struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &anth); err == nil && anth.Error.Message != "" {
		te := &transportError{Transport: transport, Status: status, Message: anth.Error.Message, Raw: rawDiff(raw, anth.Error.Message)}
		fillRateLimitFromBody(te, body)
		return te
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	te := &transportError{Transport: transport, Status: status, Message: msg, Raw: rawDiff(raw, msg)}
	fillRateLimitFromBody(te, body)
	return te
}

// isStreamRejected reports a 400 that blames streaming, the signal to retry
// the round once without `stream: true`.
func isStreamRejected(err error) bool {
	var te *transportError
	if !errors.As(err, &te) {
		return false
	}
	return te.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(te.Message), "stream")
}

// splitBodyMode peeks at the first non-space byte: `{` is a complete unary
// JSON body (fully read here); anything else is an SSE stream over br.
func splitBodyMode(br *bufio.Reader) (sse bool, raw []byte, err error) {
	for {
		c, err := br.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return false, nil, err
			}
			return false, nil, errors.New("api: empty response body")
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c == '{' {
			if err := br.UnreadByte(); err != nil {
				return false, nil, err
			}
			raw, err := io.ReadAll(br)
			return false, raw, err
		}
		if err := br.UnreadByte(); err != nil {
			return false, nil, err
		}
		return true, nil, nil
	}
}

type sseEvent struct {
	name string
	data string
}

// scanSSE dispatches server-sent events incrementally so deltas reach the UI
// while the body is still arriving. fn returning false stops the scan.
func scanSSE(ctx context.Context, br *bufio.Reader, fn func(sseEvent) bool) error {
	var name string
	var data []string
	flush := func() bool {
		ev := sseEvent{name: name, data: strings.Join(data, "\n")}
		name = ""
		data = nil
		if strings.TrimSpace(ev.data) == "" && ev.name == "" {
			return true
		}
		return fn(ev)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := br.ReadString('\n')
		if err != nil && len(line) == 0 {
			if errors.Is(err, io.EOF) {
				flush()
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			v = strings.TrimPrefix(v, " ")
			switch k {
			case "event":
				name = v
			case "data":
				data = append(data, v)
			}
		}
	}
}
