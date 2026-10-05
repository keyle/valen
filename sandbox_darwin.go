//go:build darwin

package main

import (
	"fmt"
	"os"
	"strings"
)

const sandboxExec = "/usr/bin/sandbox-exec" // absolute: no PATH lookup to hijack

// sandboxArgv wraps command in a Seatbelt profile. Paths go in as -D
// parameters, never into the profile text, so odd characters can't break out.
// In SBPL later rules win, so allows come after the denies they override:
// $HOME (and credential paths) are denied, then the safe subset (workdir,
// sibling worktrees, approved folders, private temp, tool caches) is
// re-allowed for both reads and writes (shared /tmp included).
func sandboxArgv(command string, p sandboxPolicy) (string, []string, error) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return "", nil, fmt.Errorf("shell sandbox unavailable: %s not found", sandboxExec)
	}
	var prof strings.Builder
	var defs []string
	prof.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	for i, h := range p.Hidden {
		fmt.Fprintf(&prof, "(deny file-read* file-write* (subpath (param \"H%d\")))\n", i)
		defs = append(defs, "-D", fmt.Sprintf("H%d=%s", i, h))
	}
	// System paths (e.g. /etc) ask first via gateShell and stay blocked until
	// approved: without this, obfuscated constructions (`S=$(printf '\057')`)
	// bypass the string-matching gate while the default-allow reads let them
	// through. Deny reads here; an approved folder is re-allowed below via
	// writableRoots (later rules win). Writes stay denied by the rule after
	// the writable allow.
	for i, s := range p.SystemHidden {
		fmt.Fprintf(&prof, "(deny file-read* (subpath (param \"S%d\")))\n", i)
		defs = append(defs, "-D", fmt.Sprintf("S%d=%s", i, s))
	}
	prof.WriteString("(allow file-write* (subpath \"/dev\")")
	for i, w := range p.Writable {
		fmt.Fprintf(&prof, " (subpath (param \"W%d\"))", i)
		defs = append(defs, "-D", fmt.Sprintf("W%d=%s", i, w))
	}
	prof.WriteString(")\n")
	// /etc stays read-only even when approved for reads (approved writes to
	// system paths must not succeed).
	for i := range p.SystemHidden {
		fmt.Fprintf(&prof, "(deny file-write* (subpath (param \"S%d\")))\n", i)
	}
	prof.WriteString("(allow file-read*")
	for i := range p.Writable {
		fmt.Fprintf(&prof, " (subpath (param \"W%d\"))", i)
	}
	for i, r := range p.Readable {
		fmt.Fprintf(&prof, " (subpath (param \"RW%d\"))", i)
		defs = append(defs, "-D", fmt.Sprintf("RW%d=%s", i, r))
	}
	prof.WriteString(")\n")
	// Re-deny the credential paths inside $HOME after the writable/read allows
	// above. A broad approval (reading a file directly in $HOME makes the whole
	// home a root) must not re-expose ~/.ssh, ~/.aws, ~/.config, histories:
	// later rules win, and those were allowed away above. $HOME itself is
	// skipped so safe subpaths (caches, approved folders) stay reachable;
	// sibling user homes live outside $HOME and stay approvable for reads.
	home, _ := os.UserHomeDir()
	homeReal := ""
	if home != "" {
		homeReal = real(home)
	}
	for i, hid := range p.Hidden {
		if hid == home || hid == homeReal || !withinRoot(home, hid) {
			continue
		}
		fmt.Fprintf(&prof, "(deny file-read* file-write* (subpath (param \"H%d\")))\n", i)
	}
	// Git stats its configs on every run; re-allow reads only (writes stay
	// denied) after the $HOME and /etc denies so `git status` works under the
	// sandbox. A directory allow is a subpath (read its contents), a file one a
	// literal.
	for i := range p.ReadOnly {
		kind := "literal"
		if fi, err := os.Stat(p.ReadOnly[i]); err == nil && fi.IsDir() {
			kind = "subpath"
		}
		fmt.Fprintf(&prof, "(allow file-read* (%s (param \"R%d\")))\n", kind, i)
		defs = append(defs, "-D", fmt.Sprintf("R%d=%s", i, p.ReadOnly[i]))
	}
	// Tools like git and go lstat every parent of the workdir (/Users/n, ...).
	// Denying $HOME above also denied that metadata, which made
	// `git worktree add` fail with "Invalid path '/Users/n'". Allow metadata
	// only (stat, not listing or reading) on the ancestors of writable roots
	// and of re-allowed read-only files (so /etc/gitconfig is reachable; /etc
	// itself stays unlistable and /etc/passwd unreadable).
	roots := append(append([]string{}, p.Writable...), p.ReadOnly...)
	for i, a := range ancestorDirs(roots) {
		fmt.Fprintf(&prof, "(allow file-read-metadata (literal (param \"A%d\")))\n", i)
		defs = append(defs, "-D", fmt.Sprintf("A%d=%s", i, a))
	}
	if !p.Network {
		// Deny all network ops, then re-allow outbound connections to
		// loopback. The blanket deny also covers bind/listen, so an
		// in-sandbox server (or an httptest-style test) cannot bind unless
		// network is on; only loopback egress works here.
		prof.WriteString("(deny network*)\n(allow network* (remote ip \"localhost:*\"))\n")
	}
	// .env files anywhere, except templates (keep in sync with isSecretFile).
	// Both reads and writes are denied: secrets must not leak out or be
	// clobbered through shell redirection.
	prof.WriteString(`(deny file-read* file-write* (regex #"/\.env$" #"/\.env\.[^/]*$"))` + "\n")
	prof.WriteString(`(allow file-read* file-write* (regex #"/\.env\.[^/]*\.(example|sample|template)$" #"/\.env\.(example|sample|template)$"))` + "\n")
	args := append([]string{"-p", prof.String()}, defs...)
	return sandboxExec, append(args, "sh", "-c", command), nil
}
