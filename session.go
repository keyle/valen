package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// ToolRecord is one tool invocation within a turn, as the model saw it.
type ToolRecord struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Args   string `json:"args"`
	Output string `json:"output"`
	Err    string `json:"err,omitempty"`
	// DurMs is wall execution time of this call, shown as e.g. "shell (1.2s)".
	DurMs int64 `json:"dur_ms,omitempty"`
}

type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// Turn is one user→assistant exchange, including tool work and any failure.
type Turn struct {
	At       time.Time                      `json:"at"`
	Prompt   string                         `json:"prompt"`
	Thinking string                         `json:"thinking,omitempty"`
	Reply    string                         `json:"reply"`
	Tools    []ToolRecord                   `json:"tools,omitempty"`
	Usage    Usage                          `json:"usage"`
	Error    *FriendlyError                 `json:"error,omitempty"`
	Model    string                         `json:"model,omitempty"`
	Effort   string                         `json:"effort,omitempty"`
	History  []openai.ChatCompletionMessage `json:"history,omitempty"`
	// DurMs is wall time from submit to commit, shown as e.g. "✦ thinking (1m3s)".
	DurMs int64 `json:"dur_ms,omitempty"`
}

// SessionMeta is the sidecar alongside the append-only JSONL: title from the
// first prompt plus the model/thinking level in use at the time.
type SessionMeta struct {
	Title  string `json:"title,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// Session accumulates turns and appends them to a JSONL log on disk.
type Session struct {
	ID         string
	Path       string
	Turns      []Turn
	TotalUsage Usage
	Title      string
	Model      string
	Effort     string
}

func sessionsDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "valen", "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "valen", "sessions"), nil
}

func newSession(cfg Config) (*Session, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Session{ID: cfg.SessionID, Path: filepath.Join(dir, cfg.SessionID+".jsonl")}
	if meta, err := loadSessionMeta(metaPath(s.Path)); err == nil {
		s.Title, s.Model, s.Effort = meta.Title, meta.Model, meta.Effort
	}
	return s, nil
}

func metaPath(jsonlPath string) string {
	return jsonlPath + ".meta.json"
}

func loadSessionMeta(path string) (SessionMeta, error) {
	var meta SessionMeta
	b, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func saveSessionMeta(path string, meta SessionMeta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// sessionTitle truncates the first prompt to 60 runes, single-line.
func sessionTitle(prompt string) string {
	t := prompt
	for _, sep := range []string{"\n", "\r"} {
		if j := strings.Index(t, sep); j >= 0 {
			t = t[:j]
		}
	}
	r := []rune(strings.TrimSpace(t))
	if len(r) > 60 {
		r = r[:60]
	}
	return string(r)
}

func (s *Session) ensureMeta(model, effort string) {
	if s == nil {
		return
	}
	if len(s.Turns) > 0 && s.Title != "" {
		return
	}
	prompt := ""
	if len(s.Turns) > 0 {
		prompt = s.Turns[len(s.Turns)-1].Prompt
	}
	if s.Title == "" && prompt != "" {
		s.Title = sessionTitle(prompt)
	}
	if s.Model == "" {
		s.Model = model
	}
	if s.Effort == "" {
		s.Effort = effort
	}
	_ = saveSessionMeta(metaPath(s.Path), SessionMeta{Title: s.Title, Model: s.Model, Effort: s.Effort})
}

func (s *Session) append(t Turn) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	// Disk keeps every checkpoint; memory needs only the latest one. Copy the
	// slice because the UI reseeds and prunes message contents in place.
	if len(t.History) > 0 {
		for i := len(s.Turns) - 1; i >= 0; i-- {
			if len(s.Turns[i].History) > 0 {
				s.Turns[i].History = nil
				break
			}
		}
		t.History = append([]openai.ChatCompletionMessage(nil), t.History...)
	}
	// Keep completed work visible even if saving it fails.
	s.Turns = append(s.Turns, t)
	s.TotalUsage.Prompt += t.Usage.Prompt
	s.TotalUsage.Completion += t.Usage.Completion
	s.TotalUsage.Total += t.Usage.Total
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func loadSession(path string) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var turns []Turn
	// A bounded continuation checkpoint can exceed Scanner's old 1 MiB line
	// limit. Decode one JSONL record at a time without imposing a smaller cap.
	dec := json.NewDecoder(f)
	checkpoint := -1
	for {
		var t Turn
		if err := dec.Decode(&t); err == io.EOF {
			return turns, nil
		} else if err != nil {
			return nil, err
		}
		if len(t.History) > 0 {
			if checkpoint >= 0 {
				turns[checkpoint].History = nil
			}
			checkpoint = len(turns)
		}
		turns = append(turns, t)
	}
}

// listSessions returns session JSONL paths, newest first.
func listSessions() ([]string, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	type fi struct {
		path string
		mod  time.Time
	}
	var files []fi
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		files = append(files, fi{p, info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.path)
	}
	return out, nil
}
