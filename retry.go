package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
	"valen/internal/dvlog"
)

// Round-level retry: a failed model round (any provider error, 4xx included)
// is retried with exponential backoff before the turn gives up. Nothing was
// executed from a failed round, so replaying it is safe. These are vars so
// offline tests can shrink the waits.
var (
	roundMaxAttempts = 5
	roundRetryBase   = time.Second
	roundRetryMax    = 30 * time.Second
)

// roundRetryDelay doubles per attempt (1s, 2s, 4s, 8s…) up to roundRetryMax,
// with up to 20% jitter so parallel sessions don't retry in lockstep.
func roundRetryDelay(attempt int) time.Duration {
	d := roundRetryBase
	for i := 0; i < attempt && d < roundRetryMax; i++ {
		d *= 2
	}
	if d > roundRetryMax {
		d = roundRetryMax
	}
	if j := int64(d) / 5; j > 0 {
		d += time.Duration(rand.Int63n(j))
	}
	return d
}

// shouldRetryRound reports whether err is worth another attempt. Final:
// user cancellation, local misconfiguration (missing key), and 4xx
// refusals (except 408/429). Everything else (5xx, network, stream
// errors) may be transient.
func shouldRetryRound(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if strings.Contains(strings.ToLower(err.Error()), "is not set") {
		return false
	}
	// Context overflow is recovered by compaction, and content filters
	// already get their own single retry; repeating either only burns time.
	if isContextOverflow(err) || errors.Is(err, errContentFilter) {
		return false
	}
	// 4xx means the request itself was refused (bad payload, key, credits,
	// model): resending it unchanged gets the same answer. 408 and 429 are
	// the transient exceptions.
	if s := errStatus(err); s >= 400 && s < 500 && s != http.StatusRequestTimeout && s != http.StatusTooManyRequests {
		return false
	}
	return true
}

// errStatus extracts the HTTP status from a provider error, or 0.
func errStatus(err error) int {
	var te *transportError
	if errors.As(err, &te) {
		return te.Status
	}
	var ae *openai.APIError
	if errors.As(err, &ae) {
		return ae.HTTPStatusCode
	}
	var re *openai.RequestError
	if errors.As(err, &re) {
		return re.HTTPStatusCode
	}
	return 0
}

// roundWithRetry runs one model round, retrying failures with backoff and
// announcing each wait as a "retry" event. Usage from failed attempts is
// still summed so the token counter stays honest.
func roundWithRetry(ctx context.Context, onEvent func(Event), history []openai.ChatCompletionMessage, round func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var total openai.Usage
	for attempt := 0; ; attempt++ {
		msg, u, err := round(ctx, history)
		total.PromptTokens += u.PromptTokens
		total.CompletionTokens += u.CompletionTokens
		total.TotalTokens += u.TotalTokens
		if err == nil || attempt+1 >= roundMaxAttempts || !shouldRetryRound(ctx, err) {
			return msg, total, err
		}
		delay := roundRetryDelay(attempt)
		dvlog.Logf("round retry attempt=%d/%d sleep=%s err=%v", attempt+1, roundMaxAttempts, delay, err)
		if onEvent != nil {
			onEvent(Event{Kind: "retry", Text: fmt.Sprintf("Provider error — retrying in %s (attempt %d/%d)…", delay.Round(100*time.Millisecond), attempt+2, roundMaxAttempts)})
		}
		select {
		case <-ctx.Done():
			return openai.ChatCompletionMessage{}, total, ctx.Err()
		case <-time.After(delay):
		}
	}
}
