//go:build !windows

// This file tests the POSIX transport: the unix socket path, its length
// fallback into a short temp root, and listening/probing a real socket file.
// Everything here is inherently unix (sockaddr_un has a hard 104-byte cap);
// endpoint_windows_test.go covers the named-pipe counterpart.
package hostpath

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// longPath builds an absolute path of exactly n bytes without touching disk.
// Callers use it to exercise the socket-path length fallback, which only cares
// about the string.
func longPath(n int) string {
	return "/" + strings.Repeat("d", n-1)
}

func TestSocketPathShortDirUnchanged(t *testing.T) {
	dir := t.TempDir()
	if got, want := SocketPath(dir), filepath.Join(dir, "server.sock"); got != want {
		t.Fatalf("SocketPath(%q) = %q, want %q", dir, got, want)
	}
	if got := SocketPath(dir); len(got) > maxSocketPath {
		t.Fatalf("short dir produced %d-byte socket path %q", len(got), got)
	}
}

func TestSocketPathLengthBoundary(t *testing.T) {
	suffix := "/server.sock"

	// Exactly at the limit: still a plain path, no hashing.
	atLimit := longPath(maxSocketPath - len(suffix))
	if len(atLimit)+len(suffix) != maxSocketPath {
		t.Fatalf("test setup: %d-byte dir does not land on the limit", len(atLimit))
	}
	if got, want := SocketPath(atLimit), atLimit+suffix; got != want {
		t.Fatalf("SocketPath at the limit = %q, want the plain path %q", got, want)
	}

	// One byte over: the hashed fallback kicks in and is short by construction.
	overLimit := longPath(maxSocketPath - len(suffix) + 1)
	got := SocketPath(overLimit)
	if got == overLimit+suffix {
		t.Fatalf("over-long dir kept the plain path %q", got)
	}
	if len(got) > maxSocketPath {
		t.Fatalf("hashed fallback is %d bytes: %q", len(got), got)
	}
	if filepath.Dir(got) != os.TempDir() {
		t.Fatalf("fallback not under the OS temp dir: %q", got)
	}
	if base := filepath.Base(got); !strings.HasPrefix(base, "pigo-") || !strings.HasSuffix(base, ".sock") {
		t.Fatalf("fallback name = %q, want pigo-<hash>.sock", base)
	}
}

func TestSocketPathHashIsDeterministicAndUnique(t *testing.T) {
	a := longPath(200)
	b := longPath(201) // differs only in length

	first, second := SocketPath(a), SocketPath(a)
	if first != second {
		t.Fatalf("fallback is not deterministic: %q vs %q", first, second)
	}
	if SocketPath(b) == first {
		t.Fatalf("two different long dirs collided on %q", first)
	}

	// The hashed name must be a usable socket path, which is the whole point of
	// the fallback: listen, probe, and let the helper clean it up.
	ln, err := Listen(first)
	if err != nil {
		t.Fatalf("listen on fallback socket: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close(); CleanEndpoint(first) })
	if !Probe(first, 200*time.Millisecond) {
		t.Fatal("probe did not see the listener on the fallback socket path")
	}
}

func TestSocketPathLongTempDirStillFits(t *testing.T) {
	// $TMPDIR can itself be long (a deep CI workspace, a sandboxed build root).
	// The hashed fallback must then pick a shorter root instead of returning a
	// path that still exceeds the unix-socket limit.
	longTmp := longPath(150)
	t.Setenv("TMPDIR", longTmp)
	if len(os.TempDir()) <= maxSocketPath {
		t.Fatalf("test setup: TMPDIR %q is too short to exercise the fallback", os.TempDir())
	}

	dir := longPath(220)
	got := SocketPath(dir)
	if len(got) > maxSocketPath {
		t.Fatalf("fallback with a long TMPDIR is %d bytes (> %d): %q", len(got), maxSocketPath, got)
	}
	if filepath.Dir(got) == longTmp {
		t.Fatalf("fallback stayed in the over-long TMPDIR: %q", got)
	}
	// It must still be the same deterministic name, just rooted elsewhere, so two
	// processes agree on where to meet.
	if base := filepath.Base(got); !strings.HasPrefix(base, "pigo-") || !strings.HasSuffix(base, ".sock") {
		t.Fatalf("fallback name = %q, want pigo-<hash>.sock", base)
	}
	if again := SocketPath(dir); again != got {
		t.Fatalf("fallback is not deterministic with a long TMPDIR: %q vs %q", got, again)
	}
	// And it must be usable: shorten further only if the chosen root does not
	// exist (a container without /tmp is unusual, but the test should not fail
	// for that reason).
	ln, err := Listen(got)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			t.Skipf("chosen root %q does not exist: %v", filepath.Dir(got), err)
		}
		t.Fatalf("listen on fallback socket %q: %v", got, err)
	}
	t.Cleanup(func() { _ = ln.Close(); CleanEndpoint(got) })
	if !Probe(got, 200*time.Millisecond) {
		t.Fatal("probe did not see the listener on the fallback socket path")
	}
}

func TestLongDirStateAndTokenRoundTrip(t *testing.T) {
	// A deep, long state directory still works: everything except the socket is
	// an ordinary file, and the socket falls back to the temp dir. The nesting is
	// long enough (even under a short t.TempDir) that the fallback is the path
	// actually under test here.
	dir := filepath.Join(t.TempDir(), strings.Repeat("nested/", 12)+"state")
	socket := SocketPath(dir)
	if socket == filepath.Join(dir, "server.sock") {
		t.Fatalf("expected the hashed socket fallback for a %d-byte dir, got %q", len(dir), socket)
	}
	if len(socket) > maxSocketPath {
		t.Fatalf("long dir produced an over-long socket path: %q", socket)
	}
	started := time.Date(2024, 5, 1, 12, 30, 15, 123456789, time.UTC)
	want := State{PID: 4242, Version: 1, StartedAt: started, Socket: socket}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("WriteState in a long dir: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState in a long dir: %v", err)
	}
	if got.PID != want.PID || got.Version != want.Version || got.Socket != want.Socket {
		t.Fatalf("state = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(started) {
		t.Fatalf("startedAt = %s, want %s", got.StartedAt, started)
	}
}

// fileExists reports whether path is present, whatever it is.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestProbeSeesOnlyOwnHost is the crash-recovery contract on POSIX: a socket
// file left by a dead host must not probe as live, and anything that is not a
// socket (a regular file at the same path) must not either.
func TestProbeSeesOnlyOwnHost(t *testing.T) {
	t.Run("live then dead", func(t *testing.T) {
		socket := SocketPath(t.TempDir())
		ln, err := Listen(socket)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { CleanEndpoint(socket) })
		if !Probe(socket, 500*time.Millisecond) {
			t.Fatal("live socket probed as dead")
		}
		// A crashed host leaves the file behind; after the listener goes away the
		// probe must report the socket as stale so the next start can reclaim it.
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
		if !fileExists(socket) {
			t.Skip("this platform unlinks the socket on close, leaving nothing to probe")
		}
		if Probe(socket, 500*time.Millisecond) {
			t.Fatal("closed socket probed as live")
		}
	})

	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-a-socket")
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if Probe(path, 100*time.Millisecond) {
			t.Fatal("a regular file probed as a live host")
		}
	})
}

// TestStaleStateDetection covers the shutdown-after-crash path the CLI relies
// on: ReadState still yields a pid, but Probe says nobody is listening, which is
// what `pigo serve status` reports as "not responding".
func TestStaleStateDetection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)
	socket := SocketPath(dir)

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	live := State{PID: os.Getpid(), Version: 1, StartedAt: time.Now().UTC(), Socket: socket}
	if err := WriteState(dir, live); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if !Probe(live.Socket, 500*time.Millisecond) {
		t.Fatal("a host that is listening probed as dead")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	st, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState after the host died: %v", err)
	}
	if st.PID != live.PID {
		t.Fatalf("stale state pid = %d, want %d", st.PID, live.PID)
	}
	if Probe(st.Socket, 200*time.Millisecond) {
		t.Fatal("stale state should not probe as live")
	}

	// RemoveAll is what the next start (or `serve stop`) uses to clear it.
	RemoveAll(dir)
	if _, err := ReadState(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadState after RemoveAll = %v, want os.ErrNotExist", err)
	}
}

func TestRemoveAllRemovesOnlyHostFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)

	if err := WriteState(dir, State{PID: 1, Version: 1, StartedAt: time.Now().UTC(), Socket: SocketPath(dir)}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := WriteToken(dir, "secret"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	socket := SocketPath(dir)
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatalf("seed socket file: %v", err)
	}
	// An unrelated file must survive: RemoveAll owns three names, not the dir.
	keep := filepath.Join(dir, "config.json")
	if err := os.WriteFile(keep, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}

	RemoveAll(dir)

	for _, path := range Footprint(dir) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after RemoveAll (err=%v)", path, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("RemoveAll removed an unrelated file: %v", err)
	}
}

// TestRemoveAllIsBestEffort covers the shutdown path when one of the files
// cannot be removed: cleanup must still remove the rest rather than aborting on
// the first failure.
func TestRemoveAllIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)

	// A non-empty directory where the state file belongs cannot be removed with
	// os.Remove.
	if err := os.Mkdir(StatePath(dir), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(StatePath(dir), "inner"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed inner file: %v", err)
	}
	if err := WriteToken(dir, "tok"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := os.WriteFile(SocketPath(dir), nil, 0o600); err != nil {
		t.Fatalf("seed socket: %v", err)
	}

	RemoveAll(dir)

	if _, err := os.Stat(TokenPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token survived RemoveAll: %v", err)
	}
	if _, err := os.Stat(SocketPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket survived RemoveAll: %v", err)
	}
	// The undeletable state directory is still there, as expected.
	if _, err := os.Stat(StatePath(dir)); err != nil {
		t.Fatalf("state dir vanished: %v", err)
	}
}
