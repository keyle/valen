package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func verboseSample(v *headlessVerbose) {
	v.event(Event{Kind: "round_start", Text: "Round 1/8"})
	v.event(Event{Kind: "thinking_delta", Text: "summary plan", Index: 0})
	v.event(Event{Kind: "thinking_delta", Text: "raw analysis", Index: 0, Raw: true})
	v.event(Event{Kind: "text_delta", Text: "answer"})
	v.event(Event{Kind: "tool_start", Text: "read", Args: `{"path":"x"}`})
	v.event(Event{Kind: "tool_result", Text: "read", Output: "body", DurMs: 5})
	v.closeLine()
}

func TestVerboseHidesRawAnalysis(t *testing.T) {
	var out bytes.Buffer
	verboseSample(newHeadlessVerbose(&out, 1))
	got := out.String()
	for _, want := range []string{"[Round 1/8]", "[thinking] summary plan", "[tool] read:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, no := range []string{"raw analysis", "[answer]", "body", "[result]"} {
		if strings.Contains(got, no) {
			t.Fatalf("unexpected %q in:\n%s", no, got)
		}
	}
}

func TestVeryVerboseShowsEverything(t *testing.T) {
	var out bytes.Buffer
	verboseSample(newHeadlessVerbose(&out, 2))
	got := out.String()
	for _, want := range []string{
		"[thinking] summary plan", "[thinking] raw analysis", "[answer] answer",
		"[tool] read {\"path\":\"x\"}", "[result] read 5ms", "body",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestChatVerboseStreamsThinking(t *testing.T) {
	const stream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"weigh \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"options\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	for _, tc := range []struct {
		level      int
		want, omit []string
	}{
		{1, []string{"[thinking] weigh options", "[tokens in="}, []string{"[answer] done"}},
		{2, []string{"[thinking] weigh options", "[answer] done", "[tokens in="}, nil},
	} {
		var bodies []string
		srv := sseServer(t, &bodies, stream)
		cfg := newTestConfig(srv.URL)
		var stdout, stderr bytes.Buffer
		err := chatWithAttachments(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), NewRegistry(), "hi", nil, tc.level, &stdout, &stderr)
		if err != nil {
			t.Fatalf("level %d: %v", tc.level, err)
		}
		if !strings.Contains(stdout.String(), "done") {
			t.Fatalf("level %d stdout=%q", tc.level, stdout.String())
		}
		for _, want := range tc.want {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("level %d missing %q in:\n%s", tc.level, want, stderr.String())
			}
		}
		for _, no := range tc.omit {
			if strings.Contains(stderr.String(), no) {
				t.Fatalf("level %d unexpected %q in:\n%s", tc.level, no, stderr.String())
			}
		}
	}
}

func TestVerboseFallbackThinking(t *testing.T) {
	var quiet bytes.Buffer
	newHeadlessVerbose(&quiet, 1).fallbackThinking("committed thought")
	if !strings.Contains(quiet.String(), "[thinking] committed thought") {
		t.Fatalf("fallback missing: %q", quiet.String())
	}

	var raw bytes.Buffer
	v := newHeadlessVerbose(&raw, 1)
	v.event(Event{Kind: "thinking_delta", Text: "raw", Raw: true})
	v.fallbackThinking("raw")
	if strings.Contains(raw.String(), "raw") {
		t.Fatalf("raw-only thinking must stay hidden at -v: %q", raw.String())
	}
}
