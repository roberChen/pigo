// This file keeps the daemon's local names for the host's on-disk footprint.
// The implementation lives in internal/hostpath so that a thin client can find a
// running host without importing the daemon (which links the whole run
// assembly); the aliases below keep the server's call sites reading naturally.
package hostserver

import (
	"os"
	"time"

	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostpath"
)

// DefaultDir is the host's state directory (see hostpath.DefaultDir).
func DefaultDir() string { return hostpath.DefaultDir() }

// SocketPath returns the unix socket path for dir (see hostpath.SocketPath).
func SocketPath(dir string) string { return hostpath.SocketPath(dir) }

// TokenPath returns the bearer-token file path for dir.
func TokenPath(dir string) string { return hostpath.TokenPath(dir) }

// StatePath returns the state-file path for dir.
func StatePath(dir string) string { return hostpath.StatePath(dir) }

// State is the content of the state file.
type State = hostpath.State

// ReadState loads the state file.
func ReadState(dir string) (State, error) { return hostpath.ReadState(dir) }

// NewToken returns a fresh random bearer token.
func NewToken() (string, error) { return hostpath.NewToken() }

// WriteToken writes token to dir with owner-only permissions.
func WriteToken(dir, token string) error { return hostpath.WriteToken(dir, token) }

// ReadToken reads the token written by a running host.
func ReadToken(dir string) (string, error) { return hostpath.ReadToken(dir) }

// tokenMatches compares a presented token with the host's in constant time.
func tokenMatches(want, got string) bool { return hostpath.TokenMatches(want, got) }

// probeExisting reports whether a live host already listens on socket.
func probeExisting(socket string, timeout time.Duration) bool { return hostpath.Probe(socket, timeout) }

// cleanup removes the host's socket, state and token files.
func cleanup(dir string) { hostpath.RemoveAll(dir) }

// writeFileAtomic writes a file atomically (used for the state file).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return hostpath.WriteFileAtomic(path, data, mode)
}

// Version is re-exported for callers that only import hostserver (the CLI), so
// they need not also import hostapi for the number.
const Version = hostapi.Version
