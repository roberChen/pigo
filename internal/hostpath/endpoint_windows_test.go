//go:build windows

// This file tests the Windows transport: the named-pipe name derived from a
// state directory, binding it with an owner-only DACL, dialing it, and the
// properties that make it a drop-in replacement for the POSIX socket file
// (`Probe`, `CleanEndpoint`, `Footprint`).
//
// It is the counterpart of endpoint_posix_test.go, which keeps the unix-socket
// cases (path length fallback, mode bits, stale file cleanup) that do not exist
// on this platform. The build tag means it runs on no CI leg today; the file is
// still compiled and vetted by `GOOS=windows go vet ./...`, which is how the
// package stays honest about the Windows path.
package hostpath

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketPathIsAPipeName(t *testing.T) {
	dir := t.TempDir()
	got := SocketPath(dir)
	if !strings.HasPrefix(got, pipePrefix) {
		t.Fatalf("SocketPath(%q) = %q, want a name under %q", dir, got, pipePrefix)
	}
	// The name must not disclose the directory: pipe names are enumerable by any
	// local user, and a readable path would leak the user's layout.
	if strings.Contains(strings.ToLower(got), strings.ToLower(filepath.Base(dir))) {
		t.Fatalf("pipe name %q leaks the state directory", got)
	}
	// Well inside the 256-character pipe-name limit.
	if len(got) > 256 {
		t.Fatalf("pipe name is %d chars: %q", len(got), got)
	}
	if again := SocketPath(dir); again != got {
		t.Fatalf("pipe name is not deterministic: %q vs %q", got, again)
	}
}

func TestSocketPathIsCaseInsensitive(t *testing.T) {
	// Windows paths are case-insensitive, so the same directory spelled
	// differently must map to the same pipe — otherwise a client started with a
	// differently-cased --dir would never find its host.
	base := t.TempDir()
	upper := filepath.Join(base, "HOSTSTATE")
	lower := filepath.Join(base, "hoststate")
	if got, want := SocketPath(upper), SocketPath(lower); got != want {
		t.Fatalf("case variants mapped to different pipes: %q vs %q", got, want)
	}
}

func TestSocketPathIsUniquePerDir(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if SocketPath(a) == SocketPath(b) {
		t.Fatalf("two directories collided on %q", SocketPath(a))
	}
}

func TestRelativeDirResolvesToTheSamePipe(t *testing.T) {
	// A relative --dir is resolved against the working directory, so a host
	// started with `--dir run/host` and a client started the same way agree.
	base := t.TempDir()
	t.Chdir(base)
	if got, want := SocketPath("run/host"), SocketPath(filepath.Join(base, "run", "host")); got != want {
		t.Fatalf("relative dir mapped to %q, want %q", got, want)
	}
}

func TestListenDialRoundTrip(t *testing.T) {
	endpoint := SocketPath(t.TempDir())
	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	conn, err := Dial(endpoint, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	select {
	case srv := <-accepted:
		if srv == nil {
			t.Fatal("Accept failed")
		}
		_ = srv.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never accepted the connection")
	}

	// The pair must carry a line the way the NDJSON protocol needs, including one
	// longer than the 4 KiB default pipe buffer.
	want := strings.Repeat("x", 32*1024) + "\n"
	if _, err := conn.Write([]byte(want)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestProbeLiveThenGone(t *testing.T) {
	endpoint := SocketPath(t.TempDir())
	if Probe(endpoint, 200*time.Millisecond) {
		t.Fatal("a pipe nobody serves probed as live")
	}
	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if !Probe(endpoint, time.Second) {
		t.Fatal("a listening pipe probed as dead")
	}
	// A named pipe vanishes with its listener: the next Probe must say nobody is
	// there, so the next `serve` starts cleanly instead of reporting a stale host.
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for Probe(endpoint, 100*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a closed pipe still probed as live")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTwoHostsCannotShareAPipe(t *testing.T) {
	// Second host on the same state directory must be refused by the listen
	// itself, not just by the state-file probe.
	endpoint := SocketPath(t.TempDir())
	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer ln.Close()
	if ln2, err := Listen(endpoint); err == nil {
		ln2.Close()
		t.Fatal("a second listener bound the same pipe")
	}
}

func TestCleanEndpointAndFootprint(t *testing.T) {
	dir := t.TempDir()
	// CleanEndpoint is a no-op: a pipe cannot be left behind by a crashed host, and
	// it must never delete a path that a live host happens to own.
	endpoint := SocketPath(dir)
	CleanEndpoint(endpoint)

	// Footprint therefore lists only the two files. A pipe must never appear there:
	// callers stat/remove every entry, and \\.\pipe\... is not removable.
	for _, p := range Footprint(dir) {
		if strings.HasPrefix(p, pipePrefix) {
			t.Fatalf("Footprint contains the pipe name %q", p)
		}
	}
	want := []string{StatePath(dir), TokenPath(dir)}
	if got := Footprint(dir); len(got) != len(want) {
		t.Fatalf("Footprint = %v, want %v", got, want)
	}
	for i, p := range Footprint(dir) {
		if p != want[i] {
			t.Fatalf("Footprint[%d] = %q, want %q", i, p, want[i])
		}
	}
}

func TestOwnerOnlyPipeSDDL(t *testing.T) {
	sd, err := ownerOnlyPipeSDDL()
	if err != nil {
		t.Fatalf("ownerOnlyPipeSDDL: %v", err)
	}
	// D:P = protected DACL (no inherited ACEs); A;;GA;;;<sid> = one generic-all
	// grant; the SID must be the current user's.
	if !strings.HasPrefix(sd, "D:P(A;;GA;;;S-1-") || !strings.HasSuffix(sd, ")") {
		t.Fatalf("SDDL = %q, want D:P(A;;GA;;;<user sid>)", sd)
	}
	if strings.Count(sd, ";;") != 1 {
		t.Fatalf("SDDL = %q grants more than one ACE", sd)
	}
}

// TestFootprintRoundTripThroughRemoveAll mirrors the POSIX cleanup test with the
// Windows artifact list: the token and state file go away, nothing else does.
func TestFootprintRoundTripThroughRemoveAll(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)
	if err := WriteState(dir, State{PID: 1, Version: 1, StartedAt: time.Now().UTC(), Socket: SocketPath(dir)}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := WriteToken(dir, "secret"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	keep := filepath.Join(dir, "config.json")
	if err := os.WriteFile(keep, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}

	RemoveAll(dir)

	for _, p := range Footprint(dir) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after RemoveAll (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("RemoveAll removed an unrelated file: %v", err)
	}
}

// compile-time guard: Listen must keep returning a net.Listener, which the
// server and every test rely on. (No side effect: the assertion is on the
// function type, not a call.)
var _ func(string) (net.Listener, error) = Listen
