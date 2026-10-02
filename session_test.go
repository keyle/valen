package main

import (
	"path/filepath"
	"testing"
	"time"
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
