package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionPathFromStderr finds the "[session <path>]" line a headless run
// prints after saving its turn.
func sessionPathFromStderr(t *testing.T, stderr string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "[session ") && strings.HasSuffix(line, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "[session "), "]")
		}
	}
	t.Fatalf("no [session <path>] line in stderr: %q", stderr)
	return ""
}

// sessionFiles lists the saved JSONL logs under the test's data dir. The
// child runs in another directory, so sessionsDir() would hash a different
// root; the folder layout is enough here.
func sessionFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(os.Getenv("XDG_DATA_HOME"), "valen", "sessions")
	paths, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// Plain -p keeps stdout as the answer and reports the session JSONL on
// stderr, so an external driver can continue the conversation.
func TestHeadlessReportsSessionPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	var bodies []string
	srv := sseServer(t, &bodies, chatTextStream("saved answer"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	stdout, stderr, err := runValenCLIFull(t, dir, "-p", "-config", cfgPath, "remember me")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout); got != "saved answer" {
		t.Fatalf("stdout=%q, want only the answer", got)
	}
	path := sessionPathFromStderr(t, stderr)
	if !filepath.IsAbs(path) {
		t.Fatalf("session path %q is not absolute", path)
	}
	turns, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Prompt != "remember me" {
		t.Fatalf("turns=%+v", turns)
	}
	if files := sessionFiles(t); len(files) != 1 || files[0] != path {
		t.Fatalf("session files=%v want [%s]", files, path)
	}
}

// A failed turn still reports the saved session, so the caller can resume and
// retry instead of losing the prompt.
func TestHeadlessFailureReportsSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	_, stderr, err := runValenCLIFull(t, t.TempDir(), "-p", "-config", cfgPath, "try once")
	if err == nil {
		t.Fatal("want a failed turn")
	}
	path := sessionPathFromStderr(t, stderr)
	turns, lerr := loadSession(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(turns) != 1 || turns[0].Error == nil || turns[0].Prompt != "try once" {
		t.Fatalf("turns=%+v, want the failed turn on disk", turns)
	}
}

// --stream stays pure NDJSON on stdout and carries the session in done.
func TestStreamDoneCarriesSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	var bodies []string
	srv := sseServer(t, &bodies, chatTextStream("streamed answer"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	stdout, stderr, err := runValenCLIFull(t, dir, "-p", "--stream", "-config", cfgPath, "stream it")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var done struct {
		Kind    string `json:"kind"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &done); err != nil {
		t.Fatalf("done line: %v: %s", err, lines[len(lines)-1])
	}
	if done.Kind != "done" || done.Session == "" {
		t.Fatalf("done=%+v", done)
	}
	if strings.Contains(stderr, "[session ") {
		t.Fatalf("stream mode should carry the path in done, stderr=%q", stderr)
	}
	if _, err := os.Stat(done.Session); err != nil {
		t.Fatalf("done.session %q: %v", done.Session, err)
	}
}

// -r appends to the same JSONL, keeps the title, and sends the earlier
// conversation to the model before the new prompt.
func TestResumeContinuesSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	record, take := cliBodies(t)
	srv := sseServerWith(t, record, chatTextStream("first answer"), chatTextStream("second answer"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	_, stderr, err := runValenCLIFull(t, dir, "-p", "-config", cfgPath, "first question")
	if err != nil {
		t.Fatal(err)
	}
	path := sessionPathFromStderr(t, stderr)

	stdout, _, err := runValenCLIFull(t, dir, "-p", "-r", path, "-config", cfgPath, "second question")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout); got != "second answer" {
		t.Fatalf("stdout=%q", got)
	}
	if files := sessionFiles(t); len(files) != 1 || files[0] != path {
		t.Fatalf("resume must append to the same file, files=%v want [%s]", files, path)
	}
	turns, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Prompt != "first question" || turns[1].Prompt != "second question" {
		t.Fatalf("turns=%+v", turns)
	}
	meta, err := loadSessionMeta(metaPath(path))
	if err != nil || meta.Title != "first question" {
		t.Fatalf("title=%q err=%v, want the first prompt kept", meta.Title, err)
	}
	take() // first run's request
	req := parseWireRequest(t, take())
	if len(req.Messages) < 4 {
		t.Fatalf("messages=%d, want the prior conversation plus the new prompt", len(req.Messages))
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || last.Content != "second question" {
		t.Fatalf("last message=%+v", last)
	}
	var contents []string
	for _, m := range req.Messages {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "first question") || !strings.Contains(joined, "first answer") {
		t.Fatalf("resumed request lost the prior turn: %q", joined)
	}
}

// A bare session ID resolves in the project's session folder.
func TestResumeByID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	var bodies []string
	srv := sseServer(t, &bodies, chatTextStream("one"), chatTextStream("two"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	_, stderr, err := runValenCLIFull(t, dir, "-p", "-config", cfgPath, "hello")
	if err != nil {
		t.Fatal(err)
	}
	path := sessionPathFromStderr(t, stderr)
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	if _, _, err := runValenCLIFull(t, dir, "-p", "--resume", id, "-config", cfgPath, "again"); err != nil {
		t.Fatal(err)
	}
	turns, err := loadSession(path)
	if err != nil || len(turns) != 2 {
		t.Fatalf("turns=%d err=%v, want 2", len(turns), err)
	}
}

// -r and --history are ambiguous and rejected before any request.
func TestResumeRejectsHistory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	history := filepath.Join(dir, "history.json")
	if err := os.WriteFile(history, []byte(`[{"role":"user","content":"old"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	requested := make(chan string, 1)
	srv := sseServerWith(t, func(raw string) { requested <- raw }, chatTextStream("unused"))
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	_, _, err := runValenCLIFull(t, dir, "-p", "-r", "ses-x", "--history", history, "-config", cfgPath, "hi")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("err=%v, want the combination error", err)
	}
	select {
	case raw := <-requested:
		t.Fatalf("request made before validation: %s", raw)
	default:
	}
}

// A resumed run sends the same x-opencode-session as the original turn.
func TestResumeKeepsSessionID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	ids := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids <- r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chatTextStream("ok"))
	}))
	defer srv.Close()
	cfgPath := writeConfig(t, fmt.Sprintf("model = \"kimi-k3\"\nbase_url = %q\n", srv.URL))

	_, stderr, err := runValenCLIFull(t, dir, "-p", "-config", cfgPath, "first")
	if err != nil {
		t.Fatal(err)
	}
	path := sessionPathFromStderr(t, stderr)
	if _, _, err := runValenCLIFull(t, dir, "-p", "-r", path, "-config", cfgPath, "second"); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if first, second := <-ids, <-ids; first != want || second != want {
		t.Fatalf("session headers=%q,%q want %q on both runs", first, second, want)
	}
}
