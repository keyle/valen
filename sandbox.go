package main

// Shell sandbox. Every `shell` command runs under an OS-enforced policy:
//
//   - writes only inside the workdir, sibling git worktrees, folders the user
//     approved this session (see checkFileAccess), temp dirs and tool caches;
//   - credential files (~/.ssh, ~/.aws, ~/.netrc, ... and any .env*) are hidden;
//   - network is off unless the user allowed it this session (localhost stays);
//   - everything else (reads, processes) behaves normally.
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

// sandboxPolicy is what one command may do. Root is the workdir.
type sandboxPolicy struct {
	Root     string
	Writable []string // existing dirs shell may write to
	Hidden   []string // existing files/dirs shell cannot read or write
	Network  bool
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
const sandboxHint = "[sandbox: shell may only write inside the workdir, sibling git worktrees, approved folders and temp/cache dirs. To get another folder, call glob or read on it (the user is asked once per folder), then re-run.]"

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
		Root: getFileRoot(), Writable: writableRoots(), Hidden: hiddenPaths(), Network: network,
	})
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, name, args...), nil
}

// writableRoots lists the existing, symlink-resolved directories shell may
// write to. Paths must exist: both backends refuse to bind a missing path.
func writableRoots() []string {
	root := getFileRoot()
	c := []string{root, os.TempDir(), "/tmp", "/var/tmp", os.Getenv("GOCACHE")}
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

// hiddenPaths lists credential files and dirs the sandbox hides from the
// command. Only ones that exist (both backends need a real path to mask).
func hiddenPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var c []string
	for _, d := range []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure",
		".netrc", ".npmrc", ".pypirc", ".config/gh", ".config/gcloud"} {
		c = append(c, filepath.Join(home, d))
	}
	return existing(c, false)
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
