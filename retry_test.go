package main

import (
	"context"
	"errors"
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
				return openai.ChatCompletionMessage{}, openai.Usage{TotalTokens: 1}, &transportError{Transport: "openrouter", Status: 400, Message: "bad"}
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
			return openai.ChatCompletionMessage{}, openai.Usage{}, &transportError{Transport: "openrouter", Status: 402, Message: "credits"}
		})
	if err == nil || calls != roundMaxAttempts {
		t.Fatalf("err=%v calls=%d", err, calls)
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

func TestEmptyToolOutputSentAsText(t *testing.T) {
	out := chatSendMessages([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleTool, ToolCallID: "x", Content: ""}})
	if out[0].Content == "" {
		t.Fatal("empty tool output must be non-empty on the wire")
	}
}
