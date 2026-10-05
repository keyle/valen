//go:build linux

package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// maxEnvScan bounds the .env search under the workdir.
const maxEnvScan = 20000

// sandboxArgv wraps command in bubblewrap: the whole filesystem read-only,
// $HOME masked (reads/writes ask first), writable binds re-expose the safe
// subset (workdir, worktrees, approved, private temp, tool caches), sensitive
// paths stay masked, and a private network namespace (loopback only) unless
// the policy allows network.
func sandboxArgv(command string, p sandboxPolicy) (string, []string, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return "", nil, fmt.Errorf("shell sandbox unavailable: bubblewrap (bwrap) not found on PATH; install it (e.g. apt install bubblewrap)")
	}
	args := []string{"--ro-bind", "/", "/", "--dev", "/dev"}
	home, _ := os.UserHomeDir()
	if home != "" {
		if r := real(home); r != "" {
			args = append(args, maskArgs(r)...)
		}
	}
	for _, w := range p.Writable {
		args = append(args, "--bind", w, w)
	}
	for _, h := range p.Hidden {
		if home != "" && (h == home || h == real(home)) {
			continue
		}
		args = append(args, maskArgs(h)...)
	}
	// bwrap has no patterns: mask the .env files that exist right now.
	for _, f := range findEnvFiles(p.Root) {
		args = append(args, maskArgs(f)...)
	}
	if !p.Network {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--die-with-parent", "--new-session", "--", "sh", "-c", command)
	return bwrap, args, nil
}

// maskArgs hides a dir behind an empty tmpfs and a file behind /dev/null.
func maskArgs(path string) []string {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return []string{"--tmpfs", path}
	}
	return []string{"--ro-bind", "/dev/null", path}
}

// findEnvFiles lists secret .env files under root (skipping vendored dirs).
func findEnvFiles(root string) []string {
	var out []string
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
		if isSecretFile(p) {
			out = append(out, real(p))
		}
		return nil
	})
	return out
}
