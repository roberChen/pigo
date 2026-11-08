// This file defines the host's transport endpoint and the platform-neutral
// operations on it. The concrete transport is a unix domain socket on POSIX
// (endpoint_posix.go) and a named pipe on Windows (endpoint_windows.go), but
// everything else — server, client, CLI — deals in an opaque endpoint *string*
// and calls Listen/Dial here, so the two platforms cannot drift apart and a UI
// never has to know which one it is running on.
//
// The string stays human-readable on both: an absolute path ending in
// server.sock on POSIX, and \\.\pipe\pigo-<hash> on Windows. It is what the
// state file records and what `pigo serve status` prints.
package hostpath

import (
	"errors"
	"os"
	"time"
)

// Probe reports whether a live host is listening on endpoint. A failed dial
// means nobody is there: on POSIX a crashed host can leave the socket file
// behind, and on Windows the pipe simply does not exist once its listener closes.
func Probe(endpoint string, timeout time.Duration) bool {
	if endpoint == "" {
		return false
	}
	conn, err := Dial(endpoint, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Footprint lists every artifact a host leaves behind in dir, sorted so callers
// that print or poll them get a stable order. On POSIX that includes the socket
// file; on Windows the pipe is not a file, so only the token and state file
// appear.
func Footprint(dir string) []string {
	return append(socketArtifact(dir), StatePath(dir), TokenPath(dir))
}

// RemoveAll removes the host's endpoint, state and token. Missing entries are
// not an error: shutdown after a crash still tidies what is left.
func RemoveAll(dir string) {
	for _, path := range Footprint(dir) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Best effort: a leftover file must not turn a clean shutdown into a
			// failure (the next start replaces it).
			continue
		}
	}
}
