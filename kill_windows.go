//go:build windows

package main

import (
	"os"
	"os/exec"
)

// interruptSignals is just ctrl+c on Windows; SIGTERM has no handler there.
func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// Windows has no process groups: fall back to killing the shell itself.
// Pipeline orphans may delay Run past the timeout there.
func isolateProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
