//go:build windows

// This file implements the Windows counterpart of process-group isolation: there
// is no POSIX process group to signal, so a command is stopped by killing the
// shell process directly.
package agenttool

import "os/exec"

// configureProcessGroup is a no-op on Windows.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills the command's process on Windows.
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
