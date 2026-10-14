package main

// Shell sandbox. Every `shell` command runs under an OS-enforced policy:
//
//   - writes only inside the workdir, sibling git worktrees, folders the user
//     approved for write this session (see checkFileAccess), shared /tmp, the
//     private temp dir and tool caches. Read approvals re-expose reads only;
//     a read approval never grants writes;
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
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// sharedTmp is the shared scratch dir shell may use without asking.
const sharedTmp = "/tmp"

// sandboxPolicy is what one command may do. Root is the workdir.
type sandboxPolicy struct {
	Root         string
	Writable     []string // existing dirs shell may write to
	Readable     []string // existing dirs shell may read but not write (read-only approvals)
	ReadOnly     []string // existing files/dirs shell may read but not write (git + system configs)
	Hidden       []string // existing files/dirs shell cannot read or write
	SystemHidden []string // system paths shell cannot read (approvable via folder approval)
	Network      bool
	AllowBind    bool     // loopback bind/listen even with Network off (Darwin only)
	IPCPaths     []string // existing unix sockets (docker) shell may connect to
}

// SandboxConfig mirrors the optional [sandbox] table. A nil list keeps the
// built-in default; an empty one (`[]`) switches that default off.
type SandboxConfig struct {
	Allow     []string `toml:"allow"`      // extra read+write roots
	Deny      []string `toml:"deny"`       // extra hidden paths; win over every allow
	AllowBind []string `toml:"allow_bind"` // loopback addresses; default on
	IPCAllow  []string `toml:"ipc_allow"`  // unix sockets; replaces the docker defaults
}

var sandboxCfg atomic.Pointer[SandboxConfig]

// setSandboxConfig installs the [sandbox] table, resolving its paths once.
func setSandboxConfig(c SandboxConfig) {
	c.Allow, c.Deny, c.IPCAllow = normPaths(c.Allow), normPaths(c.Deny), normPaths(c.IPCAllow)
	sandboxCfg.Store(&c)
}

func sandboxConf() SandboxConfig {
	if c := sandboxCfg.Load(); c != nil {
		return *c
	}
	return SandboxConfig{}
}

// normPaths resolves ~/ and symlinks; nil stays nil (default), the rest is non-nil.
func normPaths(in []string) []string {
	if in == nil {
		return nil
	}
	out := []string{}
	for _, p := range in {
		if n := normalizeAccessPath(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// validate rejects relative paths and non-loopback bind addresses.
func (c SandboxConfig) validate() error {
	for _, l := range [][]string{c.Allow, c.Deny, c.IPCAllow} {
		for _, p := range l {
			if p != "~" && !strings.HasPrefix(p, "~/") && !filepath.IsAbs(p) {
				return fmt.Errorf("sandbox: path %q must be absolute or start with ~/", p)
			}
		}
	}
	for _, a := range c.AllowBind {
		if ip := net.ParseIP(a); a != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("sandbox: allow_bind %q is not a loopback address", a)
		}
	}
	return nil
}

// allowBind: loopback listeners are on unless allow_bind = [].
func allowBind() bool {
	b := sandboxConf().AllowBind
	return b == nil || len(b) > 0
}

// ipcPaths lists the existing unix sockets shell may connect to (docker by
// default: colima, Docker Desktop, the system socket), minus denied folders.
func ipcPaths() []string {
	list := sandboxConf().IPCAllow
	if list == nil {
		list = []string{"/var/run/docker.sock"}
		if home, _ := os.UserHomeDir(); home != "" {
			for _, s := range []string{".colima/default/docker.sock", ".colima/docker.sock", ".docker/run/docker.sock"} {
				list = append(list, filepath.Join(home, s))
			}
		}
	}
	var out []string
	for _, p := range list {
		if p = real(p); !accessDenied(p) {
			out = append(out, p)
		}
	}
	return existing(out, false)
}

// homeToolDirs are the $HOME toolchain/cache dirs shell may write and that
// never prompt. They stay subject to dropBroadRoots.
var homeToolDirs = []string{"go", ".cache", ".npm", ".cargo", "fvm", ".fvm", ".pub-cache", ".rustup", ".gradle"}

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

// maxEnvScan bounds the .env search under the workdir.
const maxEnvScan = 20000

// findSecretFiles lists the secret .env files under root (skipping vendored
// dirs) plus every other name for the same file: a hardlink such as
// notes.txt has no .env in its path, so the name-based Seatbelt regex cannot
// see it and bubblewrap has nothing to mask. Both backends deny or mask the
// returned paths literally. Symlinks need no entry: the kernel resolves them
// to the denied target.
func findSecretFiles(root string) []string {
	var secrets []string
	var infos []os.FileInfo
	walk := func(visit func(path string, d fs.DirEntry)) {
		n := 0
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if n++; n > maxEnvScan {
				return filepath.SkipAll
			}
			if d.IsDir() {
				if skippedDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			visit(p, d)
			return nil
		})
	}
	walk(func(p string, d fs.DirEntry) {
		if !isSecretFile(p) {
			return
		}
		secrets = append(secrets, real(p))
		if info, err := d.Info(); err == nil {
			infos = append(infos, info)
		}
	})
	if len(infos) == 0 {
		return secrets // common case: no .env, so no second pass or lstat
	}
	walk(func(p string, d fs.DirEntry) {
		if isSecretFile(p) || !d.Type().IsRegular() {
			return
		}
		info, err := d.Info()
		if err != nil {
			return
		}
		for _, s := range infos {
			if os.SameFile(info, s) {
				secrets = append(secrets, real(p))
				return
			}
		}
	})
	return secrets
}

// sandboxHint is appended to a failed command's output when it looks like a
// sandbox denial, so the model knows what to do instead of retrying blindly.
const sandboxHint = "[sandbox: shell may only write inside the workdir, sibling git worktrees, approved folders, /tmp, the private temp dir and tool caches ($HOME is blocked, as is any folder in access.toml deny). To get another folder, call glob or read on it (the user is asked once per folder), then re-run.]"

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
		Root: getFileRoot(), Writable: writableRoots(), Readable: readableRoots(),
		ReadOnly: append(gitReadOnlyPaths(), systemReadOnlyPaths()...),
		Hidden:   hiddenPaths(), SystemHidden: systemHiddenPaths(), Network: network,
		AllowBind: allowBind(), IPCPaths: ipcPaths(),
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
// The match is token-based (see sensitiveEnvKey) so PATH != PAT and
// SSH_AUTH_SOCK != AUTH.
func sensitiveEnvKey(key string) bool {
	u := strings.ToUpper(key)
	switch u {
	// Not secret despite a matching token: keep so git/ssh keep working.
	case "SSH_AUTH_SOCK", "SSH_AGENT_PID", "PWD", "OLDPWD":
		return false
	}
	for _, s := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE"} {
		if strings.Contains(u, s) {
			return true
		}
	}
	for _, t := range strings.FieldsFunc(u, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/' || r == ':'
	}) {
		switch t {
		case "PASS", "PWD", "PAT", "AUTH", "URL", "DSN", "SK", "BEARER":
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
// Environment-derived roots (TMPDIR, GOCACHE) that would re-expose $HOME are
// dropped; explicit user approvals are honored even when broad.
func writableRoots() []string {
	root := getFileRoot()
	home, _ := os.UserHomeDir()
	c := []string{root, os.TempDir(), sharedTmp, os.Getenv("GOCACHE")}
	if cd, err := os.UserCacheDir(); err == nil {
		c = append(c, cd)
	}
	if home != "" {
		for _, d := range homeToolDirs {
			c = append(c, filepath.Join(home, d))
		}
	}
	c = dropBroadRoots(c, home)
	approvedMu.Lock()
	for dir := range approvedWritable {
		c = append(c, dir)
	}
	approvedMu.Unlock()
	c = append(c, accessDirs(ruleWrite)...) // "FOR ALL SESSIONS" grants
	c = append(c, siblingWorktrees(root)...)
	return dropDeniedRoots(existingDirs(c), root)
}

// dropDeniedRoots removes allowed roots that lie inside a persistently denied
// folder: deny wins over any allow, session or persistent. The workdir itself
// is never dropped.
func dropDeniedRoots(paths []string, root string) []string {
	deny := accessDirs(ruleDeny)
	if len(deny) == 0 {
		return paths
	}
	out := paths[:0]
	for _, p := range paths {
		drop := false
		for _, d := range deny {
			if inApproved(d, p) && p != real(root) {
				drop = true
			}
		}
		if !drop {
			out = append(out, p)
		}
	}
	return out
}

// readableRoots lists folders approved for read this session; writable
// approvals are already re-exposed by writableRoots.
func readableRoots() []string {
	approvedMu.Lock()
	c := make([]string, 0, len(approvedRead))
	for dir := range approvedRead {
		c = append(c, dir)
	}
	approvedMu.Unlock()
	c = append(c, accessDirs(ruleRead)...)
	return dropDeniedRoots(existingDirs(c), getFileRoot())
}

// dropBroadRoots removes implicit roots that equal $HOME or contain it (an
// ancestor such as GOCACHE=$HOME or a TMPDIR above the home dir). Binding one
// would erase the home confinement.
func dropBroadRoots(paths []string, home string) []string {
	if home == "" {
		return paths
	}
	h := real(home)
	out := paths[:0]
	for _, p := range paths {
		if p == "" {
			continue
		}
		rp := real(p)
		if rp == h || withinRoot(rp, h) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ancestorDirs returns the sorted, de-duplicated proper ancestors of dirs
// (excluding "/"), e.g. /home/user/code for /home/user/code/valen.
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
	// Persistent denials (access.toml) hide the folder from the kernel too;
	// they sit in the hidden list, which is applied after every allow.
	c = append(c, accessDirs(ruleDeny)...)
	// Sibling user homes: only the current user's home is denied above, so
	// other accounts' dirs (and their credentials) would otherwise stay
	// readable by default. Hide them too, except one that contains the
	// workdir. Restricted to the standard /Users and /home parents so temp
	// dirs that merely happen to be siblings are left alone.
	parent := filepath.Dir(home)
	if parent != "/" && parent != home && (filepath.Base(parent) == "Users" || filepath.Base(parent) == "home") {
		root := real(getFileRoot())
		if entries, err := os.ReadDir(parent); err == nil {
			for _, e := range entries {
				p := filepath.Join(parent, e.Name())
				if !e.IsDir() || real(p) == real(home) || withinRoot(real(p), root) {
					continue
				}
				c = append(c, p)
			}
		}
	}
	return existing(c, false)
}

// gitReadOnlyPaths lists git config files the sandbox re-allows for reads
// only. Git stats these on every invocation (even `git status` in the
// workdir), and hiding $HOME made git fail with "unable to access
// '/home/user/.gitconfig': Operation not permitted". Later SBPL rules win, so
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
// via readableRoots on a read approval (later rules win on Darwin; bind after
// mask on Linux).
func systemHiddenPaths() []string {
	return existing([]string{"/etc", real("/etc")}, true)
}

// systemReadOnlyPaths lists harmless system config files the sandbox re-allows
// for reads and the shell gate never prompts for. /etc is denied wholesale on
// Darwin (so obfuscated reads of /etc/passwd fail closed), but a few
// world-readable files are still needed: a system gitconfig and path list, and
// the TLS cert bundle. They are re-allowed by path, like gitReadOnlyPaths for
// $HOME; everything else under /etc stays hidden. Only existing paths are
// listed (bwrap cannot bind a missing one).
func systemReadOnlyPaths() []string {
	return existing([]string{
		"/etc/gitconfig",
		"/etc/paths",
		"/etc/paths.d",
		"/etc/ssl/cert.pem",
	}, false)
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
