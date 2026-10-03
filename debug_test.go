package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

// saveDebugGlobals snapshots the tracer globals so tests can restore them.
func saveDebugGlobals() (bool, io.Writer, *os.File, time.Time) {
	return debugVerbose, debugOut, debugFile, debugStart
}

func restoreDebugGlobals(v bool, w io.Writer, f *os.File, s time.Time) {
	debugVerbose = v
	if w != nil {
		debugOut = w
	}
	debugFile = f
	debugStart = s
}

func TestDlogDisabledNoOutput(t *testing.T) {
	v, w, f, s := saveDebugGlobals()
	defer restoreDebugGlobals(v, w, f, s)
	debugVerbose = false
	var b strings.Builder
	debugOut = &b
	dlogf("round %d should not appear", 1)
	if b.Len() != 0 {
		t.Fatalf("dlogf wrote while disabled: %q", b.String())
	}
}

func TestDlogEnabledFormat(t *testing.T) {
	v, w, f, s := saveDebugGlobals()
	defer restoreDebugGlobals(v, w, f, s)
	debugVerbose = true
	debugStart = time.Now()
	var b strings.Builder
	debugOut = &b
	dlogf("round %d chat ok", 3)
	line := b.String()
	if !strings.HasPrefix(line, "[dv ") || !strings.Contains(line, "round 3 chat ok") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("bad dv line: %q", line)
	}
}

func TestDvPreview(t *testing.T) {
	if got := dvPreview("a\nb\tc  d", 100); got != "a b c d" {
		t.Fatalf("flatten: %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := dvPreview(long, 160); len([]rune(got)) != 161 { // 160 + ellipsis
		t.Fatalf("truncate len=%d: %q", len([]rune(got)), got)
	}
}

func TestDvSendBytes(t *testing.T) {
	n, ok := dvSendBytes(map[string]string{"a": "b"})
	if !ok || n <= 0 {
		t.Fatalf("marshal ok=%v n=%d", ok, n)
	}
	if _, ok := dvSendBytes(func() {}); ok {
		t.Fatal("expected marshal failure for func value")
	}
}

func TestDvHistoryLine(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: "yo", ToolCalls: []openai.ToolCall{{ID: "1", Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "1", Content: "out"},
	}
	line := dvHistoryLine(h)
	for _, want := range []string{"msgs=4", "sys=1", "user=1", "asst=1", "tool=1", "est~"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestDvCallsLine(t *testing.T) {
	calls := []openai.ToolCall{
		{Function: openai.FunctionCall{Name: "shell", Arguments: "12345"}},
		{Function: openai.FunctionCall{Name: "read", Arguments: "12"}},
	}
	if got := dvCallsLine(calls); got != "shell(args=5B) read(args=2B)" {
		t.Fatalf("calls line: %q", got)
	}
	if got := dvCallsLine(nil); got != "" {
		t.Fatalf("empty calls line: %q", got)
	}
}

func TestDvTailDetailResolvesToolNames(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "c1", Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls -la"}`}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: "total 0"},
	}
	detail := dvTailDetail(h, 6)
	if !strings.Contains(detail, "->shell") || !strings.Contains(detail, "ls -la") {
		t.Fatalf("tail detail: %q", detail)
	}
	if got := dvTailDetail(nil, 6); !strings.Contains(got, "empty history") {
		t.Fatalf("empty tail: %q", got)
	}
}

func TestInitDebugVerboseFile(t *testing.T) {
	v, w, f, s := saveDebugGlobals()
	defer restoreDebugGlobals(v, w, f, s)
	if debugFile != nil {
		_ = debugFile.Close()
		debugFile = nil
	}
	path := filepath.Join(t.TempDir(), "dv.log")
	if err := initDebugVerbose(false, path); err != nil {
		t.Fatal(err)
	}
	if !debugVerbose {
		t.Fatal("dvlog should imply verbose")
	}
	dlogf("hello file")
	closeDebugLog()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hello file") {
		t.Fatalf("file log: %q", raw)
	}
	debugVerbose = false
}

func dvStubRegistry() *Registry {
	r := NewRegistry()
	_ = r.Add(Tool{Name: "dvprobe", Description: "probe", Fn: func(ctx context.Context, args string) (string, error) {
		return "probe-output:" + args, nil
	}})
	return r
}

func TestRunToolCallsSerialDv(t *testing.T) {
	v, w, f, s := saveDebugGlobals()
	defer restoreDebugGlobals(v, w, f, s)
	debugVerbose = true
	debugStart = time.Now()
	var b strings.Builder
	debugOut = &b
	reg := dvStubRegistry()
	calls := []openai.ToolCall{{ID: "1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "dvprobe", Arguments: `{"x":1}`}}}
	out := runToolCallsSerial(context.Background(), reg, calls, nil)
	if len(out) != 1 || !strings.HasPrefix(out[0].Content, "probe-output:") {
		t.Fatalf("tool result: %+v", out)
	}
	log := b.String()
	for _, want := range []string{"tool dvprobe", "batch 1 tools serial"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in %q", want, log)
		}
	}
}

func TestRunToolCallsParallelDv(t *testing.T) {
	v, w, f, s := saveDebugGlobals()
	defer restoreDebugGlobals(v, w, f, s)
	debugVerbose = true
	debugStart = time.Now()
	var b strings.Builder
	debugOut = &b
	reg := dvStubRegistry()
	calls := []openai.ToolCall{
		{ID: "1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "dvprobe", Arguments: `{"x":1}`}},
		{ID: "2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "dvprobe", Arguments: `{"x":2}`}},
	}
	out := runToolCalls(context.Background(), reg, calls, nil)
	if len(out) != 2 {
		t.Fatalf("parallel results: %+v", out)
	}
	log := b.String()
	if strings.Count(log, "tool dvprobe") != 2 || !strings.Contains(log, "batch 2 tools total=") {
		t.Fatalf("parallel log: %q", log)
	}
}
