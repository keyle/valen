package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRegistryKeepsInsertionOrder(t *testing.T) {
	r := NewRegistry()
	r.LoadBuiltins([]string{"web_fetch", "shell", "web_search"})
	want := []string{"web_fetch", "shell", "web_search"}
	got := r.Names()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	for i, tool := range r.OpenAITools() {
		if tool.Function.Name != want[i] {
			t.Fatalf("order %d: %s", i, tool.Function.Name)
		}
	}
}

func TestRegistryReportsUnknownBuiltins(t *testing.T) {
	r := NewRegistry()
	if unknown := r.LoadBuiltins([]string{"shell", "nope"}); len(unknown) != 1 || unknown[0] != "nope" {
		t.Fatalf("unknown=%v", unknown)
	}
}

func TestRegistryRejectsBadNames(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"", "has space", "server__tool__too_long_" + strings.Repeat("x", 60)} {
		if err := r.Add(Tool{Name: name, Fn: func(context.Context, string) (string, error) { return "", nil }}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := r.Add(Tool{Name: "dup", Fn: func(context.Context, string) (string, error) { return "", nil }}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Tool{Name: "dup", Fn: func(context.Context, string) (string, error) { return "", nil }}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestRegistryRunHandlesErrorsAndTruncation(t *testing.T) {
	r := NewRegistry()
	r.Add(Tool{Name: "boom", Fn: func(context.Context, string) (string, error) { return "", errors.New("kaboom") }})
	if got := r.Run(context.Background(), "boom", "{}"); got != "error: kaboom" {
		t.Fatalf("got %q", got)
	}
	if got := r.Run(context.Background(), "missing", "{}"); !strings.HasPrefix(got, "error: unknown tool") {
		t.Fatalf("got %q", got)
	}

	r.Add(Tool{Name: "big", Fn: func(context.Context, string) (string, error) { return strings.Repeat("a", maxToolOutput+100), nil }})
	got := r.Run(context.Background(), "big", "")
	if len(got) > maxToolOutput+64 || !strings.Contains(got, "truncated 100 bytes") {
		t.Fatalf("len=%d tail=%q", len(got), got[len(got)-40:])
	}
}

func TestRegistryPassesEmptyArgsAsObject(t *testing.T) {
	r := NewRegistry()
	var seen string
	r.Add(Tool{Name: "noop", Fn: func(_ context.Context, args string) (string, error) { seen = args; return "", nil }})
	r.Run(context.Background(), "noop", "")
	if seen != "{}" {
		t.Fatalf("args=%q", seen)
	}
}

func TestShellTool(t *testing.T) {
	requireShellSandbox(t)
	shell := ShellTool()
	out, err := shell.Fn(context.Background(), `{"command":"echo hi"}`)
	if err != nil || out != "hi" {
		t.Fatalf("out=%q err=%v", out, err)
	}

	out, err = shell.Fn(context.Background(), `{"command":"echo oops >&2; exit 3"}`)
	if err != nil {
		t.Fatalf("non-zero exit should be content, got err=%v", err)
	}
	if !strings.Contains(out, "oops") || !strings.Contains(out, "[exit 3]") {
		t.Fatalf("out=%q", out)
	}

	if _, err := shell.Fn(context.Background(), `{"command":"  "}`); err == nil {
		t.Fatal("expected error for empty command")
	}
	if _, err := shell.Fn(context.Background(), `{`); err == nil {
		t.Fatal("expected error for malformed args")
	}
}

func TestShellTimeoutMarksPartial(t *testing.T) {
	requireShellSandbox(t)
	old := shellTimeout
	shellTimeout = 50 * time.Millisecond
	defer func() { shellTimeout = old }()

	shell := ShellTool()
	out, err := shell.Fn(context.Background(), `{"command":"echo part; sleep 5; echo never"}`)
	if err != nil {
		t.Fatalf("timeout should be content, got err=%v", err)
	}
	if !strings.Contains(out, "part") || strings.Contains(out, "never") || !strings.Contains(out, "timed out") {
		t.Fatalf("out=%q", out)
	}
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	got := truncate(strings.Repeat("é", maxToolOutput), maxToolOutput-1)
	if !strings.HasSuffix(got, "]") || strings.Contains(got, "\ufffd") {
		t.Fatalf("bad truncation: %q", got[len(got)-30:])
	}
}

func TestRegistryCanceledCallDoesNotRun(t *testing.T) {
	r := NewRegistry()
	ran := false
	_ = r.Add(Tool{Name: "write", Fn: func(context.Context, string) (string, error) {
		ran = true
		return "written", nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Call(ctx, "write", `{}`); !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
}

func TestShellOutputBoundedHeadAndTail(t *testing.T) {
	for _, chunk := range []int{1, 999, 8000, 16000, 100000} {
		var buf shellOutput
		input := "HEAD" + strings.Repeat("x", 100000) + "TAIL"
		for start := 0; start < len(input); start += chunk {
			part := input[start:min(start+chunk, len(input))]
			n, err := buf.Write([]byte(part))
			if err != nil || n != len(part) || len(buf.data) > maxToolOutput {
				t.Fatalf("chunk=%d n=%d err=%v size=%d", chunk, n, err, len(buf.data))
			}
		}
		if out := buf.String(); !strings.HasPrefix(out, "HEAD") || !strings.HasSuffix(out, "TAIL") || !strings.Contains(out, "truncated") {
			t.Fatalf("chunk=%d missing head/tail", chunk)
		}
	}
}

func TestShellLargeOutputKeepsExitStatus(t *testing.T) {
	requireShellSandbox(t)
	r := NewRegistry()
	r.LoadBuiltins([]string{"shell"})
	out := r.Run(context.Background(), "shell", `{"command":"yes x | dd bs=1000 count=40 2>/dev/null; echo FINAL_DIAGNOSTIC; exit 7"}`)
	if len(out) > maxToolOutput+100 || !strings.Contains(out, "FINAL_DIAGNOSTIC") || !strings.HasSuffix(out, "[exit 7]") {
		t.Fatalf("size=%d tail=%q", len(out), out[max(0, len(out)-100):])
	}
}
