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
	prof.WriteString(")\n")
	// Git stats its configs on every run; re-allow reads only (writes stay
	// denied) after the $HOME deny so `git status` works under the sandbox.
	for i := range p.ReadOnly {
		fmt.Fprintf(&prof, "(allow file-read* (literal (param \"R%d\")))\n", i)
		defs = append(defs, "-D", fmt.Sprintf("R%d=%s", i, p.ReadOnly[i]))
	}
	// Tools like git and go lstat every parent of the workdir (/Users/n, ...).
	// Denying $HOME above also denied that metadata, which made
	// `git worktree add` fail with "Invalid path '/Users/n'". Allow metadata
	// only (stat, not listing or reading) on the ancestors of writable roots.
	for i, a := range ancestorDirs(p.Writable) {
		fmt.Fprintf(&prof, "(allow file-read-metadata (literal (param \"A%d\")))\n", i)
		defs = append(defs, "-D", fmt.Sprintf("A%d=%s", i, a))
	}
	if !p.Network {
		// Loopback stays up: dev servers and httptest-style tests keep working.
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
