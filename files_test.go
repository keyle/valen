package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// requireShellSandbox skips tests that run commands through the real shell
// sandbox when the backend can't start here, e.g. when `go test` itself runs
// inside another Seatbelt sandbox (nesting fails with "sandbox_apply:
// Operation not permitted"). Any other probe failure is a real bug, so the
// test runs and fails on its own instead of being hidden by a skip.
func requireShellSandbox(t *testing.T) {
	t.Helper()
	if reason := shellSandboxUnavailable(); reason != "" {
		t.Skipf("shell sandbox backend unavailable here: %s", reason)
	}
}

var shellSandboxUnavailable = sync.OnceValue(func() string {
	cmd, err := sandboxCommand(context.Background(), "true", false)
	if err != nil {
		return err.Error()
	}
	out, err := cmd.CombinedOutput()
	if err != nil && looksSandboxDenied(string(out)) {
		return strings.TrimSpace(string(out))
	}
	return ""
})

func useTempRoot(t *testing.T) string {
	t.Helper()
	oldPrompter := askPrompter
	askPrompter = nil
	dir := t.TempDir()
	setFileRoot(dir)
	t.Cleanup(func() {
		askPrompter = oldPrompter
		setFileRoot("")
	})
	return dir
}

func stubPrompter(answer string, err error) func() {
	old := askPrompter
	askPrompter = func(context.Context, []AskQuestion) ([]string, error) {
		return []string{answer}, err
	}
	return func() { askPrompter = old }
}

func TestSplitWorkdirArg(t *testing.T) {
	dir := t.TempDir()
	if w, rest := splitWorkdirArg(dir, []string{"hi"}, false); w != dir || len(rest) != 1 {
		t.Fatalf("flag wins: w=%q rest=%v", w, rest)
	}
	if w, rest := splitWorkdirArg("", []string{dir}, true); w != dir || len(rest) != 0 {
		t.Fatalf("positional dir in TUI: w=%q rest=%v", w, rest)
	}
	if w, rest := splitWorkdirArg("", []string{dir}, false); w != "" || len(rest) != 1 {
		t.Fatalf("headless keeps prompt: w=%q rest=%v", w, rest)
	}
	if w, rest := splitWorkdirArg("", []string{"not-a-dir"}, true); w != "" || len(rest) != 1 {
		t.Fatalf("non-dir stays prompt: w=%q rest=%v", w, rest)
	}
}

func TestLoadBuiltinsIncludesFileTools(t *testing.T) {
	r := NewRegistry()
	if unknown := r.LoadBuiltins([]string{"read", "write", "edit", "glob", "grep", "shell"}); len(unknown) != 0 {
		t.Fatalf("unknown=%v", unknown)
	}
	for _, name := range []string{"read", "write", "edit", "glob", "grep"} {
		if !r.Has(name) {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestReadWriteEditRoundTrip(t *testing.T) {
	root := useTempRoot(t)
	ctx := context.Background()

	out, err := WriteTool().Fn(ctx, `{"path":"a.txt","content":"one\ntwo\nthree\n"}`)
	if err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("write out=%q err=%v", out, err)
	}
	out, err = ReadTool().Fn(ctx, `{"path":"a.txt"}`)
	if err != nil || !strings.Contains(out, "2: two") {
		t.Fatalf("read out=%q err=%v", out, err)
	}
	out, err = EditTool().Fn(ctx, `{"path":"a.txt","oldString":"two","newString":"TWO"}`)
	if err != nil {
		t.Fatalf("edit err=%v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if !strings.Contains(string(raw), "TWO") {
		t.Fatalf("content=%q", raw)
	}
	out, err = ReadTool().Fn(ctx, `{"path":"a.txt","offset":2,"limit":1}`)
	if err != nil || !strings.Contains(out, "2: TWO") || strings.Contains(out, "three") {
		t.Fatalf("windowed read out=%q err=%v", out, err)
	}
}

// TestReadDefaultsWideWithByteCap: the default window covers 2000 lines (no
// more 200-line paging for normal files) while huge outputs cap at ~50KB
// with an offset hint to continue.
func TestReadDefaultsWideWithByteCap(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()

	var many strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&many, "line %d\n", i)
	}
	if _, err := WriteTool().Fn(ctx, `{"path":"many.txt","content":`+quoteJSON(many.String())+`}`); err != nil {
		t.Fatal(err)
	}
	out, err := ReadTool().Fn(ctx, `{"path":"many.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "300: line 300") || strings.Contains(out, "more lines") || strings.Contains(out, "capped") {
		t.Fatalf("300-line file must read whole by default:\n%s", out[:500])
	}

	big := strings.Repeat("x", 900) + "\n"
	var huge strings.Builder
	for i := 0; i < 200; i++ {
		huge.WriteString(big)
	}
	if _, err := WriteTool().Fn(ctx, `{"path":"huge.txt","content":`+quoteJSON(huge.String())+`}`); err != nil {
		t.Fatal(err)
	}
	out, err = ReadTool().Fn(ctx, `{"path":"huge.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "capped at ~12KB") || !strings.Contains(out, "re-read with offset") {
		t.Fatalf("huge file must cap with continue hint:\n%s", out[len(out)-300:])
	}
	if strings.Contains(out, "[truncated") {
		t.Fatalf("in-tool cap must fit under the registry truncate:\n%s", out[len(out)-300:])
	}
}

func TestReadDeniesOutsideWithoutPrompter(t *testing.T) {
	useTempRoot(t)
	if _, err := ReadTool().Fn(context.Background(), `{"path":"../outside.txt"}`); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err=%v", err)
	}
	abs := filepath.Join(t.TempDir(), "x.txt")
	if _, err := ReadTool().Fn(context.Background(), `{"path":`+quoteJSON(abs)+`}`); err == nil {
		t.Fatalf("expected denial for %s", abs)
	}
}

func TestWriteCreatesParents(t *testing.T) {
	root := useTempRoot(t)
	if _, err := WriteTool().Fn(context.Background(), `{"path":"sub/dir/b.txt","content":"hi"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "dir", "b.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestEditRefusesAmbiguous(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()
	if _, err := WriteTool().Fn(ctx, `{"path":"m.txt","content":"a x a x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := EditTool().Fn(ctx, `{"path":"m.txt","oldString":"x","newString":"y"}`); err == nil || !strings.Contains(err.Error(), "2 matches") {
		t.Fatalf("err=%v", err)
	}
	if _, err := EditTool().Fn(ctx, `{"path":"m.txt","oldString":"missing","newString":"y"}`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err=%v", err)
	}
	if _, err := EditTool().Fn(ctx, `{"path":"m.txt","oldString":"x","newString":"y","replaceAll":true}`); err != nil {
		t.Fatalf("replaceAll err=%v", err)
	}
}

func TestGlobFindsFiles(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()
	for _, f := range []string{"a/b.go", "a/c.txt", "d.go"} {
		if _, err := WriteTool().Fn(ctx, `{"path":`+quoteJSON(f)+`,"content":"x"}`); err != nil {
			t.Fatal(err)
		}
	}
	out, err := GlobTool().Fn(ctx, `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a/b.go") || !strings.Contains(out, "d.go") || strings.Contains(out, "c.txt") {
		t.Fatalf("out=%q", out)
	}
}

func TestGrepFindsPattern(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()
	if _, err := WriteTool().Fn(ctx, `{"path":"g.txt","content":"hello needle world\nplain\n"}`); err != nil {
		t.Fatal(err)
	}
	out, err := GrepTool().Fn(ctx, `{"pattern":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "g.txt:1:") {
		t.Fatalf("out=%q", out)
	}
	if _, err := GrepTool().Fn(ctx, `{"pattern":"("}`); err == nil {
		t.Fatal("expected bad pattern error")
	}
}

func TestGrepGlobSkipDependencyDirs(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()
	for _, f := range []string{
		"src/app.go",
		"node_modules/dep/index.js",
		"target/debug/build.rs",
		"vendor/lib/x.go",
	} {
		if _, err := WriteTool().Fn(ctx, `{"path":`+quoteJSON(f)+`,"content":"needle here\n"}`); err != nil {
			t.Fatal(err)
		}
	}
	out, err := GrepTool().Fn(ctx, `{"pattern":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "src/app.go") {
		t.Fatalf("src must match: %q", out)
	}
	for _, dep := range []string{"node_modules", "target", "vendor"} {
		if strings.Contains(out, dep) {
			t.Fatalf("dependency dir must be skipped: %q", out)
		}
	}
	gout, err := GlobTool().Fn(ctx, `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gout, "src/app.go") || strings.Contains(gout, "vendor") {
		t.Fatalf("glob must skip dependency dirs: %q", gout)
	}
}

func TestGrepParallelMultiFileSortedAndCapped(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()
	for _, f := range []string{"z/a.txt", "a/b.txt", "m/c.txt"} {
		if _, err := WriteTool().Fn(ctx, `{"path":`+quoteJSON(f)+`,"content":"needle here\n"}`); err != nil {
			t.Fatal(err)
		}
	}
	out, err := GrepTool().Fn(ctx, `{"pattern":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("out=%q", out)
	}
	if !strings.HasPrefix(lines[0], "a/b.txt") || !strings.HasPrefix(lines[1], "m/c.txt") || !strings.HasPrefix(lines[2], "z/a.txt") {
		t.Fatalf("matches should be sorted, got %q", out)
	}
	// Cap: 100 matches max with a truncation note.
	big := strings.Repeat("needle\n", 150)
	if _, err := WriteTool().Fn(ctx, `{"path":"big.txt","content":`+quoteJSON(big)+`}`); err != nil {
		t.Fatal(err)
	}
	capped, err := GrepTool().Fn(ctx, `{"pattern":"needle","path":"big.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Per-file isolation: one noisy file fills at most maxGrepPerFile slots,
	// so it can't starve matches from other files.
	if got := strings.Count(capped, "big.txt:"); got != maxGrepPerFile {
		t.Fatalf("expected %d capped matches, got %d", maxGrepPerFile, got)
	}
	if !strings.Contains(capped, "showing first 100 matches") {
		t.Fatalf("missing truncation note: %q", capped)
	}
}

func TestGrepPerFileIsolation(t *testing.T) {
	useTempRoot(t)
	ctx := context.Background()
	noisy := strings.Repeat("needle\n", 150)
	if _, err := WriteTool().Fn(ctx, `{"path":"noisy.txt","content":`+quoteJSON(noisy)+`}`); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTool().Fn(ctx, `{"path":"quiet.txt","content":"needle here\n"}`); err != nil {
		t.Fatal(err)
	}
	out, err := GrepTool().Fn(ctx, `{"pattern":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	// The quiet file's match must survive even though noisy.txt overflows.
	if !strings.Contains(out, "quiet.txt:1: needle here") {
		t.Fatalf("quiet match starved by noisy file: %q", out)
	}
}

func TestOutsideAccessAskAllowAndDeny(t *testing.T) {
	useTempRoot(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	restore := stubPrompter("Deny", nil)
	_, err := ReadTool().Fn(ctx, `{"path":`+quoteJSON(outside)+`}`)
	restore()
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("deny err=%v", err)
	}

	restore = stubPrompter("Allow", nil)
	defer restore()
	out, err := ReadTool().Fn(ctx, `{"path":`+quoteJSON(outside)+`}`)
	if err != nil {
		t.Fatalf("allow err=%v", err)
	}
	if !strings.Contains(out, "s3cret") {
		t.Fatalf("out=%q", out)
	}
}

func TestOutsideApprovalCoversFolder(t *testing.T) {
	useTempRoot(t)
	dir := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	asked := 0
	old := askPrompter
	askPrompter = func(context.Context, []AskQuestion) ([]string, error) { asked++; return []string{"Allow"}, nil }
	defer func() { askPrompter = old }()
	for _, n := range []string{"a.txt", "b.txt"} {
		if _, err := ReadTool().Fn(context.Background(), `{"path":`+quoteJSON(filepath.Join(dir, n))+`}`); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want 1", asked)
	}
}

func TestSiblingWorktreeNeedsNoApproval(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := useTempRoot(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "init")
	sib := filepath.Join(t.TempDir(), "sib")
	git("worktree", "add", "-q", sib, "-b", "sib")
	if err := os.WriteFile(filepath.Join(sib, "f.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTool().Fn(context.Background(), `{"path":`+quoteJSON(filepath.Join(sib, "f.txt"))+`}`); err != nil {
		t.Fatalf("sibling worktree read: %v", err)
	}
	other := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTool().Fn(context.Background(), `{"path":`+quoteJSON(other)+`}`); err == nil {
		t.Fatal("unrelated folder should be denied without prompter")
	}
}

func TestShellRunsInFileRoot(t *testing.T) {
	requireShellSandbox(t)
	root := useTempRoot(t)
	out, err := ShellTool().Fn(context.Background(), `{"command":"pwd"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != root {
		t.Fatalf("pwd=%q root=%q", out, root)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
