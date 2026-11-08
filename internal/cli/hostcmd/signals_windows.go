//go:build windows

// This file lists the events that shut a foreground host down on Windows.
package hostcmd

import "os"

// shutdownSignals returns the events a foreground `pigo serve` treats as "stop
// gracefully".
//
// Only Ctrl-C is listed. Windows has no SIGTERM: a console close or a logoff
// arrives as a separate handler that the Go runtime does not map onto
// os.Signal, and a detached host (the `--daemon` path) is stopped through the
// `server.shutdown` RPC — `pigo serve stop` — not through a signal. `Ctrl-Break`
// is deliberately absent too: it is the "hard stop" users expect to bypass
// cleanup.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
