package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func TestSessionAppendAndLoad(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	s, err := newSession(Config{SessionID: "ses-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(tmp, "valen", "sessions", "ses-test-1.jsonl"); s.Path != want {
		t.Fatalf("path %q want %q", s.Path, want)
	}
	turn := Turn{
		At:       time.Now().UTC().Truncate(time.Second),
		Prompt:   "hi",
		Reply:    "yo",
		Thinking: "hmm",
		Tools:    []ToolRecord{{Name: "shell", Args: `{"command":"ls"}`, Output: "ok"}},
		Usage:    Usage{Prompt: 3, Completion: 4, Total: 7},
	}
	if err := s.append(turn); err != nil {
		t.Fatal(err)
	}
	turns, err := loadSession(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Prompt != "hi" || turns[0].Thinking != "hmm" || len(turns[0].Tools) != 1 || turns[0].Tools[0].Name != "shell" {
		t.Fatalf("round trip mismatch: %+v", turns)
	}
	if s.TotalUsage.Total != 7 {
		t.Fatalf("usage not accumulated: %+v", s.TotalUsage)
	}
}

func TestSessionHistoryCheckpointRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s, err := newSession(Config{SessionID: "ses-checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "work"},
		{Role: openai.ChatMessageRoleAssistant, Content: "intermediate", ToolCalls: []openai.ToolCall{{ID: "call-1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read", Arguments: `{"path":"a.go"}`}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: "contents"},
	}
	if err := s.append(Turn{Prompt: "work", History: h}); err != nil {
		t.Fatal(err)
	}
	h[0].Content = "changed"
	if s.Turns[0].History[0].Content != "system" {
		t.Fatal("checkpoint aliases live history")
	}
	// Valid configured histories can encode to more than the old 1 MiB cap.
	h[3].Content = strings.Repeat("x", 1024*1024+1)
	if err := s.append(Turn{Prompt: "more", History: h}); err != nil {
		t.Fatal(err)
	}
	if err := s.append(Turn{Prompt: "legacy", Reply: "reply"}); err != nil {
		t.Fatal(err)
	}
	if s.Turns[0].History != nil || len(s.Turns[1].History) == 0 {
		t.Fatal("memory must retain only the latest checkpoint")
	}
	turns, err := loadSession(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[0].History != nil || !reflect.DeepEqual(turns[1].History, h) || turns[2].History != nil {
		t.Fatalf("checkpoint/legacy round trip mismatch: %d turns", len(turns))
	}
	// Releasing older checkpoints in memory must not change the disk log.
	f, err := os.Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var first Turn
	if err := json.NewDecoder(f).Decode(&first); err != nil || first.History[0].Content != "system" {
		t.Fatalf("first disk checkpoint lost: %v", err)
	}
	b, err := json.Marshal(Turn{Prompt: "legacy"})
	if err != nil || strings.Contains(string(b), `"history"`) {
		t.Fatalf("legacy turns must omit history: %s %v", b, err)
	}
}

func TestSessionSaveFailureRetainsWorkingState(t *testing.T) {
	s := &Session{Path: t.TempDir()}
	h := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "work"}}
	if err := s.append(Turn{Prompt: "work", History: h, Usage: Usage{Total: 3}}); err == nil {
		t.Fatal("saving to a directory must fail")
	}
	if len(s.Turns) != 1 || len(s.Turns[0].History) != 1 || s.TotalUsage.Total != 3 {
		t.Fatal("failed disk save discarded working state")
	}
}

func TestListSessionsNewestFirst(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	s1, _ := newSession(Config{SessionID: "ses-a"})
	s2, _ := newSession(Config{SessionID: "ses-b"})
	s1.append(Turn{At: time.Now(), Prompt: "a"})
	time.Sleep(10 * time.Millisecond)
	s2.append(Turn{At: time.Now(), Prompt: "b"})
	paths, err := listSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("want 2 sessions, got %v", paths)
	}
	if filepath.Base(paths[0]) != "ses-b.jsonl" {
		t.Fatalf("newest first violated: %v", paths)
	}
}
