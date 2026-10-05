package main

// Shell sandbox. Every `shell` command runs under an OS-enforced policy:
//
//   - writes only inside the workdir, sibling git worktrees, folders the user
//     approved this session (see checkFileAccess), shared /tmp, the private
//     temp dir and tool caches;
//   - $HOME is hidden (reads and writes ask first via gateShell), as are
//     credential files (~/.ssh, ~/.aws, ~/.netrc, ... and any .env* secrets,
//     reads and writes; templates stay readable);
//   - network is off unless the user allowed it this session (localhost stays);
//   - system reads outside $HOME (toolchain, /usr, libraries) stay allowed so
//     `go test` never prompts.
//
// The kernel enforces it, so `$(...)`, `eval`, scripts and `python -c` can't
// get around it. The per-OS part is sandboxArgv (sandbox_darwin.go uses
// Seatbelt's sandbox-exec, sandbox_linux.go uses bubblewrap). Other platforms
// do not compile (sandbox_other.go): running unsandboxed is not an option.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// sharedTmp is the shared scratch dir shell may use without asking.
const sharedTmp = "/tmp"

// sandboxPolicy is what one command may do. Root is the workdir.
type sandboxPolicy struct {
	Root         string
	Writable     []string // existing dirs shell may write to
	ReadOnly     []string // existing files shell may read but not write (git configs)
	Hidden       []string // existing files/dirs shell cannot read or write
	SystemHidden []string // system paths shell cannot read (approvable via folder approval)
	Network      bool
}

// isSecretFile reports whether name is a .env file that holds secrets.
// Templates (.env.example, .sample, .template) stay readable.
func isSecretFile(name string) bool {
	b := filepath.Base(name)
	if b == ".env" {
		return true
	}
	if !strings.HasPrefix(b, ".env.") {
		return false
	}
	for _, t := range []string{".example", ".sample", ".template"} {
		if strings.HasSuffix(b, t) {
			return false
		}
	}
	return true
}

// sandboxHint is appended to a failed command's output when it looks like a
// sandbox denial, so the model knows what to do instead of retrying blindly.
const sandboxHint = "[sandbox: shell may only write inside the workdir, sibling git worktrees, approved folders, /tmp, the private temp dir and tool caches ($HOME is blocked). To get another folder, call glob or read on it (the user is asked once per folder), then re-run.]"

// looksSandboxDenied matches the errno texts a blocked write produces:
// EPERM from Seatbelt, EROFS from bubblewrap's read-only mounts.
func looksSandboxDenied(out string) bool {
	return strings.Contains(out, "Operation not permitted") || strings.Contains(out, "Read-only file system")
}

// networkHint is appended when a command failed like it had no network.
const networkHint = `[sandbox: network is off for shell. If this command needs the network (downloads, git fetch/push, curl), re-run it with "network": true; the user is asked once per session.]`

func looksNetworkDenied(out string) bool {
	for _, m := range []string{"Could not resolve host", "Temporary failure in name resolution", "Network is unreachable", "no such host", "Could not resolve hostname"} {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}

// sandboxCommand builds the exec.Cmd that runs command under the sandbox with
// the policy as it stands now (approvals made mid-session are picked up).
func sandboxCommand(ctx context.Context, command string, network bool) (*exec.Cmd, error) {
	name, args, err := sandboxArgv(command, sandboxPolicy{
		Root: getFileRoot(), Writable: writableRoots(), ReadOnly: gitReadOnlyPaths(),
		Hidden: hiddenPaths(), SystemHidden: systemHiddenPaths(), Network: network,
	})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = scrubShellEnv()
	return cmd, nil
}

// sensitiveEnvSubstrings marks env vars that never reach shell commands.
// The value itself is the secret (API keys, tokens), not the fact a tool
// needs them: valen's own HTTP calls keep using the process env, only the
// sandboxed `sh -c` child gets the scrubbed copy, so `printenv`/`env` can't
// leak them even via `$VAR`/obfuscated construction that gateShell can't see.
func sensitiveEnvKey(key string) bool {
	u := strings.ToUpper(key)
	for _, sub := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIALS"} {
		if strings.Contains(u, sub) {
			return true
		}
	}
	return false
}

// scrubShellEnv drops secret-bearing vars from the env inherited by shell.
// Non-secret vars (PATH, HOME, TMPDIR, LANG, ...) pass through: HOME must
// stay so git/go resolve the workdir parents, and its contents stay hidden
// by the kernel profile, not by env removal.
func scrubShellEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && sensitiveEnvKey(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// writableRoots lists the existing, symlink-resolved directories shell may
// write to. Paths must exist: both backends refuse to bind a missing path.
// Shared /tmp is writable by default (scratch space, and the preferred place
// for git worktrees: `git worktree add /tmp/<name>`). /var/tmp is not. The
// private per-user temp dir (os.TempDir, /var/folders/... on macOS, used by
// t.TempDir and go builds) stays writable so `go test` never prompts.
func writableRoots() []string {
	root := getFileRoot()
	c := []string{root, os.TempDir(), sharedTmp, os.Getenv("GOCACHE")}
	approvedMu.Lock()
	for dir := range approvedOutside {
		c = append(c, dir)
	}
	approvedMu.Unlock()
	c = append(c, siblingWorktrees(root)...)
	if cd, err := os.UserCacheDir(); err == nil {
		c = append(c, cd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{"go", ".cache", ".npm", ".cargo"} {
			c = append(c, filepath.Join(home, d))
		}
	}
	return existingDirs(c)
}

// ancestorDirs returns the sorted, de-duplicated proper ancestors of dirs
// (excluding "/"), e.g. /Users/n/Code for /Users/n/Code/valen.
func ancestorDirs(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		for p := filepath.Dir(d); p != "/" && p != "." && p != filepath.Dir(p); p = filepath.Dir(p) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// hiddenPaths lists credential files and dirs the sandbox hides from the
// command. Only ones that exist (both backends need a real path to mask).
// The home directory itself is hidden: reads/writes outside the workdir ask
// first (see gateShell/checkFileAccess) and the kernel blocks unapproved
// access, so `cat ~/.zshrc` or `cat ~/.config/valen/config.toml` cannot leak
// personal info. Safe home subpaths (tool caches, approved folders, sibling
// worktrees, the workdir itself when under $HOME) are re-allowed after this
// deny (later SBPL rules win; bwrap re-binds after the tmpfs).
func hiddenPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var c []string
	c = append(c, home)
	for _, d := range []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure",
		".netrc", ".npmrc", ".pypirc", ".config/gh", ".config/gcloud", ".config",
		".local/share/valen", ".bash_history", ".zsh_history", ".zshrc", ".bashrc"} {
		c = append(c, filepath.Join(home, d))
	}
	return existing(c, false)
}

// gitReadOnlyPaths lists git config files the sandbox re-allows for reads
// only. Git stats these on every invocation (even `git status` in the
// workdir), and hiding $HOME made git fail with "unable to access
// '/Users/n/.gitconfig': Operation not permitted". Later SBPL rules win, so
// a literal file-read allow after the HOME deny restores `git` without
// opening $HOME for writes or listing; on Linux the file is ro-bound over
// the HOME tmpfs. Credential dirs (~/.ssh) stay hidden.
func gitReadOnlyPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	c := []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".gitignore"),
		filepath.Join(home, ".gitattributes"),
		filepath.Join(home, ".config", "git", "config"),
		filepath.Join(home, ".config", "git", "ignore"),
	}
	return existing(c, false)
}

// systemHiddenPaths lists system paths whose reads ask first via gateShell
// and are blocked by the kernel until approved. The profile defaults to
// allow reads (so the toolchain never prompts), which let obfuscated
// constructions (`S=$(printf '\057'); cat ${S}etc/passwd`) bypass the
// string-matching gate and read /etc with no prompt. Denying /etc here
// fails closed: direct `cat /etc/passwd` still asks, obfuscated reads now
// hit "Operation not permitted", and an approved /etc folder is re-allowed
// via writableRoots (later rules win on Darwin; bind after mask on Linux).
func systemHiddenPaths() []string {
	return existing([]string{"/etc", real("/etc")}, true)
}

// existingDirs keeps directories that exist, resolving symlinks.
func existingDirs(in []string) []string { return existing(in, true) }

// existing keeps paths that exist (dirs only when dirOnly), resolves symlinks
// (the kernel matches real paths: macOS /tmp -> /private/tmp), drops duplicates.
func existing(in []string, dirOnly bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err != nil || (dirOnly && !fi.IsDir()) {
			continue
		}
		if r := real(p); !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}
