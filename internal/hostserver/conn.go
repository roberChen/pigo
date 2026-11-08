// This file implements one client connection: the NDJSON line loop, the hello
// handshake, the per-connection session ownership set, and the serialized
// writer every goroutine that answers a request or pushes an event goes
// through.
//
// Two concurrency rules keep a connection responsive. Each request is handled on
// its own goroutine, so a long prompt never blocks the read loop — otherwise
// `cancel` and `permission.decide` could not arrive while the turn that needs
// them is running. And every write takes conn.writeMu, because responses,
// streamed session events and permission requests are produced by different
// goroutines and must not interleave bytes inside a JSON line.
package hostserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
)

// conn is one authenticated client connection.
type conn struct {
	srv *Server
	nc  net.Conn

	// writeMu serializes whole JSON lines onto the socket.
	writeMu sync.Mutex

	// mu guards authed, client and owned. It is deliberately separate from
	// writeMu so taking it never blocks behind a slow socket write.
	mu     sync.Mutex
	authed bool
	client string
	owned  map[string]*sessionState

	closeOnce sync.Once
	closed    chan struct{}
}

// newConn wraps an accepted socket.
func newConn(srv *Server, nc net.Conn) *conn {
	return &conn{
		srv:    srv,
		nc:     nc,
		owned:  make(map[string]*sessionState),
		closed: make(chan struct{}),
	}
}

// serve runs the read loop until the peer disconnects or the server shuts down.
func (c *conn) serve() {
	defer c.close()
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			c.replyError("", codeParse, "invalid JSON: "+err.Error())
			continue
		}
		// Each request is independent; the loop must keep reading so a second
		// request (cancel, permission.decide, a session.create from a multiplexing
		// UI) is never starved by a long one.
		go c.handle(env)
	}
}

// handle dispatches one envelope and writes exactly one response.
func (c *conn) handle(env envelope) {
	if env.IsEvent() {
		c.replyError(env.ID, codeInvalidRequest, "clients may not send events")
		return
	}
	if env.ID == "" || env.Method == "" {
		c.replyError(env.ID, codeInvalidRequest, "request needs an id and a method")
		return
	}
	c.srv.wg.Add(1)
	defer c.srv.wg.Done()
	if env.Method != methodHello && !c.isAuthed() {
		c.replyError(env.ID, codeUnauthorized, "send hello with the server token first")
		return
	}

	switch env.Method {
	case methodHello:
		c.handleHello(env)
	case methodServerStatus:
		c.handleServerStatus(env)
	case methodServerShutdown:
		c.handleServerShutdown(env)
	case methodSessionCreate:
		c.handleSessionCreate(env)
	case methodSessionList:
		c.handleSessionList(env)
	case methodSessionResume:
		c.handleSessionResume(env)
	case methodSessionClose:
		c.handleSessionClose(env)
	case methodPrompt:
		c.handlePrompt(env)
	case methodCancel:
		c.handleCancel(env)
	case methodPermissionDecide:
		c.handlePermissionDecide(env)
	default:
		c.replyError(env.ID, codeMethodNotFound, "unknown method "+env.Method)
	}
}

// isAuthed reports whether hello has succeeded on this connection.
func (c *conn) isAuthed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authed
}

// close tears the connection down. It is idempotent because both the read loop
// and a server shutdown may call it.
//
// A turn belongs to the host, so a disconnect only *detaches* its sessions: a
// running turn finishes and persists, and an idle session is closed. During
// shutdown there is no host left to finish anything, so sessions are closed
// outright. Either way the transcript on disk is the reconnect path.
func (c *conn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.nc.Close()
		shutdown := c.srv.isShuttingDown()
		for _, s := range c.snapshotOwned() {
			if shutdown {
				c.srv.closeSession(s, "server shutdown")
			} else {
				c.srv.detachSession(s)
			}
		}
	})
}

// snapshotOwned returns a copy of the owned session list so callers can iterate
// without holding c.mu across a close (which takes other locks).
func (c *conn) snapshotOwned() []*sessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*sessionState, 0, len(c.owned))
	for _, s := range c.owned {
		out = append(out, s)
	}
	return out
}

// owns reports whether this connection owns the session. Ownership, not
// existence, is what authorizes every session-scoped request: two UIs attached
// to one host must not be able to drive each other's turns.
func (c *conn) owns(id string) (*sessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.owned[id]
	return s, ok
}

// claim records ownership of a session.
func (c *conn) claim(s *sessionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.owned[s.id] = s
}

// release forgets a session (after close).
func (c *conn) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owned, id)
}

// writeLine marshals v and writes it as one line. Writes are serialized and
// never block the caller indefinitely: a dead socket makes the write fail,
// which callers report and then drop the notification.
func (c *conn) writeLine(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.nc.Write(data)
	return err
}

// reply sends a successful result. A nil result is encoded as an empty object so
// the response still satisfies IsResponse.
func (c *conn) reply(id string, result any) {
	if result == nil {
		result = struct{}{}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		c.replyError(id, codeInternal, err.Error())
		return
	}
	_ = c.writeLine(envelope{ID: id, Result: raw})
}

// replyError sends a structured failure for request id.
func (c *conn) replyError(id string, code int, message string) {
	_ = c.writeLine(envelope{ID: id, Error: &protoError{Code: code, Message: message}})
}

// notify pushes a server event. A failed write means the peer is gone; the read
// loop will observe the closed socket and clean up, so the error is only
// returned for tests.
func (c *conn) notify(event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.writeLine(envelope{Event: event, Params: raw})
}

// errConnClosed is returned by helpers that race with a disconnect.
var errConnClosed = errors.New("host: connection closed")

// isClosed reports whether the connection has been torn down.
func (c *conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// ensure io is used for the reader docs above (Scanner wraps it).
var _ io.Reader = (*net.TCPConn)(nil)
