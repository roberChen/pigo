// This file implements the host daemon: the singleton socket, the connection
// registry, the per-method handlers, and the permission gate that stands in for
// the REPL's terminal confirmation prompt.
//
// The design goal is that the host — not the UI — owns the session: a turn keeps
// running when its client disconnects, and a second client can neither see nor
// steer the first one's sessions (docs/architecture.md §6.1/§6.5). Everything
// transport-independent (run assembly, persistence) is delegated to the Factory
// seam and internal/session; this file is only the machine that wires them to
// the wire protocol.
package hostserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/diag"
	"github.com/roberChen/pigo/internal/hostapi"
	"github.com/roberChen/pigo/internal/hostpath"
	"github.com/roberChen/pigo/internal/runtime"
	"github.com/roberChen/pigo/internal/session"
	"github.com/roberChen/pigo/internal/trust"
)

// maxLineBytes bounds one NDJSON line. A prompt can be large (a pasted file), and
// a resume result can be much larger, but both are far below this; the cap only
// exists so a hostile client cannot make the server allocate without bound.
const maxLineBytes = 32 << 20

// shutdownWriteGrace bounds the final event write to each client. A client that
// stopped reading would otherwise block the write until its pipe is drained,
// which shutdown must not wait for.
const shutdownWriteGrace = 2 * time.Second

// handlerShutdownGrace bounds the wait for request handlers to cancel, persist
// and close their agents during shutdown.
const handlerShutdownGrace = 5 * time.Second

// Config controls a host server.
type Config struct {
	// Dir is the state directory (socket, token, state file). Defaults to
	// DefaultDir().
	Dir string
	// Socket overrides the socket path; empty derives it from Dir.
	Socket string
	// Factory builds the agent behind each session. Required.
	Factory Factory
	// Store is the session store. Defaults to opening the default store.
	Store *session.Store
	// Trust gates side-effect tools per directory. When nil, a manager over the
	// default trust file is created (or trust is disabled if that fails).
	Trust *trust.Manager
	// Daemon records whether this host was started detached; it is reported by
	// server.status and echoed into the state file.
	Daemon bool
	// Build is the version string reported to clients ("dev" from source).
	Build string
	// PermissionTimeout bounds how long a permission request waits for its owner.
	// 0 waits until the client answers or the turn is cancelled.
	PermissionTimeout time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Server is a running host.
type Server struct {
	cfg    Config
	dir    string
	socket string
	store  *session.Store
	trust  *trust.Manager

	ln       net.Listener
	token    string
	mu       sync.Mutex
	conns    map[*conn]struct{}
	sessions map[string]*sessionState

	startedAt time.Time
	pid       int
	// lastIDTime is the last timestamp handed to session.NewID by this host. New
	// ids are forced strictly past it, because NewID has only microsecond
	// resolution and every create request runs on its own goroutine: two creates
	// landing in the same microsecond would otherwise get the same id, and then
	// share one on-disk transcript.
	lastIDTime time.Time

	// wg tracks in-flight request handlers so Shutdown can wait for them.
	wg      sync.WaitGroup
	turnSeq uint64

	shutdownOnce sync.Once
	// runCtx cancels agent assembly and in-flight turns during shutdown. It is
	// deliberately not tied to a client connection: an ordinary disconnect must
	// not cancel a running turn.
	runCtx    context.Context
	runCancel context.CancelFunc
	// done is closed as soon as shutdown begins, which is what unblocks clients
	// and the accept loop; stopped is closed after the last cleanup step, so
	// Wait reports a host whose files are actually gone.
	done    chan struct{}
	stopped chan struct{}
	// shutdownReason is the reason passed to Shutdown, reported to clients.
	shutdownReason string
}

// ErrAlreadyRunning reports that another host owns the socket.
type ErrAlreadyRunning struct {
	PID int
}

func (e *ErrAlreadyRunning) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("a pigo host is already running (pid %d)", e.PID)
	}
	return "a pigo host is already running"
}

// NewServer validates cfg and constructs the server. Listen must be called
// before Serve.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Factory == nil {
		return nil, errors.New("host: Config.Factory is required")
	}
	dir := cfg.Dir
	if dir == "" {
		dir = DefaultDir()
	}
	if dir == "" {
		return nil, errors.New("host: cannot resolve the state directory (set PIGO_HOME)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("host: create state dir: %w", err)
	}
	socket := cfg.Socket
	if socket == "" {
		socket = SocketPath(dir)
	}
	store := cfg.Store
	if store == nil {
		s, err := session.NewStore(session.DefaultDir())
		if err != nil {
			return nil, err
		}
		store = s
	}
	mgr := cfg.Trust
	if mgr == nil {
		m, err := trust.NewManager(trust.DefaultPath())
		if err != nil {
			// Trust is a safety feature: if its store is unreadable, run without it
			// rather than refusing to serve, matching how the REPL degrades.
			diag.Diagf("host: trust store: %v", err)
		} else {
			mgr = m
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	// Store the resolved clock on cfg: handlers reach it through cfg.Now().
	cfg.Now = now
	runCtx, runCancel := context.WithCancel(context.Background())
	return &Server{
		cfg:       cfg,
		dir:       dir,
		socket:    socket,
		store:     store,
		trust:     mgr,
		conns:     make(map[*conn]struct{}),
		sessions:  make(map[string]*sessionState),
		startedAt: now(),
		pid:       os.Getpid(),
		runCtx:    runCtx,
		runCancel: runCancel,
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}, nil
}

// Socket returns the bound socket path (valid after Listen).
func (s *Server) Socket() string { return s.socket }

// Dir returns the state directory.
func (s *Server) Dir() string { return s.dir }

// Listen binds the socket and publishes the token and state files. It refuses to
// start when another live host owns the socket; a stale socket left by a crashed
// host is removed (see probeExisting).
func (s *Server) Listen() error {
	if probeExisting(s.socket, 200*time.Millisecond) {
		pid := 0
		if st, err := ReadState(s.dir); err == nil {
			pid = st.PID
		}
		return &ErrAlreadyRunning{PID: pid}
	}
	// Only remove an endpoint when the platform proves it is stale. A timeout or
	// permission failure is not evidence that a live listener may be unlinked.
	if !hostpath.CanCleanEndpoint(s.socket) {
		return fmt.Errorf("host: cannot verify endpoint %s", s.socket)
	}
	CleanEndpoint(s.socket)
	// The transport restricts the endpoint to its owner (socket mode on POSIX,
	// a one-ACE pipe DACL on Windows); the token is the explicit second factor
	// for a same-user process that found it.
	ln, err := Listen(s.socket)
	if err != nil {
		// A second bind on an existing name is "already running", not a raw
		// transport error. On Windows it is also the common startup race:
		// probeExisting dials, and a named pipe only accepts a connection while
		// its owner is in Accept (Serve), so a host that has bound the pipe but
		// not reached Serve yet probes as dead.
		if hostpath.IsEndpointInUse(err) {
			pid := 0
			if st, rerr := ReadState(s.dir); rerr == nil {
				pid = st.PID
			}
			return &ErrAlreadyRunning{PID: pid}
		}
		return fmt.Errorf("host: listen %s: %w", s.socket, err)
	}
	token, err := NewToken()
	if err != nil {
		ln.Close()
		return err
	}
	if err := WriteToken(s.dir, token); err != nil {
		ln.Close()
		return err
	}
	s.token = token
	s.ln = ln
	st := State{PID: s.pid, Version: Version, StartedAt: s.startedAt, Socket: s.socket}
	if data, err := marshalState(st); err == nil {
		if err := writeFileAtomic(StatePath(s.dir), data, 0o600); err != nil {
			ln.Close()
			return fmt.Errorf("host: write state: %w", err)
		}
	}
	return nil
}

// Serve accepts connections until ctx is cancelled or Shutdown is called. It is
// safe to call after Listen only.
//
// Serve does not return until the shutdown has finished removing the host's
// files. A shutdown that starts in another goroutine (ctx cancellation, or a
// server.shutdown request) closes the listener first, which lets this loop
// return while cleanup is still running; a caller that then exits the process
// leaves the state and token files behind (observed on Windows, where the
// daemon lost that race every time).
func (s *Server) Serve(ctx context.Context) error {
	err := s.serve(ctx)
	// Idempotent: this is a no-op when serve() already triggered the shutdown,
	// and the cleanup path when it returned on its own.
	s.Shutdown("serve loop exited")
	s.Wait()
	return err
}

// serve is the accept loop behind Serve; it returns as soon as the listener
// stops accepting, without waiting for the shutdown cleanup.
func (s *Server) serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("host: Serve called before Listen")
	}
	// A cancelled ctx must unblock Accept.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.Shutdown("context cancelled")
		case <-s.done:
		case <-stop:
		}
	}()
	defer close(stop)
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("host: accept: %w", err)
		}
		s.mu.Lock()
		if s.isShuttingDown() {
			s.mu.Unlock()
			_ = nc.Close()
			return nil
		}
		c := newConn(s, nc)
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

// serveConn runs one connection and unregisters it when it ends.
func (s *Server) serveConn(c *conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	c.serve()
}

// Shutdown stops accepting, tells clients, closes their connections and removes
// the host's files. It is idempotent and safe to call from a request handler.
func (s *Server) Shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		s.shutdownReason = reason
		close(s.done)
		s.runCancel()
		if s.ln != nil {
			// go-winio can deadlock when Close races an in-flight Accept: its
			// listener routine may be waiting for the response channel while the
			// accept loop is shutting down. Close it asynchronously so the accept
			// loop and cleanup never wait on that third-party lock.
			go func() { _ = s.ln.Close() }()
		}
		// Tell every client before closing, so an attached UI can print a reason
		// instead of a bare EOF. A client that stopped reading cannot be allowed
		// to hold the shutdown open: a full pipe buffer blocks the write forever,
		// and Serve now waits for this cleanup before returning.
		conns := s.snapshotConns()
		deadline := time.Now().Add(shutdownWriteGrace)
		for _, c := range conns {
			_ = c.nc.SetWriteDeadline(deadline)
		}
		for _, c := range conns {
			_ = c.notify(eventServerShutdown, hostapi.ServerShutdownEvent{Reason: reason})
		}
		for _, c := range conns {
			c.close()
		}
		// Detached sessions have no owning connection to close. They must still
		// be cancelled and tombstoned so no turn can commit after shutdown.
		for _, st := range s.snapshotSessions() {
			s.closeSession(st, "server shutdown")
		}
		cleanup(s.dir)
		close(s.stopped)
	})
}

// Wait blocks until shutdown cleanup has finished and request handlers have
// had a bounded window to cancel, persist and close their agents.
func (s *Server) Wait() {
	<-s.stopped
	handlers := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(handlers)
	}()
	select {
	case <-handlers:
	case <-time.After(handlerShutdownGrace):
	}
}

// snapshotSessions copies the live session set for shutdown.
func (s *Server) snapshotSessions() []*sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sessionState, 0, len(s.sessions))
	for _, st := range s.sessions {
		out = append(out, st)
	}
	return out
}

// snapshotConns copies the connection set.
func (s *Server) snapshotConns() []*conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, c)
	}
	return out
}

// liveSession returns the live session with id, or nil. Used by tests and by
// status reporting to reach a session without going through a connection.
func (s *Server) liveSession(id string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// liveSessionCount is the number of sessions currently attached to a client.
func (s *Server) liveSessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// closeSession ends a session: it aborts a turn, releases the agent, drops it
// from the live map and notifies the owner. Persistence has already happened by
// the time the turn ended, so this is not destructive.
func (s *Server) closeSession(st *sessionState, reason string) {
	s.closeSessionIf(st, reason, nil)
}

// closeSessionIfUnattached closes a session only if no client has adopted it,
// no turn is in flight and no read-only observer remains. The disconnect path
// must not kill a running turn, and an observed session must survive its turn's
// completion long enough for the observer to adopt it.
func (s *Server) closeSessionIfUnattached(st *sessionState, reason string) {
	s.closeSessionIf(st, reason, func(st *sessionState) bool {
		return st.owner == nil && !st.running && len(st.observers) == 0
	})
}

// closeSessionIf ends a session when keep reports true. Closing a running
// session normally leaves a tombstone until its prompt has persisted; shutdown
// instead releases resources immediately so an uncooperative agent cannot hang
// process exit.
func (s *Server) closeSessionIf(st *sessionState, reason string, keep func(*sessionState) bool) {
	s.mu.Lock()
	if _, ok := s.sessions[st.id]; !ok {
		s.mu.Unlock()
		// A create/resume handler may be aborted before it ever published the
		// session. It still owns an assembled agent that must be released.
		st.mu.Lock()
		if st.closed {
			st.mu.Unlock()
			return
		}
		st.closed = true
		agent := st.agent
		st.agent = nil
		st.mu.Unlock()
		st.perms.abortAll()
		if agent != nil {
			agent.Close()
		}
		return
	}
	st.mu.Lock()
	if st.closed || (keep != nil && !keep(st)) {
		st.mu.Unlock()
		s.mu.Unlock()
		return
	}
	st.closed = true
	cancel := st.cancel
	st.cancel = nil
	agent := st.agent
	st.agent = nil
	owner := st.owner
	closeOnFinish := st.running && !s.isShuttingDown()
	st.closeOnFinish = closeOnFinish
	st.owner = nil
	st.mu.Unlock()
	if !closeOnFinish {
		delete(s.sessions, st.id)
	}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	st.perms.abortAll()
	if agent != nil && !closeOnFinish {
		agent.Close()
	}
	if owner != nil {
		_ = owner.notify(eventSessionClosed, hostapi.SessionClosedEvent{SessionID: st.id, Reason: reason})
		owner.release(st.id)
	}
	st.closeObservers(reason)
}

// detachSession forgets the connection that owned a session without necessarily
// ending it. A turn in flight keeps running (the host, not the UI, owns it) and
// the handler closes the orphan once it finishes; an idle session is closed
// immediately.
func (s *Server) detachSession(st *sessionState) {
	st.mu.Lock()
	st.owner = nil
	st.mu.Unlock()
	s.closeSessionIfUnattached(st, "client disconnected")
}

// adoptSession hands a live, idle, unowned session to a reconnecting client
// instead of replaying it from disk into a second agent. That keeps "disconnect
// and resume" from racing a still-running turn with a second writer.
//
// An adopted session's earlier read-only watchers are dropped: the session now
// belongs to this client, and emit fans events out to owner *and* observers, so
// leaving them registered would stream the new owner's turn to another UI.
func (s *Server) adoptSession(id string, c *conn) (*sessionState, bool) {
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		return nil, false
	}
	st.mu.Lock()
	if st.closed || st.running || st.owner != nil || st.agent == nil {
		st.mu.Unlock()
		return nil, false
	}
	st.owner = c
	dropped := st.takeObservers()
	st.mu.Unlock()
	st.releaseObservers(dropped, "session adopted by another client")
	return st, true
}

// nextTurnID allocates a unique id for one prompt's event log.
func (s *Server) nextTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnSeq++
	return fmt.Sprintf("turn-%d", s.turnSeq)
}

// nextSessionTime returns a clock value for a new session id. Allocations are
// strictly increasing (at microsecond granularity, which is what session.NewID
// encodes), so concurrent creates cannot collide; the clock only advances into the
// future when two creates land inside the same microsecond.
func (s *Server) nextSessionTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now().Truncate(time.Microsecond)
	if !now.After(s.lastIDTime) {
		now = s.lastIDTime.Add(time.Microsecond)
	}
	s.lastIDTime = now
	return now
}

// reserveSession publishes a placeholder before Factory.NewAgent runs. A second
// resume sees the owner and cannot build a second writer over the same file.
// The placeholder has no agent and is removed by abandonSession on failure.
func (s *Server) reserveSession(c *conn, st *sessionState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isShuttingDown() || s.sessions[st.id] != nil {
		return false
	}
	s.sessions[st.id] = st
	c.claim(st)
	return true
}

// abandonSession rolls back a failed create/resume reservation.
func (s *Server) abandonSession(c *conn, st *sessionState) {
	s.mu.Lock()
	if s.sessions[st.id] == st {
		delete(s.sessions, st.id)
	}
	s.mu.Unlock()
	st.mu.Lock()
	st.closed = true
	st.mu.Unlock()
	st.perms.abortAll()
	c.release(st.id)
}

// publishSession commits a previously reserved session, or closes it if the
// host began shutting down or its owner vanished during agent assembly.
func (s *Server) publishSession(c *conn, st *sessionState) bool {
	if s.isShuttingDown() {
		s.closeSession(st, "server shutdown")
		return false
	}
	s.mu.Lock()
	current := s.sessions[st.id]
	if current != nil && current != st {
		s.mu.Unlock()
		s.closeSession(st, "session replaced")
		return false
	}
	s.sessions[st.id] = st
	s.mu.Unlock()
	c.claim(st)

	st.mu.Lock()
	owner := st.owner
	st.mu.Unlock()
	if owner == nil || owner.isClosed() {
		s.detachSession(st)
	}
	return true
}

// beginRequest admits a handler into the shutdown wait group atomically. Once
// shutdown starts, no new handler may commit work.
func (s *Server) beginRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isShuttingDown() {
		return false
	}
	s.wg.Add(1)
	return true
}

// isShuttingDown reports whether Shutdown has begun. A disconnect during shutdown
// closes sessions instead of letting their turns outlive the host.
func (s *Server) isShuttingDown() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// ---- handlers ---------------------------------------------------------

func (c *conn) handleHello(env envelope) {
	var p hostapi.HelloParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	if c.srv.token == "" {
		c.replyError(env.ID, codeUnavailable, "host is shutting down")
		return
	}
	if len(env.Params) == 0 || bytes.Equal(bytes.TrimSpace(env.Params), []byte("null")) {
		c.replyError(env.ID, codeUnauthorized, "hello with the server token is required")
		go c.close()
		return
	}
	if p.Version > Version {
		c.replyError(env.ID, codeInvalidParams, fmt.Sprintf("protocol version %d is newer than this host (%d)", p.Version, Version))
		return
	}
	if p.Version < 0 {
		c.replyError(env.ID, codeInvalidParams, fmt.Sprintf("protocol version %d is not supported by this host (%d)", p.Version, Version))
		return
	}
	if !tokenMatches(c.srv.token, p.Token) {
		c.replyError(env.ID, codeUnauthorized, "invalid server token")
		// A bad token is not retried on this connection: close after the reply so
		// a probe cannot keep guessing.
		go c.close()
		return
	}
	c.mu.Lock()
	if c.authed {
		c.mu.Unlock()
		c.replyError(env.ID, codeUnauthorized, "hello has already been sent on this connection")
		return
	}
	c.authed = true
	c.client = p.Client
	c.mu.Unlock()
	c.reply(env.ID, hostapi.HelloResult{
		Version: Version,
		PID:     c.srv.pid,
		Build:   c.srv.cfg.Build,
		Started: c.srv.startedAt,
	})
}

func (c *conn) handleServerStatus(env envelope) {
	c.reply(env.ID, hostapi.ServerStatusResult{
		PID:      c.srv.pid,
		Build:    c.srv.cfg.Build,
		Started:  c.srv.startedAt,
		Sessions: c.srv.liveSessionCount(),
		Socket:   c.srv.socket,
		Daemon:   c.srv.cfg.Daemon,
	})
}

func (c *conn) handleServerShutdown(env envelope) {
	c.reply(env.ID, struct {
		OK bool `json:"ok"`
	}{OK: true})
	// Shutdown from a handler: run it on its own goroutine so the reply is flushed
	// and the handler's WaitGroup entry is released before the accept loop closes.
	go c.srv.Shutdown("requested by " + c.clientName())
}

// clientName is the label sent in hello, for shutdown reasons and status.
func (c *conn) clientName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == "" {
		return "client"
	}
	return c.client
}

func (c *conn) handleSessionCreate(env envelope) {
	var p hostapi.SessionCreateParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	if c.srv.isShuttingDown() {
		c.replyError(env.ID, codeUnavailable, "host is shutting down")
		return
	}
	fallback, _ := os.Getwd()
	cwd := resolveCwd(p.Cwd, fallback)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		c.replyError(env.ID, codeInvalidParams, "cwd is not a directory: "+cwd)
		return
	}
	now := c.srv.nextSessionTime()
	id := session.NewID(now)
	st := &sessionState{
		id:    id,
		srv:   c.srv,
		owner: c,
		cwd:   cwd,
		title: p.Title,
		perms: newPermissionTable(),
	}
	if !c.srv.reserveSession(c, st) {
		c.replyError(env.ID, codeUnavailable, "session cannot be created")
		return
	}
	ctx, stop := c.assemblyContext()
	defer stop()
	agent, err := c.srv.cfg.Factory.NewAgent(ctx, AgentSpec{
		SessionID:      id,
		Cwd:            cwd,
		Model:          p.Model,
		BeforeToolCall: c.srv.permissionGate(st),
	})
	if err != nil {
		c.srv.abandonSession(c, st)
		c.replyError(env.ID, codeInternal, err.Error())
		return
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		agent.Close()
		c.replyError(env.ID, codeUnavailable, "session was closed during assembly")
		return
	}
	st.agent = agent
	st.header = newSessionHeader(id, now, cwd, agent.Model(), agent.Provider(), agent.SystemPrompt())
	st.mu.Unlock()

	if !c.srv.publishSession(c, st) {
		c.replyError(env.ID, codeUnavailable, "host is shutting down")
		return
	}
	c.reply(env.ID, hostapi.SessionCreateResult{Session: st.infoLocked()})
}

// infoLocked renders a session's metadata under its own lock.
func (s *sessionState) infoLocked() hostapi.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info()
}

func (c *conn) handleSessionList(env envelope) {
	owned := c.snapshotOwned()
	infos := make([]hostapi.SessionInfo, 0, len(owned))
	for _, st := range owned {
		infos = append(infos, st.infoLocked())
	}
	c.reply(env.ID, hostapi.SessionListResult{Sessions: infos})
}

func (c *conn) handleSessionResume(env envelope) {
	var p hostapi.SessionResumeParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	if p.ID == "" {
		c.replyError(env.ID, codeInvalidParams, "session id is required")
		return
	}
	// Ownership is checked before existence so a connection cannot probe which
	// session ids exist by watching the error code change.
	if _, ok := c.owns(p.ID); ok {
		c.replyError(env.ID, codeBusy, "session is already attached to this connection")
		return
	}
	if c.watching(p.ID) {
		st := c.srv.liveSession(p.ID)
		if st == nil {
			c.unwatch(p.ID)
		} else if rr, ok := st.replay(); ok {
			// Repeated read-only inspection is idempotent: return the latest
			// snapshot without registering another observer or changing ownership.
			c.reply(env.ID, rr)
			return
		} else {
			st.mu.Lock()
			busy := st.running || st.closed
			if !busy {
				delete(st.observers, c)
				c.unwatch(p.ID)
			}
			st.mu.Unlock()
			if busy {
				c.replyError(env.ID, codeBusy, "session is still running")
				return
			}
		}
	}
	if other := c.srv.ownerOf(p.ID); other != nil && other != c {
		c.replyError(env.ID, codeForbidden, "session is attached to another client")
		return
	}
	// A running, ownerless session may be observed read-only. This preserves
	// live progress and the bounded event log without allowing a second writer.
	// observe also registers the delivery-side watch, so the snapshot, the
	// observer set and the queue are committed together.
	if st := c.srv.liveSession(p.ID); st != nil {
		if rr, ok := st.observe(c); ok {
			c.reply(env.ID, rr)
			c.watchReadyAfterResume(st.id)
			return
		}
	}
	// A session whose client disconnected mid-turn is still live (the host keeps
	// the turn running). Adopt it when it is idle rather than rebuilding a second
	// agent over the same on-disk transcript.
	if st, ok := c.srv.adoptSession(p.ID, c); ok {
		if !c.srv.publishSession(c, st) {
			c.replyError(env.ID, codeUnavailable, "host is shutting down")
			return
		}
		st.mu.Lock()
		// publishSession can detach the adopted session again (its owner vanished
		// while it was being handed over) and closeSessionIf then clears agent. A
		// blind dereference here would panic the whole daemon, so re-check.
		if st.closed || st.agent == nil {
			st.mu.Unlock()
			c.replyError(env.ID, codeBusy, "session was closed during resume")
			return
		}
		msgs := st.agent.Messages()
		info := st.info()
		st.mu.Unlock()
		c.reply(env.ID, hostapi.SessionResumeResult{Session: info, Messages: msgs})
		return
	}
	// Still live but busy: creating a second writer for the same session would
	// interleave two transcripts. A running session that still has an owner can
	// only be observed by that owner, not by another connection.
	if c.srv.liveSession(p.ID) != nil {
		c.replyError(env.ID, codeBusy, "session is finishing a turn; retry when it completes")
		return
	}
	if c.srv.isShuttingDown() {
		c.replyError(env.ID, codeUnavailable, "host is shutting down")
		return
	}
	header, path, err := c.srv.store.LoadPath(p.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.replyError(env.ID, codeNotFound, "no such session: "+p.ID)
			return
		}
		c.replyError(env.ID, codeInternal, err.Error())
		return
	}
	if header.IsSubAgent() {
		c.replyError(env.ID, codeForbidden, session.ErrNotResumableSubAgent.Error())
		return
	}
	msgs := session.MessagesSinceLastCompaction(session.Messages(path))
	st := &sessionState{
		id:        header.ID,
		srv:       c.srv,
		owner:     c,
		cwd:       resolveCwd(header.Cwd, c.srv.processCwd()),
		perms:     newPermissionTable(),
		header:    header,
		curLeaf:   session.TipLeaf(path),
		persisted: len(msgs),
	}
	if !c.srv.reserveSession(c, st) {
		c.replyError(env.ID, codeBusy, "session is being resumed")
		return
	}
	ctx, stop := c.assemblyContext()
	defer stop()
	agent, err := c.srv.cfg.Factory.NewAgent(ctx, AgentSpec{
		SessionID:      header.ID,
		Cwd:            st.cwd,
		Model:          header.Model,
		SystemPrompt:   header.SystemPrompt,
		Messages:       msgs,
		BeforeToolCall: c.srv.permissionGate(st),
	})
	if err != nil {
		c.srv.abandonSession(c, st)
		c.replyError(env.ID, codeInternal, err.Error())
		return
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		agent.Close()
		c.replyError(env.ID, codeUnavailable, "session was closed during assembly")
		return
	}
	st.agent = agent
	// A resumed session keeps its original prompt; only a header without one (an
	// older file) adopts the freshly assembled prompt.
	if st.header.SystemPrompt == "" {
		st.header.SystemPrompt = agent.SystemPrompt()
	}
	if st.header.Model == "" {
		st.header.Model = agent.Model()
	}
	if st.header.Provider == "" {
		st.header.Provider = agent.Provider()
	}
	st.mu.Unlock()

	if !c.srv.publishSession(c, st) {
		c.replyError(env.ID, codeUnavailable, "host is shutting down")
		return
	}
	c.reply(env.ID, hostapi.SessionResumeResult{Session: st.infoLocked(), Messages: msgs})
}

func (c *conn) handleSessionClose(env envelope) {
	var p hostapi.SessionCloseParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	st, ok := c.owns(p.ID)
	if !ok {
		c.replyNotOwned(env.ID, p.ID)
		return
	}
	c.srv.closeSession(st, "closed by client")
	c.reply(env.ID, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

func (c *conn) handleCancel(env envelope) {
	var p hostapi.CancelParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	st, ok := c.owns(p.SessionID)
	if !ok {
		c.replyNotOwned(env.ID, p.SessionID)
		return
	}
	st.mu.Lock()
	cancel := st.cancel
	st.mu.Unlock()
	// Cancelling an idle session is not an error: a UI may optimistically send it
	// when the user hits Esc just as the turn finishes.
	if cancel != nil {
		cancel()
		st.perms.abortAll()
	}
	c.reply(env.ID, struct {
		OK      bool `json:"ok"`
		Running bool `json:"running"`
	}{OK: true, Running: cancel != nil})
}

func (c *conn) handlePermissionDecide(env envelope) {
	var p hostapi.PermissionDecideParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	st, ok := c.owns(p.SessionID)
	if !ok {
		c.replyNotOwned(env.ID, p.SessionID)
		return
	}
	if !st.perms.resolve(p.RequestID, permissionAnswer{approve: p.Approve, always: p.Always}) {
		c.replyError(env.ID, codeNotFound, "no pending permission request "+p.RequestID)
		return
	}
	if p.Approve && p.Always {
		// "always" is scoped to this session. Granting it on the host-wide trust
		// manager would silently approve every other client's session in the same
		// directory, which is precisely what connection isolation forbids.
		st.mu.Lock()
		st.alwaysApproved = true
		st.mu.Unlock()
	}
	c.reply(env.ID, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

// replyNotOwned distinguishes "you never had this session" from "it is gone":
// both are forbidden from this connection's point of view.
func (c *conn) replyNotOwned(id, sessionID string) {
	if sessionID == "" {
		c.replyError(id, codeInvalidParams, "session id is required")
		return
	}
	c.replyError(id, codeForbidden, "session is not owned by this connection")
}

func (c *conn) handlePrompt(env envelope) {
	var p hostapi.PromptParams
	if err := env.DecodeParams(&p); err != nil {
		c.replyError(env.ID, codeInvalidParams, err.Error())
		return
	}
	st, ok := c.owns(p.SessionID)
	if !ok {
		c.replyNotOwned(env.ID, p.SessionID)
		return
	}
	// The turn id is allocated before st.mu is taken: nextTurnID locks s.mu, and
	// the shutdown path holds s.mu while taking st.mu. Allocating inside the st.mu
	// critical section would invert that order and deadlock a shutdown.
	turnID := c.srv.nextTurnID()
	st.mu.Lock()
	if st.agent == nil || st.closed {
		st.mu.Unlock()
		c.replyError(env.ID, codeNotFound, "session is closed")
		return
	}
	if st.running {
		st.mu.Unlock()
		c.replyError(env.ID, codeBusy, "a turn is already running for this session")
		return
	}
	agent := st.agent
	ctx, cancel := context.WithCancel(c.srv.runCtx)
	st.running = true
	st.cancel = cancel
	// startTurn replaces the replay log and the observer set. Watchers of an
	// earlier turn are told their subscription ended, or they would never hear
	// from this session again and could not resume it.
	droppedObservers := st.takeObservers()
	st.startTurn(turnID, agent.Messages())
	st.mu.Unlock()
	st.releaseObservers(droppedObservers, "new turn started")

	// finishTurn marks the session idle. It must run before the prompt result is
	// sent: a client that reacts to a returned prompt (the next turn, a compact,
	// a close) is entitled to find the session idle, and sending the reply first
	// left a window in which the host answered "a turn is already running for
	// this session" to a client that had just been told the turn was over.
	finished := false
	finishTurn := func() {
		if finished {
			return
		}
		finished = true
		st.mu.Lock()
		st.running = false
		st.cancel = nil
		st.mu.Unlock()
		st.perms.abortAll()
		cancel()
	}
	defer func() {
		finishTurn()
		// The client may have vanished while this turn ran. The turn itself is
		// allowed to finish (the host owns it), but the now-idle orphan is closed
		// unless a reconnecting client adopted it in the meantime.
		c.srv.closeSessionIfUnattached(st, "client disconnected")
	}()

	mapper := &hostapi.EventMapper{SessionID: st.id}
	handler := runtime.StreamHandler{
		OnText: func(delta string) {
			if ev := mapper.TextDelta(delta); ev != nil {
				st.emit(*ev)
			}
		},
		OnEvent: func(ev agentcore.AgentEvent) {
			for _, out := range mapper.Handle(ev) {
				st.emit(out)
			}
		},
	}
	msg, runErr := agent.Run(ctx, p.Text, handler)

	// Persist whatever the run produced, even on failure, so a partial turn is
	// resumable — the same rule the headless driver follows.
	st.mu.Lock()
	msgs := agent.Messages()
	perr := st.appendTail(msgs)
	st.mu.Unlock()
	if perr != nil {
		c.srv.discardClosedPrompt(st, agent)
		finishTurn()
		c.replyError(env.ID, codeInternal, "persist session: "+perr.Error())
		return
	}
	c.srv.finishPromptSession(st, agent)

	res := hostapi.PromptResult{SessionID: st.id}
	if msg != nil {
		res.Text = agentcore.ContentToText(msg.Content)
		res.StopReason = msg.StopReason
		if msg.StopReason == agentcore.StopReasonError {
			// A provider failure is an in-band outcome: the turn is over and
			// persisted, but the UI must be able to say *why*. The error field is
			// what attach renders; the stop reason alone loses the message.
			res.Error = msg.ErrorMessage
			if res.Error == "" {
				res.Error = "turn failed"
			}
		}
	} else if ctx.Err() != nil {
		// Cancelled before the turn produced anything (an immediate cancel): there
		// is no message to carry the abort, but it is still an abort, not success.
		res.StopReason = agentcore.StopReasonAborted
	}
	// Idle before replying: see finishTurn.
	finishTurn()
	switch {
	case runErr == nil:
		c.reply(env.ID, res)
	case errors.Is(runErr, context.Canceled):
		// A cancelled turn is an expected outcome (the user pressed cancel), not a
		// transport failure: report it in-band so the UI can render the partial text.
		res.StopReason = agentcore.StopReasonAborted
		c.reply(env.ID, res)
	default:
		c.replyError(env.ID, codeInternal, runErr.Error())
	}
}

// discardClosedPrompt rolls back a session tombstoned while its turn was
// running when persistence fails. It releases the agent and the reservation so
// the id cannot remain closed and un-resumable forever.
func (s *Server) discardClosedPrompt(st *sessionState, agent Agent) {
	st.mu.Lock()
	closed := st.closed
	closeAgent := st.closeOnFinish
	st.closeOnFinish = false
	st.mu.Unlock()
	if !closed {
		return
	}
	if closeAgent && agent != nil {
		agent.Close()
	}
	s.mu.Lock()
	if s.sessions[st.id] == st {
		delete(s.sessions, st.id)
	}
	s.mu.Unlock()
}

// finishPromptSession releases a session tombstoned while its turn was running.
// It runs after persistence, so a close cannot race a late transcript append or
// let a second writer resume the session before the first handler has finished.
func (s *Server) finishPromptSession(st *sessionState, agent Agent) {
	st.mu.Lock()
	closeAgent := st.closeOnFinish
	st.closeOnFinish = false
	st.mu.Unlock()
	if !closeAgent {
		return
	}
	if agent != nil {
		agent.Close()
	}
	s.mu.Lock()
	if s.sessions[st.id] == st {
		delete(s.sessions, st.id)
	}
	s.mu.Unlock()
}

// ownerOf reports which connection holds a live session, or nil.
func (s *Server) ownerOf(id string) *conn {
	s.mu.Lock()
	st, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.owner
}

// processCwd is the host's own working directory, used as the fallback for a
// session whose header does not record one.
func (s *Server) processCwd() string {
	wd, _ := os.Getwd()
	return wd
}

// ---- permission gate --------------------------------------------------

// permissionGate builds the BeforeToolCall seam for one session. It mirrors
// trust.BeforeToolCall, but asks the owning connection instead of a terminal:
// trusted directories pass straight through, and an untrusted directory's
// side-effect call becomes a permission_request event that the client answers
// with permission.decide. No answer (or a cancelled turn) denies the call.
func (s *Server) permissionGate(st *sessionState) agentcore.BeforeToolCallFunc {
	return func(ctx context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		if !trust.SideEffectTools[call.Name] {
			return nil
		}
		st.mu.Lock()
		approved := st.alwaysApproved
		st.mu.Unlock()
		if approved || s.trust == nil || s.trust.IsTrusted(st.cwd) {
			return nil
		}
		// A cancelled turn must not wait for an answer that may never come.
		if err := ctx.Err(); err != nil {
			return denyDecision(call.Name, st.cwd)
		}
		st.mu.Lock()
		owner := st.owner
		st.mu.Unlock()
		if owner == nil || owner.isClosed() {
			return denyDecision(call.Name, st.cwd)
		}
		id, ch := st.perms.open()
		req := hostapi.PermissionRequest{
			SessionID: st.id,
			RequestID: id,
			TurnID:    st.turnID,
			ToolName:  call.Name,
			Summary:   trust.ToolCallSummary(call),
		}
		if s.cfg.PermissionTimeout > 0 {
			req.TimeoutMS = int(s.cfg.PermissionTimeout / time.Millisecond)
		}
		if err := owner.notify(eventPermission, req); err != nil {
			st.perms.resolve(id, permissionAnswer{})
			return denyDecision(call.Name, st.cwd)
		}
		var timeout <-chan time.Time
		if s.cfg.PermissionTimeout > 0 {
			t := time.NewTimer(s.cfg.PermissionTimeout)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case ans := <-ch:
			if ans.approve {
				if ans.always {
					st.mu.Lock()
					st.alwaysApproved = true
					st.mu.Unlock()
				}
				return nil
			}
			if msg := denyMessage(call.Name, st.cwd); msg != "" {
				content := agentcore.ContentList{agentcore.NewTextContent(msg)}
				return &agentcore.BeforeToolCallDecision{Block: true, Content: &content}
			}
			return &agentcore.BeforeToolCallDecision{Block: true}
		case <-timeout:
			st.perms.resolve(id, permissionAnswer{})
			return denyDecision(call.Name, st.cwd)
		case <-ctx.Done():
			// Drop the pending entry so a late answer is reported as unknown rather
			// than draining into a channel nobody reads.
			st.perms.resolve(id, permissionAnswer{})
			return denyDecision(call.Name, st.cwd)
		}
	}
}

// denyDecision is the blocking decision returned when a side-effect tool is
// denied. The text becomes the tool's result, so the model learns why it could
// not proceed instead of retrying blindly.
func denyDecision(name, cwd string) *agentcore.BeforeToolCallDecision {
	content := agentcore.ContentList{agentcore.NewTextContent(denyMessage(name, cwd))}
	return &agentcore.BeforeToolCallDecision{Block: true, Content: &content}
}

// denyMessage explains a denial. It deliberately mirrors trust.BeforeToolCall's
// wording, so a user who switches between the REPL and a host client sees the
// same explanation.
func denyMessage(name, cwd string) string {
	return fmt.Sprintf("tool %q blocked: %s is not trusted (use /trust to trust this project)", name, cwd)
}

// marshalState encodes the state file; kept separate so tests can compare the
// shape without re-reading through ReadState.
func marshalState(st State) ([]byte, error) { return json.MarshalIndent(st, "", "  ") }
