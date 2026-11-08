//go:build !windows

// This file implements the POSIX transport: a unix domain socket file inside the
// host's state directory. Every non-Windows platform gets it; the Windows
// counterpart is endpoint_windows.go (named pipes).
package hostpath

import (
	"crypto/sha1"
	"encoding/hex"
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

// Listen binds endpoint and restricts it to its owner.
//
// The socket mode is the primary defense on POSIX (any local user could
// otherwise connect); it is set before the listener accepts its first client
// because Listen has not started serving yet.
func Listen(endpoint string) (net.Listener, error) {
	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("host: chmod socket: %w", err)
	}
	return ln, nil
}

// Dial connects to a host listening on endpoint.
func Dial(endpoint string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", endpoint, timeout)
}

// socketArtifact returns the on-disk leftovers of a host's transport: the
// socket file itself, which a crashed host does not remove.
func socketArtifact(dir string) []string { return []string{SocketPath(dir)} }

// CleanEndpoint removes a stale endpoint left behind by a crashed host. A live
// host is not affected: Listen calls this only after Probe said nobody answers.
func CleanEndpoint(endpoint string) { _ = os.Remove(endpoint) }
