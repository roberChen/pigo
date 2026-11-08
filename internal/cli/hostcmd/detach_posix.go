//go:build !windows

// This file implements daemon detachment on POSIX: Setsid puts the child in a
// new session with no controlling terminal, so Ctrl-C, SIGHUP on terminal close
// and process-group signals aimed at the parent all miss it.
package hostcmd

import (
	"os/exec"
	"syscall"
)

// detachProcess makes cmd survive its parent's terminal and process group. The
// host owns its own shutdown (`pigo serve stop`), so nothing else may kill it.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
