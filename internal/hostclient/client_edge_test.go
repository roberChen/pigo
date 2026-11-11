// This file tests hostclient's failure modes and concurrency edges: refused and
// half-finished connects, handshake failures, subscription/dispatch semantics (a
// subscriber registered mid-turn, a callback that blocks, a callback that
// panics), typed protocol errors, Done/ReadErr, and goroutine hygiene under
// concurrent calls.
//
// The fixtures are the ones client_test.go already uses — startHostWithServer
// plus the scripted factory over a real unix socket — because the interesting
// behaviour is the pair, not the client alone. Where the real host cannot
// misbehave on purpose (a silent hello, a newer protocol version, a garbage
// line), a hand-rolled host on its own socket stands in for it.
//
// Everything here is hermetic (temp state dirs, temp trust/session stores) and
// synchronizes by polling with a deadline instead of sleeping.
package hostclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostpath"
	"github.com/smallnest/pigo/internal/hostserver"
	pigoruntime "github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// edgeTimeout bounds every wait in this file. Tests poll a condition against it
// rather than sleeping a fixed amount, so a loaded CI box slows them down
// instead of making them flaky.
const edgeTimeout = 10 * time.Second

// waitFor polls cond until it holds, failing the test at the deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(edgeTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitGoroutines polls until the process goroutine count falls back to limit
// (baseline plus a small slack for runtime noise), dumping every stack if it
// does not. A client that leaked its read or dispatch goroutine fails here.
func waitGoroutines(t *testing.T, limit int, what string) {
	t.Helper()
	deadline := time.Now().Add(edgeTimeout)
	for {
		n := runtime.NumGoroutine()
		if n <= limit {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%s: %d goroutines still running, want <= %d\n%s",
				what, n, limit, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(time.Millisecond)
	}
}

// startHostWithServer is startHost plus the server handle, so an edge test can
// shut the host down (or let it die) in the middle of a live connection.
func startHostWithServer(t *testing.T, f hostserver.Factory) (string, *hostserver.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	mgr, err := trust.NewManager(t.TempDir() + "/trust.json")
	if err != nil {
		t.Fatalf("trust: %v", err)
	}
	srv, err := hostserver.NewServer(hostserver.Config{
		Dir:     dir,
		Factory: f,
		Store:   store,
		Trust:   mgr,
		Build:   "test",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		srv.Shutdown("test cleanup")
		<-done
	})
	return dir, srv
}

// dial connects with explicit options, returning the error instead of failing.
// A connection that was made is closed in cleanup.
func dial(t *testing.T, opts Options) (*Client, error) {
	t.Helper()
	c, err := Connect(opts)
	if c != nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

// wantCode asserts err is a protocol error carrying code and a message.
func wantCode(t *testing.T, err error, code int) *hostapi.Error {
	t.Helper()
	var protoErr *hostapi.Error
	if !errors.As(err, &protoErr) {
		t.Fatalf("error = %v (%T), want *hostapi.Error", err, err)
	}
	if protoErr.Code != code {
		t.Fatalf("error code = %d (%s), want %d", protoErr.Code, protoErr.Message, code)
	}
	if protoErr.Message == "" {
		t.Fatal("protocol error carries no message")
	}
	return protoErr
}

// transportErr reports whether err is the kind of failure a peer that vanished
// mid-handshake produces (as opposed to a protocol-level answer).
func transportErr(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed)
}

// ---- hand-rolled host --------------------------------------------------

// rawConn is one accepted socket of a hand-rolled host, with a line scanner so a
// test can read requests and answer them (or not) however it likes.
type rawConn struct {
	nc net.Conn
	sc *bufio.Scanner
}

// read returns the next request envelope, or the read error.
func (r *rawConn) read() (hostapi.Envelope, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return hostapi.Envelope{}, err
		}
		return hostapi.Envelope{}, io.EOF
	}
	var env hostapi.Envelope
	if err := json.Unmarshal(r.sc.Bytes(), &env); err != nil {
		return hostapi.Envelope{}, err
	}
	return env, nil
}

// send writes v as one JSON line.
func (r *rawConn) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = r.nc.Write(append(data, '\n'))
	return err
}

// sendRaw writes an arbitrary line, for framing that is not JSON at all.
func (r *rawConn) sendRaw(line string) error {
	_, err := r.nc.Write([]byte(line + "\n"))
	return err
}

// startRawHost listens on a state dir's socket and runs handle for every
// accepted connection. The listener and its handlers are torn down in cleanup so
// a deliberately broken fixture cannot outlive the test.
//
// Handlers must never call t.Fatalf: they run on their own goroutines.
func startRawHost(t *testing.T, handle func(*rawConn)) (dir, socket string) {
	t.Helper()
	dir = t.TempDir()
	socket = hostpath.SocketPath(dir)
	ln, err := hostpath.Listen(socket)
	if err != nil {
		t.Fatalf("listen %s: %v", socket, err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			rc := &rawConn{nc: nc, sc: bufio.NewScanner(nc)}
			rc.sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer nc.Close()
				handle(rc)
			}()
		}
	}()
	return dir, socket
}

// drainQuietly reads and discards requests until the peer goes away, which is
// what keeps a fixture connection alive without answering anything else.
func drainQuietly(rc *rawConn) {
	for {
		if _, err := rc.read(); err != nil {
			return
		}
	}
}

// ---- connect failures --------------------------------------------------

func TestConnectWithoutAnyStateDir(t *testing.T) {
	// No Options.Dir, no PIGO_HOME and no home directory: the client has nowhere
	// to look and must say so instead of dialing a relative path. Windows takes
	// the home directory from USERPROFILE, the rest from HOME.
	t.Setenv("PIGO_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	_, err := Connect(Options{})
	if err == nil {
		t.Fatal("connect without a state directory succeeded")
	}
	if !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("error = %v, want a state-directory complaint", err)
	}
}

func TestConnectMissingStateDir(t *testing.T) {
	// A state directory that does not exist yet is the "host was never started
	// here" case, and the error names the socket the caller expected.
	dir := filepath.Join(t.TempDir(), "never-created")
	_, err := Connect(Options{Dir: dir})
	var notRunning ErrNotRunning
	if !errors.As(err, &notRunning) {
		t.Fatalf("error = %v, want ErrNotRunning", err)
	}
	if notRunning.Dir != dir {
		t.Fatalf("ErrNotRunning.Dir = %q, want %q", notRunning.Dir, dir)
	}
	if notRunning.Socket == "" {
		t.Fatal("ErrNotRunning does not name the socket")
	}
	if !strings.Contains(err.Error(), notRunning.Socket) {
		t.Fatalf("error %q does not mention socket %q", err, notRunning.Socket)
	}
}

func TestConnectMissingSocket(t *testing.T) {
	// Token present, socket gone: a host that crashed after writing its token.
	dir := t.TempDir()
	if err := hostpath.WriteToken(dir, "orphan-token"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	_, err := Connect(Options{Dir: dir})
	var notRunning ErrNotRunning
	if !errors.As(err, &notRunning) {
		t.Fatalf("error = %v, want ErrNotRunning", err)
	}
	if notRunning.Socket != hostpath.SocketPath(dir) {
		t.Fatalf("socket = %q, want %q", notRunning.Socket, hostpath.SocketPath(dir))
	}
}

func TestConnectStaleSocketFile(t *testing.T) {
	// A crashed host leaves the socket inode behind. Dialing a plain file must
	// report "not running" — not hang, not panic on a half-open connection.
	// A named pipe is not a file: it cannot be left behind, so the equivalent
	// crash footprint is covered by TestConnectMissingSocket.
	if runtime.GOOS == "windows" {
		t.Skip("a windows endpoint is a named pipe, not a file on disk")
	}
	dir := t.TempDir()
	if err := hostpath.WriteToken(dir, "stale-token"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	socket := hostpath.SocketPath(dir)
	if err := os.WriteFile(socket, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	_, err := Connect(Options{Dir: dir})
	var notRunning ErrNotRunning
	if !errors.As(err, &notRunning) {
		t.Fatalf("error = %v, want ErrNotRunning", err)
	}
}

func TestConnectUnreadableTokenFile(t *testing.T) {
	// An unreadable token is reported as a token problem, not as "no host is
	// running": the difference is what tells a user to fix permissions instead of
	// starting a daemon. A directory at the token path is unreadable even as root,
	// which a chmod-0000 file is not.
	dir := t.TempDir()
	if err := os.MkdirAll(hostpath.TokenPath(dir), 0o700); err != nil {
		t.Fatalf("mkdir token path: %v", err)
	}
	_, err := Connect(Options{Dir: dir})
	if err == nil {
		t.Fatal("connect with an unreadable token succeeded")
	}
	var notRunning ErrNotRunning
	if errors.As(err, &notRunning) {
		t.Fatalf("unreadable token reported as a missing host: %v", err)
	}
	if !strings.Contains(err.Error(), "read token") {
		t.Fatalf("error = %v, want a token-read complaint", err)
	}
}

func TestConnectGarbageTokenFileIsRejectedByHost(t *testing.T) {
	// A readable but wrong token: the dial succeeds and the *handshake* fails, so
	// the caller sees a typed unauthorized error rather than "not running".
	dir := startHost(t, &scriptedFactory{reply: "x"})
	if err := hostpath.WriteToken(dir, "garbage-not-the-host-token"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	_, err := Connect(Options{Dir: dir})
	protoErr := wantCode(t, err, hostapi.CodeUnauthorized)
	if !strings.Contains(protoErr.Message, "token") {
		t.Fatalf("message = %q, want a token complaint", protoErr.Message)
	}
	var notRunning ErrNotRunning
	if errors.As(err, &notRunning) {
		t.Fatalf("wrong token reported as a missing host: %v", err)
	}
}

func TestConnectHelloTimeout(t *testing.T) {
	// A host that accepts the socket and then says nothing must not hang a UI
	// startup: Hello is bounded by Options.CallTimeout. Repeat it, because an
	// abandoned handshake that leaks its reader would leak one per attempt.
	dir, _ := startRawHost(t, drainQuietly)
	// Warm up first. A named-pipe listener only creates its next pipe instance
	// (and that instance's connect goroutine) when Accept is called, so the first
	// connection adds a fixture goroutine after the ready signal. Measure the
	// baseline once that steady state has settled, or the fixture looks like one
	// leaked client goroutine.
	opts := Options{Dir: dir, Token: "t", Client: "test", CallTimeout: 150 * time.Millisecond}
	if _, err := dial(t, opts); err == nil {
		t.Fatal("warm-up handshake unexpectedly succeeded")
	}
	baseline := settledGoroutines(t)
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, err := dial(t, opts)
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d: error = %v, want context.DeadlineExceeded", i, err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("attempt %d: connect took %v despite a 150ms CallTimeout", i, elapsed)
		}
	}
	// Every abandoned connection closed its socket, so the fixture's handlers exit
	// and the count returns to where it started.
	waitGoroutines(t, baseline, "after three hello timeouts")
}

// settledGoroutines waits until the goroutine count stops changing, then
// returns it. Async teardown (a transport's reader, the fixture's next accept
// instance) makes an immediate reading racy as a baseline.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	prev := runtime.NumGoroutine()
	deadline := time.Now().Add(edgeTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	return prev
}

func TestConnectSeesServerReportedVersion(t *testing.T) {
	// The client neither assumes nor rewrites the negotiated version: it reports
	// what the host said, and leaves the comparison with hostapi.Version to the
	// caller (the host, not the client, is the one that refuses newer peers).
	dir, _ := startRawHost(t, func(rc *rawConn) {
		env, err := rc.read()
		if err != nil {
			return
		}
		_ = rc.send(map[string]any{
			"id":     env.ID,
			"result": hostapi.HelloResult{Version: 999, PID: 4242, Build: "future"},
		})
		drainQuietly(rc)
	})
	c, err := dial(t, Options{Dir: dir, Token: "t", Client: "test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	info := c.ServerInfo()
	if info.Version != 999 || info.PID != 4242 || info.Build != "future" {
		t.Fatalf("ServerInfo = %+v, want the host's own values", info)
	}
	if hostapi.Version == info.Version {
		t.Fatal("fixture must report a version other than the client's")
	}
}

func TestConnectSurfacesHostVersionRefusal(t *testing.T) {
	// A host that refuses the client's protocol version answers hello with an
	// invalid-params error; the client must surface it as a typed error so the UI
	// can say "upgrade one of the two".
	dir, _ := startRawHost(t, func(rc *rawConn) {
		env, err := rc.read()
		if err != nil {
			return
		}
		_ = rc.send(map[string]any{
			"id":    env.ID,
			"error": hostapi.Err(hostapi.CodeInvalidParams, "protocol version 2 is newer than this host (1)"),
		})
		drainQuietly(rc)
	})
	_, err := dial(t, Options{Dir: dir, Token: "t"})
	protoErr := wantCode(t, err, hostapi.CodeInvalidParams)
	if !strings.Contains(protoErr.Message, "protocol version") {
		t.Fatalf("message = %q, want a protocol-version complaint", protoErr.Message)
	}
}

func TestConnectDuringShutdown(t *testing.T) {
	// Racing a shutdown: the token read, the dial and the handshake can each land
	// on either side of it. Every outcome must be a clean error (or a usable
	// connection that won the race) — never a hang or a panic.
	for i := 0; i < 8; i++ {
		dir, srv := startHostWithServer(t, &scriptedFactory{reply: "x"})
		go srv.Shutdown("racing shutdown")
		c, err := Connect(Options{
			Dir:         dir,
			Client:      "test",
			DialTimeout: time.Second,
			CallTimeout: 2 * time.Second,
		})
		if err == nil {
			if cerr := c.Close(); cerr != nil {
				t.Logf("iteration %d: close after winning the race: %v", i, cerr)
			}
			continue
		}
		var notRunning ErrNotRunning
		var protoErr *hostapi.Error
		switch {
		case errors.As(err, &notRunning), transportErr(err):
			// Nothing was listening yet, or the host vanished mid-handshake.
		case errors.As(err, &protoErr):
			if protoErr.Code != hostapi.CodeUnavailable && protoErr.Code != hostapi.CodeUnauthorized {
				t.Fatalf("iteration %d: unexpected error code %d (%s)", i, protoErr.Code, protoErr.Message)
			}
		default:
			t.Fatalf("iteration %d: connect error = %v", i, err)
		}
	}

	// Once a shutdown has completed there is nothing left to connect to.
	dir, srv := startHostWithServer(t, &scriptedFactory{reply: "x"})
	srv.Shutdown("done")
	srv.Wait()
	if _, err := hostpath.ReadToken(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token survived shutdown: %v", err)
	}
	var notRunning ErrNotRunning
	if _, err := Connect(Options{Dir: dir}); !errors.As(err, &notRunning) {
		t.Fatalf("connect after shutdown = %v, want ErrNotRunning", err)
	}
}

func TestClientIdentityAndSocket(t *testing.T) {
	// The negotiated identity and the socket a client is talking to are what
	// `pigo attach` prints and what an error message names, so both must survive
	// the handshake (and an explicit Socket override must be reported verbatim).
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	want := hostpath.SocketPath(dir)
	if got := c.Socket(); got != want {
		t.Fatalf("Socket() = %q, want %q", got, want)
	}
	info := c.ServerInfo()
	if info.Version != hostapi.Version {
		t.Fatalf("hello version = %d, want %d", info.Version, hostapi.Version)
	}
	if info.PID != os.Getpid() || info.Build != "test" {
		t.Fatalf("hello = %+v, want this process and the test build", info)
	}
	if info.Started.IsZero() {
		t.Fatal("hello carries no start time")
	}

	token, err := hostpath.ReadToken(dir)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	overridden, err := dial(t, Options{Socket: want, Token: token, Client: "test"})
	if err != nil {
		t.Fatalf("connect with an explicit socket: %v", err)
	}
	if got := overridden.Socket(); got != want {
		t.Fatalf("overridden Socket() = %q, want %q", got, want)
	}
}

// ---- subscriptions and dispatch ---------------------------------------

// decodeSessionEvent decodes the SessionEvent carried by an EventSession
// envelope. It returns false instead of failing the test because it is called
// from subscription callbacks, which run on the dispatch goroutine: t.Fatalf
// there would kill the wrong goroutine, so failures are counted and asserted on
// the test's own goroutine.
func decodeSessionEvent(env hostapi.Envelope) (hostapi.SessionEvent, bool) {
	if env.Event != hostapi.EventSession {
		return hostapi.SessionEvent{}, false
	}
	var ev hostapi.SessionEvent
	if err := env.DecodeParams(&ev); err != nil {
		return hostapi.SessionEvent{}, false
	}
	return ev, true
}

func TestEverySubscriberGetsEveryEvent(t *testing.T) {
	const (
		subscribers = 4
		turns       = 2
	)
	dir := startHost(t, &scriptedFactory{reply: "abcdefgh"})
	c := connect(t, dir)

	var mu sync.Mutex
	got := make([][]string, subscribers)
	for i := 0; i < subscribers; i++ {
		c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
			var ev hostapi.SessionEvent
			if err := env.DecodeParams(&ev); err != nil || ev.Type != hostapi.SessionEventTextDelta {
				return
			}
			mu.Lock()
			got[i] = append(got[i], ev.Text)
			mu.Unlock()
		})
	}

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < turns; i++ {
		if _, err := c.Prompt(context.Background(), sid.ID, "q"); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}

	want := 2 * turns
	waitFor(t, "every subscriber to see every delta", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, deltas := range got {
			if len(deltas) != want {
				return false
			}
		}
		return true
	})
	// Every subscriber saw the same sequence, in the order the host produced it.
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < subscribers; i++ {
		if strings.Join(got[i], "|") != strings.Join(got[0], "|") {
			t.Fatalf("subscriber %d saw %q, subscriber 0 saw %q", i, got[i], got[0])
		}
	}
	if strings.Join(got[0], "") != strings.Repeat("abcdefgh", turns) {
		t.Fatalf("deltas = %q, want the replies in order", got[0])
	}
}

func TestSessionEventsAreScopedBySessionID(t *testing.T) {
	// One connection can own several sessions running at the same time. The client
	// delivers every session_event on the connection and tags it with its session
	// id, so a UI filters on that id; this test pins both halves: nothing is lost
	// and nothing is mis-tagged.
	dir := startHost(t, &scriptedFactory{reply: "reply"})
	c := connect(t, dir)

	ids := make([]string, 3)
	for i := range ids {
		info, err := c.CreateSession(t.TempDir(), "", "")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids[i] = info.ID
	}

	var mu sync.Mutex
	perSession := make(map[string][]string) // session id -> delta texts
	turnEnds := make(map[string]int)        // session id -> turn_end count
	strangers := new(strings.Builder)       // any event tagged with an unknown session
	badDecodes := 0                         // envelopes this subscriber could not read

	c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
		ev, ok := decodeSessionEvent(env)
		mu.Lock()
		defer mu.Unlock()
		if !ok {
			badDecodes++
			return
		}
		known := false
		for _, id := range ids {
			if ev.SessionID == id {
				known = true
			}
		}
		if !known {
			strangers.WriteString(ev.SessionID + " ")
			return
		}
		switch ev.Type {
		case hostapi.SessionEventTextDelta:
			perSession[ev.SessionID] = append(perSession[ev.SessionID], ev.Text)
		case hostapi.SessionEventTurnEnd:
			turnEnds[ev.SessionID]++
		}
	})

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Prompt(context.Background(), id, "q")
			if err != nil {
				t.Errorf("prompt %s: %v", id, err)
				return
			}
			if res.SessionID != id || res.Text != "reply" {
				t.Errorf("prompt result = %+v, want session %s", res, id)
			}
		}()
	}
	wg.Wait()

	waitFor(t, "every session's turn to be dispatched", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range ids {
			if len(perSession[id]) != 2 || turnEnds[id] != 1 {
				return false
			}
		}
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	if badDecodes != 0 {
		t.Fatalf("%d session events did not decode", badDecodes)
	}
	if strangers.Len() != 0 {
		t.Fatalf("events tagged with unknown sessions: %s", strangers)
	}
	for id, deltas := range perSession {
		if strings.Join(deltas, "") != "reply" {
			t.Fatalf("session %s deltas = %q", id, deltas)
		}
	}
}

func TestUnsubscribeStopsDeliveryAndIsIdempotent(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "abcd"})
	c := connect(t, dir)

	var mu sync.Mutex
	selfDeliveries := 0
	survivorDeliveries := 0

	var cancelSelf func()
	cancelSelf = c.OnEvent(hostapi.EventSession, func(hostapi.Envelope) {
		mu.Lock()
		selfDeliveries++
		mu.Unlock()
		// Documented: unsubscribing from inside the callback is safe. The client
		// copies the subscriber list per event, so the current event still reaches
		// the other subscribers.
		cancelSelf()
	})
	c.OnEvent(hostapi.EventSession, func(hostapi.Envelope) {
		mu.Lock()
		survivorDeliveries++
		mu.Unlock()
	})

	// Repeated and concurrent unsubscribes must be no-ops, not double removes.
	cancelSelf()
	cancelSelf()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cancelSelf()
		}()
	}
	wg.Wait()

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Prompt(context.Background(), sid.ID, "q"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	// text_delta, text_delta, turn_end for one scripted turn.
	const want = 3
	waitFor(t, "the surviving subscriber to see the whole turn", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return survivorDeliveries == want
	})
	mu.Lock()
	defer mu.Unlock()
	if selfDeliveries != 0 {
		t.Fatalf("an already-unsubscribed callback fired %d times", selfDeliveries)
	}
}

func TestSubscribeDuringTurnOnlySeesLaterEvents(t *testing.T) {
	// The client does not replay history to a new subscriber: drainEvents hands
	// each event to the subscribers registered when it is dispatched. A UI that
	// subscribes mid-turn therefore gets the tail of the turn and must seed its
	// transcript from the prompt result (or a resume), not from events.
	first := make(chan struct{}, 1)
	release := make(chan struct{})
	dir := startHost(t, &gatedFactory{reply: "0123456789", first: first, release: release})
	c := connect(t, dir)

	var mu sync.Mutex
	var early []string
	c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil || ev.Type != hostapi.SessionEventTextDelta {
			return
		}
		mu.Lock()
		early = append(early, ev.Text)
		mu.Unlock()
	})

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	prompted := make(chan error, 1)
	go func() {
		_, err := c.Prompt(context.Background(), sid.ID, "q")
		prompted <- err
	}()

	// Wait until the first delta has actually been dispatched to the early
	// subscriber, so it is out of the queue when the late one registers.
	<-first
	waitFor(t, "the early subscriber to see the first delta", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(early) == 1
	})

	var late []string
	c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil || ev.Type != hostapi.SessionEventTextDelta {
			return
		}
		mu.Lock()
		late = append(late, ev.Text)
		mu.Unlock()
	})

	close(release)
	if err := <-prompted; err != nil {
		t.Fatalf("prompt: %v", err)
	}
	waitFor(t, "the late subscriber to see the rest of the turn", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(late) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if len(early) != 2 {
		t.Fatalf("early subscriber saw %q, want both deltas", early)
	}
	if strings.Join(late, "") == "" {
		t.Fatal("late subscriber saw nothing of the turn it subscribed to")
	}
	if strings.Contains(strings.Join(late, "|"), early[0]) {
		t.Fatalf("late subscriber was replayed the delta it missed: %q", late)
	}
}

func TestBlockedCallbackDoesNotStallRequests(t *testing.T) {
	// A callback runs on the dispatch goroutine, never on the read loop: that is
	// what lets a UI answer a permission request from inside its own callback.
	// Here the callback blocks outright, and requests must still round-trip.
	dir := startHost(t, &scriptedFactory{reply: "abcdef"})
	c := connect(t, dir)

	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	var mu sync.Mutex
	var types []string
	c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			return
		}
		mu.Lock()
		types = append(types, ev.Type)
		first := len(types) == 1
		mu.Unlock()
		if first {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-block
		}
	})

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := c.Prompt(context.Background(), sid.ID, "q")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.Text != "abcdef" {
		t.Fatalf("prompt text = %q", res.Text)
	}
	<-entered // the dispatcher is now parked inside the callback

	if _, err := c.ServerStatus(); err != nil {
		t.Fatalf("server.status while a callback is blocked: %v", err)
	}
	if list, err := c.ListSessions(); err != nil || len(list) != 1 {
		t.Fatalf("session.list while a callback is blocked: %v (%d sessions)", err, len(list))
	}

	close(block)
	// Everything that arrived behind the blocked callback is delivered, in order.
	waitFor(t, "the queued events to be drained", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(types) == 3
	})
	mu.Lock()
	defer mu.Unlock()
	if types[len(types)-1] != hostapi.SessionEventTurnEnd {
		t.Fatalf("event order = %q, want turn_end last", types)
	}
}

func TestCallbackPanicIsNotCoveredByClient(t *testing.T) {
	// The client calls callbacks directly and does not recover them (dispatchLoop
	// has no recover), so an *unrecovered* panic in a callback takes the dispatch
	// goroutine — and the process — down. A UI must recover in its own callback;
	// this test does exactly that and then asserts the client keeps working, which
	// is the behaviour the code documents by omission.
	dir := startHost(t, &scriptedFactory{reply: "abcdef"})
	c := connect(t, dir)

	var mu sync.Mutex
	panics := 0
	after := 0
	c.OnEvent(hostapi.EventSession, func(hostapi.Envelope) {
		defer func() {
			if r := recover(); r != nil {
				mu.Lock()
				panics++
				mu.Unlock()
			}
		}()
		panic("callback boom")
	})
	// A second subscriber must keep being served even though its neighbour panics.
	c.OnEvent(hostapi.EventSession, func(hostapi.Envelope) {
		mu.Lock()
		after++
		mu.Unlock()
	})

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Prompt(context.Background(), sid.ID, "q"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	const perTurn = 3 // two deltas and turn_end
	waitFor(t, "both subscribers to see the turn", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return panics == perTurn && after == perTurn
	})

	// The connection is still usable after the recovered panics.
	if _, err := c.Prompt(context.Background(), sid.ID, "again"); err != nil {
		t.Fatalf("second prompt after a callback panic: %v", err)
	}
	waitFor(t, "the second turn to be dispatched", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return panics == 2*perTurn
	})
}

func TestSubscriptionEventNameFilters(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "abcd"})
	c := connect(t, dir)

	var mu sync.Mutex
	counts := map[string]int{}
	count := func(name string) {
		mu.Lock()
		counts[name]++
		mu.Unlock()
	}
	c.OnEvent(hostapi.EventSession, func(hostapi.Envelope) { count("session") })
	c.OnEvent(hostapi.EventPermission, func(hostapi.Envelope) { count("permission") })
	c.OnEvent(hostapi.EventSessionClosed, func(hostapi.Envelope) { count("closed") })
	c.OnEvent(hostapi.EventServerShutdown, func(hostapi.Envelope) { count("shutdown") })
	c.OnEvent("", func(hostapi.Envelope) { count("all") })

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Prompt(context.Background(), sid.ID, "q"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := c.CloseSession(sid.ID); err != nil {
		t.Fatalf("close session: %v", err)
	}
	if err := c.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(edgeTimeout):
		t.Fatal("connection did not close after shutdown")
	}
	waitFor(t, "the shutdown event to be dispatched before the connection winds down", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return counts["shutdown"] == 1 && counts["all"] == 5
	})

	mu.Lock()
	defer mu.Unlock()
	if counts["session"] != 3 {
		t.Fatalf("session subscriber saw %d events, want 3", counts["session"])
	}
	if counts["closed"] != 1 {
		t.Fatalf("session_closed subscriber saw %d events, want 1", counts["closed"])
	}
	if counts["permission"] != 0 {
		t.Fatalf("permission subscriber saw %d events, want none", counts["permission"])
	}
	if counts["all"] != counts["session"]+counts["closed"]+counts["shutdown"] {
		t.Fatalf("catch-all subscriber saw %d events, want %d", counts["all"],
			counts["session"]+counts["closed"]+counts["shutdown"])
	}
}

// ---- concurrency -------------------------------------------------------

func TestConcurrentCallsOnOneConnection(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "answer"})
	c := connect(t, dir)

	const sessions = 3
	ids := make([]string, sessions)
	for i := range ids {
		info, err := c.CreateSession(t.TempDir(), "", "")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids[i] = info.ID
	}
	status, err := c.ServerStatus()
	if err != nil {
		t.Fatalf("server.status: %v", err)
	}

	errCh := make(chan error, 128)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				st, err := c.ServerStatus()
				if err != nil {
					errCh <- fmt.Errorf("server.status: %w", err)
					return
				}
				if st.PID != status.PID {
					errCh <- fmt.Errorf("server.status pid = %d, want %d", st.PID, status.PID)
					return
				}
				list, err := c.ListSessions()
				if err != nil {
					errCh <- fmt.Errorf("session.list: %w", err)
					return
				}
				if len(list) != sessions {
					errCh <- fmt.Errorf("session.list = %d sessions, want %d", len(list), sessions)
					return
				}
			}
		}()
	}
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Prompt(context.Background(), id, "q")
			if err != nil {
				errCh <- fmt.Errorf("prompt %s: %w", id, err)
				return
			}
			if res.SessionID != id || res.Text != "answer" {
				errCh <- fmt.Errorf("prompt result = %+v, want session %s answered", res, id)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// All three turns are durable and the connection is still healthy.
	list, err := c.ListSessions()
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	for _, info := range list {
		if info.Running {
			t.Fatalf("session %s is still marked running", info.ID)
		}
	}
}

func TestConcurrentCancelAndPrompt(t *testing.T) {
	// Cancel is what a UI sends while a prompt call is outstanding, so the two
	// must not deadlock or corrupt each other. The gated agent signals `first`
	// only after it has streamed, which makes "the turn is running" a fact rather
	// than a guess; the outcome is still one of the documented ones (aborted, or
	// completed if the cancel lost the race).
	first := make(chan struct{}, 4)
	release := make(chan struct{})
	dir := startHost(t, &gatedFactory{reply: "slow answer", first: first, release: release})
	c := connect(t, dir)

	const rounds = 3
	for i := 0; i < rounds; i++ {
		sid, err := c.CreateSession(t.TempDir(), "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		var (
			res    hostapi.PromptResult
			perr   error
			cancel error
			wg     sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			res, perr = c.Prompt(context.Background(), sid.ID, "q")
		}()
		go func() {
			defer wg.Done()
			<-first // the turn is in flight: the agent has already streamed
			cancel = c.Cancel(sid.ID)
		}()
		wg.Wait()
		if cancel != nil {
			t.Fatalf("round %d: cancel: %v", i, cancel)
		}
		if perr != nil {
			t.Fatalf("round %d: prompt: %v", i, perr)
		}
		if res.SessionID != sid.ID {
			t.Fatalf("round %d: prompt result = %+v", i, res)
		}
		switch res.StopReason {
		case agentcore.StopReasonEndTurn, agentcore.StopReasonAborted:
		default:
			t.Fatalf("round %d: stop reason = %q", i, res.StopReason)
		}
	}

	// Cancelling an idle session is explicitly not an error: a UI may fire it just
	// as the turn finishes.
	waitFor(t, "every session to go idle", func() bool {
		list, err := c.ListSessions()
		if err != nil {
			return false
		}
		for _, info := range list {
			if info.Running {
				return false
			}
		}
		return true
	})
	list, err := c.ListSessions()
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	for _, info := range list {
		if err := c.Cancel(info.ID); err != nil {
			t.Fatalf("cancel of an idle session: %v", err)
		}
	}
	// `release` is never closed: every turn above ended through its cancel, which
	// is the property under test.
	select {
	case <-release:
		t.Fatal("release was closed")
	default:
	}
}

func TestCloseWhilePromptOutstanding(t *testing.T) {
	first := make(chan struct{}, 1)
	release := make(chan struct{})
	agentClosed := make(chan struct{}, 1)
	dir, srv := startHostWithServer(t, &gatedFactory{
		reply:   "slow answer",
		first:   first,
		release: release,
		closed:  agentClosed,
	})
	c := connect(t, dir)
	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	baseline := runtime.NumGoroutine()

	outcome := make(chan error, 1)
	go func() {
		_, err := c.Prompt(context.Background(), sid.ID, "q")
		outcome <- err
	}()
	<-first // the gated agent has streamed: the turn is really in flight

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v on a live peer", d)
	}

	select {
	case err := <-outcome:
		if err == nil {
			t.Fatal("the outstanding prompt reported success after Close")
		}
		if !strings.Contains(err.Error(), "closed by client") {
			t.Fatalf("prompt error = %v, want the client's close reason", err)
		}
	case <-time.After(edgeTimeout):
		t.Fatal("the outstanding prompt never returned after Close")
	}
	if err := c.ReadErr(); err == nil {
		t.Fatal("ReadErr is nil after Close")
	}

	// Close() waits for both of its goroutines: the reader reached EOF and the
	// dispatcher drained and stopped.
	select {
	case <-c.doneCh:
	default:
		t.Fatal("the read goroutine is still running after Close")
	}
	select {
	case <-c.dispDone:
	default:
		t.Fatal("the dispatch goroutine is still running after Close")
	}

	// The host owns the turn, so closing the client only detaches it: the turn
	// finishes on the host, persists, and then the now-idle orphan is closed —
	// which is what releases the agent. Waiting for that keeps the session store's
	// temp dir quiet before cleanup removes it.
	close(release)
	select {
	case <-agentClosed:
	case <-time.After(edgeTimeout):
		t.Fatal("the detached turn never finished on the host")
	}
	srv.Shutdown("test over")
	srv.Wait()
	waitGoroutines(t, baseline+1, "after Close and host shutdown")
}

func TestCloseIsIdempotentAndDoesNotHang(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done is still open right after Close")
	}
	// A second Close reports the already-closed socket (the CLI ignores it) but
	// must return promptly and never panic.
	start := time.Now()
	if err := c.Close(); err == nil {
		t.Log("second Close returned nil")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("second Close took %v", d)
	}
}

func TestMethodsAfterCloseFailFast(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	// A long CallTimeout proves the fast path: a closed client must report the
	// connection error immediately instead of waiting for the timeout.
	c, err := dial(t, Options{Dir: dir, Client: "test", CallTimeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	calls := []struct {
		name string
		fn   func() error
	}{
		{"ServerStatus", func() error { _, err := c.ServerStatus(); return err }},
		{"ListSessions", func() error { _, err := c.ListSessions(); return err }},
		{"Resume", func() error { _, err := c.Resume(sid.ID); return err }},
		{"CreateSession", func() error { _, err := c.CreateSession(t.TempDir(), "", ""); return err }},
		{"CloseSession", func() error { return c.CloseSession(sid.ID) }},
		{"Cancel", func() error { return c.Cancel(sid.ID) }},
		{"Decide", func() error { return c.Decide(sid.ID, "perm-1", true, false) }},
		{"Shutdown", func() error { return c.Shutdown() }},
		{"Prompt", func() error { _, err := c.Prompt(context.Background(), sid.ID, "q"); return err }},
		{"Hello", func() error { _, err := c.Hello("token"); return err }},
	}
	for _, call := range calls {
		start := time.Now()
		err := call.fn()
		if err == nil {
			t.Errorf("%s on a closed client returned no error", call.name)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s took %v on a closed client", call.name, d)
		}
	}

	// Close racing every call must not panic either.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.ServerStatus()
			_ = c.Close()
		}()
	}
	wg.Wait()
}

// ---- Done and ReadErr --------------------------------------------------

func TestPeerDisconnectClosesDoneWithEOF(t *testing.T) {
	dir, srv := startHostWithServer(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	if err := c.ReadErr(); err != nil {
		t.Fatalf("a live client reports ReadErr %v", err)
	}
	select {
	case <-c.Done():
		t.Fatal("Done closed while the host was still up")
	default:
	}

	srv.Shutdown("peer gone")
	select {
	case <-c.Done():
	case <-time.After(edgeTimeout):
		t.Fatal("Done did not close after the host went away")
	}
	err := c.ReadErr()
	if err == nil {
		t.Fatal("ReadErr is nil after the peer disconnected")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadErr = %v, want io.EOF for a clean disconnect", err)
	}
	// Reads are stable and calls report the same reason instead of blocking.
	if again := c.ReadErr(); again == nil || again.Error() != err.Error() {
		t.Fatalf("second ReadErr = %v, want %v", again, err)
	}
	if _, err := c.ServerStatus(); err == nil {
		t.Fatal("a call on a disconnected client succeeded")
	}
}

func TestReadErrIsNilUntilTheConnectionEnds(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	for i := 0; i < 3; i++ {
		if _, err := c.ServerStatus(); err != nil {
			t.Fatalf("server.status: %v", err)
		}
		if err := c.ReadErr(); err != nil {
			t.Fatalf("ReadErr = %v while the connection is healthy", err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.ReadErr(); err == nil || !strings.Contains(err.Error(), "closed by client") {
		t.Fatalf("ReadErr after Close = %v, want the client's own reason", err)
	}
}

// ---- typed protocol errors --------------------------------------------

func TestTypedProtocolErrors(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x", delay: 30 * time.Second})
	c := connect(t, dir)
	other := connect(t, dir)

	t.Run("busy", func(t *testing.T) {
		sid, err := c.CreateSession(t.TempDir(), "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		inflight := make(chan error, 1)
		go func() {
			_, err := c.Prompt(ctx, sid.ID, "first")
			inflight <- err
		}()
		waitFor(t, "the first turn to be in flight", func() bool {
			list, err := c.ListSessions()
			return err == nil && len(list) == 1 && list[0].Running
		})
		// Prompts are not queued: a second one on the same session is refused.
		_, err = c.Prompt(context.Background(), sid.ID, "second")
		protoErr := wantCode(t, err, hostapi.CodeBusy)
		if !strings.Contains(protoErr.Message, "turn") {
			t.Fatalf("message = %q, want a turn-in-flight complaint", protoErr.Message)
		}
		cancel()
		if err := <-inflight; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("first prompt: %v", err)
		}
	})

	t.Run("notfound", func(t *testing.T) {
		// A session id the store has never seen.
		_, err := c.Resume("no-such-session")
		wantCode(t, err, hostapi.CodeNotFound)
		// A permission request id that is not pending on an owned session.
		sid, err := c.CreateSession(t.TempDir(), "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		wantCode(t, c.Decide(sid.ID, "perm-does-not-exist", true, false), hostapi.CodeNotFound)
	})

	t.Run("forbidden", func(t *testing.T) {
		// A session another connection owns is not addressable from this one.
		sid, err := other.CreateSession(t.TempDir(), "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		wantCode(t, c.CloseSession(sid.ID), hostapi.CodeForbidden)
		wantCode(t, c.Cancel(sid.ID), hostapi.CodeForbidden)
		_, err = c.Prompt(context.Background(), sid.ID, "hijack")
		wantCode(t, err, hostapi.CodeForbidden)
	})

	t.Run("invalidparams", func(t *testing.T) {
		_, err := c.Resume("")
		wantCode(t, err, hostapi.CodeInvalidParams)
		notDir := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		_, err = c.CreateSession(notDir, "", "")
		wantCode(t, err, hostapi.CodeInvalidParams)
	})
}

func TestMalformedServerLineFailsConnectionCleanly(t *testing.T) {
	// One undecodable line ends the connection (the client does not skip it), and
	// every outstanding call gets that decode error instead of its own timeout.
	garbage := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(garbage) }) }
	t.Cleanup(release)

	dir, _ := startRawHost(t, func(rc *rawConn) {
		env, err := rc.read()
		if err != nil {
			return
		}
		_ = rc.send(map[string]any{
			"id":     env.ID,
			"result": hostapi.HelloResult{Version: hostapi.Version, PID: os.Getpid()},
		})
		<-garbage
		_ = rc.sendRaw("{this is not json")
		drainQuietly(rc)
	})

	c, err := dial(t, Options{Dir: dir, Token: "t", Client: "test", CallTimeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	outstanding := make(chan error, 1)
	go func() {
		_, err := c.ServerStatus() // the fixture never answers this
		outstanding <- err
	}()
	release()

	select {
	case <-c.Done():
	case <-time.After(edgeTimeout):
		t.Fatal("the connection survived a malformed line")
	}
	readErr := c.ReadErr()
	if readErr == nil || !strings.Contains(readErr.Error(), "decode line") {
		t.Fatalf("ReadErr = %v, want a decode error", readErr)
	}
	select {
	case err := <-outstanding:
		if err == nil {
			t.Fatal("the outstanding call reported success")
		}
		if !strings.Contains(err.Error(), "decode line") {
			t.Fatalf("outstanding call error = %v, want the decode error", err)
		}
	case <-time.After(edgeTimeout):
		t.Fatal("the outstanding call was not woken by the read failure")
	}
	// Even with a long CallTimeout, later calls fail fast.
	start := time.Now()
	if _, err := c.ServerStatus(); err == nil {
		t.Fatal("call on a broken connection succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("call on a broken connection took %v", d)
	}
}

// ---- resume, list, create ---------------------------------------------

func TestListSessionsEmptyThenPopulated(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)

	list, err := c.ListSessions()
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a fresh connection lists %d sessions, want 0", len(list))
	}

	cwds := []string{t.TempDir(), t.TempDir()}
	ids := make([]string, len(cwds))
	for i, cwd := range cwds {
		info, err := c.CreateSession(cwd, "", fmt.Sprintf("title-%d", i))
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if info.ID == "" || info.Cwd != cwd {
			t.Fatalf("session info = %+v", info)
		}
		ids[i] = info.ID
	}

	list, err = c.ListSessions()
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	if len(list) != len(ids) {
		t.Fatalf("session.list = %d sessions, want %d", len(list), len(ids))
	}
	seen := map[string]bool{}
	for _, info := range list {
		seen[info.ID] = true
		if info.CreatedAt.IsZero() || info.UpdatedAt.IsZero() {
			t.Fatalf("session %s has no timestamps: %+v", info.ID, info)
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("session.list is missing %s", id)
		}
	}

	// The listing is per-connection by contract: isolation is the point of the
	// host, so a second UI sees none of the first one's sessions.
	stranger := connect(t, dir)
	other, err := stranger.ListSessions()
	if err != nil {
		t.Fatalf("session.list on a second connection: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("a second connection sees %d of another connection's sessions", len(other))
	}

	// Closing a session removes it from the listing immediately.
	if err := c.CloseSession(ids[0]); err != nil {
		t.Fatalf("close session: %v", err)
	}
	list, err = c.ListSessions()
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	if len(list) != 1 || list[0].ID != ids[1] {
		t.Fatalf("after close, session.list = %+v", list)
	}
}

func TestCreateSessionDefaultCwdAndModelOverride(t *testing.T) {
	f := &scriptedFactory{reply: "x"}
	dir := startHost(t, f)
	c := connect(t, dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// Empty cwd means the host's own working directory, and the model falls back
	// to the factory default.
	info, err := c.CreateSession("", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.Cwd != wd {
		t.Fatalf("default cwd = %q, want the host's %q", info.Cwd, wd)
	}
	if info.Model != "scripted-model" || info.Provider != "scripted" {
		t.Fatalf("default model/provider = %q/%q", info.Model, info.Provider)
	}
	if info.Title != "" {
		t.Fatalf("default title = %q", info.Title)
	}

	// A model override reaches the factory and is echoed back on the session.
	over, err := c.CreateSession(t.TempDir(), "override-model", "custom title")
	if err != nil {
		t.Fatalf("create with model: %v", err)
	}
	if over.Model != "override-model" {
		t.Fatalf("model = %q, want the override", over.Model)
	}
	if over.Title != "custom title" {
		t.Fatalf("title = %q", over.Title)
	}

	// A relative cwd resolves against the host's working directory.
	rel, err := c.CreateSession(".", "", "")
	if err != nil {
		t.Fatalf("create with relative cwd: %v", err)
	}
	if rel.Cwd != wd {
		t.Fatalf("resolved cwd = %q, want %q", rel.Cwd, wd)
	}
}

func TestResumeUnknownSessionIsTypedNotFound(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	if _, err := c.Resume("0123456789abcdef"); !errors.As(err, new(*hostapi.Error)) {
		t.Fatalf("resume of an unknown id = %v, want *hostapi.Error", err)
	} else {
		wantCode(t, err, hostapi.CodeNotFound)
	}
}

// ---- prompt streaming --------------------------------------------------

func TestPromptStreamOrderingAndResultFields(t *testing.T) {
	// The host writes the turn's events, then the prompt response, so turn_end is
	// already queued when Prompt returns — but the callback runs on the dispatch
	// goroutine, so it may be invoked just before *or* just after Prompt returns.
	// A client must therefore poll for the event; this test pins the ordering it
	// can rely on (deltas in order, turn_end last, result repeating the text).
	const reply = "streamed answer"
	dir := startHost(t, &scriptedFactory{reply: reply})
	c := connect(t, dir)

	type event struct{ typ, text string }
	var mu sync.Mutex
	var events []event
	c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			return
		}
		mu.Lock()
		events = append(events, event{typ: ev.Type, text: ev.Text})
		mu.Unlock()
	})

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := c.Prompt(context.Background(), sid.ID, "question")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.SessionID != sid.ID {
		t.Fatalf("result session = %q, want %q", res.SessionID, sid.ID)
	}
	if res.Text != reply {
		t.Fatalf("result text = %q, want %q", res.Text, reply)
	}
	if res.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if res.Error != "" {
		t.Fatalf("result carries error %q", res.Error)
	}

	waitFor(t, "the turn's three events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 3
	})
	mu.Lock()
	got := append([]event(nil), events...)
	mu.Unlock()

	if got[0].typ != hostapi.SessionEventTextDelta || got[1].typ != hostapi.SessionEventTextDelta {
		t.Fatalf("event order = %+v, want two deltas first", got)
	}
	if got[2].typ != hostapi.SessionEventTurnEnd {
		t.Fatalf("event order = %+v, want turn_end third", got)
	}
	if joined := got[0].text + got[1].text; joined != reply {
		t.Fatalf("deltas %q + %q != %q", got[0].text, got[1].text, reply)
	}
	if len(got) != 3 {
		t.Fatalf("turn produced %d events, want 3: %+v", len(got), got)
	}
}

// ---- scripted fixtures with gates -------------------------------------

// gatedFactory builds agents whose turn the test steps by hand: the first half
// of the reply is streamed and signalled on first, then the turn blocks until
// release is closed. That makes "subscribe mid-turn" and "cancel a running turn"
// deterministic without a sleep.
type gatedFactory struct {
	reply   string
	first   chan<- struct{}
	release <-chan struct{}
	// closed, when set, is signalled by Agent.Close so a test can wait for the
	// host to have finished with (and persisted) a turn.
	closed chan<- struct{}
}

func (f *gatedFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "gated-model"
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = "gated system prompt"
	}
	return &gatedAgent{
		spec:     spec,
		messages: append(agentcore.MessageList(nil), spec.Messages...),
		reply:    f.reply,
		first:    f.first,
		release:  f.release,
		closed:   f.closed,
	}, nil
}

type gatedAgent struct {
	mu       sync.Mutex
	spec     hostserver.AgentSpec
	messages agentcore.MessageList
	reply    string
	first    chan<- struct{}
	release  <-chan struct{}
	closed   chan<- struct{}
}

func (a *gatedAgent) Run(ctx context.Context, prompt string, h pigoruntime.StreamHandler) (*agentcore.AssistantMessage, error) {
	a.mu.Lock()
	if prompt != "" {
		a.messages = append(a.messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	reply := a.reply
	a.mu.Unlock()

	half := len(reply) / 2
	if h.OnText != nil {
		h.OnText(reply[:half])
	}
	select {
	case a.first <- struct{}{}:
	default:
	}
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	if h.OnText != nil {
		h.OnText(reply[half:])
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
	}
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.mu.Unlock()
	return &msg, nil
}

func (a *gatedAgent) Messages() agentcore.MessageList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(agentcore.MessageList(nil), a.messages...)
}

func (a *gatedAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *gatedAgent) Model() string        { return a.spec.Model }
func (a *gatedAgent) Provider() string     { return "gated" }

// Close reports that the host has released the agent, which happens only after
// the turn's transcript was persisted.
func (a *gatedAgent) Close() {
	if a.closed != nil {
		select {
		case a.closed <- struct{}{}:
		default:
		}
	}
}
