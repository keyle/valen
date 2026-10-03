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
