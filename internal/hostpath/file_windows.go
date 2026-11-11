//go:build windows

// This file implements the Windows file primitives WriteFileAtomic needs:
// replacing a file another handle may have open, and giving it an owner-only
// DACL. os.Chmod only toggles the read-only attribute on Windows, so the 0600
// callers ask for has to be expressed as an ACL.
package hostpath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// maxPath is the conventional threshold below which a path does not need the
// extended-length prefix. Windows documents 260 as MAX_PATH, but the kernel is
// only reliably happy below 248 (the limit must leave room for an 8.3 name).
const maxPath = 248

// replaceFile renames old to new, retrying while Windows refuses to replace an
// open target.
//
// MoveFileEx cannot replace a file that another handle has open without
// FILE_SHARE_DELETE, and Go opens files without it. A client reading the token
// or state file at the wrong moment therefore makes the rename fail with
// ERROR_ACCESS_DENIED even though the data is complete. Those readers close
// within microseconds, so a short bounded retry hides the race instead of
// failing a host start or a token rotation.
func replaceFile(old, new string) error {
	const (
		attempts = 200
		delay    = 2 * time.Millisecond
	)
	var err error
	for i := 0; i < attempts; i++ {
		err = os.Rename(old, new)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) &&
			!errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		time.Sleep(delay)
	}
	return err
}

// removeFile deletes path, retrying while a reader holds it open. Shutdown
// removes the token and state file while a concurrent `serve status` may still
// have one open; without the retry the files survive the exit and a later
// `serve stop` reports a host that will never go away.
func removeFile(path string) error {
	const (
		attempts = 200
		delay    = 2 * time.Millisecond
	)
	var err error
	for i := 0; i < attempts; i++ {
		err = os.Remove(path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) &&
			!errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		time.Sleep(delay)
	}
	return err
}

// secureFile gives path a protected DACL that grants the current user full
// access and nobody else anything.
//
// It is the Windows equivalent of the 0600 the caller asked for: without it the
// file inherits its directory's ACL, so a PIGO_HOME on a shared or world-readable
// drive would expose the bearer token. A protected DACL also blocks inherited
// ACEs from widening it later.
func secureFile(path string) error {
	path = extendedPath(path)
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	var pinner runtime.Pinner
	pinner.Pin(sid)
	defer pinner.Unpin()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("host: build file acl: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("host: secure %s: %w", path, err)
	}
	return nil
}

// currentUserSID returns the SID of the process's user. Both the pipe DACL and
// the state/token file ACLs derive from it.
func currentUserSID() (*windows.SID, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open process token: %w", err)
	}
	defer tok.Close()
	user, err := tok.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("get token user: %w", err)
	}
	if user.User.Sid == nil {
		return nil, errors.New("token has no user SID")
	}
	return user.User.Sid, nil
}

// readFileShared reads path with FILE_SHARE_DELETE.
//
// Go's os.Open does not include that flag, so an open returned by it would make
// replaceFile wait for the reader to close. A state or token read is exactly the
// kind of short handle that collides with a rotation, and sharing delete lets
// the rename happen underneath it. Windows may still fail the open with
// ERROR_SHARING_VIOLATION while a replacement is in flight, so the open is
// retried for the same reason the rename is.
func readFileShared(path string) ([]byte, error) {
	const (
		attempts = 50
		delay    = 2 * time.Millisecond
	)
	var err error
	for i := 0; i < attempts; i++ {
		data, rerr := readFileOnce(path)
		if rerr == nil {
			return data, nil
		}
		err = rerr
		if !errors.Is(rerr, windows.ERROR_SHARING_VIOLATION) {
			return nil, rerr
		}
		time.Sleep(delay)
	}
	return nil, err
}

// readFileOnce is one shared-read attempt.
func readFileOnce(path string) ([]byte, error) {
	path = extendedPath(path)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.New("invalid handle")}
	}
	defer f.Close()
	return io.ReadAll(f)
}

// extendedPath adds the \\?\-prefix when path is at or past MAX_PATH, which is
// what the os package does internally and what a raw CreateFile does not.
// Without it, a state directory deeper than ~248 bytes would read fine through
// os.ReadFile but fail here.
func extendedPath(path string) string {
	if len(path) < maxPath ||
		strings.HasPrefix(path, `\\?\`) ||
		strings.HasPrefix(path, `\??\`) {
		return path
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	n, err := windows.GetFullPathName(p, 0, nil, nil)
	if err != nil || n == 0 {
		return path
	}
	buf := make([]uint16, n)
	if _, err := windows.GetFullPathName(p, n, &buf[0], nil); err != nil {
		return path
	}
	abs := windows.UTF16ToString(buf)
	if len(abs) < maxPath {
		return abs
	}
	// A UNC path takes \\?\UNC\ and drops its two leading separators; a device
	// path must not be prefixed at all.
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}
