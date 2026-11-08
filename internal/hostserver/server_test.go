// This file tests the host daemon over a real unix socket with a scripted agent
// factory. The point is to exercise the machine — handshake, ownership,
// serialization, streaming events, resume, permission gating, persistence —
// without a provider, credentials or the network, matching the project's
// preference for testing against real seams (internal/runtime/faux_provider_test.go)
// rather than mocking the code under test.
package hostserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// fakeAgent is a scripted Agent. Each Run appends a user message and an
// assistant reply, streams the reply in two chunks (so the server's delta path
// is exercised), and honours ctx cancellation the way a real loop does.
type fakeAgent struct {
	mu       sync.Mutex
	spec     AgentSpec
	messages agentcore.MessageList
	reply    string
	delay    time.Duration
	closed   bool
}

func (a *fakeAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	a.mu.Lock()
	if prompt != "" {
		a.messages = append(a.messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	reply := a.reply
	delay := a.delay
	a.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	half := len(reply) / 2
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnStartEvent{})
	}
	if h.OnText != nil {
		h.OnText(reply[:half])
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

func (a *fakeAgent) Messages() agentcore.MessageList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(agentcore.MessageList(nil), a.messages...)
}

func (a *fakeAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *fakeAgent) Model() string        { return a.spec.Model }
func (a *fakeAgent) Provider() string     { return "fake" }

func (a *fakeAgent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
}

// fakeFactory builds fakeAgents and records the specs it was given.
type fakeFactory struct {
	mu    sync.Mutex
	specs []AgentSpec
	reply string
	delay time.Duration
	// refuse makes NewAgent fail for a cwd, for the create-error path.
	refuse string
}

func (f *fakeFactory) NewAgent(_ context.Context, spec AgentSpec) (Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != "" && spec.Cwd == f.refuse {
		return nil, &testError{"factory refused " + spec.Cwd}
	}
	f.specs = append(f.specs, spec)
	if spec.Model == "" {
		spec.Model = "fake-model"
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = "fake system prompt"
	}
	return &fakeAgent{spec: spec, messages: append(agentcore.MessageList(nil), spec.Messages...), reply: f.reply, delay: f.delay}, nil
}

func (f *fakeFactory) specCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

func (f *fakeFactory) specAt(i int) AgentSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[i]
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// testClient is a minimal raw client: it speaks the wire format directly so a
// bug in the client library cannot hide a server bug.
//
// A dedicated reader goroutine demultiplexes responses (matched by request id)
// from server events, which is what lets a test have a prompt in flight while it
// sends cancel or answers a permission request on the same connection — exactly
// the interleaving the protocol promises.
type testClient struct {
	t  *testing.T
	nc net.Conn

	mu      sync.Mutex
	next    int
	pending map[string]chan *hostapi.Envelope
	events  []hostapi.Envelope
	closed  bool
	readErr error
	wake    chan struct{}
}

// fixture is a running server plus everything a test needs to talk to it.
type fixture struct {
	srv   *Server
	token string
	store *session.Store
	dir   string
}

// newFixture starts a server on temp dirs and returns it with its token.
func newFixture(t *testing.T, f Factory, mutate ...func(*Config)) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	// Trust is created over a temp file so the tests never read the developer's
	// real trust decisions (AGENTS.md testing rule).
	mgr, err := trust.NewManager(t.TempDir() + "/trust.json")
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
	token, err := ReadToken(dir)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	return &fixture{srv: srv, token: token, store: store, dir: dir}
}

// dial connects and completes hello.
func (fx *fixture) dial(t *testing.T) *testClient {
	t.Helper()
	c := fx.rawDial(t)
	resp, err := c.call(hostapi.MethodHello, hostapi.HelloParams{Version: hostapi.Version, Token: fx.token, Client: "test"})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("hello failed: %v", resp.Error)
	}
	return c
}

// rawDial connects without handshaking, for the unauthorized-path tests.
func (fx *fixture) rawDial(t *testing.T) *testClient {
	t.Helper()
	nc, err := net.Dial("unix", fx.srv.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &testClient{
		t:       t,
		nc:      nc,
		pending: make(map[string]chan *hostapi.Envelope),
		wake:    make(chan struct{}, 1),
	}
	go c.read()
	t.Cleanup(c.close)
	return c
}

// read is the connection's only reader.
func (c *testClient) read() {
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		var env hostapi.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			c.finish(err)
			return
		}
		if env.IsEvent() {
			c.mu.Lock()
			c.events = append(c.events, env)
			c.mu.Unlock()
			c.signal()
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[env.ID]
		delete(c.pending, env.ID)
		c.mu.Unlock()
		if ok {
			ch <- &env
		}
	}
	c.finish(sc.Err())
}

// finish records the read loop's end and fails every waiting call.
func (c *testClient) finish(err error) {
	c.mu.Lock()
	c.closed = true
	c.readErr = err
	pending := c.pending
	c.pending = make(map[string]chan *hostapi.Envelope)
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	c.signal()
}

// close shuts the socket down (idempotent).
func (c *testClient) close() {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		_ = c.nc.Close()
	}
}

// signal nudges a waiter without blocking.
func (c *testClient) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// call sends a request and waits for its response.
func (c *testClient) call(method string, params any) (*hostapi.Envelope, error) {
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
			return nil, err
		}
		env["params"] = json.RawMessage(raw)
	}
	line, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if _, err := c.nc.Write(append(line, '\n')); err != nil {
		return nil, err
	}
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

// mustCall sends a request and fails the test on a transport error.
func (c *testClient) mustCall(method string, params any) *hostapi.Envelope {
	c.t.Helper()
	resp, err := c.call(method, params)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return resp
}

// decodeResult unmarshals a successful response's result.
func decodeResult[T any](t *testing.T, resp *hostapi.Envelope) T {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %v", resp.Error)
	}
	var v T
	if err := resp.DecodeResult(&v); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return v
}

// expectError asserts the response is an error with the given code.
func expectError(t *testing.T, resp *hostapi.Envelope, code int) {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("expected error %d, got result %s", code, resp.Result)
	}
	if resp.Error.Code != code {
		t.Fatalf("error code = %d (%s), want %d", resp.Error.Code, resp.Error.Message, code)
	}
}

// takeEvent removes and returns the first buffered event of a session type.
func (c *testClient) takeEvent(typ string) (hostapi.SessionEvent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, env := range c.events {
		if env.Event != hostapi.EventSession {
			continue
		}
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil || ev.Type != typ {
			continue
		}
		c.events = append(c.events[:i], c.events[i+1:]...)
		return ev, true
	}
	return hostapi.SessionEvent{}, false
}

// collectEvents drains every buffered session event of a type.
func (c *testClient) collectEvents(typ string) []hostapi.SessionEvent {
	var out []hostapi.SessionEvent
	for {
		ev, ok := c.takeEvent(typ)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

// waitEvent blocks until an event with the given envelope event name arrives.
func (c *testClient) waitEvent(t *testing.T, name string) hostapi.Envelope {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		for i, env := range c.events {
			if env.Event == name {
				c.events = append(c.events[:i], c.events[i+1:]...)
				c.mu.Unlock()
				return env
			}
		}
		closed, readErr := c.closed, c.readErr
		c.mu.Unlock()
		if closed {
			t.Fatalf("connection closed while waiting for event %q (read error: %v)", name, readErr)
		}
		select {
		case <-c.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", name)
		}
	}
}

func TestHelloRejectsBadToken(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	c := fx.rawDial(t)
	resp := c.mustCall(hostapi.MethodHello, hostapi.HelloParams{Version: hostapi.Version, Token: "wrong"})
	expectError(t, resp, hostapi.CodeUnauthorized)
}

func TestUnauthenticatedRequestRefused(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	c := fx.rawDial(t)
	resp := c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{})
	expectError(t, resp, hostapi.CodeUnauthorized)
}

func TestCreatePromptStreamsEventsAndPersists(t *testing.T) {
	f := &fakeFactory{reply: "hello there"}
	fx := newFixture(t, f)
	c := fx.dial(t)

	created := decodeResult[hostapi.SessionCreateResult](t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir(), Title: "demo"}))
	if created.Session.ID == "" {
		t.Fatal("create returned no session id")
	}
	if created.Session.Model != "fake-model" || created.Session.Provider != "fake" {
		t.Fatalf("create metadata = %+v", created.Session)
	}
	if created.Session.Title != "demo" {
		t.Fatalf("title lost: %+v", created.Session)
	}

	res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: created.Session.ID, Text: "hi"}))
	if res.Text != "hello there" {
		t.Fatalf("prompt result text = %q", res.Text)
	}
	if res.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	deltas := c.collectEvents(hostapi.SessionEventTextDelta)
	if len(deltas) != 2 {
		t.Fatalf("text deltas = %d (%+v), want 2", len(deltas), deltas)
	}
	if deltas[0].Text+deltas[1].Text != "hello there" {
		t.Fatalf("deltas do not reconstruct the reply: %+v", deltas)
	}
	if got := c.collectEvents(hostapi.SessionEventTurnEnd); len(got) != 1 {
		t.Fatalf("turn_end events = %d, want 1", len(got))
	}
	if got := c.collectEvents(hostapi.SessionEventAgentStart); len(got) != 0 {
		t.Fatalf("fake agent should not emit agent_start, got %+v", got)
	}

	// The turn must be on disk: the file exists and holds the user + assistant
	// messages the agent accumulated.
	header, msgs, err := fx.store.Load(created.Session.ID)
	if err != nil {
		t.Fatalf("load persisted session: %v", err)
	}
	if header.Cwd == "" {
		t.Fatal("persisted header has no cwd")
	}
	if len(msgs) != 2 {
		t.Fatalf("persisted %d messages, want 2 (%+v)", len(msgs), msgs)
	}
}

func TestPromptRefusedWhileRunningThenCancel(t *testing.T) {
	f := &fakeFactory{reply: "slow", delay: 5 * time.Second}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	// Start the turn on its own goroutine; it stays in flight because the fake
	// agent sleeps.
	firstDone := make(chan *hostapi.Envelope, 1)
	go func() {
		resp, err := c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "first"})
		if err != nil {
			t.Errorf("first prompt: %v", err)
			firstDone <- nil
			return
		}
		firstDone <- resp
	}()

	// Wait until the host reports the session running, so the busy check below
	// cannot race the first request's arrival.
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
		running := len(list.Sessions) == 1 && list.Sessions[0].Running
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first prompt never started running")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A second prompt must be refused rather than queued.
	expectError(t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "second"}), hostapi.CodeBusy)

	// Cancel must be answerable while the turn is in flight — this is the whole
	// reason requests are handled on their own goroutine.
	cancelResp := decodeResult[struct {
		OK      bool `json:"ok"`
		Running bool `json:"running"`
	}](t, c.mustCall(hostapi.MethodCancel, hostapi.CancelParams{SessionID: sid}))
	if !cancelResp.OK || !cancelResp.Running {
		t.Fatalf("cancel did not report a running turn: %+v", cancelResp)
	}
	select {
	case resp := <-firstDone:
		if resp == nil {
			t.Fatal("first prompt never got a response")
		}
		if resp.Error != nil {
			t.Fatalf("cancelled turn returned a transport error: %v", resp.Error)
		}
		res := decodeResult[hostapi.PromptResult](t, resp)
		if res.StopReason != agentcore.StopReasonAborted {
			t.Fatalf("cancelled turn stop reason = %q, want aborted", res.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not unblock the running turn")
	}

	// The session is usable again afterwards.
	if res := decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "again"})); res.SessionID != sid {
		t.Fatalf("post-cancel turn ran under %q", res.SessionID)
	}
}

func TestSessionsAreIsolatedPerConnection(t *testing.T) {
	fx := newFixture(t, &fakeFactory{reply: "ok"})
	owner := fx.dial(t)
	other := fx.dial(t)

	sid := decodeResult[hostapi.SessionCreateResult](t, owner.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	// The other connection's list is empty: isolation is per connection, not a
	// filter over a global list.
	list := decodeResult[hostapi.SessionListResult](t, other.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 0 {
		t.Fatalf("other connection sees %d sessions, want 0", len(list.Sessions))
	}
	if own := decodeResult[hostapi.SessionListResult](t, owner.mustCall(hostapi.MethodSessionList, nil)); len(own.Sessions) != 1 {
		t.Fatalf("owner sees %d sessions, want 1", len(own.Sessions))
	}

	// Prompting, cancelling, closing and answering permissions for someone else's
	// session are all forbidden.
	expectError(t, other.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "hi"}), hostapi.CodeForbidden)
	expectError(t, other.mustCall(hostapi.MethodCancel, hostapi.CancelParams{SessionID: sid}), hostapi.CodeForbidden)
	expectError(t, other.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}), hostapi.CodeForbidden)
	expectError(t, other.mustCall(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{SessionID: sid, RequestID: "x"}), hostapi.CodeForbidden)
}

func TestResumeReplaysHistoryAfterClose(t *testing.T) {
	f := &fakeFactory{reply: "answer"}
	fx := newFixture(t, f)
	c := fx.dial(t)
	cwd := t.TempDir()
	sid := decodeResult[hostapi.SessionCreateResult](t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "question"}))

	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}))
	// Closing twice: the session is gone from this connection, so the second
	// close must be refused rather than silently succeed.
	expectError(t, c.mustCall(hostapi.MethodSessionClose, hostapi.SessionCloseParams{ID: sid}), hostapi.CodeForbidden)

	// A second connection resumes by id: the conversation replays and the header
	// metadata (including cwd) is preserved.
	c2 := fx.dial(t)
	rr := decodeResult[hostapi.SessionResumeResult](t, c2.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}))
	if rr.Session.ID != sid {
		t.Fatalf("resumed %q, want %q", rr.Session.ID, sid)
	}
	if rr.Session.Cwd != cwd {
		t.Fatalf("resumed cwd = %q, want %q", rr.Session.Cwd, cwd)
	}
	if len(rr.Messages) != 2 {
		t.Fatalf("resume replayed %d messages, want 2", len(rr.Messages))
	}
	// The factory must have been told to resume in the session's cwd, not the
	// host process's.
	spec := f.specAt(f.specCount() - 1)
	if spec.Cwd != cwd {
		t.Fatalf("resume spec cwd = %q, want %q", spec.Cwd, cwd)
	}
	if len(spec.Messages) != 2 {
		t.Fatalf("resume spec seeded %d messages, want 2", len(spec.Messages))
	}
	if spec.SystemPrompt != "fake system prompt" {
		t.Fatalf("resume spec prompt = %q", spec.SystemPrompt)
	}

	// A resumed turn continues the same session rather than starting a new one.
	res := decodeResult[hostapi.PromptResult](t, c2.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "follow-up"}))
	if res.SessionID != sid {
		t.Fatalf("follow-up ran under %q", res.SessionID)
	}
	_, msgs, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load after follow-up: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("session holds %d messages after follow-up, want 4", len(msgs))
	}
}

func TestResumeRejectsSubAgentAndUnknownSessions(t *testing.T) {
	fx := newFixture(t, &fakeFactory{reply: "x"})
	c := fx.dial(t)
	expectError(t, c.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: "nope"}), hostapi.CodeNotFound)

	// A sub-agent session is not a standalone conversation.
	now := time.Now().UTC()
	h := session.SessionHeader{ID: session.NewID(now), CreatedAt: now, UpdatedAt: now, SubAgent: "task"}
	if err := fx.store.Save(h, agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}); err != nil {
		t.Fatalf("save sub-agent session: %v", err)
	}
	expectError(t, c.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: h.ID}), hostapi.CodeForbidden)
}

func TestPermissionRequestIsPushedToStringOwner(t *testing.T) {
	fx := newFixture(t, &fakeFactory{reply: "done"}, func(cfg *Config) { cfg.PermissionTimeout = 5 * time.Second })
	c := fx.dial(t)
	cwd := t.TempDir() // untrusted: the fixture's trust store is empty
	sid := decodeResult[hostapi.SessionCreateResult](t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID

	// The fake agent asks the gate directly and reports the decision, so the test
	// exercises the real event/answer round-trip rather than the loop.
	type gateResult struct{ allowed bool }
	gateCh := make(chan gateResult, 1)
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("created session is not live")
	}
	gate := fx.srv.permissionGate(st)
	go func() {
		dec := gate(context.Background(), agentcore.AgentToolCall{Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf /tmp/x"}`)})
		gateCh <- gateResult{allowed: dec == nil}
	}()

	perm := c.waitEvent(t, hostapi.EventPermission)
	var req hostapi.PermissionRequest
	if err := perm.DecodeParams(&req); err != nil {
		t.Fatalf("decode permission request: %v", err)
	}
	if req.SessionID != sid || req.ToolName != "bash" || !strings.Contains(req.Summary, "rm -rf") {
		t.Fatalf("permission request = %+v", req)
	}
	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{
		SessionID: sid, RequestID: req.RequestID, Approve: true,
	}))
	res := <-gateCh
	if !res.allowed {
		t.Fatal("approved request was still denied")
	}

	// An unknown request id must not report success.
	expectError(t, c.mustCall(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{
		SessionID: sid, RequestID: "perm-999", Approve: true,
	}), hostapi.CodeNotFound)
}

func TestPermissionDeniedWhenNoOwnerAnswers(t *testing.T) {
	fx := newFixture(t, &fakeFactory{reply: "x"}, func(cfg *Config) { cfg.PermissionTimeout = 50 * time.Millisecond })
	cwd := t.TempDir()
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd})).Session.ID
	st := fx.srv.liveSession(sid)
	if st == nil {
		t.Fatal("created session is not live")
	}
	gate := fx.srv.permissionGate(st)
	dec := gate(context.Background(), agentcore.AgentToolCall{Name: "write"})
	if dec == nil || !dec.Block {
		t.Fatalf("timed-out permission should block, got %+v", dec)
	}
	if dec.Content == nil || !strings.Contains(agentcore.ContentToText(*dec.Content), "not trusted") {
		t.Fatalf("denial text = %+v", dec.Content)
	}
}

func TestCreateRejectsBadCwd(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	c := fx.dial(t)
	resp := c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: "/definitely/not/here"})
	expectError(t, resp, hostapi.CodeInvalidParams)
}

func TestCreateReportsFactoryFailure(t *testing.T) {
	refused := t.TempDir()
	fx := newFixture(t, &fakeFactory{refuse: refused})
	c := fx.dial(t)
	expectError(t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: refused}), hostapi.CodeInternal)
}

func TestServerStatusAndShutdown(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	c := fx.dial(t)
	st := decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))
	if st.PID == 0 || st.Socket != fx.srv.Socket() {
		t.Fatalf("status = %+v", st)
	}
	if st.Sessions != 0 {
		t.Fatalf("status sessions = %d, want 0", st.Sessions)
	}

	decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodServerShutdown, nil))
	// The client is told why it is about to lose the connection.
	shutdown := c.waitEvent(t, hostapi.EventServerShutdown)
	var payload hostapi.ServerShutdownEvent
	if err := shutdown.DecodeParams(&payload); err != nil {
		t.Fatalf("decode shutdown event: %v", err)
	}
	if payload.Reason == "" {
		t.Fatal("shutdown event carries no reason")
	}
	fx.srv.Wait()
	if _, err := ReadState(fx.dir); err == nil {
		t.Fatal("state file survived shutdown")
	}
}

func TestSecondListenIsRefused(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	store, _ := session.NewStore(t.TempDir())
	other, err := NewServer(Config{Dir: fx.dir, Factory: &fakeFactory{}, Store: store})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	err = other.Listen()
	if err == nil {
		other.Shutdown("unexpected")
		t.Fatal("second Listen on the same socket succeeded")
	}
	var already *ErrAlreadyRunning
	if !errors.As(err, &already) {
		t.Fatalf("second Listen error = %v, want ErrAlreadyRunning", err)
	}
}

func TestUnknownMethodDoesNotCloseConnection(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	c := fx.dial(t)
	expectError(t, c.mustCall("nope.nope", nil), hostapi.CodeMethodNotFound)
	// Still usable afterwards.
	decodeResult[hostapi.ServerStatusResult](t, c.mustCall(hostapi.MethodServerStatus, nil))
}

func TestSocketAndTokenPermissions(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	info, err := os.Stat(fx.srv.Socket())
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
	tokInfo, err := os.Stat(TokenPath(fx.dir))
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if tokInfo.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o, want 600", tokInfo.Mode().Perm())
	}
}

func TestSessionClosedOnDisconnect(t *testing.T) {
	fx := newFixture(t, &fakeFactory{})
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t, c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID
	if fx.srv.liveSessionCount() != 1 {
		t.Fatalf("live sessions = %d, want 1", fx.srv.liveSessionCount())
	}
	c.nc.Close()
	deadline := time.Now().Add(3 * time.Second)
	for fx.srv.liveSessionCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session %s still live after disconnect", sid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
