package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// Steer is a message typed into a turn that had already started. It is kept
// with the turn so the transcript and a resumed session show the whole
// conversation, not just the opening prompt.
type Steer struct {
	Text string `json:"text"`
	Plan bool   `json:"plan,omitempty"`
}

// Turn is one user→assistant exchange, including tool work and any failure.
type Turn struct {
	At       time.Time                      `json:"at"`
	Prompt   string                         `json:"prompt"`
	Thinking string                         `json:"thinking,omitempty"`
	Reply    string                         `json:"reply"`
	Tools    []ToolRecord                   `json:"tools,omitempty"`
	Steers   []Steer                        `json:"steers,omitempty"`
	Usage    Usage                          `json:"usage"`
	Error    *FriendlyError                 `json:"error,omitempty"`
	Model    string                         `json:"model,omitempty"`
	Effort   string                         `json:"effort,omitempty"`
	Plan     bool                           `json:"plan,omitempty"`
	History  []openai.ChatCompletionMessage `json:"history,omitempty"`
	// DurMs is wall time from submit to commit, shown as e.g. "✦ thinking (1m3s)".
	DurMs int64 `json:"dur_ms,omitempty"`
	// ORMeta is the last OpenRouter round's serving metadata, kept so a
	// resumed session can restore the footer exactly as it was after the turn.
	ORMeta *ORMeta `json:"or_meta,omitempty"`
	// Cost is the billed cost summed across every request in this turn.
	Cost float64 `json:"cost,omitempty"`
	// InPrice/OutPrice are the model's per-token catalog rates at turn time,
	// stored so the footer's $/M rates survive a restart without a refetch.
	InPrice  float64 `json:"in_price,omitempty"`
	OutPrice float64 `json:"out_price,omitempty"`
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
	// Keep bounds this project folder's saved sessions; the first append of
	// a brand-new file prunes the oldest so the budget including it holds.
	Keep int
}

// sessionsDir is the per-project session folder: sessions/<base>-<hash of the
// working folder>, so projects never share a session list. Flat legacy
// sessions/*.jsonl files are simply no longer listed.
func sessionsDir() (string, error) {
	base := filepath.Join(os.Getenv("XDG_DATA_HOME"), "valen", "sessions")
	if os.Getenv("XDG_DATA_HOME") == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share", "valen", "sessions")
	}
	root := filepath.Clean(getFileRoot())
	sum := sha256.Sum256([]byte(root))
	name := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, filepath.Base(root))
	return filepath.Join(base, name+"-"+hex.EncodeToString(sum[:4])), nil
}

func newSession(cfg Config) (*Session, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	_ = pruneSessions(savedSessions(cfg))
	s := &Session{ID: cfg.SessionID, Path: filepath.Join(dir, cfg.SessionID+".jsonl"), Keep: savedSessions(cfg)}
	if meta, err := loadSessionMeta(metaPath(s.Path)); err == nil {
		s.Title, s.Model, s.Effort = meta.Title, meta.Model, meta.Effort
	}
	return s, nil
}

func metaPath(jsonlPath string) string {
	return jsonlPath + ".meta.json"
}

// resolveSessionArg maps -r/--resume to a JSONL path: an absolute path or an
// existing file wins, anything else names a session in this project's session
// folder. A miss names both paths tried.
func resolveSessionArg(arg string) (string, error) {
	if filepath.IsAbs(arg) {
		return arg, nil
	}
	if abs, err := filepath.Abs(arg); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
	}
	dir, err := sessionsDir()
	if err != nil {
		return "", err
	}
	name := arg
	if !strings.HasSuffix(name, ".jsonl") {
		name += ".jsonl"
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		abs, _ := filepath.Abs(arg)
		return "", fmt.Errorf("resume %q: no session at %s or %s", arg, abs, path)
	}
	return path, nil
}

// openSession loads a saved JSONL for -r/--resume without creating, titling or
// pruning anything. The ID comes from the file name, so the resumed run keeps
// the same x-opencode-session.
func openSession(path string) (*Session, error) {
	turns, err := loadSession(path)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Turns: turns}
	for _, t := range turns {
		s.TotalUsage.Prompt += t.Usage.Prompt
		s.TotalUsage.Completion += t.Usage.Completion
		s.TotalUsage.Total += t.Usage.Total
	}
	if meta, err := loadSessionMeta(metaPath(path)); err == nil {
		s.Title, s.Model, s.Effort = meta.Title, meta.Model, meta.Effort
	}
	return s, nil
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
	if s.Title == "" {
		prompt := ""
		if len(s.Turns) > 0 {
			prompt = s.Turns[len(s.Turns)-1].Prompt
		}
		if prompt != "" {
			s.Title = sessionTitle(prompt)
		}
	}
	if s.Model == "" {
		s.Model = model
	}
	if s.Effort == "" {
		s.Effort = effort
	}
	_ = saveSessionMeta(metaPath(s.Path), SessionMeta{Title: s.Title, Model: s.Model, Effort: s.Effort})
}

// nameSession titles an unnamed session from its first submitted prompt so the
// header and Sessions view show a real name while the first turn is still
// running. It never overwrites an existing title.
func (s *Session) nameSession(prompt string) {
	if s == nil || strings.TrimSpace(s.Title) != "" {
		return
	}
	title := sessionTitle(prompt)
	if title == "" {
		return
	}
	s.Title = title
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
	// A brand-new session file adds one to the folder's count, so prune
	// before creating it and keep the configured budget including it. Blank
	// /new sessions never touch disk and so never prune.
	if s.Keep > 0 {
		if _, err := os.Stat(s.Path); os.IsNotExist(err) {
			_ = pruneSessions(s.Keep - 1)
		}
	}
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

// historyFromTurns rebuilds the wire history for a resumed session: the newest
// checkpoint, then the turns after it replayed as prompt, tool calls/results
// and reply. The system message is refreshed from the current config, so a
// resumed run never carries a stale prompt.
func historyFromTurns(cfg Config, turns []Turn) []openai.ChatCompletionMessage {
	history := seedHistory(cfg)
	start := 0
	for i := len(turns) - 1; i >= 0; i-- {
		if len(turns[i].History) > 0 {
			history = append([]openai.ChatCompletionMessage(nil), turns[i].History...)
			start = i + 1
			break
		}
	}
	for i := start; i < len(turns); i++ {
		t := turns[i]
		history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: t.Prompt})
		for j, tr := range t.Tools {
			id := tr.ID
			if id == "" {
				id = fmt.Sprintf("legacy-%d-%d", i, j)
			}
			history = append(history,
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: tr.Name, Arguments: tr.Args}}}},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: tr.Output})
		}
		if t.Reply != "" {
			history = append(history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: t.Reply})
		}
	}
	if len(history) > 0 && history[0].Role == openai.ChatMessageRoleSystem {
		history[0].Content = effectiveSystem(cfg)
	}
	return history
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

// pruneSessions deletes all but the newest keep session logs (and their
// sidecars) in the current project folder.
func pruneSessions(keep int) error {
	if keep <= 0 {
		return nil
	}
	paths, err := listSessions()
	if err != nil {
		return err
	}
	if len(paths) <= keep {
		return nil
	}
	for _, p := range paths[keep:] {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(metaPath(p))
	}
	return nil
}

// deleteSessionFiles removes one session log and its metadata sidecar.
func deleteSessionFiles(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(metaPath(path))
	return nil
}
