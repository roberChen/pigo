// Package hostclient is the client side of the host protocol: connect to a
// running `pigo serve`, handshake with the token, and drive sessions.
//
// It is deliberately thin (hostapi + hostpath + stdlib). A UI links this package
// and gets framing, request correlation, event demultiplexing and the typed
// method wrappers; it never imports internal/hostserver, so a UI binary does not
// link the run assembly (providers, tools, MCP, memory).
//
// One goroutine reads the socket and dispatches: responses go to the waiter for
// their request id, events go to the subscription callbacks. That is what lets a
// UI keep sending (cancel, permission.decide) while a prompt is outstanding,
// which the protocol requires (docs/architecture.md §6.1).
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
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostpath"
)

// maxLineBytes matches the server's cap, so a client can read any result the
// server can produce (a resume result is the largest).
const maxLineBytes = 32 << 20

// Options configures Connect.
type Options struct {
	// Dir is the host state directory. Defaults to hostpath.DefaultDir().
	Dir string
	// Socket overrides the socket path; empty derives it from Dir.
	Socket string
	// Token overrides the token read from Dir. Mostly for tests.
	Token string
	// Client is the free-form label reported in hello ("attach", "tui", …).
	Client string
	// DialTimeout bounds the connect. Defaults to 5s.
	DialTimeout time.Duration
	// CallTimeout bounds a non-prompt request. Prompts are unbounded because a
	// turn can legitimately run for minutes; cancel is the way out.
	CallTimeout time.Duration
}

// Client is one connection to a host.
type Client struct {
	nc  net.Conn
	opt Options

	// writeMu serializes whole lines.
	writeMu sync.Mutex

	mu      sync.Mutex
	next    int
	nextSub uint64
	pending map[string]chan result
	subs    []subscriber
	// queue holds events waiting for the dispatcher. It is an explicit slice
	// rather than a channel so a burst of text deltas can never block the read
	// loop (which would stall the responses a callback may be waiting for).
	queue   []hostapi.Envelope
	closed  bool
	readErr error

	hello    hostapi.HelloResult
	closeCh  chan struct{}
	doneCh   chan struct{}
	queueSig chan struct{}
	dispDone chan struct{}
}

// result is one request's outcome.
type result struct {
	env *hostapi.Envelope
	err error
}

// subscriber is one event callback registration.
type subscriber struct {
	id uint64
	// event is the envelope-level event name to deliver (hostapi.EventSession…);
	// empty means every event.
	event string
	fn    func(hostapi.Envelope)
}

// Connect dials the host and completes the handshake.
func Connect(opts Options) (*Client, error) {
	if opts.Dir == "" {
		opts.Dir = hostpath.DefaultDir()
	}
	if opts.Dir == "" {
		return nil, errors.New("hostclient: cannot determine the host state directory (set PIGO_HOME)")
	}
	socket := opts.Socket
	if socket == "" {
		socket = hostpath.SocketPath(opts.Dir)
	}
	token := opts.Token
	if token == "" {
		t, err := hostpath.ReadToken(opts.Dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrNotRunning{Dir: opts.Dir, Socket: socket}
			}
			return nil, fmt.Errorf("hostclient: read token: %w", err)
		}
		token = t
	}
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	nc, err := hostpath.Dial(socket, timeout)
	if err != nil {
		// A pipe created by another local user is a security signal, not the
		// ordinary "nobody is listening" case: report why the client refused.
		if errors.Is(err, hostpath.ErrEndpointOwner) {
			return nil, fmt.Errorf("hostclient: refusing endpoint %s: %w", socket, err)
		}
		return nil, ErrNotRunning{Dir: opts.Dir, Socket: socket}
	}
	c := &Client{
		nc:       nc,
		opt:      opts,
		pending:  make(map[string]chan result),
		closeCh:  make(chan struct{}),
		doneCh:   make(chan struct{}),
		queueSig: make(chan struct{}, 1),
		dispDone: make(chan struct{}),
	}
	go c.read()
	go c.dispatchLoop()
	hello, err := c.Hello(token)
	if err != nil {
		nc.Close()
		return nil, err
	}
	c.hello = hello
	return c, nil
}

// ErrNotRunning reports that no host answered on the socket.
type ErrNotRunning struct {
	Dir    string
	Socket string
}

func (e ErrNotRunning) Error() string {
	return fmt.Sprintf("no pigo host is running (socket %s)", e.Socket)
}

// read is the connection's only reader: it routes responses to their waiter and
// events to subscribers.
func (c *Client) read() {
	defer close(c.doneCh)
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		var env hostapi.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			c.fail(fmt.Errorf("hostclient: decode line: %w", err))
			return
		}
		if env.IsEvent() {
			c.enqueue(env)
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[env.ID]
		delete(c.pending, env.ID)
		c.mu.Unlock()
		if ok {
			ch <- result{env: &env}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(err)
}

// fail marks the connection dead and wakes every waiter.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.readErr = err
	pending := c.pending
	c.pending = make(map[string]chan result)
	c.mu.Unlock()
	close(c.closeCh)
	for _, ch := range pending {
		ch <- result{err: err}
	}
}

// enqueue adds an event to the dispatcher queue and wakes the dispatcher.
func (c *Client) enqueue(env hostapi.Envelope) {
	c.mu.Lock()
	c.queue = append(c.queue, env)
	c.mu.Unlock()
	select {
	case c.queueSig <- struct{}{}:
	default:
	}
}

// dispatchLoop delivers queued events on its own goroutine, so a callback may
// call back into the client (a permission handler answering its own prompt is
// the expected case) without deadlocking the socket reader.
func (c *Client) dispatchLoop() {
	defer close(c.dispDone)
	for {
		select {
		case <-c.closeCh:
			// Deliver what already arrived (a final server_shutdown event, for
			// example) before stopping.
			c.drainEvents()
			return
		case <-c.queueSig:
			c.drainEvents()
		}
	}
}

// drainEvents delivers every queued event in arrival order.
func (c *Client) drainEvents() {
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			return
		}
		env := c.queue[0]
		c.queue = c.queue[1:]
		subs := make([]subscriber, len(c.subs))
		copy(subs, c.subs)
		c.mu.Unlock()

		for _, s := range subs {
			if s.event != "" && s.event != env.Event {
				continue
			}
			s.fn(env)
		}
	}
}

// OnEvent registers a callback for server events. An empty name subscribes to
// all events. The returned function unsubscribes; it is safe to call more than
// once, and safe to call from inside the callback itself.
func (c *Client) OnEvent(event string, fn func(hostapi.Envelope)) (cancel func()) {
	c.mu.Lock()
	c.nextSub++
	id := c.nextSub
	c.subs = append(c.subs, subscriber{id: id, event: event, fn: fn})
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i := range c.subs {
			if c.subs[i].id == id {
				c.subs = append(c.subs[:i], c.subs[i+1:]...)
				return
			}
		}
	}
}

// Hello completes the handshake. It is bounded by CallTimeout (default 30s):
// a host that accepts the socket but never answers must not hang a UI startup.
func (c *Client) Hello(token string) (hostapi.HelloResult, error) {
	timeout := c.opt.CallTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out hostapi.HelloResult
	err := c.callAs(ctx, hostapi.MethodHello, hostapi.HelloParams{
		Version: hostapi.Version,
		Token:   token,
		Client:  c.opt.Client,
	}, &out)
	return out, err
}

// ServerInfo is the negotiated identity of the host.
func (c *Client) ServerInfo() hostapi.HelloResult { return c.hello }

// Socket is the socket the client is connected to.
func (c *Client) Socket() string {
	if c.opt.Socket != "" {
		return c.opt.Socket
	}
	return hostpath.SocketPath(c.opt.Dir)
}

// Close tears the connection down.
func (c *Client) Close() error {
	c.fail(errors.New("hostclient: closed by client"))
	// Close the socket *before* waiting for the reader: it is parked in Read, so
	// waiting first would burn the whole grace period on a healthy peer (Close
	// took 2s every time). Closing the socket is what makes Read return.
	err := c.nc.Close()
	select {
	case <-c.doneCh:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-c.dispDone:
	case <-time.After(2 * time.Second):
	}
	return err
}

// Done is closed when the connection is gone (peer disconnect, host shutdown or
// Close). ReadErr reports why.
func (c *Client) Done() <-chan struct{} { return c.closeCh }

// ReadErr is the reason the connection ended, or nil while it is live.
func (c *Client) ReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// call sends a request and waits for its response envelope.
func (c *Client) call(ctx context.Context, method string, params any) (*hostapi.Envelope, error) {
	id, ch, err := c.send(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.env, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

// send registers a waiter and writes the request line. The returned id is the
// waiter's key, the channel receives exactly one outcome.
func (c *Client) send(method string, params any) (string, chan result, error) {
	ch := make(chan result, 1)
	c.mu.Lock()
	if c.closed {
		err := c.readErr
		c.mu.Unlock()
		return "", nil, err
	}
	c.next++
	id := "c-" + itoa(c.next)
	c.pending[id] = ch
	c.mu.Unlock()

	env := map[string]any{"id": id, "method": method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			c.forget(id)
			return "", nil, err
		}
		env["params"] = json.RawMessage(raw)
	}
	line, err := json.Marshal(env)
	if err != nil {
		c.forget(id)
		return "", nil, err
	}
	if err := c.writeLine(line); err != nil {
		c.forget(id)
		return "", nil, err
	}
	return id, ch, nil
}

// forget drops a waiter that will never be answered (write failure or timeout).
func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// callAs sends a request and decodes a successful result into out, converting a
// protocol error into *hostapi.Error.
func (c *Client) callAs(ctx context.Context, method string, params, out any) error {
	env, err := c.call(ctx, method, params)
	if err != nil {
		return err
	}
	if env.Error != nil {
		return env.Error
	}
	if out == nil {
		return nil
	}
	return env.DecodeResult(out)
}

// writeLine writes one JSON line under the write lock.
func (c *Client) writeLine(line []byte) error {
	line = append(line, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.nc.Write(line); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

// request is the shared entry point for the typed wrappers.
func (c *Client) request(method string, params, out any) error {
	ctx := context.Background()
	var cancel context.CancelFunc
	if c.opt.CallTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.opt.CallTimeout)
		defer cancel()
	}
	return c.callAs(ctx, method, params, out)
}

// ServerStatus asks the host for its identity and live session count.
func (c *Client) ServerStatus() (hostapi.ServerStatusResult, error) {
	var out hostapi.ServerStatusResult
	err := c.request(hostapi.MethodServerStatus, nil, &out)
	return out, err
}

// Shutdown asks the host to exit. The connection will close right after.
func (c *Client) Shutdown() error {
	return c.request(hostapi.MethodServerShutdown, nil, nil)
}

// CreateSession starts a conversation rooted at cwd (empty means the host's own
// working directory).
func (c *Client) CreateSession(cwd, model, title string) (hostapi.SessionInfo, error) {
	var out hostapi.SessionCreateResult
	err := c.request(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd, Model: model, Title: title}, &out)
	return out.Session, err
}

// ListSessions lists the sessions owned by this connection.
func (c *Client) ListSessions() ([]hostapi.SessionInfo, error) {
	var out hostapi.SessionListResult
	err := c.request(hostapi.MethodSessionList, nil, &out)
	return out.Sessions, err
}

// Resume attaches to a persisted session and returns its replayed conversation.
func (c *Client) Resume(id string) (hostapi.SessionResumeResult, error) {
	var out hostapi.SessionResumeResult
	err := c.request(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: id}, &out)
	return out, err
}

// CloseSession ends a session on the host.
func (c *Client) CloseSession(id string) error {
	return c.request(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: id}, nil)
}

// Prompt runs one turn. Streaming output arrives through OnEvent while the call
// is outstanding. Cancelling ctx asks the host to abort the turn and then waits
// (bounded) for its reply, so the returned result still carries the aborted
// turn's partial text alongside ctx's error; the error is returned unchanged.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (hostapi.PromptResult, error) {
	var out hostapi.PromptResult
	id, ch, err := c.send(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sessionID, Text: text})
	if err != nil {
		return out, err
	}
	select {
	case r := <-ch:
		return decodePromptResult(out, r)
	case <-ctx.Done():
		// The turn is still running on the host: cancel it rather than leaving a
		// turn nobody is watching, then keep waiting for the aborted turn's reply.
		if cerr := c.Cancel(sessionID); cerr != nil {
			c.forget(id)
			return out, cerr
		}
		select {
		case r := <-ch:
			out, _ = decodePromptResult(out, r)
			return out, ctx.Err()
		case <-time.After(promptCancelSettle):
			c.forget(id)
			return out, ctx.Err()
		}
	}
}

// promptCancelSettle bounds the wait for a cancelled turn's reply. The host
// answers as soon as the run unwinds, which is immediate for a cancel, so a
// wait this long means the host is wedged and the connection is better left
// alone than the caller hung.
const promptCancelSettle = 10 * time.Second

// decodePromptResult turns a prompt waiter's outcome into a typed result.
func decodePromptResult(out hostapi.PromptResult, r result) (hostapi.PromptResult, error) {
	if r.err != nil {
		return out, r.err
	}
	if r.env.Error != nil {
		return out, r.env.Error
	}
	if err := r.env.DecodeResult(&out); err != nil {
		return out, err
	}
	return out, nil
}

// Cancel aborts the session's in-flight turn. running reports whether a turn was
// actually in flight (cancelling an idle session is not an error).
func (c *Client) Cancel(sessionID string) error {
	var out struct {
		OK      bool `json:"ok"`
		Running bool `json:"running"`
	}
	return c.request(hostapi.MethodCancel, hostapi.CancelParams{SessionID: sessionID}, &out)
}

// Decide answers a permission request. always grants session-scoped trust for
// the session's directory.
func (c *Client) Decide(sessionID, requestID string, approve, always bool) error {
	return c.request(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{
		SessionID: sessionID,
		RequestID: requestID,
		Approve:   approve,
		Always:    always,
	}, nil)
}

// itoa avoids strconv for one call site (kept local to stay dependency-light).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
