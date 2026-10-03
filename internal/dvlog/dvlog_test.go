package dvlog

import (
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// withCapture redirects the trace to a builder for one test.
func withCapture(t *testing.T, on bool) *strings.Builder {
	t.Helper()
	var b strings.Builder
	prev := SetOutput(&b)
	was := Enabled()
	Enable(on)
	t.Cleanup(func() {
		Enable(was)
		SetOutput(prev)
	})
	return &b
}

func TestLogDisabledNoOutput(t *testing.T) {
	b := withCapture(t, false)
	Logf("round %d should not appear", 1)
	if b.Len() != 0 {
		t.Fatalf("Logf wrote while disabled: %q", b.String())
	}
}

func TestLogEnabledFormat(t *testing.T) {
	b := withCapture(t, true)
	Logf("round %d chat ok", 3)
	line := b.String()
	if !strings.HasPrefix(line, "[dv ") || !strings.Contains(line, "round 3 chat ok") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("bad dv line: %q", line)
	}
}

func TestPreview(t *testing.T) {
	if got := Preview("a\nb\tc  d", 100); got != "a b c d" {
		t.Fatalf("flatten: %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := Preview(long, 160); len([]rune(got)) != 161 { // 160 + ellipsis
		t.Fatalf("truncate len=%d: %q", len([]rune(got)), got)
	}
}

func TestSendBytes(t *testing.T) {
	n, ok := SendBytes(map[string]string{"a": "b"})
	if !ok || n <= 0 {
		t.Fatalf("marshal ok=%v n=%d", ok, n)
	}
	if _, ok := SendBytes(func() {}); ok {
		t.Fatal("expected marshal failure for func value")
	}
}

func TestHistoryLine(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: "yo", ToolCalls: []openai.ToolCall{{ID: "1", Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "1", Content: "out"},
	}
	line := HistoryLine(h, 100)
	for _, want := range []string{"msgs=4", "sys=1", "user=1", "asst=1", "tool=1", "est~100tok"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestCallsLine(t *testing.T) {
	calls := []openai.ToolCall{
		{Function: openai.FunctionCall{Name: "shell", Arguments: "12345"}},
		{Function: openai.FunctionCall{Name: "read", Arguments: "12"}},
	}
	if got := CallsLine(calls); got != "shell(args=5B) read(args=2B)" {
		t.Fatalf("calls line: %q", got)
	}
	if got := CallsLine(nil); got != "" {
		t.Fatalf("empty calls line: %q", got)
	}
}

func TestTailDetailResolvesToolNames(t *testing.T) {
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "c1", Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls -la"}`}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: "total 0"},
	}
	detail := TailDetail(h, 6)
	if !strings.Contains(detail, "->shell") || !strings.Contains(detail, "ls -la") {
		t.Fatalf("tail detail: %q", detail)
	}
	if got := TailDetail(nil, 6); !strings.Contains(got, "empty history") {
		t.Fatalf("empty tail: %q", got)
	}
}
