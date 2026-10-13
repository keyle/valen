package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keyle/valen/internal/dvlog"
	"github.com/sashabaranov/go-openai"
)

// withDvLog redirects the trace for one test and restores it after.
func withDvLog(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	prev := dvlog.SetOutput(&b)
	was := dvlog.Enabled()
	dvlog.Enable(true)
	t.Cleanup(func() {
		dvlog.Enable(was)
		dvlog.SetOutput(prev)
	})
	return &b
}

func TestInitDvlogFile(t *testing.T) {
	prev := dvlog.SetOutput(os.Stderr)
	was := dvlog.Enabled()
	defer func() {
		dvlog.Enable(was)
		dvlog.SetOutput(prev)
	}()
	dvlog.Close()
	path := filepath.Join(t.TempDir(), "dv.log")
	if err := dvlog.Init(false, path); err != nil {
		t.Fatal(err)
	}
	if !dvlog.Enabled() {
		t.Fatal("dvlog should imply verbose")
	}
	dvlog.Logf("hello file")
	dvlog.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hello file") {
		t.Fatalf("file log: %q", raw)
	}
}

func dvStubRegistry() *Registry {
	r := NewRegistry()
	_ = r.Add(Tool{Name: "dvprobe", Description: "probe", Fn: func(ctx context.Context, args string) (string, error) {
		return "probe-output:" + args, nil
	}})
	return r
}

func TestRunToolCallsSerialDv(t *testing.T) {
	b := withDvLog(t)
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
	b := withDvLog(t)
	reg := NewRegistry()
	_ = reg.Add(Tool{Name: "read", Fn: func(context.Context, string) (string, error) { return "probe-output", nil }})
	calls := []openai.ToolCall{
		{ID: "1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read", Arguments: `{"x":1}`}},
		{ID: "2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read", Arguments: `{"x":2}`}},
	}
	out := runToolCalls(context.Background(), reg, calls, nil)
	if len(out) != 2 {
		t.Fatalf("parallel results: %+v", out)
	}
	log := b.String()
	if strings.Count(log, "tool read") != 2 || !strings.Contains(log, "batch 2 tools total=") {
		t.Fatalf("parallel log: %q", log)
	}
}
