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
// In SBPL later rules win, so deny rules come after the allows they override.
func sandboxArgv(command string, p sandboxPolicy) (string, []string, error) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return "", nil, fmt.Errorf("shell sandbox unavailable: %s not found", sandboxExec)
	}
	var prof strings.Builder
	var defs []string
	prof.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	prof.WriteString("(allow file-write* (subpath \"/dev\")")
	for i, w := range p.Writable {
		fmt.Fprintf(&prof, " (subpath (param \"W%d\"))", i)
		defs = append(defs, "-D", fmt.Sprintf("W%d=%s", i, w))
	}
	prof.WriteString(")\n")
	if !p.Network {
		// Loopback stays up: dev servers and httptest-style tests keep working.
		prof.WriteString("(deny network*)\n(allow network* (remote ip \"localhost:*\"))\n")
	}
	for i, h := range p.Hidden {
		fmt.Fprintf(&prof, "(deny file-read* file-write* (subpath (param \"H%d\")))\n", i)
		defs = append(defs, "-D", fmt.Sprintf("H%d=%s", i, h))
	}
	// .env files anywhere, except templates (keep in sync with isSecretFile).
	prof.WriteString(`(deny file-read* (regex #"/\.env$" #"/\.env\.[^/]*$"))` + "\n")
	prof.WriteString(`(allow file-read* (regex #"/\.env\.[^/]*\.(example|sample|template)$" #"/\.env\.(example|sample|template)$"))` + "\n")
	args := append([]string{"-p", prof.String()}, defs...)
	return sandboxExec, append(args, "sh", "-c", command), nil
}
