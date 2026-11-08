//go:build windows

// This file implements daemon detachment on Windows.
//
// There is no session concept to escape. What matters is that the child is not
// in the parent's console process group (so Ctrl-C in that console does not
// reach it) and that it gets no console window of its own: DETACHED_PROCESS is
// exactly that combination, and it is also why the child's stdin/stdout must not
// be the parent's — they are redirected to the log file before Start.
package hostcmd

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcess makes cmd survive the console that launched it.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
