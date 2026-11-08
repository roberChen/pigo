// Package hostpath owns the host's on-disk footprint: the state directory, the
// transport endpoint (a unix socket or a named pipe), the bearer token and the
// informational state file.
//
// It is a package of its own so that a *client* can find a running host without
// importing the daemon — `hostserver` links the whole run assembly (providers,
// tools, MCP, memory), which a thin UI has no business pulling in. Keeping paths
// and permissions in one small place also means the security-relevant bits are
// reviewable together (docs/architecture.md §6.2).
package hostpath

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
