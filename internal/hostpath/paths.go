// Package hostpath owns the host's on-disk footprint: the state directory, the
// unix socket path, the bearer token and the informational state file.
//
// It is a package of its own so that a *client* can find a running host without
// importing the daemon — `hostserver` links the whole run assembly (providers,
// tools, MCP, memory), which a thin UI has no business pulling in. Keeping paths
// and permissions in one small place also means the security-relevant bits are
// reviewable together (docs/architecture.md §6.2).
package hostpath

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// maxSocketPath is a conservative bound on sockaddr_un.sun_path. Linux allows
// 108 bytes, macOS 104; staying under the smaller limit keeps the same layout
// working everywhere.
const maxSocketPath = 100

// DefaultDir is the host's state directory: $PIGO_HOME, or ~/.pigo. It matches
// the trust store and the session store so one PIGO_HOME moves all of pigo's
// per-user state.
func DefaultDir() string {
	if dir := os.Getenv("PIGO_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pigo")
}

// SocketPath returns the unix socket path for the host state directory dir.
//
// A unix socket path is capped at ~104 bytes, and $PIGO_HOME can easily be
// longer (CI temp dirs, deep home directories). Rather than fail at listen time
// with a cryptic "invalid argument", a too-long path is replaced by a hashed
// name in the OS temp dir. The hash covers the absolute directory, so two
// different PIGO_HOMEs never collide. The candidate roots are tried shortest
// first, because $TMPDIR itself can be long enough to blow the limit.
func SocketPath(dir string) string {
	path := filepath.Join(dir, "server.sock")
	if len(path) <= maxSocketPath {
		return path
	}
	sum := sha1.Sum([]byte(dir))
	name := "pigo-" + hex.EncodeToString(sum[:8]) + ".sock"
	// os.TempDir() normally fits, but a long $TMPDIR does not; the fixed roots
	// are the standard short ones. The choice depends only on the environment,
	// so a client and its host compute the same path.
	candidate := ""
	for _, root := range socketRoots() {
		p := filepath.Join(root, name)
		if len(p) <= maxSocketPath {
			return p
		}
		if candidate == "" {
			candidate = p
		}
	}
	// Nothing fits: return the first candidate rather than a path the caller
	// cannot distinguish from a working one. listen will fail with a clear error.
	return candidate
}

// socketRoots lists the directories a hashed socket may live in, shortest
// first. os.TempDir() is first so the common case is unchanged; the literals are
// the conventional short temp roots on unix.
func socketRoots() []string {
	roots := []string{os.TempDir()}
	for _, root := range []string{"/tmp", "/var/tmp"} {
		dup := false
		for _, have := range roots {
			if have == root {
				dup = true
				break
			}
		}
		if !dup {
			roots = append(roots, root)
		}
	}
	return roots
}

// TokenPath returns the bearer-token file path for dir.
func TokenPath(dir string) string { return filepath.Join(dir, "server.token") }

// StatePath returns the state-file path for dir.
func StatePath(dir string) string { return filepath.Join(dir, "server.json") }

// State is the content of the state file. It is informational only: every
// mutating request still requires a live connection and the token, so a stale
// or tampered state file cannot be used to act on the host.
type State struct {
	PID       int       `json:"pid"`
	Version   int       `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Socket    string    `json:"socket"`
}

// ReadState loads the state file, returning os.ErrNotExist when no host has
// written one.
func ReadState(dir string) (State, error) {
	data, err := os.ReadFile(StatePath(dir))
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("host: parse %s: %w", StatePath(dir), err)
	}
	return st, nil
}

// WriteState atomically writes the state file.
func WriteState(dir string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(StatePath(dir), data, 0o600)
}

// WriteFileAtomic writes data to path with mode via a temp file + rename, so a
// reader never observes a half-written token or state file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NewToken returns a fresh random bearer token (32 bytes, hex encoded).
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("host: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// WriteToken writes token to dir with owner-only permissions, replacing any
// token from a previous run.
func WriteToken(dir, token string) error {
	return WriteFileAtomic(TokenPath(dir), []byte(token+"\n"), 0o600)
}

// ReadToken reads the token written by a running host.
func ReadToken(dir string) (string, error) {
	data, err := os.ReadFile(TokenPath(dir))
	if err != nil {
		return "", err
	}
	token := string(data)
	for len(token) > 0 && (token[len(token)-1] == '\n' || token[len(token)-1] == '\r') {
		token = token[:len(token)-1]
	}
	return token, nil
}

// TokenMatches compares in constant time so a client cannot learn the token
// prefix from response timing.
func TokenMatches(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// Probe reports whether a live host is listening on socket. A dial failure means
// the file is stale (a crashed host leaves the socket behind), which the caller
// resolves by removing it.
func Probe(socket string, timeout time.Duration) bool {
	if socket == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// RemoveAll removes the host's socket, state and token files. Missing files are
// not an error: shutdown after a crash still tidies what is left.
func RemoveAll(dir string) {
	for _, path := range []string{SocketPath(dir), StatePath(dir), TokenPath(dir)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Best effort: a leftover file must not turn a clean shutdown into a
			// failure (the next start overwrites it).
			_ = err
		}
	}
}
