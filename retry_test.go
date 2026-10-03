package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func TestRoundRetryDelayExponential(t *testing.T) {
	oldB, oldM := roundRetryBase, roundRetryMax
	roundRetryBase, roundRetryMax = 100*time.Millisecond, 500*time.Millisecond
	defer func() { roundRetryBase, roundRetryMax = oldB, oldM }()
	for attempt, want := range []time.Duration{100, 200, 400, 500, 500} {
		want *= time.Millisecond
		got := roundRetryDelay(attempt)
		if got < want || got > want+want/5 {
			t.Fatalf("attempt %d: got %s, want %s..+20%%", attempt, got, want)
		}
	}
}

func okMsg() openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: "assistant", Content: "done"}
}

func TestRoundWithRetryRecovers(t *testing.T) {
	calls := 0
	var events []string
	msg, usage, err := roundWithRetry(context.Background(), func(e Event) { events = append(events, e.Kind) }, nil,
		func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
			calls++
			if calls < 3 {
				return openai.ChatCompletionMessage{}, openai.Usage{TotalTokens: 1}, &transportError{Transport: "openrouter", Status: 503, Message: "down"}
			}
			return okMsg(), openai.Usage{TotalTokens: 1}, nil
		})
	if err != nil || msg.Content != "done" || calls != 3 {
		t.Fatalf("err=%v msg=%q calls=%d", err, msg.Content, calls)
	}
	if usage.TotalTokens != 3 || len(events) != 2 || events[0] != "retry" {
		t.Fatalf("usage=%d events=%v", usage.TotalTokens, events)
	}
}

func TestRoundWithRetryGivesUp(t *testing.T) {
	calls := 0
	_, _, err := roundWithRetry(context.Background(), nil, nil,
		func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
			calls++
			return openai.ChatCompletionMessage{}, openai.Usage{}, &transportError{Transport: "openrouter", Status: 503, Message: "down"}
		})
	if err == nil || calls != roundMaxAttempts {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestShouldRetryRoundStatus(t *testing.T) {
	for status, want := range map[int]bool{
		400: false, 401: false, 402: false, 403: false, 404: false,
		408: true, 429: true, 500: true, 502: true, 503: true, 0: true,
	} {
		var err error = &transportError{Transport: "openrouter", Status: status, Message: "x"}
		if got := shouldRetryRound(context.Background(), err); got != want {
			t.Errorf("transport %d: got %v want %v", status, got, want)
		}
	}
	if shouldRetryRound(context.Background(), &openai.APIError{HTTPStatusCode: 402, Message: "credits"}) {
		t.Error("openai APIError 402 must not retry")
	}
	if !shouldRetryRound(context.Background(), &openai.APIError{HTTPStatusCode: 502, Message: "gw"}) {
		t.Error("openai APIError 502 must retry")
	}
	if !shouldRetryRound(context.Background(), errors.New("unexpected EOF")) {
		t.Error("network errors must retry")
	}
}

func TestRoundWithRetryNoRetryOnCancelOrMissingKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, _, err := roundWithRetry(ctx, nil, nil, func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		calls++
		cancel()
		return openai.ChatCompletionMessage{}, openai.Usage{}, errors.New("boom")
	})
	if err == nil || calls != 1 {
		t.Fatalf("cancel: err=%v calls=%d", err, calls)
	}
	calls = 0
	_, _, err = roundWithRetry(context.Background(), nil, nil, func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		calls++
		return openai.ChatCompletionMessage{}, openai.Usage{}, errors.New("OPENROUTER_API_KEY is not set")
	})
	if err == nil || calls != 1 {
		t.Fatalf("missing key: err=%v calls=%d", err, calls)
	}
}

// Every message on the wire must serialize a content key, except assistant
// tool-call messages (which the provider accepts without one).
func TestChatSendMessagesNeverOmitsContent(t *testing.T) {
	in := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: ""},
		{Role: openai.ChatMessageRoleAssistant, Content: "  \n"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "a", Type: "function"}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "a", Content: ""},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "b", Content: " \n"},
	}
	out := chatSendMessages(in)
	if len(out) != 5 {
		t.Fatalf("blank text-only assistant must be dropped, got %d msgs", len(out))
	}
	for i, m := range out {
		b, _ := json.Marshal(m)
		if !strings.Contains(string(b), `"content"`) && len(m.ToolCalls) == 0 {
			t.Fatalf("msg %d (%s) has no content key: %s", i, m.Role, b)
		}
	}
	if in[1].Content != "" || len(in) != 6 {
		t.Fatal("stored history must not be modified")
	}
}

func TestEmptyToolOutputSentAsText(t *testing.T) {
	out := chatSendMessages([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleTool, ToolCallID: "x", Content: ""}})
	if out[0].Content == "" {
		t.Fatal("empty tool output must be non-empty on the wire")
	}
}
