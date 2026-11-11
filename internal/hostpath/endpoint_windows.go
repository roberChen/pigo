//go:build windows

// This file implements the Windows transport: a named pipe under the global
// \\.\pipe\ namespace.
//
// A unix socket would work on Windows 10 1803+, but its file-system path is
// capped at ~100 bytes, does not follow the "one endpoint per host" model, and
// leaves a stale inode behind on a crash. Named pipes are the native mechanism:
// they are inherently stream-oriented, disappear with their listener, support
// per-user ACLs, and are what every other Windows agent (Docker, VS Code, the
// OpenSSH service) uses.
package hostpath

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipePrefix is the Windows named-pipe namespace. It is global, so the name must
// not disclose where the user's state directory is: only a hash of it appears.
const pipePrefix = `\\.\pipe\`

// SocketPath returns the named-pipe name for the host state directory dir.
//
// The name is a hash of the absolute directory: pipe names are enumerable by any
// local user, so a readable "$PIGO_HOME/server.sock" would leak the user's
// directory layout to anyone who lists the namespace. The hash keeps the name
// short (well inside the 256-character pipe limit) and unique per PIGO_HOME,
// which is exactly the property a client needs to find its host.
func SocketPath(dir string) string {
	abs := dir
	if a, err := filepath.Abs(dir); err == nil {
		abs = a
	}
	// Windows paths are case-insensitive; the same directory spelled differently
	// must map to the same pipe.
	abs = strings.ToLower(filepath.Clean(abs))
	sum := sha1.Sum([]byte(abs))
	return pipePrefix + "pigo-" + hex.EncodeToString(sum[:8])
}

// Listen binds endpoint and restricts the pipe to its owner.
//
// A named pipe is reachable by any local user unless its DACL says otherwise, and
// unlike a file it has no directory permissions to inherit. The security
// descriptor therefore grants full access to the current user only, with a
// protected DACL so no inherited ACE widens it. FILE_PIPE_REJECT_REMOTE_CLIENTS
// (set by go-winio) keeps the pipe off the network share.
func Listen(endpoint string) (net.Listener, error) {
	sd, err := ownerOnlyPipeSDDL()
	if err != nil {
		return nil, fmt.Errorf("host: pipe acl: %w", err)
	}
	return winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: sd,
		// The protocol is NDJSON and a resume result can be a few MB; the default
		// 4 KiB pipe buffer would make every large line a series of round trips.
		InputBufferSize:  64 * 1024,
		OutputBufferSize: 64 * 1024,
	})
}

// Dial connects to a host listening on endpoint and verifies that the pipe
// belongs to the current user.
//
// Named pipes live in a global namespace: any local user can create
// \\.\pipe\pigo-<hash> before the real host does. A client that connected
// blindly would then send the bearer token to that process in hello. The owner
// of a pipe is fixed by the kernel to the user of the process that created it
// (changing it needs SeRestorePrivilege, which a normal local user does not
// have), so checking the owner rejects a squatter without any shared secret. A
// same-user impostor is out of scope: it can already read the token file.
func Dial(endpoint string, timeout time.Duration) (net.Conn, error) {
	conn, err := winio.DialPipe(endpoint, &timeout)
	if err != nil {
		return nil, err
	}
	if err := verifyPipeOwner(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// verifyPipeOwner rejects a connection whose server pipe is owned by another
// user, so the token is never handed to a name-squatting process.
func verifyPipeOwner(conn net.Conn) error {
	h, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("host: pipe connection does not expose its handle")
	}
	owner, err := pipeOwner(h.Fd())
	if err != nil {
		return err
	}
	want, err := currentUserSID()
	if err != nil {
		return err
	}
	if !ownerMatches(owner, want) {
		got := "unknown"
		if owner != nil {
			got = owner.String()
		}
		return fmt.Errorf("%w: pipe owner is %s", ErrEndpointOwner, got)
	}
	return nil
}

// pipeOwner returns the owner SID of an open pipe handle.
func pipeOwner(fd uintptr) (*windows.SID, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(fd), windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("host: pipe security: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, fmt.Errorf("host: pipe owner: %w", err)
	}
	return owner, nil
}

// ownerMatches reports whether a pipe's owner is the expected SID. A missing
// owner is never a match.
func ownerMatches(owner, want *windows.SID) bool {
	return owner != nil && want != nil && owner.Equals(want)
}

// IsEndpointInUse reports whether binding an endpoint failed because another
// listener already holds the name. A named pipe that exists rejects a second
// FILE_CREATE with ERROR_ACCESS_DENIED.
func IsEndpointInUse(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_PIPE_BUSY)
}

// socketArtifact returns the on-disk leftovers of a host's transport. A named
// pipe is not a file: it vanishes when its listener closes, so there is no stale
// endpoint to clean up.
func socketArtifact(string) []string { return nil }

// ownerOnlyPipeSDDL builds "D:P(A;;GA;;;<current user>)" — a protected DACL with
// one ACE granting generic-all to the process's user.
func ownerOnlyPipeSDDL() (string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	// P protects the DACL from inheritance; GA is generic-all.
	return "D:P(A;;GA;;;" + sid.String() + ")", nil
}

// CleanEndpoint is a no-op on Windows: a named pipe exists only while some
// process holds it, so a crashed host leaves nothing to remove.
func CleanEndpoint(string) {}
