package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// scriptAccess installs a prompter that answers from script in order and
// records every question asked.
func scriptAccess(t *testing.T, script ...string) *[]AskQuestion {
	t.Helper()
	var asked []AskQuestion
	old := askPrompter
	askPrompter = func(_ context.Context, qs []AskQuestion) ([]string, error) {
		asked = append(asked, qs[0])
		if len(asked) > len(script) {
			return nil, errors.New("unexpected extra prompt")
		}
		return []string{script[len(asked)-1]}, nil
	}
	t.Cleanup(func() { askPrompter = old })
	return &asked
}

func TestAccessChoiceParsing(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want accessChoice
	}{
		{[]string{lblAllowSession}, accessAllowSession},
		{[]string{lblAllowAlways}, accessAllowAlways},
		{[]string{lblDenySession}, accessDenySession},
		{[]string{lblDenyAlways}, accessDenyAlways},
		{nil, accessDenySession},
		{[]string{""}, accessDenySession},
		{[]string{"Allow"}, accessDenySession},
		{[]string{"allow everything please"}, accessDenySession},
		{[]string{lblAllowAlways + " "}, accessDenySession},
	} {
		if got := parseAccessChoice(tc.in); got != tc.want {
			t.Errorf("parse(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestAccessPromptShape(t *testing.T) {
	asked := scriptAccess(t, lblAllowSession)
	c, err := promptAccess(context.Background(), AskQuestion{Question: "Allow?", Header: "File access"}, "/some/dir", "write = /some/dir", "deny = /some/dir")
	if err != nil || c != accessAllowSession {
		t.Fatalf("c=%v err=%v", c, err)
	}
	q := (*asked)[0]
	if !q.NoFreeText || len(q.Options) != 4 || len(q.Highlight) != 1 || q.Highlight[0] != "/some/dir" {
		t.Fatalf("bad shape: %+v", q)
	}
	for i, l := range []string{lblAllowSession, lblAllowAlways, lblDenySession, lblDenyAlways} {
		if q.Options[i].Label != l {
			t.Errorf("option %d=%q want %q", i, q.Options[i].Label, l)
		}
	}
	if len(*asked) != 1 {
		t.Fatalf("session choice must not confirm, asked %d", len(*asked))
	}
}

func TestAccessConfirmDefaultsYes(t *testing.T) {
	useAccessFile(t)
	asked := scriptAccess(t, lblAllowAlways, lblConfirmYes)
	c, err := promptAccess(context.Background(), AskQuestion{Question: "Allow?"}, "/d", "write = /d", "deny = /d")
	if err != nil || c != accessAllowAlways {
		t.Fatalf("c=%v err=%v", c, err)
	}
	if len(*asked) != 2 {
		t.Fatalf("asked %d, want prompt + confirm", len(*asked))
	}
	cq := (*asked)[1]
	if !cq.NoFreeText || len(cq.Options) != 2 || cq.Options[0].Label != lblConfirmYes {
		t.Fatalf("confirm must be 2 options with Yes first (default): %+v", cq)
	}
	if len(cq.Highlight) != 1 || cq.Highlight[0] != "write = /d" || !strings.Contains(cq.Question, "access.toml") {
		t.Fatalf("confirm must name rule and file: %+v", cq)
	}
}

func TestAccessConfirmNoDowngrades(t *testing.T) {
	useAccessFile(t)
	for _, tc := range []struct {
		first string
		want  accessChoice
	}{{lblAllowAlways, accessAllowSession}, {lblDenyAlways, accessDenySession}} {
		scriptAccess(t, tc.first, lblConfirmNo)
		c, err := promptAccess(context.Background(), AskQuestion{Question: "q"}, "/d", "r", "r")
		if err != nil || c != tc.want {
			t.Errorf("%s + No: c=%v err=%v want %v", tc.first, c, err, tc.want)
		}
	}
	// Garbage confirm answer also declines.
	scriptAccess(t, lblAllowAlways, "sure")
	if c, _ := promptAccess(context.Background(), AskQuestion{Question: "q"}, "/d", "r", "r"); c != accessAllowSession {
		t.Errorf("garbage confirm: c=%v", c)
	}
}

func TestAccessConfirmShowsRuleForChoice(t *testing.T) {
	useAccessFile(t)
	asked := scriptAccess(t, lblDenyAlways, lblConfirmYes)
	if c, _ := promptAccess(context.Background(), AskQuestion{Question: "q"}, "/d", "write = /d", "deny = /d"); c != accessDenyAlways {
		t.Fatalf("c=%v", c)
	}
	if got := (*asked)[1].Highlight[0]; got != "deny = /d" {
		t.Fatalf("deny confirm shows %q", got)
	}
}

// useAccessFile pins a fresh rules file for the test and restores
// session-only mode afterwards.
func useAccessFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "valen", userAccessName)
	setAccessFile(path)
	t.Cleanup(func() { setAccessFile("") })
	return path
}

func TestAccessTomlRoundTrip(t *testing.T) {
	path := useAccessFile(t)
	a, b := real(t.TempDir()), real(t.TempDir())
	for _, c := range []struct{ kind, v string }{
		{ruleWrite, b}, {ruleWrite, a}, {ruleWrite, a}, // dedupe + sorted
		{ruleRead, a}, {ruleDeny, b},
		{ruleShellAllow, "network"}, {ruleShellDeny, "sudo"},
	} {
		if err := saveAccessRule(c.kind, c.v); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh cache (new process) reads the same rules back.
	setAccessFile(path)
	w := accessDirs(ruleWrite)
	if len(w) != 2 || w[0] > w[1] {
		t.Fatalf("write=%v want 2 sorted entries", w)
	}
	if !accessMatch(ruleRead, filepath.Join(a, "sub", "f")) || accessMatch(ruleRead, b) {
		t.Fatal("read prefix match wrong")
	}
	if !accessDenied(filepath.Join(b, "x")) || accessDenied(a) {
		t.Fatal("deny match wrong")
	}
	if al, de := accessShellRule("network"); !al || de {
		t.Fatalf("network allow=%v deny=%v", al, de)
	}
	if al, de := accessShellRule("sudo"); al || !de {
		t.Fatalf("sudo allow=%v deny=%v", al, de)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# Written by valen") || !strings.Contains(string(raw), "[shell]") {
		t.Fatalf("file lacks header/shell table:\n%s", raw)
	}
	if err := saveAccessRule(ruleWrite, "relative/path"); err == nil {
		t.Fatal("relative path must be rejected")
	}
}

func TestAccessTomlExpandsHome(t *testing.T) {
	path := useAccessFile(t)
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("write = [\"~/.valen-access-probe\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAccessFile(path)
	if !accessMatch(ruleWrite, filepath.Join(home, ".valen-access-probe", "x")) {
		t.Fatalf("~/ not expanded: %v", accessDirs(ruleWrite))
	}
}

func TestAccessTomlMalformedFailsClosedAndIsNotClobbered(t *testing.T) {
	path := useAccessFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const broken = "write = [oops\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	setAccessFile(path)
	if len(accessDirs(ruleWrite)) != 0 {
		t.Fatal("malformed file must grant nothing")
	}
	if err := saveAccessRule(ruleWrite, real(t.TempDir())); err == nil {
		t.Fatal("save must refuse to overwrite a malformed file")
	}
	if raw, _ := os.ReadFile(path); string(raw) != broken {
		t.Fatalf("user file clobbered: %q", raw)
	}
}

func TestAccessTomlSeedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "valen")
	if _, err := ensureUserConfig(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, userAccessName)
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "read = []") || !strings.Contains(string(raw), "[shell]") {
		t.Fatalf("not seeded: %v %q", err, raw)
	}
	var r accessRules
	if _, err := toml.DecodeFile(path, &r); err != nil {
		t.Fatalf("seed must parse: %v", err)
	}
	if err := os.WriteFile(path, []byte("deny = [\"/x\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureUserConfig(dir); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "deny = [\"/x\"]\n" {
		t.Fatalf("seed overwrote user file: %q", raw)
	}
}

func TestAccessSessionOnlyWithoutFile(t *testing.T) {
	setAccessFile("")
	asked := scriptAccess(t, lblAllowAlways)
	c, err := promptAccess(context.Background(), AskQuestion{Question: "q"}, "/d", "write = /d", "deny = /d")
	if err != nil || c != accessAllowSession {
		t.Fatalf("c=%v err=%v: -config runs must stay session-only", c, err)
	}
	if len(*asked) != 1 {
		t.Fatalf("no confirm without a file, asked %d", len(*asked))
	}
	if err := saveAccessRule(ruleWrite, "/d"); err != nil {
		t.Fatalf("save without file must be a no-op: %v", err)
	}
}

func TestAccessDenyWinsOverAllow(t *testing.T) {
	useTempRoot(t)
	useAccessFile(t)
	dir := real(t.TempDir())
	if err := saveAccessRule(ruleDeny, dir); err != nil {
		t.Fatal(err)
	}
	// A session allow and a persistent write grant for the same folder both lose.
	approvedMu.Lock()
	approvedRead[dir], approvedWritable[dir] = true, true
	approvedMu.Unlock()
	if err := saveAccessRule(ruleWrite, dir); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "a.txt")
	if allowedWithoutPrompt(getFileRoot(), f) || allowedWriteWithoutPrompt(getFileRoot(), f) {
		t.Fatal("persistent deny must beat session + persistent allows")
	}
	asked := scriptAccess(t) // any prompt fails the test below
	err := checkFileAccess(context.Background(), "read", f)
	if err == nil || !strings.Contains(err.Error(), "access.toml") {
		t.Fatalf("err=%v, want persistent-deny error", err)
	}
	if len(*asked) != 0 {
		t.Fatal("a persistently denied folder must not prompt")
	}
	// The kernel policy hides it and drops it from the allowed roots.
	if !contains(hiddenPaths(), dir) {
		t.Fatalf("deny dir missing from hiddenPaths: %v", hiddenPaths())
	}
	if contains(writableRoots(), dir) || contains(readableRoots(), dir) {
		t.Fatal("denied dir must not stay an allowed root")
	}
}

func TestAccessAlwaysWidensSandbox(t *testing.T) {
	useTempRoot(t)
	path := useAccessFile(t)
	dir := real(t.TempDir())
	rd := real(t.TempDir())
	asked := scriptAccess(t, lblAllowAlways, lblConfirmYes, lblAllowAlways, lblConfirmYes)
	ctx := context.Background()
	if err := checkFileAccess(ctx, "write", filepath.Join(dir, "x.txt")); err != nil {
		t.Fatal(err)
	}
	if err := checkFileAccess(ctx, "read", filepath.Join(rd, "y.txt")); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 4 {
		t.Fatalf("asked %d", len(*asked))
	}
	if !contains(writableRoots(), dir) || !contains(readableRoots(), rd) {
		t.Fatalf("always grants must widen the kernel policy: w=%v r=%v", writableRoots(), readableRoots())
	}
	// "Restart": new process state, same file, no session approvals, no prompt.
	setFileRoot(getFileRoot())
	setAccessFile(path)
	scriptAccess(t)
	if err := checkFileAccess(ctx, "write", filepath.Join(dir, "sub", "z.txt")); err != nil {
		t.Fatalf("persisted write grant must survive a restart: %v", err)
	}
	if err := checkFileAccess(ctx, "read", filepath.Join(rd, "z.txt")); err != nil {
		t.Fatalf("persisted read grant must survive a restart: %v", err)
	}
	if err := checkFileAccess(ctx, "write", filepath.Join(rd, "z.txt")); err == nil {
		t.Fatal("a read grant must not allow writes (no prompt available)")
	}
	if !contains(shellSafePrefixes(), dir) || !contains(shellSafePrefixes(), rd) {
		t.Fatal("persistent folders must stop the shell gate prompting")
	}
}

func TestAccessDenyAlwaysPersistsAndFailsClosed(t *testing.T) {
	useTempRoot(t)
	path := useAccessFile(t)
	dir := real(t.TempDir())
	scriptAccess(t, lblDenyAlways, lblConfirmYes)
	if err := checkFileAccess(context.Background(), "read", filepath.Join(dir, "a")); err == nil {
		t.Fatal("deny must fail the access")
	}
	setFileRoot(getFileRoot())
	setAccessFile(path)
	asked := scriptAccess(t)
	if err := checkFileAccess(context.Background(), "read", filepath.Join(dir, "a")); err == nil || len(*asked) != 0 {
		t.Fatalf("persisted deny must block without prompting: err=%v asked=%d", err, len(*asked))
	}
}

func TestAccessConfirmNoKeepsSessionOnly(t *testing.T) {
	useTempRoot(t)
	path := useAccessFile(t)
	dir := real(t.TempDir())
	scriptAccess(t, lblAllowAlways, lblConfirmNo)
	if err := checkFileAccess(context.Background(), "read", filepath.Join(dir, "a")); err != nil {
		t.Fatalf("downgraded grant still allows this session: %v", err)
	}
	if got := accessDirs(ruleRead); len(got) != 0 {
		t.Fatalf("confirm No must not persist: %v", got)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), dir) {
		t.Fatalf("file written despite No:\n%s", raw)
	}
}

func TestAccessBroadAllowNeverReexposesSecrets(t *testing.T) {
	root := useTempRoot(t)
	useAccessFile(t)
	if err := saveAccessRule(ruleWrite, root); err != nil {
		t.Fatal(err)
	}
	if !blocksSecret(filepath.Join(root, ".env")) {
		t.Fatal(".env must stay blocked under a broad persistent write grant")
	}
	home, _ := os.UserHomeDir()
	if home != "" && !contains(hiddenPaths(), real(home)) {
		t.Fatal("home must stay hidden")
	}
}

func TestGateShellAllSessionsPersist(t *testing.T) {
	useTempRoot(t)
	path := useAccessFile(t)
	resetShellApprovals()
	t.Cleanup(resetShellApprovals)
	scriptAccess(t, lblAllowAlways, lblConfirmYes, lblDenyAlways, lblConfirmYes)
	ctx := context.Background()
	if err := gateShell(ctx, "git push", false); err != nil {
		t.Fatal(err)
	}
	if err := gateShell(ctx, "sudo true", false); err == nil {
		t.Fatal("sudo denied forever must fail")
	}
	// New session: no session maps, same file, no prompts at all.
	resetShellApprovals()
	setAccessFile(path)
	asked := scriptAccess(t)
	if err := gateShell(ctx, "git push origin x", false); err != nil {
		t.Fatalf("persisted allow must not prompt or fail: %v", err)
	}
	if err := gateShell(ctx, "sudo ls", false); err == nil || !strings.Contains(err.Error(), "access.toml") {
		t.Fatalf("persisted deny must fail closed: %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("asked %d, want 0", len(*asked))
	}
}

// TestAccessDenyHidesFolderFromShell proves the kernel side: a folder denied
// in access.toml is unreadable by a sandboxed command even though it lives
// under an otherwise writable root (the private temp dir).
func TestAccessDenyHidesFolderFromShell(t *testing.T) {
	requireShellSandbox(t)
	useTempRoot(t)
	useAccessFile(t)
	dir := real(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() error {
		cmd, err := sandboxCommand(context.Background(), "cat "+filepath.Join(dir, "f.txt"), false)
		if err != nil {
			return err
		}
		return cmd.Run()
	}
	if err := run(); err != nil {
		t.Fatalf("readable before deny: %v", err)
	}
	if err := saveAccessRule(ruleDeny, dir); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("denied folder must be unreadable inside the sandbox")
	}
}

// TestAccessDenyOrderedAfterAllowsInProfile checks the policy text without
// needing the kernel: a persistently denied folder inside an allowed root
// (here the private temp dir) must be denied again after the allows.
func TestAccessDenyOrderedAfterAllowsInProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("SBPL profile text is Darwin-only")
	}
	useTempRoot(t)
	useAccessFile(t)
	dir := real(t.TempDir())
	if err := saveAccessRule(ruleDeny, dir); err != nil {
		t.Fatal(err)
	}
	_, args, err := sandboxArgv("true", sandboxPolicy{
		Root: getFileRoot(), Writable: writableRoots(), Readable: readableRoots(), Hidden: hiddenPaths(),
	})
	if err != nil {
		t.Skip(err)
	}
	prof := args[1]
	var idx []int
	for i, a := range args {
		if strings.HasPrefix(a, "H") && strings.HasSuffix(a, "="+dir) {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		t.Fatalf("denied dir not passed as a hidden param: %v", args)
	}
	name := strings.SplitN(args[idx[0]], "=", 2)[0]
	rule := `(deny file-read* file-write* (subpath (param "` + name + `")))`
	if strings.Count(prof, rule) < 2 {
		t.Fatalf("deny for %s must be emitted again after the allows:\n%s", name, prof)
	}
	if strings.LastIndex(prof, rule) < strings.Index(prof, "(allow file-read*") {
		t.Fatal("re-deny must come after the read allows (later rules win)")
	}
}
