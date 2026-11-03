//go:build !windows

// This file implements Unix process-group isolation for shell commands. Starting
// a command in its own group means a timeout or Ctrl-C can kill the whole tree
// (the shell plus anything it spawned) instead of orphaning grandchildren, and a
// background job is not collateral damage of a signal aimed at the foreground.
package agenttool

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts cmd in a new process group so killProcessTree can
// signal the whole tree.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree SIGKILLs cmd's process group, falling back to the process
// itself when the group is already gone (or was never created).
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
