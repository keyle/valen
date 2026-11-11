package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keyle/valen/internal/prompts"
	"github.com/sashabaranov/go-openai"
)

func TestNoQuestionRuleAddedOnce(t *testing.T) {
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "base"}}
	got := withNoQuestionRule(history)
	if !strings.Contains(got[0].Content, prompts.NoQuestionRule) || !strings.Contains(got[0].Content, "totally blocked") {
		t.Fatalf("system prompt missing the no-question rule: %q", got[0].Content)
	}
	got = withNoQuestionRule(got)
	if n := strings.Count(got[0].Content, "# No-question mode"); n != 1 {
		t.Fatalf("rule appended %d times, want once", n)
	}
	// No system message yet: the rule becomes one instead of being dropped.
	got = withNoQuestionRule([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}})
	if len(got) != 2 || got[0].Role != openai.ChatMessageRoleSystem || !strings.Contains(got[0].Content, prompts.NoQuestionRule) {
		t.Fatalf("missing prepended system rule: %#v", got)
	}
}

func TestExtraDirRootsGrantReadWrite(t *testing.T) {
	root := useTempRoot(t)
	extra := real(t.TempDir())
	setExtraRoots([]string{extra})
	t.Cleanup(func() { setExtraRoots(nil) })

	file := filepath.Join(extra, "x.txt")
	if !allowedWithoutPrompt(root, file) || !allowedWriteWithoutPrompt(root, file) {
		t.Fatal("an extra -dir root must allow reads and writes")
	}
	if err := checkFileAccess(context.Background(), "write", file); err != nil {
		t.Fatalf("write inside extra root: %v", err)
	}
	if !contains(writableRoots(), extra) {
		t.Fatalf("extra root missing from the sandbox policy: %v", writableRoots())
	}
	if err := gateShell(context.Background(), "echo hi > "+file, false); err != nil {
		t.Fatalf("shell write inside extra root: %v", err)
	}
}

func TestRunCapabilityGrants(t *testing.T) {
	useTempRoot(t)
	t.Cleanup(func() { setRunCapabilities(false, false) })

	setRunCapabilities(true, false)
	if err := approveShell(context.Background(), "network", "curl example.com"); err != nil {
		t.Fatalf("--network must pre-approve network: %v", err)
	}
	if err := approveShell(context.Background(), "sudo", "sudo ls"); err == nil {
		t.Fatal("--network must not pre-approve destructive commands")
	}

	setRunCapabilities(false, true)
	if err := approveShell(context.Background(), "sudo", "sudo ls"); err != nil {
		t.Fatalf("--exec must pre-approve destructive commands: %v", err)
	}
	if err := approveShell(context.Background(), "network", "curl example.com"); err == nil {
		t.Fatal("--exec must not grant network")
	}

	// A persistent deny still wins over both flags.
	useAccessFile(t)
	if err := saveAccessRule(ruleShellDeny, "sudo"); err != nil {
		t.Fatal(err)
	}
	if err := approveShell(context.Background(), "sudo", "sudo ls"); err == nil {
		t.Fatal("persistent deny must beat --exec")
	}
}

func TestSandboxDenialIsClearAndStructured(t *testing.T) {
	useTempRoot(t)
	outside := filepath.Join(real(t.TempDir()), "x.txt")
	err := checkFileAccess(context.Background(), "write", outside)
	if err == nil {
		t.Fatal("outside write without a prompter must be denied")
	}
	msg := err.Error()
	for _, want := range []string{"[sandbox: write", "no interactive prompter", "-dir"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("denial %q missing %q", msg, want)
		}
	}
	d := parseSandboxDenial(msg)
	if d == nil || d.Capability != "write" || d.Subject != outside {
		t.Fatalf("parsed denial=%+v, want write %q", d, outside)
	}

	hint := sandboxHint("rm /x/y.txt")
	if d := parseSandboxDenial(hint); d == nil || d.Capability != "write" || d.Subject != "rm /x/y.txt" {
		t.Fatalf("shell hint denial=%+v (%q)", d, hint)
	}
	if d := parseSandboxDenial(networkHint("curl example.com")); d == nil || d.Capability != "network" || d.Subject != "curl example.com" {
		t.Fatalf("network hint denial=%+v", d)
	}
}

func TestToolResultEventCarriesDenial(t *testing.T) {
	reg := NewRegistry()
	err := reg.Add(Tool{Name: "read", Fn: func(context.Context, string) (string, error) {
		return "", errors.New(denialText("read", "/x/secret", "outside every allowed root", "re-run with -dir /x"))
	}})
	if err != nil {
		t.Fatal(err)
	}
	var denied *Denial
	calls := []openai.ToolCall{{ID: "c1", Function: openai.FunctionCall{Name: "read", Arguments: "{}"}}}
	runToolCallsSerial(context.Background(), reg, calls, func(ev Event) {
		if ev.Kind == "tool_result" {
			denied = ev.Denied
		}
	})
	if denied == nil || denied.Capability != "read" || denied.Subject != "/x/secret" {
		t.Fatalf("tool_result denied=%+v", denied)
	}
}
