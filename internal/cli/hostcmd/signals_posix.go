//go:build !windows

// This file lists the signals that shut a foreground host down.
package hostcmd

import (
	"os"
	"syscall"
)

// shutdownSignals returns the signals a foreground `pigo serve` treats as "stop
// gracefully": Ctrl-C and the polite termination request a supervisor
// (systemd, launchd, docker stop) sends.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
