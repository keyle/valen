package main

// Prompt-before-run for shell. The OS sandbox (sandbox.go) bounds WHERE a
// command can write and read ($HOME and shared /tmp fail closed) and whether
// it can reach the network; this file covers WHAT it does inside those
// bounds. Network use, a short list of destructive commands (each once per
// session) and explicit outside-workdir paths (once per folder via
// checkFileAccess, so `cat /etc/passwd` or `cat ~/notes` ask like `git push`
// does while `go test -run 'Test' .` and shared /tmp never prompt) ask first.
// The command-text match is best-effort (a script can hide `git push`); the
// sandbox is the boundary, this is the speed bump for honest mistakes.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/keyle/valen/internal/dvlog"
)

// destructive maps a label (also the remembered key) to a command pattern.
var destructive = []struct {
	label string
	re    *regexp.Regexp
}{
	{"git push", regexp.MustCompile(`\bgit\b[^|;&\n]*\bpush\b`)},
	{"git reset --hard", regexp.MustCompile(`\bgit\b[^|;&\n]*\breset\b[^|;&\n]*--hard`)},
	{"git clean", regexp.MustCompile(`\bgit\b[^|;&\n]*\bclean\b`)},
	{"rm -r", regexp.MustCompile(`\brm\b[^|;&\n]*\s-[a-zA-Z]*[rR]|\brm\b[^|;&\n]*--recursive`)},
	{"sudo", regexp.MustCompile(`(^|[|;&(]\s*)sudo\b`)},
}

var (
	shellMu       sync.Mutex
	shellApproved = map[string]bool{} // "network" or a destructive label
	shellDenied   = map[string]bool{}
)

func resetShellApprovals() {
	shellMu.Lock()
	shellApproved = map[string]bool{}
	shellDenied = map[string]bool{}
	shellMu.Unlock()
}

// destructiveLabels returns the labels of every pattern command matches.
func destructiveLabels(command string) []string {
	var out []string
	for _, d := range destructive {
		if d.re.MatchString(command) {
			out = append(out, d.label)
		}
	}
	return out
}

// gateShell asks (once per session per label) before network use or a
// destructive command, and (once per folder) before touching paths outside
// the workdir. Safe build commands (`go test -run 'Test' .`, `go vet`,
// `gofmt`) contain no outside paths and never prompt. Without an interactive
// frontend outside access is denied.
func gateShell(ctx context.Context, command string, network bool) error {
	need := destructiveLabels(command)
	if network {
		need = append(need, "network")
	}
	for _, label := range need {
		if err := approveShell(ctx, label, command); err != nil {
			return err
		}
	}
	op := "use"
	if shellLooksWrite(command) {
		op = "write"
	}
	ctx = withToolCall(ctx, "shell: "+oneLine(command, 300))
	for _, abs := range shellOutsidePaths(command) {
		if err := checkFileAccess(ctx, op, abs); err != nil {
			return err
		}
	}
	return nil
}

// fdDupRe matches file-descriptor duplication (`2>&1`, `>&2`, `>&-`), which
// is not a write to a file. Anything else containing `>` is a redirect.
var fdDupRe = regexp.MustCompile(`\d*>&\d*-?`)

// writeCmdRe matches write-capable commands at a command position (after the
// start, a pipe, or a separator), so `grep form x` is not mistaken for `rm`.
var writeCmdRe = regexp.MustCompile(`(^|[|;&(]\s*)(tee|cp|mv|mkdir|touch|rm|chmod|chown|ln|truncate|dd|install)\b`)

// shellLooksWrite reports whether a command likely writes to an outside path
// (a file redirect or a write-capable command), so approving it grants read
// and write rather than read only. Best-effort like the rest of the gate; the
// kernel is the boundary.
func shellLooksWrite(command string) bool {
	if strings.Contains(fdDupRe.ReplaceAllString(command, ""), ">") {
		return true
	}
	return writeCmdRe.MatchString(command)
}

func approveShell(ctx context.Context, label, command string) error {
	if allow, deny := accessShellRule(label); deny {
		return fmt.Errorf("denied: %q is blocked by %s; do not retry or work around it", label, accessPathLabel())
	} else if allow {
		return nil
	}
	shellMu.Lock()
	ok := shellApproved[label]
	shellMu.Unlock()
	if ok {
		return nil
	}
	if askPrompter == nil {
		return fmt.Errorf("denied: %q needs approval and there is no interactive prompter", label)
	}
	unlock, err := lockGate(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	shellMu.Lock()
	ok, denied := shellApproved[label], shellDenied[label]
	shellMu.Unlock()
	if ok {
		return nil
	}
	if denied {
		return fmt.Errorf("denied: the user did not allow %q earlier this session; do not retry or work around it", label)
	}
	what := "run `" + label + "`"
	if label == "network" {
		what = "give shell commands network access"
	}
	name := strings.TrimSpace(currentTurnConfig(ctx, Config{}).Model)
	if name == "" {
		name = "valen"
	}
	choice, err := promptAccess(ctx, AskQuestion{
		Question: fmt.Sprintf("Allow %s to %s? Command: %s", name, what, truncateRunes(command, 200)),
		Header:   "Shell",
	}, label, "[shell] allow = "+label, "[shell] deny = "+label)
	if err != nil {
		return err
	}
	if choice.always() {
		rule := ruleShellDeny
		if choice.allowed() {
			rule = ruleShellAllow
		}
		if err := saveAccessRule(rule, label); err != nil {
			dvlog.Logf("access: persist %s %s: %v", rule, label, err)
		}
	}
	if choice.allowed() {
		shellMu.Lock()
		shellApproved[label] = true
		shellMu.Unlock()
		return nil
	}
	shellMu.Lock()
	shellDenied[label] = true
	shellMu.Unlock()
	return fmt.Errorf("denied: the user did not allow %q; do not retry or work around it", label)
}

// outsidePathRe finds candidate absolute paths and ~-home references in a
// shell command. Best-effort like destructiveLabels: quoted paths match,
// obfuscated paths (built via $vars, command substitution) do not — the OS
// sandbox still blocks unapproved HOME writes, credential files, .env secrets
// and (Darwin) /etc reads, so a bypass fails closed instead of leaking.
// shellObfuscatedPath reports dynamic absolute-path construction the regex
// above cannot see (`S=$(printf '\057'); cat ${S}etc/passwd`). /etc is the only
// tree hidden wholesale, so a marker alone proves nothing: ordinary expansion
// (`git diff ${BASE}`, `git show ${REF}`) must not ask to read /etc. Flag only
// when the command also names etc; an obfuscated /etc read that slips past
// still hits the kernel deny and fails closed.
func shellObfuscatedPath(command string) bool {
	if !strings.Contains(strings.ToLower(command), "etc") {
		return false
	}
	for _, m := range []string{"printenv", "printf", "eval ", "`", "${", "\\057", "\\x2f", "$'"} {
		if strings.Contains(command, m) {
			return true
		}
	}
	return false
}

// outsidePathRe captures an absolute path (`/x/y`) or ~-home path that starts
// at a word boundary (group 1). Without the boundary `./...`, `foo/bar` and
// `HEAD~1` would match mid-word. `)` is deliberately not a boundary, so the
// `/` in `"$(pwd)/"` is a suffix, not a path.
var outsidePathRe = regexp.MustCompile("(?:^|[\\s\"'=:,(<>|;&])(~(?:/[^\\s'\"`;|&()<>$\\\\]*)?|/[^\\s'\"`;|&()<>$\\\\]+)")

// rootPathRe matches the filesystem root only as a whole shell word (`ls /`,
// `ls "/"`), never as the tail of `"$PWD"/` or `"$(pwd)/"`.
var rootPathRe = regexp.MustCompile(`(?:^|[\s;|&(])["']?/["']?(?:$|[\s;|&)])`)

// shellSafePrefixes are absolute prefixes that never trigger an outside
// prompt: device nodes (`-o /dev/null`), the system toolchain, and tool
// caches needed for builds, and shared /tmp (writable by the sandbox, the
// preferred place for worktrees). It also carries the harmless system config
// files the sandbox re-allows for reads (systemReadOnlyPaths), so `cat
// /etc/gitconfig` needs no approval while `/etc/passwd` still asks. $HOME is
// deliberately absent: explicit use asks once per folder via checkFileAccess.
func shellSafePrefixes() []string {
	var out []string
	for _, p := range []string{"/dev", "/usr", "/bin", "/sbin", "/opt", "/System", "/Library", sharedTmp} {
		out = append(out, p)
	}
	out = append(out, real(sharedTmp))
	if cd, err := os.UserCacheDir(); err == nil && cd != "" {
		out = append(out, real(cd))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, d := range []string{"go", ".cache", ".npm", ".cargo"} {
			out = append(out, real(filepath.Join(home, d)))
		}
		if gc := os.Getenv("GOCACHE"); gc != "" {
			out = append(out, real(gc))
		}
	}
	out = append(out, systemReadOnlyPaths()...)
	out = append(out, accessDirs(ruleRead)...)
	out = append(out, accessDirs(ruleWrite)...)
	return out
}

// shellOutsidePaths returns the de-duplicated absolute paths mentioned in
// command that lie outside the workdir, sibling worktrees, approved folders
// and the safe prefixes above. Relative paths (`.`, `./...`, `f.txt`) run
// inside the workdir (cmd.Dir = root) and never prompt.
func shellOutsidePaths(command string) []string {
	root := getFileRoot()
	home, _ := os.UserHomeDir()
	safe := shellSafePrefixes()
	seen := map[string]bool{}
	var out []string
	add := func(abs string) {
		if abs == "" || seen[abs] {
			return
		}
		seen[abs] = true
		if withinRoot(root, abs) || inSiblingWorktree(root, abs) {
			return
		}
		if allowedWithoutPrompt(root, abs) {
			return
		}
		if !accessDenied(abs) { // a persistent deny must reach checkFileAccess
			for _, s := range safe {
				if s != "" && withinRoot(s, abs) {
					return
				}
			}
		}
		out = append(out, abs)
	}
	if rootPathRe.MatchString(command) {
		add("/")
	}
	for _, sm := range outsidePathRe.FindAllStringSubmatch(command, -1) {
		m := sm[1]
		var abs string
		switch {
		case m == "~" || strings.HasPrefix(m, "~/"):
			if home == "" {
				continue
			}
			abs = filepath.Clean(filepath.Join(home, strings.TrimPrefix(m, "~")))
		case strings.HasPrefix(m, "/"):
			abs = filepath.Clean(m)
			if abs == "/" { // `//` as in sed s//x/; real root is rootPathRe's job
				continue
			}
			// `grep -v /vendor/` is a pattern, not a path: a real absolute
			// path starts in an existing top-level entry (/etc, /tmp,
			// /Users, ...). The sandbox is read-only for anything missing.
			if _, err := os.Lstat("/" + strings.SplitN(strings.TrimPrefix(abs, "/"), "/", 2)[0]); err != nil {
				continue
			}
		default:
			continue
		}
		if r := real(abs); r != "" {
			// Prefer the real path when it exists (macOS /tmp ->
			// /private/tmp), else the cleaned text.
			if _, err := os.Stat(abs); err == nil {
				abs = r
			}
		}
		add(abs)
	}
	for _, env := range []string{"$HOME", "${HOME}", "$TMPDIR", "${TMPDIR}"} {
		if !strings.Contains(command, env) {
			continue
		}
		switch env {
		case "$HOME", "${HOME}":
			if home != "" {
				add(real(home))
			}
		case "$TMPDIR", "${TMPDIR}":
			if t := os.TempDir(); t != "" {
				add(filepath.Clean(t))
			}
			add(filepath.Clean("/tmp"))
		}
	}
	// Dynamic path construction asks for /etc: the literal scan above misses
	// `$vars`/`$()`-built absolutes, and the kernel blocks /etc reads until
	// approved, so force the prompt here instead of a silent failure. Only when
	// the command names etc, so plain `${VAR}` expansion (`git diff "${BASE}"`)
	// never asks.
	if shellObfuscatedPath(command) {
		add(filepath.Clean("/etc"))
	}
	return out
}
