//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sh runs one command through the real ShellTool (so the real sandbox).
func sh(t *testing.T, cmd string) string { return shNet(t, cmd, false) }

func shNet(t *testing.T, cmd string, network bool) string {
	t.Helper()
	requireShellSandbox(t)
	args, _ := json.Marshal(map[string]any{"command": cmd, "network": network})
	out, err := ShellTool().Fn(context.Background(), string(args))
	if err != nil {
		t.Skipf("sandbox backend unavailable here: %v", err)
	}
	return out
}

// outsideDir is a writable-by-user dir that is neither the root, a temp dir
// nor a cache dir: the sandbox must block it. Under $HOME, cleaned up after.
func outsideDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	dir, err := os.MkdirTemp(home, ".valen-sandbox-test-")
	if err != nil {
		t.Skipf("cannot create dir under home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return real(dir)
}

func TestSandboxAllowsWriteInRoot(t *testing.T) {
	root := useTempRoot(t)
	if out := sh(t, "echo hi > f.txt && cat f.txt"); out != "hi" {
		t.Fatalf("out=%q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "f.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxBlocksWriteOutsideRoot(t *testing.T) {
	useTempRoot(t)
	target := filepath.Join(outsideDir(t), "x.txt")
	out := sh(t, "echo no > "+target)
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("write escaped the sandbox, out=%q", out)
	}
	if !strings.Contains(out, "[sandbox:") {
		t.Fatalf("denial should carry the hint, out=%q", out)
	}
}

func TestSandboxBlocksIndirectEscapes(t *testing.T) {
	useTempRoot(t)
	dir := outsideDir(t)
	// Shell tricks the string-matching approach could never catch.
	for name, cmd := range map[string]string{
		"subst":  `d=$(echo ` + dir + `); echo no > "$d/a"`,
		"eval":   `eval "echo no > ` + dir + `/b"`,
		"nested": `sh -c 'echo no > ` + dir + `/c'`,
		"cd":     `cd ` + dir + ` && echo no > d`,
	} {
		sh(t, cmd)
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatalf("%s escaped: %v", name, entries)
		}
	}
}

func TestSandboxAllowsTempDir(t *testing.T) {
	useTempRoot(t)
	tmp := t.TempDir() // not the root, but under the OS temp dir
	if out := sh(t, "echo ok > "+filepath.Join(tmp, "t.txt")+" && echo done"); out != "done" {
		t.Fatalf("out=%q", out)
	}
}

func TestSandboxReadsOutsideAsk(t *testing.T) {
	useTempRoot(t)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	other := filepath.Join(home, ".valen-probe-r.txt")
	// Explicit outside reads ask first: no prompter denies, like git push.
	if err := gateShell(context.Background(), "cat "+other, false); err == nil {
		t.Fatal("outside read should need approval")
	}
	// Same folder approval as the file tools covers shell too.
	defer stubPrompter("Allow", nil)()
	if err := gateShell(context.Background(), "cat "+other, false); err != nil {
		t.Fatalf("approved folder should pass the gate: %v", err)
	}
}

func TestSandboxFollowsApprovedFolder(t *testing.T) {
	useTempRoot(t)
	dir := outsideDir(t)
	target := filepath.Join(dir, "ok.txt")
	sh(t, "echo no > "+target)
	if _, err := os.Stat(target); err == nil {
		t.Fatal("should be blocked before approval")
	}
	// Same approval path as the file tools: one write answer covers the folder.
	defer stubPrompter("Allow", nil)()
	seed := filepath.Join(dir, "seed.txt")
	if _, err := WriteTool().Fn(context.Background(), `{"path":`+quoteJSON(seed)+`,"content":"s"}`); err != nil {
		t.Fatalf("approve: %v", err)
	}
	sh(t, "echo yes > "+target)
	if b, err := os.ReadFile(target); err != nil || strings.TrimSpace(string(b)) != "yes" {
		t.Fatalf("approved folder should be writable: %q %v", b, err)
	}
}

// A read approval must not silently grant write: the folder stays readable,
// but writes ask separately and are denied here (no prompter).
func TestReadApprovalDoesNotGrantWrite(t *testing.T) {
	useTempRoot(t)
	dir := outsideDir(t)
	seed := filepath.Join(dir, "seed.txt")
	if err := os.WriteFile(seed, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	restore := stubPrompter("Allow", nil)
	if _, err := ReadTool().Fn(ctx, `{"path":`+quoteJSON(seed)+`}`); err != nil {
		t.Fatalf("read approval: %v", err)
	}
	restore()
	if err := checkFileAccess(ctx, "read", seed); err != nil {
		t.Fatalf("read approval should persist: %v", err)
	}
	if err := checkFileAccess(ctx, "write", filepath.Join(dir, "new.txt")); err == nil {
		t.Fatal("read approval must not grant write")
	}
}

func TestSandboxHidesCredentialDirs(t *testing.T) {
	useTempRoot(t)
	home := real(t.TempDir())
	t.Setenv("HOME", home)
	ssh := filepath.Join(home, ".ssh")
	if err := os.Mkdir(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "id_rsa"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := sh(t, "cat "+filepath.Join(ssh, "id_rsa")+" 2>&1"); strings.Contains(out, "PRIVATE") {
		t.Fatalf("secret readable: %q", out)
	}
	sh(t, "echo x > "+filepath.Join(ssh, "new"))
	if _, err := os.Stat(filepath.Join(ssh, "new")); err == nil {
		t.Fatal("hidden dir writable")
	}
}

func TestWritableRootsContents(t *testing.T) {
	root := useTempRoot(t)
	got := writableRoots()
	if !contains(got, real(root)) {
		t.Fatalf("root missing from %v", got)
	}
	for _, p := range got {
		if p == "/" || p == real(os.Getenv("HOME")) {
			t.Fatalf("too broad: %q", p)
		}
	}
	// Shared /tmp is writable by default (worktrees and scratch files).
	if tmp := real(sharedTmp); !contains(got, tmp) {
		t.Fatalf("/tmp should be writable by default: %v", got)
	}
}

func TestAncestorDirs(t *testing.T) {
	got := ancestorDirs([]string{"/Users/n/Code/valen", "/tmp", "/Users/n/go"})
	want := []string{"/Users", "/Users/n", "/Users/n/Code"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// git stats every parent of the workdir; hiding $HOME must not break that.
func TestSandboxLetsGitResolveWorkdirParents(t *testing.T) {
	root := useTempRoot(t)
	home, err := os.UserHomeDir()
	if err != nil || !withinRoot(home, root) {
		t.Skip("workdir is not under $HOME")
	}
	if out := sh(t, "stat "+filepath.Dir(root)+" >/dev/null 2>&1 && echo ok"); out != "ok" {
		t.Fatalf("parent of workdir not stat-able: %q", out)
	}
}

func TestSandboxAllowsSharedTmp(t *testing.T) {
	useTempRoot(t)
	dir, err := os.MkdirTemp(sharedTmp, "valen-sbx-test-")
	if err != nil {
		t.Skipf("cannot create dir in /tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := filepath.Join(dir, "f.txt")
	sh(t, "echo hi > "+f)
	if b, err := os.ReadFile(f); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("/tmp should be writable: %q %v", b, err)
	}
	if err := gateShell(context.Background(), "echo hi > "+f, false); err != nil {
		t.Fatalf("/tmp must not prompt: %v", err)
	}
}

func TestShellOutsidePaths(t *testing.T) {
	useTempRoot(t)
	home, _ := os.UserHomeDir()
	for cmd, want := range map[string]int{
		"go test -run 'Test' . 2>&1": 0,
		"go test ./...":              0,
		"gofmt -l .":                 0,
		"go vet ./...":               0,
		"ls":                         0,
		"cat /etc/passwd":            1,
		"echo hi > /tmp/x":           0,
		"cat ~/.zshrc":               1,
		"go test $(go list ./... | grep -v /vendor/)": 0,
		"go test ./... | grep -v /pkg/":               0,
		"go test -coverprofile=/tmp/c.out ./...":      0,
		"ls /dev/null":                                0,
		"ls /usr/bin":                                 0,
		`cd "$(pwd)/" && ls`:                          0,
		`ls "$PWD"/`:                                  0,
		`ls "$(git rev-parse --show-toplevel)"/x`:     0,
		"ls /":            1,
		"sed 's/a //' f":  0,
		"git diff HEAD~1": 0,
		`ls "/"`:          1,
		// Variable expansion is not /etc obfuscation: read-only git commands
		// (and anything else using ${...} or backticks) must not prompt for /etc.
		"git log ${REF}":                         0,
		`git diff "${BASE}"`:                     0,
		"git log --pretty=format:'${FMT}'":       0,
		"git log --oneline `git rev-parse HEAD`": 0,
		// Building the slash then naming etc still asks.
		`S=$(printf '\057'); cat ${S}etc/passwd`: 1,
	} {
		if got := len(shellOutsidePaths(cmd)); got != want {
			t.Errorf("%q -> %d outside paths, want %d (%q)", cmd, got, want, shellOutsidePaths(cmd))
		}
	}
	if home != "" {
		if len(shellOutsidePaths("ls ~")) == 0 {
			t.Error("ls ~ should ask")
		}
		if len(shellOutsidePaths("cat "+filepath.Join(home, ".zshrc"))) == 0 {
			t.Error("home dotfile should ask")
		}
	}
}

// TestShellOutsidePathsAllowsSystemReadOnly: the re-allowed system config
// files never prompt, while /etc/passwd still does (asserted in
// TestShellOutsidePaths). Skipped when /etc is hidden from this process, as
// under the sandbox that runs the tests.
func TestShellOutsidePathsAllowsSystemReadOnly(t *testing.T) {
	useTempRoot(t)
	sys := systemReadOnlyPaths()
	if len(sys) == 0 {
		t.Skip("system config paths hidden here")
	}
	for _, p := range sys {
		if got := len(shellOutsidePaths("cat " + p)); got != 0 {
			t.Errorf("system read-only %s must not ask, got %d", p, got)
		}
	}
	if got := len(shellOutsidePaths("cat /etc/passwd")); got == 0 {
		t.Error("/etc/passwd must still ask")
	}
}

func TestGateShellOutsideAsksOncePerFolder(t *testing.T) {
	useTempRoot(t)
	target := filepath.Join(outsideDir(t), "valen-gate-test-x.txt")
	asked := 0
	old := askPrompter
	askPrompter = func(context.Context, []AskQuestion) ([]string, error) { asked++; return []string{"Allow"}, nil }
	defer func() { askPrompter = old }()
	ctx := context.Background()
	if err := gateShell(ctx, "echo hi > "+target, false); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Fatalf("outside folder asked %d times, want 1", asked)
	}
	// Second use of the same folder is remembered, like file tools.
	if err := gateShell(ctx, "cat "+target, false); err != nil || asked != 1 {
		t.Fatalf("approved folder should not re-ask: err=%v asked=%d", err, asked)
	}
	// Safe builds never ask, even with an ask-counting prompter.
	if err := gateShell(ctx, "go test -run 'Test' . 2>&1", false); err != nil || asked != 1 {
		t.Fatalf("go test must not ask: err=%v asked=%d", err, asked)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestSandboxArgvKeepsCommandIntact(t *testing.T) {
	cmd := `echo "a'b" $(date)`
	_, args, err := sandboxArgv(cmd, sandboxPolicy{Writable: []string{"/tmp"}})
	if err != nil {
		t.Skip(err)
	}
	if n := len(args); n < 3 || args[n-3] != "sh" || args[n-2] != "-c" || args[n-1] != cmd {
		t.Fatalf("command must be the last argv element, unmodified: %q", args)
	}
}

func TestSandboxHidesDotEnvFiles(t *testing.T) {
	requireShellSandbox(t)
	root := useTempRoot(t)
	write := func(n, body string) {
		if err := os.WriteFile(filepath.Join(root, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "TOKEN=abc")
	write(".env.local", "TOKEN=abc")
	write(".env.example", "TOKEN=")
	for _, n := range []string{".env", ".env.local"} {
		if out := sh(t, "cat "+n+" 2>&1"); strings.Contains(out, "abc") {
			t.Fatalf("%s readable: %q", n, out)
		}
	}
	if out := sh(t, "cat .env.example"); out != "TOKEN=" {
		t.Fatalf("template should stay readable, out=%q", out)
	}
}

// Shell must not see a secret .env through an innocently-named link: the file
// tools follow links via blocksSecret, and the kernel profile/mask must too.
func TestSandboxHidesDotEnvThroughLink(t *testing.T) {
	requireShellSandbox(t)
	root := useTempRoot(t)
	env := filepath.Join(root, ".env")
	if err := os.WriteFile(env, []byte("TOKEN=abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(env, link); err != nil {
		t.Fatal(err)
	}
	if out := sh(t, "cat innocent.txt 2>&1"); strings.Contains(out, "abc") {
		t.Fatalf(".env leaked through a symlink: %q", out)
	}
	hard := filepath.Join(root, "hard.txt")
	if err := os.Link(env, hard); err == nil {
		if out := sh(t, "cat hard.txt 2>&1"); strings.Contains(out, "abc") {
			t.Fatalf(".env leaked through a hardlink: %q", out)
		}
	}
}

func TestSecretFilesHiddenFromFileTools(t *testing.T) {
	// The sandbox forbids creating .env files, so the name alone must be
	// enough: the tools deny before touching the filesystem.
	useTempRoot(t)
	if _, err := ReadTool().Fn(context.Background(), `{"path":".env"}`); err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("read .env err=%v", err)
	}
	if _, err := WriteTool().Fn(context.Background(), `{"path":".env","content":"x"}`); err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("write .env err=%v", err)
	}
	if _, err := EditTool().Fn(context.Background(), `{"path":".env.local","oldString":"a","newString":"b"}`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "secret") {
		t.Fatalf("edit .env err=%v", err)
	}
}

func TestIsSecretFile(t *testing.T) {
	for name, want := range map[string]bool{
		".env": true, "a/b/.env": true, ".env.local": true, ".env.production": true,
		".env.example": false, ".env.local.sample": false, ".env.template": false,
		"env": false, "x.env": false, ".envrc": false,
	} {
		if got := isSecretFile(name); got != want {
			t.Errorf("isSecretFile(%q)=%v want %v", name, got, want)
		}
	}
}

func TestSandboxNetworkOffByDefault(t *testing.T) {
	useTempRoot(t)
	// Loopback must keep working with network off: serve and fetch locally.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "pong") }))
	defer srv.Close()
	if out := sh(t, "curl -s -m 5 "+srv.URL); out != "pong" {
		t.Skipf("curl/loopback unavailable here: %q", out)
	}
	// Any non-loopback connect must fail (IP literal: no DNS needed).
	out := sh(t, "curl -s -m 5 -o /dev/null -w '%{http_code}' http://1.1.1.1/; echo \" exit=$?\"")
	if strings.Contains(out, "200") || strings.Contains(out, "exit=0") {
		t.Fatalf("external network reachable with network off: %q", out)
	}
	// Guard against a vacuous pass: curl exit 3 means a malformed URL, not a block.
	if strings.Contains(out, "exit=3") || !strings.Contains(out, "exit=") {
		t.Fatalf("test command is broken, not blocked: %q", out)
	}
}

func TestDestructiveLabels(t *testing.T) {
	for cmd, want := range map[string]string{
		"git push origin main":            "git push",
		"git -C ../x push --force":        "git push",
		"cd a && git reset --hard HEAD~1": "git reset --hard",
		"git clean -fdx":                  "git clean",
		"rm -rf build":                    "rm -r",
		"rm -fr build":                    "rm -r",
		"rm --recursive x":                "rm -r",
		"ls && sudo make install":         "sudo",
	} {
		got := destructiveLabels(cmd)
		if len(got) == 0 || got[0] != want {
			t.Errorf("%q -> %v, want %q", cmd, got, want)
		}
	}
	for _, cmd := range []string{"git status", "git pull", "git log --oneline", "rm file.txt", "go test ./...", "echo pushing", "grep -r sudo ."} {
		if got := destructiveLabels(cmd); len(got) != 0 {
			t.Errorf("%q should not match, got %v", cmd, got)
		}
	}
}

func TestGateShellApprovalNamesModel(t *testing.T) {
	resetShellApprovals()
	t.Cleanup(resetShellApprovals)
	old := askPrompter
	var got AskQuestion
	askPrompter = func(_ context.Context, qs []AskQuestion) ([]string, error) {
		got = qs[0]
		return []string{"Allow"}, nil
	}
	t.Cleanup(func() { askPrompter = old })
	ctx := context.WithValue(context.Background(), turnConfigKey{}, Config{Model: "deepseek-v4.1-flash"})
	if err := gateShell(ctx, "git push", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Question, "Allow deepseek-v4.1-flash to ") {
		t.Fatalf("approval must name the model: %q", got.Question)
	}
}

func TestGateShellAsksOncePerLabel(t *testing.T) {
	useTempRoot(t)
	asked := 0
	old := askPrompter
	askPrompter = func(context.Context, []AskQuestion) ([]string, error) { asked++; return []string{"Allow"}, nil }
	defer func() { askPrompter = old }()
	ctx := context.Background()
	for _, c := range []string{"git push", "git push origin x"} {
		if err := gateShell(ctx, c, false); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Fatalf("git push asked %d times, want 1", asked)
	}
	if err := gateShell(ctx, "curl x", true); err != nil || asked != 2 {
		t.Fatalf("network should ask once: err=%v asked=%d", err, asked)
	}
	if err := gateShell(ctx, "ls", false); err != nil || asked != 2 {
		t.Fatalf("harmless command must not ask: err=%v asked=%d", err, asked)
	}
}

func TestGateShellDeniesWithoutPrompterOrOnDeny(t *testing.T) {
	useTempRoot(t) // prompter nil
	if err := gateShell(context.Background(), "rm -rf x", false); err == nil {
		t.Fatal("no prompter must deny")
	}
	defer stubPrompter("Deny", nil)()
	if err := gateShell(context.Background(), "git push", false); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("deny err=%v", err)
	}
	if err := gateShell(context.Background(), "ls", false); err != nil {
		t.Fatalf("harmless: %v", err)
	}
}

func TestScrubShellEnvDropsSecrets(t *testing.T) {
	t.Setenv("VALEN_TEST_SECRET_KEY", "s3cr3t")
	t.Setenv("VALEN_TEST_API_TOKEN", "tok")
	t.Setenv("VALEN_TEST_PASSWORD", "pw")
	t.Setenv("VALEN_TEST_PLAIN", "keep")
	got := map[string]string{}
	for _, kv := range scrubShellEnv() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range []string{"VALEN_TEST_SECRET_KEY", "VALEN_TEST_API_TOKEN", "VALEN_TEST_PASSWORD"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s leaked into shell env", k)
		}
	}
	if got["VALEN_TEST_PLAIN"] != "keep" {
		t.Error("non-secret env must pass through")
	}
	if _, ok := got["PATH"]; !ok {
		t.Error("PATH must pass through")
	}
}

func TestCheckFileAccessBlocksSymlinkEscape(t *testing.T) {
	root := useTempRoot(t) // prompter nil: outside denies
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link-out")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := checkFileAccess(context.Background(), "read", link); err == nil {
		t.Fatal("symlink to outside file must deny without approval")
	}
	dirLink := filepath.Join(root, "link-dir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	if err := checkFileAccess(context.Background(), "read", filepath.Join(dirLink, "secret.txt")); err == nil {
		t.Fatal("symlinked dir escape must deny without approval")
	}
}

func TestBlocksSecretFollowsSymlink(t *testing.T) {
	// The sandboxed test runner cannot create fresh .env files (the Seatbelt
	// profile denies them), so reuse the repo's existing probe file as the
	// secret target: only the link itself is created here (allowed name).
	target := filepath.Join(getFileRoot(), ".env.probe-test")
	if _, err := os.Lstat(target); err != nil {
		cwd, _ := os.Getwd()
		target = filepath.Join(cwd, ".env.probe-test")
		if _, err := os.Lstat(target); err != nil {
			t.Skip("no .env probe file to link against")
		}
	}
	link := filepath.Join(t.TempDir(), "link-env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !blocksSecret(link) {
		t.Fatal("link to .env file must count as secret")
	}
}

func TestShellObfuscatedPathFlagsBypass(t *testing.T) {
	useTempRoot(t)
	bypass := `S=$(printf '\057'); E=etc; P=${S}${E}; cat ${P}/passwd`
	if !shellObfuscatedPath(bypass) {
		t.Fatal("printf/${ bypass must flag")
	}
	if got := shellOutsidePaths(bypass); len(got) == 0 {
		t.Fatal("obfuscated command must produce an outside path (prompt)")
	}
	for _, safe := range []string{"go test -run 'Test' . 2>&1", "go test $(go list ./... | grep -v /vendor/)", "gofmt -l ."} {
		if shellObfuscatedPath(safe) {
			t.Errorf("%q must not flag as obfuscated", safe)
		}
	}
	if got := len(shellOutsidePaths("go test $(go list ./... | grep -v /vendor/)")); got != 0 {
		t.Errorf("safe go list subst must stay prompt-free, got %d", got)
	}
}

func TestSandboxArgvReallowsGitConfig(t *testing.T) {
	// Use a synthetic read-only file: the real ~/.gitconfig is unstat-able
	// when tests run under the sandbox (HOME hidden), so gitReadOnlyPaths()
	// is empty here. The profile wiring is what matters.
	f := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, args, err := sandboxArgv("true", sandboxPolicy{Writable: []string{"/tmp"}, ReadOnly: []string{f}})
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), f) {
		t.Errorf("read-only %s missing from sandbox argv", f)
	}
}

func TestSandboxArgvReadOnlyFileAndDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("SBPL profile is darwin-only")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deny the parent so the re-allows have something to override.
	_, args, err := sandboxArgv("true", sandboxPolicy{
		Writable:     []string{"/tmp"},
		ReadOnly:     []string{file, dir},
		SystemHidden: []string{dir},
	})
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "(allow file-read* (literal (param") {
		t.Errorf("file ReadOnly must emit a literal allow: %s", joined)
	}
	if !strings.Contains(joined, "(allow file-read* (subpath (param") {
		t.Errorf("dir ReadOnly must emit a subpath allow: %s", joined)
	}
	// Later SBPL rules win: the subpath re-allow must follow the system deny.
	deny := strings.Index(joined, "(deny file-read* (subpath")
	allow := strings.Index(joined, "(allow file-read* (subpath (param")
	if deny < 0 || allow < 0 || allow < deny {
		t.Errorf("subpath re-allow must follow the system deny: %s", joined)
	}
}

func TestSandboxArgvDeniesSystemHidden(t *testing.T) {
	paths := systemHiddenPaths()
	if len(paths) == 0 {
		t.Skip("no system hidden paths here")
	}
	_, args, err := sandboxArgv("true", sandboxPolicy{Writable: []string{"/tmp"}, SystemHidden: paths})
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, p := range paths {
		if !strings.Contains(joined, p) {
			t.Errorf("system hidden %s missing from sandbox argv", p)
		}
	}
}
