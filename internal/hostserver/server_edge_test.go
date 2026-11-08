// This file tests the host daemon where it is most likely to break: an
// adversarial client and a client that disappears at the worst possible moment.
// It covers NDJSON framing damage, the hello handshake's failure modes, the
// session state machine's races (create/close/resume/prompt against a moving
// target), the event contract (ordering, mapping, isolation), the permission
// gate's answer paths, and shutdown/serve teardown.
//
// It complements server_test.go (happy paths over a real socket) and
// e2e_test.go (the real run assembly) by attacking the same seams with the
// scripted Factory. Every assertion below is derived from what the code
// promises, not from what it currently happens to do: where the code deliberately
// closes a connection (a bad token, an over-long line) the test asserts the
// close, and where it deliberately keeps it (an unknown method, a parse error) it
// asserts the next request still works.
package hostserver

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
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// ---- scripted factory -------------------------------------------------

// scriptFactory drives a full event programme through the Agent seam. The
// fakeAgent in server_test.go emits two text deltas and a turn_end, which is all
// the happy path needs; the ordering, mapping, cancellation and teardown tests
// here also need agent_start/agent_end, the tool lifecycle, compaction and
// sub-agent events, plus a way to hold a turn open and to observe that a turn's
// context was cancelled.
type scriptFactory struct {
	// echo replies "echo:<prompt>" per turn instead of reply, which makes the
	// expected answer of a turn computable from its prompt (concurrency tests).
	echo  bool
	reply string
	// events are emitted between the text deltas and turn_end, in order.
	events []agentcore.AgentEvent
	// hold, when non-nil, blocks every Run until it is closed or the turn's ctx is
	// cancelled. The test owns when a turn finishes, so no test needs a sleep.
	hold chan struct{}
	// createGate, when non-nil, blocks NewAgent until it is closed, so a test can
	// kill a connection while the host is still assembling its agent.
	createGate chan struct{}
	// createEntered, when non-nil, receives one value when NewAgent is entered, so
	// a test knows the handler is inside the factory before killing the client.
	createEntered chan struct{}
	// compactAfter, when > 0, makes the agent on its Nth turn replace its context
	// with the last two messages, which is what the loop's compaction does to
	// agentCtx.Messages (a shorter list, not a rewritten prefix).
	compactAfter int

	mu          sync.Mutex
	specs       []AgentSpec
	runs        int
	cancelled   int
	agentCloses int
	cancelSig   chan struct{}
}

func newScriptFactory() *scriptFactory {
	return &scriptFactory{cancelSig: make(chan struct{})}
}

func (f *scriptFactory) NewAgent(ctx context.Context, spec AgentSpec) (Agent, error) {
	if f.createEntered != nil {
		select {
		case f.createEntered <- struct{}{}:
		default:
		}
	}
	if f.createGate != nil {
		select {
		case <-f.createGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Record what the host asked for, then fill the blanks the way a real factory
	// resolves defaults, so a test can assert both sides.
	f.specs = append(f.specs, spec)
	if spec.Model == "" {
		spec.Model = "script-model"
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = "scripted system prompt"
	}
	return &scriptAgent{f: f, spec: spec, messages: append(agentcore.MessageList(nil), spec.Messages...)}, nil
}

func (f *scriptFactory) specCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

func (f *scriptFactory) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func (f *scriptFactory) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agentCloses
}

// observeCancel records that a turn returned because its context was cancelled
// and closes cancelSig once, so a test can wait for the cancellation instead of
// polling a counter.
func (f *scriptFactory) observeCancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled++
	select {
	case <-f.cancelSig:
	default:
		close(f.cancelSig)
	}
}

func (f *scriptFactory) observeClose() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentCloses++
}

// scriptAgent is one session's scripted agent.
type scriptAgent struct {
	f    *scriptFactory
	spec AgentSpec

	mu       sync.Mutex
	messages agentcore.MessageList
	turns    int
}

func (a *scriptAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	f := a.f
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()

	a.mu.Lock()
	if prompt != "" {
		a.messages = append(a.messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	reply := f.reply
	if f.echo {
		reply = "echo:" + prompt
	}
	a.mu.Unlock()

	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			f.observeCancel()
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		f.observeCancel()
		return nil, err
	}

	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.AgentStartEvent{SessionID: a.spec.SessionID})
	}
	if h.OnText != nil {
		if half := len(reply) / 2; half > 0 {
			h.OnText(reply[:half])
			h.OnText(reply[half:])
		}
	}
	if h.OnEvent != nil {
		for _, ev := range f.events {
			h.OnEvent(ev)
		}
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
		h.OnEvent(agentcore.AgentEndEvent{Messages: []agentcore.AgentMessage{msg}})
	}
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.turns++
	if f.compactAfter > 0 && a.turns == f.compactAfter && len(a.messages) > 2 {
		a.messages = append(agentcore.MessageList(nil), a.messages[len(a.messages)-2:]...)
	}
	a.mu.Unlock()
	return &msg, nil
}

func (a *scriptAgent) Messages() agentcore.MessageList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(agentcore.MessageList(nil), a.messages...)
}

func (a *scriptAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *scriptAgent) Model() string        { return a.spec.Model }
func (a *scriptAgent) Provider() string     { return "script" }

func (a *scriptAgent) Close() { a.f.observeClose() }

// ---- helpers: raw line client -----------------------------------------

// lineConn is a synchronous raw client. The demultiplexing testClient cannot
// observe a response the server sends with an empty id (a parse or framing error
// has no request to correlate), and it delivers only the first response for a
// repeated id, so the protocol-robustness tests read the socket line by line.
type lineConn struct {
	t  *testing.T
	nc net.Conn
	r  *bufio.Reader
}

// newLineConn dials without handshaking.
func newLineConn(t *testing.T, fx *fixture) *lineConn {
	t.Helper()
	nc, err := net.Dial("unix", fx.srv.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return &lineConn{t: t, nc: nc, r: bufio.NewReader(nc)}
}

// handshake sends a valid hello and returns its response.
func (lc *lineConn) handshake(token string) *hostapi.Envelope {
	lc.t.Helper()
	lc.request("hello-edge", hostapi.MethodHello, hostapi.HelloParams{
		Version: hostapi.Version, Token: token, Client: "edge-test",
	})
	return lc.recv()
}

// request writes one request envelope.
func (lc *lineConn) request(id, method string, params any) {
	lc.t.Helper()
	env := map[string]any{"id": id, "method": method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			lc.t.Fatalf("marshal params for %s: %v", method, err)
		}
		env["params"] = json.RawMessage(raw)
	}
	lc.sendJSON(env)
}

// sendJSON writes v as one line.
func (lc *lineConn) sendJSON(v any) {
	lc.t.Helper()
	line, err := json.Marshal(v)
	if err != nil {
		lc.t.Fatalf("marshal line: %v", err)
	}
	lc.sendLine(string(line))
}

// sendLine writes s followed by a newline. A write error is not fatal on its own:
// the tests that provoke it (an over-long line) assert on the read side, and any
// other failure surfaces there as a missing response.
func (lc *lineConn) sendLine(s string) {
	_ = lc.nc.SetWriteDeadline(time.Now().Add(60 * time.Second))
	_, _ = io.WriteString(lc.nc, s)
	_, _ = io.WriteString(lc.nc, "\n")
}

// recvWithin reads exactly one envelope, honouring d as a read deadline.
func (lc *lineConn) recvWithin(d time.Duration) (*hostapi.Envelope, error) {
	if err := lc.nc.SetReadDeadline(time.Now().Add(d)); err != nil {
		return nil, err
	}
	line, err := lc.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var env hostapi.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		lc.t.Fatalf("server sent a non-JSON line %q: %v", line, err)
	}
	return &env, nil
}

// recv reads the next envelope, failing the test on EOF or timeout.
func (lc *lineConn) recv() *hostapi.Envelope {
	lc.t.Helper()
	env, err := lc.recvWithin(10 * time.Second)
	if err != nil {
		lc.t.Fatalf("read response: %v", err)
	}
	return env
}

// expectClosed asserts the server closed the connection instead of answering.
func (lc *lineConn) expectClosed(d time.Duration) {
	lc.t.Helper()
	env, err := lc.recvWithin(d)
	if err == nil {
		lc.t.Fatalf("expected the connection to be closed, got %+v", env)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		lc.t.Fatalf("connection still open %v after the provoking request", d)
	}
}

// ---- helpers: polling and event inspection -----------------------------

// waitFor polls cond until it holds, failing the test after d. Polling with a
// deadline (rather than sleeping) is what keeps these tests green under -race and
// -count=N, where a scheduler hiccup can stretch any single step.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// await waits for a response registered by send.
func (c *testClient) await(ch chan *hostapi.Envelope, method string) (*hostapi.Envelope, error) {
	select {
	case resp, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.readErr
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, &testError{"connection closed while waiting for " + method}
		}
		return resp, nil
	case <-time.After(10 * time.Second):
		return nil, &testError{"timeout waiting for " + method}
	}
}

// send registers a pending request id and writes the request without waiting.
// Registering before writing lets one goroutine put several requests in flight at
// once while the test client keeps a single writer on its socket (two goroutines
// calling call concurrently would interleave bytes inside a JSON line).
func (c *testClient) send(method string, params any) (string, chan *hostapi.Envelope) {
	c.t.Helper()
	c.mu.Lock()
	c.next++
	id := "req-" + itoa(c.next)
	ch := make(chan *hostapi.Envelope, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	env := map[string]any{"id": id, "method": method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			c.t.Fatalf("marshal params for %s: %v", method, err)
		}
		env["params"] = json.RawMessage(raw)
	}
	line, err := json.Marshal(env)
	if err != nil {
		c.t.Fatalf("marshal request %s: %v", method, err)
	}
	if _, err := c.nc.Write(append(line, '\n')); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
	return id, ch
}

// allEvents returns a copy of every buffered event envelope, in arrival order.
func (c *testClient) allEvents() []hostapi.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hostapi.Envelope(nil), c.events...)
}

// drainSessionEvents removes and returns every buffered session_event envelope in
// arrival order, which is the order the server wrote them.
func (c *testClient) drainSessionEvents() []hostapi.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []hostapi.Envelope
	kept := c.events[:0]
	for _, env := range c.events {
		if env.Event == hostapi.EventSession {
			out = append(out, env)
			continue
		}
		kept = append(kept, env)
	}
	c.events = kept
	return out
}

// waitHandlers blocks until every in-flight request handler has returned, so a
// test that ends with a turn still unwinding does not leave a handler writing the
// session store while t.TempDir is being removed. Shutdown deliberately does not
// wait for handlers (a provider call cannot be interrupted), so a test that needs
// the quiet has to ask for it; call this only once no new request can arrive.
func waitHandlers(t *testing.T, srv *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request handlers did not finish")
	}
}

// syncAndCheckNoEvents round-trips a server.status request on c and then asserts c
// received no event at all.
//
// The round trip is what makes the negative assertion sound: the socket is FIFO,
// so every byte the server wrote to c before answering has already been consumed
// by the reader goroutine when the response arrives; nothing has to be slept
// through.
func syncAndCheckNoEvents(t *testing.T, c *testClient, why string) {
	t.Helper()
	decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))
	if envs := c.allEvents(); len(envs) != 0 {
		t.Fatalf("%s: expected no events, got %+v", why, envs)
	}
}

// jsonKeys returns the sorted top-level field names of a raw JSON object.
func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode object %q: %v", raw, err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isRunning reports whether the live session sid is running a turn.
func isRunning(fx *fixture, sid string) bool {
	st := fx.srv.liveSession(sid)
	if st == nil {
		return false
	}
	return st.infoLocked().Running
}

// bareHost builds and binds a host over dir without starting Serve, so a test can
// drive the accept loop itself. It mirrors newFixture's hermetic setup (temp
// store, temp trust file) and tears the host down on cleanup.
func bareHost(t *testing.T, dir string, f Factory, mutate ...func(*Config)) (*Server, string) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	mgr, err := trust.NewManager(filepath.Join(t.TempDir(), "trust.json"))
	if err != nil {
		t.Fatalf("trust: %v", err)
	}
	cfg := Config{Dir: dir, Factory: f, Store: store, Trust: mgr, Build: "test"}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		srv.Shutdown("test cleanup")
		srv.Wait()
	})
	token, err := ReadToken(dir)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	return srv, token
}

// serveInBackground runs Serve until the test ends, for hosts built with bareHost.
func serveInBackground(t *testing.T, srv *Server) {
	t.Helper()
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
}

// helloLineOfSize builds a hello request line of exactly size bytes, padding the
// free-form client label.
func helloLineOfSize(t *testing.T, token string, size int) string {
	t.Helper()
	prefix := `{"id":"big","method":"hello","params":{"version":1,"token":"` + token + `","client":"`
	suffix := `"}}`
	pad := size - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("requested line size %d is smaller than the envelope overhead", size)
	}
	return prefix + strings.Repeat("x", pad) + suffix
}

// ---- malformed input ---------------------------------------------------

// TestParseErrorKeepsConnectionUsable pins the framing contract: a line that is
// not JSON is answered with CodeParse and no request id (there is no request to
// correlate), and the connection stays open so a client that appended garbage to
// a buffer can resynchronise on the next newline.
func TestParseErrorKeepsConnectionUsable(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}

	lc.sendLine("this is not json")
	resp := lc.recv()
	if resp.Error == nil || resp.Error.Code != hostapi.CodeParse {
		t.Fatalf("malformed line answered with %+v, want code %d", resp, hostapi.CodeParse)
	}
	if resp.ID != "" {
		t.Fatalf("parse error carries id %q, want none", resp.ID)
	}

	// A blank-but-not-empty line is garbage, not a keep-alive.
	lc.sendLine("   ")
	if resp := lc.recv(); resp.Error == nil || resp.Error.Code != hostapi.CodeParse {
		t.Fatalf("whitespace line answered with %+v, want code %d", resp, hostapi.CodeParse)
	}

	lc.request("after", hostapi.MethodServerStatus, nil)
	if resp := lc.recv(); resp.ID != "after" || resp.Error != nil {
		t.Fatalf("connection unusable after a parse error: %+v", resp)
	}
}

// TestEmptyLinesAreIgnored asserts an empty line is a no-op rather than an error:
// nothing is written for it, so the next line the client reads is the response to
// the following request.
func TestEmptyLinesAreIgnored(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}

	lc.sendLine("")
	lc.sendLine("")
	lc.request("target", hostapi.MethodServerStatus, nil)
	resp := lc.recv()
	if resp.ID != "target" {
		t.Fatalf("first response after empty lines is %+v, want the request's", resp)
	}
	if resp.Error != nil {
		t.Fatalf("status after empty lines failed: %v", resp.Error)
	}
}

// TestMaxLineBytesBoundary covers both sides of maxLineBytes. A line just under
// the cap is a normal request; a line that cannot fit is not answered at all —
// the read loop cannot resynchronise inside an unterminated line, so it ends the
// connection. The server itself must survive a client that does this.
func TestMaxLineBytesBoundary(t *testing.T) {
	fx := newFixture(t, newScriptFactory())

	// Just under the cap: accepted, and the reply is a normal hello result.
	near := newLineConn(t, fx)
	near.sendLine(helloLineOfSize(t, fx.token, maxLineBytes-1024))
	resp := near.recv()
	if resp.Error != nil {
		t.Fatalf("line just under maxLineBytes was rejected: %v", resp.Error)
	}
	var hello hostapi.HelloResult
	if err := resp.DecodeResult(&hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if hello.Version != hostapi.Version {
		t.Fatalf("hello version = %d, want %d", hello.Version, hostapi.Version)
	}

	// Over the cap: the scanner gives up, so the connection is closed.
	over := newLineConn(t, fx)
	over.sendLine(helloLineOfSize(t, fx.token, maxLineBytes+1024))
	over.expectClosed(30 * time.Second)

	// The host is unaffected: a fresh connection still works.
	c := fx.dial(t)
	decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))
}

// TestRequestWithoutIDOrMethodIsRejected covers the envelope-level validation in
// conn.handle: both a missing id and a missing method are a CodeInvalidRequest,
// never a panic or a silently dropped line.
func TestRequestWithoutIDOrMethodIsRejected(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}

	lc.sendLine(`{"method":"server.status"}`)
	resp := lc.recv()
	if resp.Error == nil || resp.Error.Code != hostapi.CodeInvalidRequest {
		t.Fatalf("id-less request answered with %+v, want code %d", resp, hostapi.CodeInvalidRequest)
	}
	if resp.ID != "" {
		t.Fatalf("id-less request echoed id %q", resp.ID)
	}

	lc.sendLine(`{"id":"no-method"}`)
	resp = lc.recv()
	if resp.Error == nil || resp.Error.Code != hostapi.CodeInvalidRequest {
		t.Fatalf("method-less request answered with %+v, want code %d", resp, hostapi.CodeInvalidRequest)
	}
	if resp.ID != "no-method" {
		t.Fatalf("method-less request echoed id %q, want no-method", resp.ID)
	}

	lc.request("fine", hostapi.MethodServerStatus, nil)
	if resp := lc.recv(); resp.ID != "fine" || resp.Error != nil {
		t.Fatalf("connection unusable after envelope errors: %+v", resp)
	}
}

// TestDuplicateRequestIDsAreAnsweredSeparately asserts the host does not
// correlate by id: two requests may share an id (a buggy client, or a client
// reusing a counter after a reconnect) and each gets its own answer.
func TestDuplicateRequestIDsAreAnsweredSeparately(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}

	lc.sendLine(`{"id":"dup","method":"server.status"}`)
	lc.sendLine(`{"id":"dup","method":"nope.nope"}`)

	var sawResult, sawMethodNotFound bool
	for i := 0; i < 2; i++ {
		resp := lc.recv()
		if resp.ID != "dup" {
			t.Fatalf("response %d carries id %q, want dup", i, resp.ID)
		}
		switch {
		case resp.Error == nil:
			sawResult = true
		case resp.Error.Code == hostapi.CodeMethodNotFound:
			sawMethodNotFound = true
		default:
			t.Fatalf("unexpected error for duplicate id: %v", resp.Error)
		}
	}
	if !sawResult || !sawMethodNotFound {
		t.Fatalf("duplicate ids were not both handled (result=%v methodNotFound=%v)", sawResult, sawMethodNotFound)
	}
}

// TestParamsTypeMismatchIsInvalidParams covers a params payload that is
// well-formed JSON but the wrong shape for the method. The decode happens before
// the token check in hello, and an unauthenticated connection must not be
// authenticated by a bad-params hello.
func TestParamsTypeMismatchIsInvalidParams(t *testing.T) {
	fx := newFixture(t, newScriptFactory())

	c := fx.dial(t)
	expectError(t, c.mustCall(hostapi.MethodSessionCreate, json.RawMessage(`{"cwd":42}`)), hostapi.CodeInvalidParams)
	expectError(t, c.mustCall(hostapi.MethodPrompt, json.RawMessage(`"not-an-object"`)), hostapi.CodeInvalidParams)
	// Still usable.
	decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))

	// hello is decoded before the token is checked, so a broken payload is a
	// params error rather than an auth error — and it does not authenticate.
	lc := newLineConn(t, fx)
	lc.sendLine(`{"id":"h","method":"hello","params":"nope"}`)
	resp := lc.recv()
	if resp.Error == nil || resp.Error.Code != hostapi.CodeInvalidParams {
		t.Fatalf("hello with a string payload answered with %+v, want code %d", resp, hostapi.CodeInvalidParams)
	}
	lc.request("s", hostapi.MethodServerStatus, nil)
	expectError(t, lc.recv(), hostapi.CodeUnauthorized)
}

// TestClientEventIsRejected asserts a client cannot inject a server event: the
// envelope is refused with CodeInvalidRequest and the connection survives.
func TestClientEventIsRejected(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}

	lc.sendLine(`{"event":"session_event","params":{"sessionId":"x","type":"text_delta","text":"injected"}}`)
	resp := lc.recv()
	if resp.Error == nil || resp.Error.Code != hostapi.CodeInvalidRequest {
		t.Fatalf("client event answered with %+v, want code %d", resp, hostapi.CodeInvalidRequest)
	}

	lc.request("after", hostapi.MethodServerStatus, nil)
	if resp := lc.recv(); resp.ID != "after" || resp.Error != nil {
		t.Fatalf("connection unusable after a refused event: %+v", resp)
	}
}

// ---- handshake ---------------------------------------------------------

// TestHelloTwiceIsRejected asserts a second hello is an auth error rather than a
// silent re-authentication, and that it does not tear the connection down: the
// first hello already authenticated it.
func TestHelloTwiceIsRejected(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	c := fx.dial(t)

	resp := c.mustCall(hostapi.MethodHello, hostapi.HelloParams{Version: hostapi.Version, Token: fx.token, Client: "again"})
	expectError(t, resp, hostapi.CodeUnauthorized)
	if !strings.Contains(resp.Error.Message, "already") {
		t.Fatalf("second hello message = %q, want it to say hello was already sent", resp.Error.Message)
	}
	// The connection is still authenticated and usable.
	decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))
}

// TestHelloProtocolVersionGate covers the rolling-upgrade contract: a client
// newer than the host is refused with CodeInvalidParams (not silently served), a
// client at or below the host's version is accepted, and a refused hello leaves
// the connection unauthenticated so it can retry with an older version.
func TestHelloProtocolVersionGate(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)

	lc.request("too-new", hostapi.MethodHello, hostapi.HelloParams{Version: hostapi.Version + 1, Token: fx.token})
	resp := lc.recv()
	expectError(t, resp, hostapi.CodeInvalidParams)
	if !strings.Contains(resp.Error.Message, "newer") {
		t.Fatalf("version rejection message = %q", resp.Error.Message)
	}

	// A rejected hello must not have authenticated the connection.
	lc.request("probe", hostapi.MethodServerStatus, nil)
	expectError(t, lc.recv(), hostapi.CodeUnauthorized)

	// Version 0 (the field omitted) is not newer than the host, so it is served —
	// the check is a forward-compatibility gate, not an equality check.
	lc.request("v0", hostapi.MethodHello, hostapi.HelloParams{Version: 0, Token: fx.token})
	if resp := lc.recv(); resp.Error != nil {
		t.Fatalf("hello with version 0 was refused: %v", resp.Error)
	}
	lc.request("works", hostapi.MethodServerStatus, nil)
	if resp := lc.recv(); resp.Error != nil {
		t.Fatalf("authenticated connection refused a request: %v", resp.Error)
	}
}

// TestHelloNullParamsClosesConnection covers "params": null: it decodes to the
// zero HelloParams (no token), which is the bad-token path — CodeUnauthorized
// followed by a deliberate close, so a probe cannot keep guessing.
func TestHelloNullParamsClosesConnection(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	lc := newLineConn(t, fx)

	lc.sendLine(`{"id":"h","method":"hello","params":null}`)
	resp := lc.recv()
	expectError(t, resp, hostapi.CodeUnauthorized)
	lc.expectClosed(5 * time.Second)
}

// TestRequestBeforeHelloIsRefusedThenRecoverable extends the existing
// unauthenticated test with the recovery path: the refusal must not have closed
// the connection, so a proper hello afterwards still works.
func TestRequestBeforeHelloIsRefusedThenRecoverable(t *testing.T) {
	fx := newFixture(t, newScriptFactory())

	// Even an unknown method is an auth error before hello: authentication is
	// checked before dispatch, so a probe cannot enumerate methods unauthenticated.
	lc := newLineConn(t, fx)
	lc.request("pre", "totally.unknown", nil)
	expectError(t, lc.recv(), hostapi.CodeUnauthorized)
	lc.request("pre2", hostapi.MethodSessionList, nil)
	expectError(t, lc.recv(), hostapi.CodeUnauthorized)

	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello after a refused request failed: %v", resp.Error)
	}
	lc.request("post", "totally.unknown", nil)
	expectError(t, lc.recv(), hostapi.CodeMethodNotFound)
}

// ---- session lifecycle -------------------------------------------------

// TestCreateResolvesCwd covers resolveCwd's contract: empty means the host's own
// working directory, a relative value is joined onto it, an absolute value is
// used verbatim, and a path that is not a directory is refused.
func TestCreateResolvesCwd(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	c := fx.dial(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for _, requested := range []string{"", "."} {
		res := decodeResult[hostapi.SessionCreateResult](t,
			c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: requested}))
		if res.Session.Cwd != wd {
			t.Fatalf("create with cwd %q produced %q, want the host cwd %q", requested, res.Session.Cwd, wd)
		}
	}

	abs := t.TempDir()
	res := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: abs}))
	if res.Session.Cwd != abs {
		t.Fatalf("create with absolute cwd produced %q, want %q", res.Session.Cwd, abs)
	}

	// A relative path that does not exist is not silently created.
	expectError(t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: "no-such-subdir-deadbeef"}),
		hostapi.CodeInvalidParams)
}

// TestSessionIDsAreWellFormedAndUnique pins the id shape (NewID's
// "%Y%m%d-%H%M%S-%06d") and its uniqueness, which the on-disk store and the
// resume path both depend on.
func TestSessionIDsAreWellFormedAndUnique(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	c := fx.dial(t)
	shape := regexp.MustCompile(`^\d{8}-\d{6}-\d{6}$`)

	cwd := t.TempDir()
	seen := make(map[string]bool, 8)
	for i := 0; i < 8; i++ {
		id := decodeResult[hostapi.SessionCreateResult](t,
			c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
		if !shape.MatchString(id) {
			t.Fatalf("session id %q does not match %s", id, shape)
		}
		if seen[id] {
			t.Fatalf("session id %q was handed out twice", id)
		}
		seen[id] = true
	}
}

// TestConcurrentCreatesGetDistinctSessions puts several session.create requests in
// flight on one connection at once: each handler runs on its own goroutine, so the
// ownership map, the live-session map and the id allocator are all touched
// concurrently.
//
// The clock is frozen for the test, which is the adversarial case for the
// allocator: session.NewID only carries microseconds, so a host that derived ids
// straight from the clock would hand every one of these creates the same id — and
// they would then share a single on-disk transcript. The transcripts are checked
// per session for exactly that reason.
func TestConcurrentCreatesGetDistinctSessions(t *testing.T) {
	const n = 8
	frozen := time.Now().UTC()
	f := newScriptFactory()
	f.echo = true
	fx := newFixture(t, f, func(cfg *Config) {
		cfg.Now = func() time.Time { return frozen }
	})
	c := fx.dial(t)
	cwd := t.TempDir()

	chans := make([]chan *hostapi.Envelope, 0, n)
	for i := 0; i < n; i++ {
		_, ch := c.send(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd, Title: fmt.Sprintf("t%d", i)})
		chans = append(chans, ch)
	}
	seen := make(map[string]bool, n)
	for i, ch := range chans {
		resp, err := c.await(ch, hostapi.MethodSessionCreate)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		res := decodeResult[hostapi.SessionCreateResult](t, resp)
		if res.Session.ID == "" {
			t.Fatalf("create %d returned an empty id", i)
		}
		if seen[res.Session.ID] {
			t.Fatalf("concurrent creates returned the duplicate id %q", res.Session.ID)
		}
		seen[res.Session.ID] = true
	}

	waitFor(t, 5*time.Second, fmt.Sprintf("%d live sessions", n), func() bool {
		return fx.srv.liveSessionCount() == n
	})
	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != n {
		t.Fatalf("list has %d sessions, want %d", len(list.Sessions), n)
	}

	// Each session persists to its own file: a duplicate id would put two
	// conversations in one transcript.
	for id := range seen {
		want := "echo:q-" + id
		if res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{
			SessionID: id, Text: "q-" + id,
		})); res.Text != want {
			t.Fatalf("session %s replied %q, want %q", id, res.Text, want)
		}
		_, msgs, err := fx.store.Load(id)
		if err != nil {
			t.Fatalf("load session %s: %v", id, err)
		}
		if len(msgs) != 2 {
			t.Fatalf("session %s persisted %d messages, want its own 2 (transcripts may have merged)", id, len(msgs))
		}
		last, ok := msgs[1].(agentcore.AssistantMessage)
		if !ok {
			t.Fatalf("session %s persisted %T as its last message", id, msgs[1])
		}
		if text := agentcore.ContentToText(last.Content); text != want {
			t.Fatalf("session %s persisted %q, want %q", id, text, want)
		}
	}
	files, err := filepath.Glob(filepath.Join(fx.store.Dir(), "*.jsonl"))
	if err != nil {
		t.Fatalf("glob session files: %v", err)
	}
	if len(files) != n {
		t.Fatalf("store holds %d session files, want %d", len(files), n)
	}
}

// TestSessionListContentAndUpdatedAt covers what session.list promises. The order
// is deliberately not asserted: the handler renders conn.owned, a map, so the
// order is unspecified and a client must sort for itself. What is asserted is the
// content — every owned session, nothing else — and that UpdatedAt advances only
// for the session that ran a turn (an injected clock puts CreatedAt in the past so
// the comparison cannot depend on clock resolution).
func TestSessionListContentAndUpdatedAt(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour)
	var ticks atomic.Int64
	f := newScriptFactory()
	f.echo = true
	fx := newFixture(t, f, func(cfg *Config) {
		cfg.Now = func() time.Time {
			return base.Add(time.Duration(ticks.Add(1)) * time.Microsecond)
		}
	})
	c := fx.dial(t)
	cwd := t.TempDir()

	ids := make([]string, 3)
	for i := range ids {
		ids[i] = decodeResult[hostapi.SessionCreateResult](t,
			c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd, Title: fmt.Sprintf("s%d", i)})).Session.ID
	}
	before := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(before.Sessions) != len(ids) {
		t.Fatalf("list has %d sessions, want %d", len(before.Sessions), len(ids))
	}
	byID := make(map[string]hostapi.SessionInfo, len(before.Sessions))
	for _, info := range before.Sessions {
		byID[info.ID] = info
	}
	for i, id := range ids {
		info, ok := byID[id]
		if !ok {
			t.Fatalf("session %d (%s) is missing from the list", i, id)
		}
		if info.Title != fmt.Sprintf("s%d", i) {
			t.Fatalf("session %d title = %q", i, info.Title)
		}
		if !info.CreatedAt.Equal(info.UpdatedAt) {
			t.Fatalf("session %d starts with CreatedAt %v != UpdatedAt %v", i, info.CreatedAt, info.UpdatedAt)
		}
		if !info.CreatedAt.Before(time.Now().UTC().Add(-time.Minute)) {
			t.Fatalf("session %d CreatedAt %v did not come from the injected clock", i, info.CreatedAt)
		}
	}
	// The host's local clock is not the injected one, so a turn's UpdatedAt is
	// strictly later than the create-time stamp.
	target := ids[1]
	if res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: target, Text: "hi"})); res.Text != "echo:hi" {
		t.Fatalf("prompt reply = %q", res.Text)
	}

	after := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(after.Sessions) != len(ids) {
		t.Fatalf("list has %d sessions after a turn, want %d", len(after.Sessions), len(ids))
	}
	for _, info := range after.Sessions {
		switch info.ID {
		case target:
			if !info.UpdatedAt.After(info.CreatedAt) {
				t.Fatalf("prompted session UpdatedAt %v did not advance past CreatedAt %v", info.UpdatedAt, info.CreatedAt)
			}
			if info.Running {
				t.Fatal("session still reports running after its turn finished")
			}
		default:
			if !info.UpdatedAt.Equal(info.CreatedAt) {
				t.Fatalf("idle session %s had its UpdatedAt bumped to %v", info.ID, info.UpdatedAt)
			}
		}
	}
}

// TestSessionCloseWhileTurnRunsAbortsTurn covers session.close on a session with a
// turn in flight. The host closes the session (cancelling the turn), reports the
// turn's outcome in-band as an abort rather than a transport failure, notifies the
// owner, and refuses further turns because ownership is gone.
func TestSessionCloseWhileTurnRunsAbortsTurn(t *testing.T) {
	f := newScriptFactory()
	f.hold = make(chan struct{})
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir(), Title: "busy"})).Session.ID

	type callResult struct {
		resp *hostapi.Envelope
		err  error
	}
	done := make(chan callResult, 1)
	go func() {
		resp, err := c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "long"})
		done <- callResult{resp, err}
	}()
	waitFor(t, 10*time.Second, "the turn to start", func() bool { return isRunning(fx, sid) })

	// While the turn runs the session is listed as running.
	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || !list.Sessions[0].Running {
		t.Fatalf("list during a turn = %+v, want one running session", list.Sessions)
	}

	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}))

	var closed hostapi.SessionClosedEvent
	if err := c.waitEvent(t, hostapi.EventSessionClosed).DecodeParams(&closed); err != nil {
		t.Fatalf("decode session_closed: %v", err)
	}
	if closed.SessionID != sid || closed.Reason != "closed by client" {
		t.Fatalf("session_closed = %+v", closed)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("closing the session broke the prompt transport: %v", r.err)
		}
		if r.resp.Error != nil {
			t.Fatalf("closed turn reported as an error: %v", r.resp.Error)
		}
		res := decodeResult[hostapi.PromptResult](t, r.resp)
		if res.StopReason != agentcore.StopReasonAborted {
			t.Fatalf("closed turn stop reason = %q, want %q", res.StopReason, agentcore.StopReasonAborted)
		}
		if res.SessionID != sid {
			t.Fatalf("closed turn reported session %q", res.SessionID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session.close did not unblock the running turn")
	}

	waitFor(t, 5*time.Second, "the session to leave the live map", func() bool {
		return fx.srv.liveSessionCount() == 0
	})
	// Ownership went with the session, so the connection can no longer drive it —
	// and learns nothing about whether the id still exists on disk.
	expectError(t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "again"}), hostapi.CodeForbidden)
}

// TestPromptRequiresOwnershipAndID covers the prompt handler's refusals: an empty
// session id is a params error (not a forbidden one), and a session this
// connection does not own — closed, unknown, or someone else's — is forbidden.
func TestPromptRequiresOwnershipAndID(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	owner := fx.dial(t)
	other := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		owner.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	expectError(t, owner.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{Text: "no id"}), hostapi.CodeInvalidParams)
	expectError(t, owner.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: "no-such-session", Text: "x"}), hostapi.CodeForbidden)
	expectError(t, other.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "x"}), hostapi.CodeForbidden)

	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, owner.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}))
	expectError(t, owner.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "x"}), hostapi.CodeForbidden)
}

// TestResumeOwnershipRules covers resume's three refusals: a session attached to
// another connection is forbidden (isolation), a session already attached to this
// connection is busy (no double writer), and an empty id is a params error.
func TestResumeOwnershipRules(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	owner := fx.dial(t)
	other := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		owner.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	expectError(t, other.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}), hostapi.CodeForbidden)
	expectError(t, owner.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}), hostapi.CodeBusy)
	expectError(t, owner.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{}), hostapi.CodeInvalidParams)
}

// TestResumeBusyWhileLiveTurnRunsElsewhere covers the reconnect-mid-turn window: a
// session whose client vanished is still live (the host keeps the turn running),
// so a second connection cannot attach a competing writer; it is told busy until
// the turn settles. Afterwards the transcript is resumable normally.
func TestResumeBusyWhileLiveTurnRunsElsewhere(t *testing.T) {
	f := newScriptFactory()
	f.hold = make(chan struct{})
	fx := newFixture(t, f)

	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	go func() {
		_, _ = c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "long"})
	}()
	waitFor(t, 10*time.Second, "the turn to start", func() bool { return isRunning(fx, sid) })

	// Sever the socket: the turn keeps running, the session stays live and
	// unowned, which is the window a reconnecting client lands in.
	c.close()
	waitFor(t, 10*time.Second, "the session to become unowned but live", func() bool {
		return fx.srv.ownerOf(sid) == nil && fx.srv.liveSession(sid) != nil
	})

	c2 := fx.dial(t)
	expectError(t, c2.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}), hostapi.CodeBusy)

	// Let the turn finish; the orphan is then reaped and the history is resumable.
	close(f.hold)
	waitFor(t, 10*time.Second, "the orphaned session to be reaped", func() bool {
		return fx.srv.liveSessionCount() == 0
	})
	rr := decodeResult[hostapi.SessionResumeResult](t, c2.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}))
	if rr.Session.ID != sid {
		t.Fatalf("resumed %q, want %q", rr.Session.ID, sid)
	}
	if len(rr.Messages) < 2 {
		t.Fatalf("resume replayed %d messages, want at least 2", len(rr.Messages))
	}
}

// TestCreateWithVanishedClientDoesNotLeakSession covers the create/disconnect
// race: the host builds the agent (which can take a while — a provider probe, a
// memory DB) before it claims the session, so a client that disappears inside
// Factory.NewAgent must not leave an agent pinned in the live map forever.
func TestCreateWithVanishedClientDoesNotLeakSession(t *testing.T) {
	f := newScriptFactory()
	f.createGate = make(chan struct{})
	f.createEntered = make(chan struct{}, 1)
	fx := newFixture(t, f)

	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}
	lc.request("create", hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})
	waitEntered := func() {
		t.Helper()
		select {
		case <-f.createEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("the host never entered Factory.NewAgent")
		}
	}
	waitEntered()

	// The handler is now blocked inside NewAgent while the client vanishes: the
	// connection's owned set is empty at close time, so publishing the session
	// afterwards is exactly the leak under test.
	_ = lc.nc.Close()
	waitFor(t, 10*time.Second, "the connection to be reaped", func() bool {
		return len(fx.srv.snapshotConns()) == 0
	})
	close(f.createGate)

	// The agent is built (so the session was published), and it must then be
	// reaped together with its agent rather than pinned with a dead owner.
	waitFor(t, 10*time.Second, "the agent to be built", func() bool { return f.specCount() == 1 })
	waitFor(t, 10*time.Second, "the orphaned session and its agent to be released", func() bool {
		return fx.srv.liveSessionCount() == 0 && f.closeCount() > 0
	})
}

// TestResumeWithVanishedClientDoesNotLeakSession is the same race on the resume
// path, which also publishes the session only after Factory.NewAgent returns.
func TestResumeWithVanishedClientDoesNotLeakSession(t *testing.T) {
	// Prepare a persisted session with a first host, then resume it with a client
	// that dies mid-create.
	seed := newFixture(t, newScriptFactory())
	cwd := t.TempDir()
	c := seed.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "hi"}))
	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}))

	f := newScriptFactory()
	f.createGate = make(chan struct{})
	f.createEntered = make(chan struct{}, 1)
	fx := newFixture(t, f) // its own store: resume of sid must be seeded below.
	seedHeader, seedMsgs, err := seed.store.Load(sid)
	if err != nil {
		t.Fatalf("load seeded session: %v", err)
	}
	if err := fx.store.Save(seedHeader, seedMsgs); err != nil {
		t.Fatalf("copy seeded session: %v", err)
	}

	lc := newLineConn(t, fx)
	if resp := lc.handshake(fx.token); resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}
	lc.request("resume", hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid})
	select {
	case <-f.createEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the host never entered Factory.NewAgent")
	}
	_ = lc.nc.Close()
	waitFor(t, 10*time.Second, "the connection to be reaped", func() bool {
		return len(fx.srv.snapshotConns()) == 0
	})
	close(f.createGate)

	waitFor(t, 10*time.Second, "the agent to be built", func() bool { return f.specCount() == 1 })
	waitFor(t, 10*time.Second, "the orphaned session and its agent to be released", func() bool {
		return fx.srv.liveSessionCount() == 0 && f.closeCount() > 0
	})
}

// TestCompactionRewritesPersistedSession covers appendTail's compaction branch: when
// a turn leaves the live context shorter than what is already on disk, the host
// rewrites the session from the compacted context (cli.PersistCompactedContext)
// instead of appending an empty tail — which would silently drop the turn — and the
// session keeps working afterwards.
func TestCompactionRewritesPersistedSession(t *testing.T) {
	f := newScriptFactory()
	f.echo = true
	f.compactAfter = 4 // the fourth turn compacts the context down to its last pair
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	for i := 1; i <= 3; i++ {
		decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{
			SessionID: sid, Text: fmt.Sprintf("q%d", i),
		}))
	}
	_, msgs, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load after three turns: %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("persisted %d messages after three turns, want 6", len(msgs))
	}

	// The fourth turn compacts 8 messages down to 2, below the persisted count.
	if res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{
		SessionID: sid, Text: "q4",
	})); res.Text != "echo:q4" {
		t.Fatalf("compacting turn replied %q", res.Text)
	}
	_, msgs, err = fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load after compaction: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("session holds %d messages after compaction, want the 2 retained ones (%+v)", len(msgs), msgs)
	}
	last, ok := msgs[len(msgs)-1].(agentcore.AssistantMessage)
	if !ok {
		t.Fatalf("the retained tail ends with %T, want an assistant message", msgs[len(msgs)-1])
	}
	if text := agentcore.ContentToText(last.Content); text != "echo:q4" {
		t.Fatalf("the retained tail ends with %q, want the compacting turn's answer", text)
	}

	// The persistence cursor was reset, so the next turn appends normally to the
	// compacted history rather than re-appending the whole context.
	decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "q5"}))
	_, msgs, err = fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load after the follow-up turn: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("session holds %d messages after the follow-up turn, want 4", len(msgs))
	}

	// And the compacted history is what a reconnect replays.
	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}))
	c2 := fx.dial(t)
	rr := decodeResult[hostapi.SessionResumeResult](t, c2.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}))
	if len(rr.Messages) != 4 {
		t.Fatalf("resume replayed %d messages, want the 4 compacted ones", len(rr.Messages))
	}
}

// TestResumeFillsMissingHeaderMetadata covers a session file written before the host
// recorded a model or a system prompt: resuming must adopt the freshly assembled
// values instead of running under empty ones, while a header that does have a prompt
// keeps it.
func TestResumeFillsMissingHeaderMetadata(t *testing.T) {
	f := newScriptFactory()
	f.echo = true
	fx := newFixture(t, f)
	now := time.Now().UTC()
	legacy := session.SessionHeader{ID: session.NewID(now), CreatedAt: now, UpdatedAt: now, Cwd: t.TempDir()}
	if err := fx.store.Save(legacy, agentcore.MessageList{agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("old turn")},
	}}); err != nil {
		t.Fatalf("save legacy session: %v", err)
	}

	c := fx.dial(t)
	rr := decodeResult[hostapi.SessionResumeResult](t, c.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: legacy.ID}))
	if rr.Session.Model != "script-model" || rr.Session.Provider != "script" {
		t.Fatalf("resumed metadata = %+v, want the agent's labels", rr.Session)
	}
	// The factory was told there was no recorded prompt, so it assembled one.
	if got := f.specs[0].SystemPrompt; got != "" {
		t.Fatalf("resume passed system prompt %q for a header without one", got)
	}
	st := fx.srv.liveSession(legacy.ID)
	if st == nil {
		t.Fatal("resumed session is not live")
	}
	if st.infoLocked().Cwd != legacy.Cwd {
		t.Fatalf("resumed cwd = %q, want %q", st.infoLocked().Cwd, legacy.Cwd)
	}
	st.mu.Lock()
	adopted := st.header.SystemPrompt
	st.mu.Unlock()
	if adopted == "" {
		t.Fatal("resume did not adopt the assembled system prompt onto the header")
	}
	if res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{
		SessionID: legacy.ID, Text: "new turn",
	})); res.Text != "echo:new turn" {
		t.Fatalf("prompt on a resumed legacy session replied %q", res.Text)
	}
	_, msgs, err := fx.store.Load(legacy.ID)
	if err != nil {
		t.Fatalf("load after the resumed turn: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("persisted %d messages after the resumed turn, want 3", len(msgs))
	}
}

// ---- events ------------------------------------------------------------

// TestTurnEventOrdering pins the per-turn ordering a UI renders from:
// agent_start, the text deltas, turn_end, agent_end. The prompt handler writes
// them from one goroutine through one connection, so the wire order is the
// emission order.
func TestTurnEventOrdering(t *testing.T) {
	f := newScriptFactory()
	f.reply = "alpha-beta"
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "hi"}))
	if res.Text != "alpha-beta" || res.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("prompt result = %+v", res)
	}

	envs := c.drainSessionEvents()
	types := make([]string, 0, len(envs))
	for _, env := range envs {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		if ev.SessionID != sid {
			t.Fatalf("event %s carries session %q, want %q", ev.Type, ev.SessionID, sid)
		}
		types = append(types, ev.Type)
	}
	want := []string{
		hostapi.SessionEventAgentStart,
		hostapi.SessionEventTextDelta,
		hostapi.SessionEventTextDelta,
		hostapi.SessionEventTurnEnd,
		hostapi.SessionEventAgentEnd,
	}
	if !slices.Equal(types, want) {
		t.Fatalf("event order = %v, want %v", types, want)
	}
	// The deltas reconstruct the reply, and turn_end carries the final message.
	var text strings.Builder
	for _, env := range envs {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		switch ev.Type {
		case hostapi.SessionEventTextDelta:
			text.WriteString(ev.Text)
		case hostapi.SessionEventTurnEnd:
			if ev.Message == nil {
				t.Fatal("turn_end carries no message")
			}
			if got := agentcore.ContentToText(ev.Message.Content); got != "alpha-beta" {
				t.Fatalf("turn_end message text = %q", got)
			}
		}
	}
	if text.String() != "alpha-beta" {
		t.Fatalf("deltas reconstruct %q", text.String())
	}
}

// TestToolCompactionAndSubAgentEventMapping covers the mapper's other
// discriminants: the tool lifecycle, compaction (including a failed one) and
// sub-agent progress. CompactionStartEvent deliberately has no wire
// representation, so it must not appear.
func TestToolCompactionAndSubAgentEventMapping(t *testing.T) {
	toolResult := func(s string) agentcore.AgentToolResult {
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(s)}}
	}
	f := newScriptFactory()
	f.reply = "done"
	f.events = []agentcore.AgentEvent{
		agentcore.ToolExecutionStartEvent{ToolCallID: "call-1", ToolName: "bash", Args: map[string]any{"command": "echo hi"}},
		agentcore.ToolExecutionUpdateEvent{ToolCallID: "call-1", ToolName: "bash", PartialResult: toolResult("partial output")},
		agentcore.ToolExecutionEndEvent{ToolCallID: "call-1", ToolName: "bash", Result: toolResult("full output"), IsError: true},
		agentcore.CompactionStartEvent{Reason: "threshold", TokensBefore: 1000},
		agentcore.CompactionEvent{Reason: "threshold", TokensBefore: 1000, TokensAfter: 400, SummarizedCount: 6, KeptCount: 2},
		agentcore.CompactionEvent{Reason: "overflow", TokensBefore: 900, TokensAfter: 900, ErrorMessage: "summarizer unavailable"},
		agentcore.SubAgentProgressEvent{ToolCallID: "task-1", Description: "scan the repo", Activity: "Running bash"},
		agentcore.SubAgentEndEvent{ToolCallID: "task-1"},
		agentcore.TelemetryEvent{Turns: 1},
	}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "go"}))

	byType := make(map[string][]hostapi.SessionEvent, 8)
	for _, env := range c.drainSessionEvents() {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		byType[ev.Type] = append(byType[ev.Type], ev)
	}
	if _, ok := byType["compaction_start"]; ok {
		t.Fatal("compaction_start must not reach the wire")
	}

	starts := byType[hostapi.SessionEventToolStart]
	if len(starts) != 1 {
		t.Fatalf("tool_start events = %d, want 1", len(starts))
	}
	if got := starts[0]; got.ToolCallID != "call-1" || got.ToolName != "bash" ||
		!strings.Contains(string(got.Arguments), `"echo hi"`) {
		t.Fatalf("tool_start = %+v (arguments %s)", got, got.Arguments)
	}
	updates := byType[hostapi.SessionEventToolUpdate]
	if len(updates) != 1 || updates[0].Result != "partial output" || updates[0].ToolCallID != "call-1" {
		t.Fatalf("tool_update = %+v", updates)
	}
	ends := byType[hostapi.SessionEventToolEnd]
	if len(ends) != 1 || ends[0].Result != "full output" || !ends[0].IsError || ends[0].ToolName != "bash" {
		t.Fatalf("tool_end = %+v", ends)
	}

	compactions := byType[hostapi.SessionEventCompaction]
	if len(compactions) != 2 {
		t.Fatalf("compaction events = %d, want 2", len(compactions))
	}
	if got := compactions[0]; got.TokensBefore != 1000 || got.TokensAfter != 400 || got.SummarizedCount != 6 || got.KeptCount != 2 || got.CompactionError != "" {
		t.Fatalf("compaction = %+v", got)
	}
	if got := compactions[1]; got.CompactionError != "summarizer unavailable" {
		t.Fatalf("failed compaction = %+v", got)
	}

	progress := byType[hostapi.SessionEventSubAgent]
	if len(progress) != 1 || progress[0].ToolCallID != "task-1" ||
		progress[0].Description != "scan the repo" || progress[0].Activity != "Running bash" {
		t.Fatalf("subagent_progress = %+v", progress)
	}
	subDone := byType[hostapi.SessionEventSubAgentDone]
	if len(subDone) != 1 || subDone[0].ToolCallID != "task-1" {
		t.Fatalf("subagent_end = %+v", subDone)
	}
}

// TestSessionEventWireFieldNames pins the JSON field names of the notification
// payloads — the part of the protocol another implementation (a web UI) reads. A
// renamed field is a silent break, so the key sets are asserted exactly.
func TestSessionEventWireFieldNames(t *testing.T) {
	f := newScriptFactory()
	f.reply = "ok"
	f.events = []agentcore.AgentEvent{
		agentcore.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "read", Args: map[string]any{"path": "x"}},
	}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "hi"}))

	keys := make(map[string][]string, 8)
	var turnEnd *agentcore.AssistantMessage
	for _, env := range c.drainSessionEvents() {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		keys[ev.Type] = jsonKeys(t, env.Params)
		if ev.Type == hostapi.SessionEventTurnEnd {
			turnEnd = ev.Message
		}
	}

	cases := map[string][]string{
		hostapi.SessionEventAgentStart: {"sessionId", "type"},
		hostapi.SessionEventTextDelta:  {"sessionId", "text", "type"},
		hostapi.SessionEventToolStart:  {"arguments", "sessionId", "toolCallId", "toolName", "type"},
		hostapi.SessionEventTurnEnd:    {"message", "sessionId", "type"},
		hostapi.SessionEventAgentEnd:   {"sessionId", "type"},
	}
	for typ, want := range cases {
		got, ok := keys[typ]
		if !ok {
			t.Fatalf("no %s event on the wire (saw %v)", typ, keys)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s payload fields = %v, want %v", typ, got, want)
		}
	}

	// turn_end carries the shared AssistantMessage encoding, so a client decodes it
	// with the same type the runtime uses instead of a protocol-local copy.
	if turnEnd == nil {
		t.Fatal("turn_end carries no message")
	}
	if turnEnd.Role() != agentcore.RoleAssistant {
		t.Fatalf("turn_end message role = %q, want %q", turnEnd.Role(), agentcore.RoleAssistant)
	}
	if got := agentcore.ContentToText(turnEnd.Content); got != "ok" {
		t.Fatalf("turn_end message text = %q", got)
	}
	if turnEnd.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("turn_end stop reason = %q", turnEnd.StopReason)
	}
}

// TestEventsStayOnTheirConnection asserts the stream is per connection: a second
// UI attached to the same host sees none of another session's events.
func TestEventsStayOnTheirConnection(t *testing.T) {
	f := newScriptFactory()
	f.reply = "private answer"
	fx := newFixture(t, f)
	owner := fx.dial(t)
	bystander := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		owner.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	// The bystander also owns a session, so its stream is not trivially empty of
	// anything a buggy fan-out would have to choose between.
	decodeResult[hostapi.SessionCreateResult](t,
		bystander.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()}))

	decodeResult[hostapi.PromptResult](t, owner.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "hi"}))

	if got := len(owner.drainSessionEvents()); got == 0 {
		t.Fatal("the owner received no session events")
	}
	syncAndCheckNoEvents(t, bystander, "a turn on another connection")

	// And the bystander's own turn reaches only the bystander.
	bid := decodeResult[hostapi.SessionListResult](t, bystander.mustCall(hostapi.MethodSessionList, nil)).Sessions[0].ID
	decodeResult[hostapi.PromptResult](t, bystander.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: bid, Text: "hi"}))
	for _, env := range bystander.drainSessionEvents() {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		if ev.SessionID != bid {
			t.Fatalf("bystander received an event for session %q", ev.SessionID)
		}
	}
	syncAndCheckNoEvents(t, owner, "a turn on the bystander's connection")
}

// ---- permission gate ---------------------------------------------------

// TestPermissionApproveAlwaysTrustsSession covers the "always" answer: it grants
// session trust for the session's directory, so the next side-effect call passes
// with no prompt — including from a second session in the same directory, which is
// the contract the REPL's "a" answer has.
func TestPermissionApproveAlwaysTrustsSession(t *testing.T) {
	fx := newFixture(t, newScriptFactory(), func(cfg *Config) { cfg.PermissionTimeout = 5 * time.Second })
	c := fx.dial(t)
	cwd := t.TempDir() // untrusted: the fixture's trust store is empty
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("created session is not live")
	}
	gate := fx.srv.permissionGate(st)

	allowed := make(chan bool, 1)
	go func() {
		dec := gate(context.Background(), agentcore.AgentToolCall{
			Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`),
		})
		allowed <- dec == nil
	}()

	var req hostapi.PermissionRequest
	if err := c.waitEvent(t, hostapi.EventPermission).DecodeParams(&req); err != nil {
		t.Fatalf("decode permission_request: %v", err)
	}
	if req.SessionID != sid || req.ToolName != "bash" || req.RequestID == "" {
		t.Fatalf("permission request = %+v", req)
	}
	if req.TimeoutMS != 5000 {
		t.Fatalf("permission request timeout = %d ms, want 5000", req.TimeoutMS)
	}
	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{
		SessionID: sid, RequestID: req.RequestID, Approve: true, Always: true,
	}))
	select {
	case ok := <-allowed:
		if !ok {
			t.Fatal("approved request was still denied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gate never returned after approval")
	}
	if !fx.srv.trust.IsTrusted(cwd) {
		t.Fatal("approve-always did not grant session trust for the session's directory")
	}

	// The next side-effect call in the same directory passes without a prompt.
	if dec := gate(context.Background(), agentcore.AgentToolCall{Name: "write"}); dec != nil {
		t.Fatalf("second side-effect call was still gated: %+v", dec)
	}
	syncAndCheckNoEvents(t, c, "a side-effect call in a session-trusted directory")

	// Trust is per directory, so a second session rooted there inherits it.
	sid2 := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	st2 := fx.srv.liveSession(sid2)
	if st2 == nil {
		t.Fatal("second session is not live")
	}
	if dec := fx.srv.permissionGate(st2)(context.Background(), agentcore.AgentToolCall{Name: "edit"}); dec != nil {
		t.Fatalf("a second session in the trusted directory was still gated: %+v", dec)
	}
}

// TestPermissionNotAskedForReadOnlyTools asserts only side-effect tools are gated:
// a read-only or network-read tool must never produce a permission_request.
func TestPermissionNotAskedForReadOnlyTools(t *testing.T) {
	fx := newFixture(t, newScriptFactory(), func(cfg *Config) { cfg.PermissionTimeout = 5 * time.Second })
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("created session is not live")
	}
	gate := fx.srv.permissionGate(st)

	// Syntactically valid names, including case variants and unknowns: the gate
	// keys off trust.SideEffectTools, so anything else is not its business.
	for _, name := range []string{"read", "grep", "find", "todo", "webfetch", "memory_search", "Bash", "not_a_tool"} {
		if dec := gate(context.Background(), agentcore.AgentToolCall{Name: name}); dec != nil {
			t.Fatalf("%q was gated: %+v", name, dec)
		}
	}
	syncAndCheckNoEvents(t, c, "read-only tool calls")
}

// TestPermissionStaleAnswerIsNotFound asserts an answer is consumed exactly once:
// replaying a request id (a duplicated click, a client retry) is reported as
// not-found instead of silently succeeding.
func TestPermissionStaleAnswerIsNotFound(t *testing.T) {
	fx := newFixture(t, newScriptFactory(), func(cfg *Config) { cfg.PermissionTimeout = 5 * time.Second })
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("created session is not live")
	}
	gate := fx.srv.permissionGate(st)

	done := make(chan struct{})
	go func() {
		defer close(done)
		gate(context.Background(), agentcore.AgentToolCall{Name: "bash"})
	}()
	var req hostapi.PermissionRequest
	if err := c.waitEvent(t, hostapi.EventPermission).DecodeParams(&req); err != nil {
		t.Fatalf("decode permission_request: %v", err)
	}
	answer := hostapi.PermissionDecideParams{SessionID: sid, RequestID: req.RequestID, Approve: false}
	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodPermissionDecide, answer))
	<-done
	expectError(t, c.mustCall(hostapi.MethodPermissionDecide, answer), hostapi.CodeNotFound)
}

// TestPermissionAnswerAfterTimeoutIsNotFound covers the timeout path: the wait
// table entry is dropped when the gate gives up, so a late answer is reported as
// not-found rather than pretending the decision took effect.
func TestPermissionAnswerAfterTimeoutIsNotFound(t *testing.T) {
	fx := newFixture(t, newScriptFactory(), func(cfg *Config) { cfg.PermissionTimeout = 100 * time.Millisecond })
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("created session is not live")
	}
	gate := fx.srv.permissionGate(st)

	decided := make(chan *agentcore.BeforeToolCallDecision, 1)
	go func() {
		decided <- gate(context.Background(), agentcore.AgentToolCall{Name: "bash"})
	}()
	var req hostapi.PermissionRequest
	if err := c.waitEvent(t, hostapi.EventPermission).DecodeParams(&req); err != nil {
		t.Fatalf("decode permission_request: %v", err)
	}
	// The gate returns a blocking decision once the timeout expires, and drops the
	// pending entry with it.
	select {
	case dec := <-decided:
		if dec == nil || !dec.Block {
			t.Fatalf("timed-out permission should block, got %+v", dec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gate never timed out")
	}
	expectError(t, c.mustCall(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{
		SessionID: sid, RequestID: req.RequestID, Approve: true,
	}), hostapi.CodeNotFound)
}

// TestPermissionDeniedWhenOwnerDisconnected covers the gate with nobody to ask: a
// detached session's side-effect call is denied immediately (the host must not wait
// for an owner that no longer exists) and the denial explains why.
func TestPermissionDeniedWhenOwnerDisconnected(t *testing.T) {
	f := newScriptFactory()
	f.hold = make(chan struct{})
	fx := newFixture(t, f, func(cfg *Config) { cfg.PermissionTimeout = 30 * time.Second })

	c := fx.dial(t)
	cwd := t.TempDir()
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	go func() {
		_, _ = c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "long"})
	}()
	waitFor(t, 10*time.Second, "the turn to start", func() bool { return isRunning(fx, sid) })

	c.close()
	waitFor(t, 10*time.Second, "the session to become unowned but live", func() bool {
		return fx.srv.ownerOf(sid) == nil && fx.srv.liveSession(sid) != nil
	})
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("session disappeared after the disconnect")
	}

	decided := make(chan *agentcore.BeforeToolCallDecision, 1)
	go func() {
		decided <- fx.srv.permissionGate(st)(context.Background(), agentcore.AgentToolCall{Name: "bash"})
	}()
	select {
	case dec := <-decided:
		if dec == nil || !dec.Block {
			t.Fatalf("a detached session's side-effect call was not blocked: %+v", dec)
		}
		if dec.Content == nil || !strings.Contains(agentcore.ContentToText(*dec.Content), "not trusted") {
			t.Fatalf("denial text = %+v", dec.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gate waited for an owner that had disconnected")
	}

	// Let the orphaned turn finish and its handler return before the temp store is
	// removed out from under it.
	close(f.hold)
	waitHandlers(t, fx.srv)
}

// ---- shutdown and serve ------------------------------------------------

// TestShutdownWhileTurnsInFlight asserts a shutdown gives in-flight turns a proper
// outcome (their contexts are cancelled and their agents released), tells the
// client why, and removes the host's footprint.
func TestShutdownWhileTurnsInFlight(t *testing.T) {
	f := newScriptFactory()
	f.hold = make(chan struct{}) // never closed: only shutdown can end this turn
	fx := newFixture(t, f)

	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	promptDone := make(chan struct{})
	go func() {
		defer close(promptDone)
		_, _ = c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "long"})
	}()
	waitFor(t, 10*time.Second, "the turn to start", func() bool { return isRunning(fx, sid) })
	if got := f.runCount(); got != 1 {
		t.Fatalf("agent runs = %d, want 1", got)
	}

	fx.srv.Shutdown("test in flight")

	// The client is told before the socket goes away.
	var payload hostapi.ServerShutdownEvent
	if err := c.waitEvent(t, hostapi.EventServerShutdown).DecodeParams(&payload); err != nil {
		t.Fatalf("decode server_shutdown: %v", err)
	}
	if payload.Reason != "test in flight" {
		t.Fatalf("shutdown reason = %q", payload.Reason)
	}

	// The turn's context is cancelled, its agent released, and the handler returns
	// (the reply has nowhere to go, which is not a hang).
	select {
	case <-f.cancelSig:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not cancel the in-flight turn's context")
	}
	select {
	case <-promptDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt handler did not return after shutdown")
	}
	// The handler owns the last write to the session store (persisting the partial
	// turn), and Shutdown does not wait for it, so the test must.
	waitHandlers(t, fx.srv)
	waitFor(t, 10*time.Second, "the session's agent to be closed", func() bool { return f.closeCount() > 0 })

	fx.srv.Wait()
	if got := fx.srv.liveSessionCount(); got != 0 {
		t.Fatalf("%d sessions survived shutdown", got)
	}
	for _, path := range []string{fx.srv.Socket(), TokenPath(fx.dir), StatePath(fx.dir)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived shutdown (stat error: %v)", path, err)
		}
	}
}

// TestShutdownIsIdempotent asserts Shutdown is safe to call repeatedly and
// concurrently (the CLI calls it from a signal handler while a request handler may
// already have called it).
func TestShutdownIsIdempotent(t *testing.T) {
	fx := newFixture(t, newScriptFactory())
	c := fx.dial(t)
	decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()}))
	if fx.srv.liveSessionCount() != 1 {
		t.Fatalf("live sessions before shutdown = %d, want 1", fx.srv.liveSessionCount())
	}

	fx.srv.Shutdown("first")
	fx.srv.Wait()
	// A second (and third) call must return without panicking or blocking.
	fx.srv.Shutdown("second")
	fx.srv.Wait()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fx.srv.Shutdown(fmt.Sprintf("concurrent-%d", i))
			fx.srv.Wait()
		}(i)
	}
	wg.Wait()

	if got := fx.srv.liveSessionCount(); got != 0 {
		t.Fatalf("%d sessions survived the first shutdown", got)
	}
	// The socket is gone, so nothing can reconnect to a dead host.
	if conn, err := net.Dial("unix", fx.srv.Socket()); err == nil {
		conn.Close()
		t.Fatal("a connection to the shut-down host succeeded")
	}
}

// TestServeReturnsWhenContextCancelled drives the accept loop directly: a cancelled
// context stops Serve with a nil error (not an accept error), tells clients why,
// and leaves no footprint behind.
func TestServeReturnsWhenContextCancelled(t *testing.T) {
	srv, token := bareHost(t, t.TempDir(), newScriptFactory())
	fx := &fixture{srv: srv, token: token, dir: srv.Dir()}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()

	c := fx.dial(t)
	decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v after ctx cancellation, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after ctx cancellation")
	}

	var payload hostapi.ServerShutdownEvent
	if err := c.waitEvent(t, hostapi.EventServerShutdown).DecodeParams(&payload); err != nil {
		t.Fatalf("decode server_shutdown: %v", err)
	}
	if payload.Reason != "context cancelled" {
		t.Fatalf("shutdown reason = %q, want %q", payload.Reason, "context cancelled")
	}

	srv.Wait()
	for _, path := range []string{srv.Socket(), TokenPath(srv.Dir()), StatePath(srv.Dir())} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived the context shutdown (stat error: %v)", path, err)
		}
	}
}

// TestServerConstructionGuards covers the foolproofing around NewServer/Serve: a
// host without a Factory fails at construction (before it creates any directory),
// Serve before Listen is an error rather than a nil-pointer panic, and
// ErrAlreadyRunning's message names the pid the CLI prints.
func TestServerConstructionGuards(t *testing.T) {
	// Nothing below may touch the developer's state.
	t.Setenv("PIGO_HOME", t.TempDir())

	if _, err := NewServer(Config{}); err == nil {
		t.Fatal("NewServer accepted a Config without a Factory")
	} else if !strings.Contains(err.Error(), "Factory") {
		t.Fatalf("missing-Factory error = %q", err)
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	mgr, err := trust.NewManager(filepath.Join(t.TempDir(), "trust.json"))
	if err != nil {
		t.Fatalf("trust: %v", err)
	}
	srv, err := NewServer(Config{Dir: t.TempDir(), Factory: newScriptFactory(), Store: store, Trust: mgr})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve before Listen returned nil")
	}
	srv.Shutdown("never served")
	srv.Wait()

	// The CLI prints these messages verbatim, so they must stay meaningful.
	if msg := (&ErrAlreadyRunning{PID: 4242}).Error(); !strings.Contains(msg, "4242") {
		t.Fatalf("ErrAlreadyRunning message = %q, want the pid", msg)
	}
	if msg := (&ErrAlreadyRunning{}).Error(); !strings.Contains(msg, "already running") {
		t.Fatalf("ErrAlreadyRunning message without a pid = %q", msg)
	}
}

// TestListenHandlesStaleFootprint covers the previous-host-crashed case: a socket
// file with no listener and a state file naming a dead pid must not be mistaken for
// a running host, and anything at the socket path (even a regular file) is
// replaced.
func TestListenHandlesStaleFootprint(t *testing.T) {
	t.Run("stale socket and state file", func(t *testing.T) {
		dir := t.TempDir()
		sock := SocketPath(dir)
		// A crashed host leaves the socket file behind; unlink-on-close is off so
		// closing the listener does not tidy up after us.
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
		if err != nil {
			t.Fatalf("create stale socket: %v", err)
		}
		l.SetUnlinkOnClose(false)
		if err := l.Close(); err != nil {
			t.Fatalf("close stale socket: %v", err)
		}
		if _, err := os.Stat(sock); err != nil {
			t.Fatalf("stale socket file missing: %v", err)
		}
		stale := State{PID: 1 << 22, Version: Version, StartedAt: time.Now().Add(-time.Hour), Socket: sock}
		data, err := marshalState(stale)
		if err != nil {
			t.Fatalf("marshal state: %v", err)
		}
		if err := writeFileAtomic(StatePath(dir), data, 0o600); err != nil {
			t.Fatalf("write stale state: %v", err)
		}

		srv, token := bareHost(t, dir, newScriptFactory())
		serveInBackground(t, srv)
		info, err := os.Stat(srv.Socket())
		if err != nil {
			t.Fatalf("stat socket: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
		}
		fx := &fixture{srv: srv, token: token, dir: dir}
		c := fx.dial(t)
		if got := decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil)); got.PID == 0 {
			t.Fatalf("status after a stale restart = %+v", got)
		}
	})

	t.Run("regular file at the socket path", func(t *testing.T) {
		dir := t.TempDir()
		sock := SocketPath(dir)
		if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
			t.Fatalf("write placeholder: %v", err)
		}
		srv, token := bareHost(t, dir, newScriptFactory())
		serveInBackground(t, srv)
		fx := &fixture{srv: srv, token: token, dir: dir}
		c := fx.dial(t)
		decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))
	})
}

// ---- concurrency -------------------------------------------------------

// TestConcurrentConnectionsStayIsolated is the isolation contract under load: many
// connections create and drive sessions at once, and each turn's answer, event
// stream and listing must stay its own.
func TestConcurrentConnectionsStayIsolated(t *testing.T) {
	const (
		conns   = 8
		perConn = 2
	)
	f := newScriptFactory()
	f.echo = true
	fx := newFixture(t, f)

	clients := make([]*testClient, conns)
	ids := make([][]string, conns)
	for i := range clients {
		clients[i] = fx.dial(t)
		cwd := t.TempDir()
		for j := 0; j < perConn; j++ {
			ids[i] = append(ids[i], decodeResult[hostapi.SessionCreateResult](t,
				clients[i].mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID)
		}
	}
	waitFor(t, 10*time.Second, "every session to be live", func() bool {
		return fx.srv.liveSessionCount() == conns*perConn
	})

	// Each worker drives one connection sequentially (turns on one connection must
	// not interleave anyway) while all connections run at once. Assertions are
	// reported through errs: Fatalf from a worker goroutine would only kill that
	// goroutine.
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perConn; j++ {
				want := fmt.Sprintf("echo:c%d-s%d", i, j)
				resp, err := clients[i].call(hostapi.MethodPrompt, hostapi.PromptParams{
					SessionID: ids[i][j], Text: fmt.Sprintf("c%d-s%d", i, j),
				})
				if err != nil {
					errs <- fmt.Errorf("conn %d session %d: %w", i, j, err)
					return
				}
				if resp.Error != nil {
					errs <- fmt.Errorf("conn %d session %d: %v", i, j, resp.Error)
					return
				}
				var res hostapi.PromptResult
				if err := resp.DecodeResult(&res); err != nil {
					errs <- fmt.Errorf("conn %d session %d: decode: %w", i, j, err)
					return
				}
				if res.Text != want {
					errs <- fmt.Errorf("conn %d session %d answered %q, want %q", i, j, res.Text, want)
					return
				}
				if res.SessionID != ids[i][j] {
					errs <- fmt.Errorf("conn %d session %d reported id %q", i, j, res.SessionID)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	for i := range clients {
		own := make(map[string]bool, perConn)
		for _, id := range ids[i] {
			own[id] = true
		}
		// The listing is exactly this connection's sessions.
		list := decodeResult[hostapi.SessionListResult](t, clients[i].mustCall(hostapi.MethodSessionList, nil))
		if len(list.Sessions) != perConn {
			t.Fatalf("conn %d lists %d sessions, want %d", i, len(list.Sessions), perConn)
		}
		for _, info := range list.Sessions {
			if !own[info.ID] {
				t.Fatalf("conn %d lists a session it does not own: %s", i, info.ID)
			}
		}
		// Every event it received belongs to one of its sessions, and the deltas
		// reconstruct only its own answers, in order.
		var text strings.Builder
		for _, env := range clients[i].allEvents() {
			var ev hostapi.SessionEvent
			if err := env.DecodeParams(&ev); err != nil {
				t.Fatalf("conn %d: decode session event: %v", i, err)
			}
			if !own[ev.SessionID] {
				t.Fatalf("conn %d received an event for a foreign session %q", i, ev.SessionID)
			}
			if ev.Type == hostapi.SessionEventTextDelta {
				text.WriteString(ev.Text)
			}
		}
		want := fmt.Sprintf("echo:c%d-s0echo:c%d-s1", i, i)
		if text.String() != want {
			t.Fatalf("conn %d streamed %q, want %q", i, text.String(), want)
		}
	}
}

// TestConcurrentPromptsOnSeparateSessionsOfOneConnection hammers a single
// connection with turns on two sessions at once: the per-session lock, the
// permission table and the shared event writer all have to stay consistent.
func TestConcurrentPromptsOnSeparateSessionsOfOneConnection(t *testing.T) {
	f := newScriptFactory()
	f.echo = true
	fx := newFixture(t, f)
	c := fx.dial(t)
	cwd := t.TempDir()
	a := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	b := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID

	// Register both requests before waiting so the turns really overlap; the test
	// client keeps a single writer on its socket.
	_, chA := c.send(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: a, Text: "A"})
	_, chB := c.send(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: b, Text: "B"})
	for name, ch := range map[string]chan *hostapi.Envelope{"a": chA, "b": chB} {
		resp, err := c.await(ch, hostapi.MethodPrompt)
		if err != nil {
			t.Fatalf("prompt %s: %v", name, err)
		}
		res := decodeResult[hostapi.PromptResult](t, resp)
		want := "echo:A"
		if name == "b" {
			want = "echo:B"
		}
		if res.Text != want {
			t.Fatalf("prompt %s answered %q, want %q", name, res.Text, want)
		}
	}

	// Each session's events carry its own id; both turns' deltas are present.
	perSession := map[string]string{}
	for _, env := range c.drainSessionEvents() {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		if ev.Type == hostapi.SessionEventTextDelta {
			perSession[ev.SessionID] += ev.Text
		}
	}
	if perSession[a] != "echo:A" || perSession[b] != "echo:B" {
		t.Fatalf("streamed deltas per session = %v", perSession)
	}
}
