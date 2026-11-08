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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/diag"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// maxLineBytes bounds one NDJSON line. A prompt can be large (a pasted file), and
// a resume result can be much larger, but both are far below this; the cap only
// exists so a hostile client cannot make the server allocate without bound.
const maxLineBytes = 32 << 20

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
	wg sync.WaitGroup

	shutdownOnce sync.Once
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
	// No live host: anything at the path is stale.
	_ = os.Remove(s.socket)
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("host: listen %s: %w", s.socket, err)
	}
	// Socket permissions are the primary defense; the token is the explicit
	// second factor for a same-user process that found the path.
	if err := os.Chmod(s.socket, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("host: chmod socket: %w", err)
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
func (s *Server) Serve(ctx context.Context) error {
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
		c := newConn(s, nc)
		s.mu.Lock()
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
		if s.ln != nil {
			_ = s.ln.Close()
		}
		// Tell every client before closing, so an attached UI can print a reason
		// instead of a bare EOF.
		for _, c := range s.snapshotConns() {
			_ = c.notify(eventServerShutdown, hostapi.ServerShutdownEvent{Reason: reason})
		}
		for _, c := range s.snapshotConns() {
			c.close()
		}
		cleanup(s.dir)
		close(s.stopped)
	})
}

// Wait blocks until shutdown has finished, including the removal of the host's
// files. It does not wait for in-flight handlers forever: a handler blocked on a
// provider cannot be interrupted by anything but its turn context, and Shutdown
// cancels those via conn.close.
func (s *Server) Wait() { <-s.stopped }

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

// closeSessionIfUnattached closes a session only if no client has adopted it and
// no turn is in flight. It is the disconnect path: dropping the last client must
// not kill a turn that is still running, but an idle orphan has no reason to stay
// resident — its history is already on disk.
func (s *Server) closeSessionIfUnattached(st *sessionState, reason string) {
	s.closeSessionIf(st, reason, func(st *sessionState) bool {
		return st.owner == nil && !st.running
	})
}

// closeSessionIf ends a session when keep reports true (called with st.mu held).
// The map entry, the closed flag and the ownership check are decided under one
// lock so a reconnecting client cannot adopt a session that is being closed.
func (s *Server) closeSessionIf(st *sessionState, reason string, keep func(*sessionState) bool) {
	s.mu.Lock()
	if _, ok := s.sessions[st.id]; !ok {
		s.mu.Unlock()
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
	st.mu.Unlock()
	delete(s.sessions, st.id)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	st.perms.abortAll()
	if agent != nil {
		agent.Close()
	}
	if owner != nil {
		_ = owner.notify(eventSessionClosed, hostapi.SessionClosedEvent{SessionID: st.id, Reason: reason})
		owner.release(st.id)
	}
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
func (s *Server) adoptSession(id string, c *conn) (*sessionState, bool) {
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed || st.running || st.owner != nil || st.agent == nil {
		return nil, false
	}
	st.owner = c
	return st, true
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

// publishSession records a session that was just built (created or resumed) and
// reaps it when its owner disappeared while the agent was being assembled.
//
// Building an agent can take a while (provider probing, a memory DB, MCP clients),
// and conn.close snapshots the owned set — so a client that vanishes inside
// Factory.NewAgent leaves nothing for that snapshot to detach. Without this check
// the session would stay in the live map with a dead owner for the host's
// lifetime, pinning the agent's resources and making the id un-resumable (it still
// looks attached to another client).
func (s *Server) publishSession(c *conn, st *sessionState) {
	c.claim(st)
	s.mu.Lock()
	s.sessions[st.id] = st
	s.mu.Unlock()

	st.mu.Lock()
	owner := st.owner
	st.mu.Unlock()
	if owner == nil || owner.isClosed() {
		s.detachSession(st)
	}
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
	if p.Version > Version {
		c.replyError(env.ID, codeInvalidParams, fmt.Sprintf("protocol version %d is newer than this host (%d)", p.Version, Version))
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
	already := c.authed
	c.authed = true
	c.client = p.Client
	c.mu.Unlock()
	if already {
		c.replyError(env.ID, codeUnauthorized, "hello has already been sent on this connection")
		return
	}
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
	agent, err := c.srv.cfg.Factory.NewAgent(context.Background(), AgentSpec{
		SessionID:      id,
		Cwd:            cwd,
		Model:          p.Model,
		BeforeToolCall: c.srv.permissionGate(st),
	})
	if err != nil {
		c.replyError(env.ID, codeInternal, err.Error())
		return
	}
	st.mu.Lock()
	st.agent = agent
	st.header = newSessionHeader(id, now, cwd, agent.Model(), agent.Provider(), agent.SystemPrompt())
	st.mu.Unlock()

	c.srv.publishSession(c, st)
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
	if other := c.srv.ownerOf(p.ID); other != nil && other != c {
		c.replyError(env.ID, codeForbidden, "session is attached to another client")
		return
	}
	// A session whose client disconnected mid-turn is still live (the host keeps
	// the turn running). Adopt it when it is idle rather than rebuilding a second
	// agent over the same on-disk transcript.
	if st, ok := c.srv.adoptSession(p.ID, c); ok {
		c.srv.publishSession(c, st)
		st.mu.Lock()
		msgs := st.agent.Messages()
		info := st.info()
		st.mu.Unlock()
		c.reply(env.ID, hostapi.SessionResumeResult{Session: info, Messages: msgs})
		return
	}
	// Still live but busy: creating a second writer for the same session would
	// interleave two transcripts. The client retries when the turn completes.
	if c.srv.liveSession(p.ID) != nil {
		c.replyError(env.ID, codeBusy, "session is finishing a turn; retry when it completes")
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
	agent, err := c.srv.cfg.Factory.NewAgent(context.Background(), AgentSpec{
		SessionID:      header.ID,
		Cwd:            st.cwd,
		Model:          header.Model,
		SystemPrompt:   header.SystemPrompt,
		Messages:       msgs,
		BeforeToolCall: c.srv.permissionGate(st),
	})
	if err != nil {
		c.replyError(env.ID, codeInternal, err.Error())
		return
	}
	st.mu.Lock()
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

	c.srv.publishSession(c, st)
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
	if p.Approve && p.Always && c.srv.trust != nil {
		// "always" grants session trust for the session's directory, so later
		// side-effect calls skip the prompt — the same contract as the REPL's
		// "a" answer.
		c.srv.trust.SetSessionTrust(st.cwd)
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
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	st.mu.Unlock()

	defer func() {
		st.mu.Lock()
		st.running = false
		st.cancel = nil
		st.mu.Unlock()
		st.perms.abortAll()
		cancel()
		// The client may have vanished while this turn ran. The turn itself is
		// allowed to finish (the host owns it), but the now-idle orphan is closed
		// unless a reconnecting client adopted it in the meantime.
		c.srv.closeSessionIfUnattached(st, "client disconnected")
	}()

	mapper := &hostapi.EventMapper{SessionID: st.id}
	handler := runtime.StreamHandler{
		OnText: func(delta string) {
			if ev := mapper.TextDelta(delta); ev != nil {
				_ = c.notify(eventSession, ev)
			}
		},
		OnEvent: func(ev agentcore.AgentEvent) {
			for _, out := range mapper.Handle(ev) {
				_ = c.notify(eventSession, out)
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
		diag.Diagf("host: persist session %s: %v", st.id, perr)
	}

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
		if s.trust == nil || s.trust.IsTrusted(st.cwd) {
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
				if ans.always && s.trust != nil {
					s.trust.SetSessionTrust(st.cwd)
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
