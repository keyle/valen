//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the shell in its own process group so a timeout
// can kill the whole tree: pipeline grandchildren inherit stdout, and a
// plain child kill would leave Run waiting on them past the timeout.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	p := cmd.Process
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}
