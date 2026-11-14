//go:build !windows

// This file lists the signals that shut a foreground host down.
package hostcmd

import (
	"os"
	"syscall"
)

// shutdownSignals returns the signals a foreground `pigo serve` treats as "stop
// gracefully": Ctrl-C, the polite termination request a supervisor (systemd,
// launchd, docker stop) sends, and SIGHUP from a terminal that went away.
//
// A detached host is in its own session (detach_posix.go), so it never sees the
// terminal's SIGHUP; it is stopped through the `server.shutdown` RPC instead.
// Treating SIGHUP as "stop" is what keeps a foreground host from dying dirty and
// leaving its socket and token behind when the terminal closes.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}
