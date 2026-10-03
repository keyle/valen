package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"valen/internal/dvlog"
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

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// clientDoWithRetry executes req, retrying transient statuses and network
// errors with backoff. Retryable responses are drained and closed before the
// next attempt so connections reuse. Non-retryable responses return as-is
// for the caller to parse (including SSE streams).
func clientDoWithRetry(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		t0 := time.Now()
		resp, err := client.Do(req)
		if err != nil {
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
		if !isRetryableStatus(resp.StatusCode) || attempt+1 >= retryMaxAttempts {
			dvlog.Logf("http POST %s attempt=%d status=%d dur=%s", path, attempt+1, resp.StatusCode, dvlog.Dur(time.Since(t0)))
			return resp, nil
		}
		dvlog.Logf("http POST %s attempt=%d status=%d dur=%s sleep=%s", path, attempt+1, resp.StatusCode, dvlog.Dur(time.Since(t0)), retryDelay(attempt))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelay(attempt)):
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
	return clientDoWithRetry(ctx, client, req)
}

// parseTransportError maps a non-200 body the same way across transports,
// trying the OpenAI error shape, then the Anthropic shape, then raw text.
func parseTransportError(transport string, status int, body []byte) *transportError {
	var oa struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &oa); err == nil && oa.Error.Message != "" {
		return &transportError{Transport: transport, Status: status, Message: oa.Error.Message}
	}
	var anth struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &anth); err == nil && anth.Error.Message != "" {
		return &transportError{Transport: transport, Status: status, Message: anth.Error.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &transportError{Transport: transport, Status: status, Message: msg}
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
