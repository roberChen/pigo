// This file tests the client against a real host server over a real unix
// socket: the pair is what a UI actually uses, and the interesting failures
// (event demultiplexing while a prompt is outstanding, cancel, resume, host
// shutdown) only show up end to end.
package hostclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostpath"
	"github.com/smallnest/pigo/internal/hostserver"
	"github.com/smallnest/pigo/internal/runtime"
)

// scriptedAgent replays a fixed reply in two deltas and honours cancellation.
type scriptedAgent struct {
	mu       sync.Mutex
	spec     hostserver.AgentSpec
	messages agentcore.MessageList
	reply    string
	delay    time.Duration
	// tool, when set, makes Run ask the host's permission gate for that tool
	// before answering, so the permission round trip runs through the real seam.
	tool string
}

func (a *scriptedAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	a.mu.Lock()
	if prompt != "" {
		a.messages = append(a.messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	reply, delay := a.reply, a.delay
	tool := a.tool
	spec := a.spec
	a.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if tool != "" {
		if spec.BeforeToolCall == nil {
			return nil, errors.New("host did not install the permission gate")
		}
		dec := spec.BeforeToolCall(ctx, agentcore.AgentToolCall{Name: tool, Arguments: json.RawMessage(`{"command":"ls"}`)})
		switch {
		case dec == nil:
			reply = "allowed"
		case dec.Block && dec.Content != nil:
			reply = agentcore.ContentToText(*dec.Content)
		default:
			reply = "blocked"
		}
	}

	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	half := len(reply) / 2
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

func (a *scriptedAgent) Messages() agentcore.MessageList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(agentcore.MessageList(nil), a.messages...)
}

func (a *scriptedAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *scriptedAgent) Model() string        { return a.spec.Model }
func (a *scriptedAgent) Provider() string     { return "scripted" }
func (a *scriptedAgent) Close()               {}

type scriptedFactory struct {
	reply string
	delay time.Duration
	tool  string
}

func (f *scriptedFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = "scripted system prompt"
	}
	return &scriptedAgent{spec: spec, messages: append(agentcore.MessageList(nil), spec.Messages...), reply: f.reply, delay: f.delay, tool: f.tool}, nil
}

// startHost runs a server rooted at a temp PIGO_HOME and returns its state dir.
// Tests that need the server handle itself use startHostWithServer
// (client_edge_test.go), which this wraps.
func startHost(t *testing.T, f hostserver.Factory) string {
	t.Helper()
	dir, _ := startHostWithServer(t, f)
	return dir
}

func connect(t *testing.T, dir string) *Client {
	t.Helper()
	c, err := Connect(Options{Dir: dir, Client: "test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestConnectRefusesWithoutHost(t *testing.T) {
	dir := t.TempDir()
	_, err := Connect(Options{Dir: dir})
	var notRunning ErrNotRunning
	if !errors.As(err, &notRunning) {
		t.Fatalf("connect error = %v, want ErrNotRunning", err)
	}
}

func TestConnectRejectsWrongToken(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	if _, err := Connect(Options{Dir: dir, Token: "bogus"}); err == nil {
		t.Fatal("connect with a wrong token succeeded")
	}
}

func TestPromptStreamsEventsToSubscriber(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "streamed answer"})
	c := connect(t, dir)

	var mu sync.Mutex
	var deltas []string
	unsubscribe := c.OnEvent(hostapi.EventSession, func(env hostapi.Envelope) {
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil {
			return
		}
		if ev.Type != hostapi.SessionEventTextDelta {
			return
		}
		mu.Lock()
		deltas = append(deltas, ev.Text)
		mu.Unlock()
	})
	defer unsubscribe()

	cwd := t.TempDir()
	info, err := c.CreateSession(cwd, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.Cwd != cwd || info.Model != "scripted-model" {
		t.Fatalf("session info = %+v", info)
	}

	res, err := c.Prompt(context.Background(), info.ID, "question")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.Text != "streamed answer" || res.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("prompt result = %+v", res)
	}
	// Callbacks run on the dispatch goroutine, so the turn's events are queued
	// when Prompt returns but may not have been delivered yet: poll, never read
	// once and assume.
	waitFor(t, "the turn's two deltas", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(deltas) == 2
	})
	mu.Lock()
	got := append([]string(nil), deltas...)
	mu.Unlock()
	if got[0]+got[1] != "streamed answer" {
		t.Fatalf("deltas = %q", got)
	}

	// After unsubscribing, nothing more is delivered.
	unsubscribe()
	mu.Lock()
	before := len(deltas)
	mu.Unlock()
	if _, err := c.Prompt(context.Background(), info.ID, "again"); err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	mu.Lock()
	after := len(deltas)
	mu.Unlock()
	if after != before {
		t.Fatalf("unsubscribed callback still fired (%d -> %d)", before, after)
	}
}

func TestCancelStopsTurnAndReportsAborted(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "slow", delay: 10 * time.Second})
	c := connect(t, dir)
	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Wait for the turn to actually be in flight, then cancel.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			sessions, err := c.ListSessions()
			if err == nil && len(sessions) == 1 && sessions[0].Running {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	res, err := c.Prompt(ctx, sid.ID, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prompt error = %v, want context.Canceled", err)
	}
	// The abort is reported in-band: the caller keeps the turn's outcome (the
	// stop reason, and whatever text the host managed to produce) even though
	// its own context is what triggered the cancel.
	if res.StopReason != agentcore.StopReasonAborted {
		t.Fatalf("cancelled prompt stop reason = %q, want aborted", res.StopReason)
	}
	// The cancel must have reached the host: the session is no longer running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sessions, err := c.ListSessions()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(sessions) == 1 && !sessions[0].Running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session still running after cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResumeFromAnotherClient(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "answer"})
	first := connect(t, dir)
	sid, err := first.CreateSession(t.TempDir(), "", "title")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := first.Prompt(context.Background(), sid.ID, "q"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := first.CloseSession(sid.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := connect(t, dir)
	rr, err := second.Resume(sid.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(rr.Messages) != 2 {
		t.Fatalf("resumed %d messages, want 2", len(rr.Messages))
	}
	if rr.Session.Cwd != sid.Cwd {
		t.Fatalf("resumed cwd = %q, want %q", rr.Session.Cwd, sid.Cwd)
	}
	// A session owned by another connection cannot be resumed while it is live.
	shared := connect(t, dir)
	live, err := shared.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := second.Resume(live.ID); err == nil {
		t.Fatal("resuming another connection's live session succeeded")
	}
}

func TestPermissionRoundTripThroughClient(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x", tool: "bash"})
	c := connect(t, dir)
	cwd := t.TempDir() // untrusted: the fixture's trust store is empty

	answered := make(chan string, 1)
	c.OnEvent(hostapi.EventPermission, func(env hostapi.Envelope) {
		var req hostapi.PermissionRequest
		if err := env.DecodeParams(&req); err != nil {
			return
		}
		answered <- req.ToolName
		// Answer from the event callback: the client must be able to write while
		// its read loop is delivering, or a permission prompt would deadlock.
		if err := c.Decide(req.SessionID, req.RequestID, true, true); err != nil {
			t.Errorf("decide: %v", err)
		}
	})

	sid, err := c.CreateSession(cwd, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := c.Prompt(context.Background(), sid.ID, "run something")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.Text != "allowed" {
		t.Fatalf("prompt text = %q, want the tool to have been allowed", res.Text)
	}
	select {
	case tool := <-answered:
		if tool != "bash" {
			t.Fatalf("permission request named tool %q", tool)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no permission request reached the client")
	}
}

func TestPermissionDeniedWhenClientRefuses(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x", tool: "write"})
	c := connect(t, dir)
	c.OnEvent(hostapi.EventPermission, func(env hostapi.Envelope) {
		var req hostapi.PermissionRequest
		if err := env.DecodeParams(&req); err != nil {
			return
		}
		if err := c.Decide(req.SessionID, req.RequestID, false, false); err != nil {
			t.Errorf("decide: %v", err)
		}
	})
	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := c.Prompt(context.Background(), sid.ID, "write a file")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(res.Text, "not trusted") {
		t.Fatalf("denial text = %q", res.Text)
	}
}

func TestShutdownEventWakesClient(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	closed := make(chan hostapi.ServerShutdownEvent, 1)
	c.OnEvent(hostapi.EventServerShutdown, func(env hostapi.Envelope) {
		var ev hostapi.ServerShutdownEvent
		if err := env.DecodeParams(&ev); err == nil {
			closed <- ev
		}
	})
	if err := c.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case ev := <-closed:
		if ev.Reason == "" {
			t.Fatal("shutdown event has no reason")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no shutdown event")
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("connection did not close after shutdown")
	}
}

func TestProtocolErrorIsTyped(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	c := connect(t, dir)
	err := c.CloseSession("does-not-exist")
	var protoErr *hostapi.Error
	if !errors.As(err, &protoErr) {
		t.Fatalf("error = %v, want *hostapi.Error", err)
	}
	if protoErr.Code != hostapi.CodeForbidden && protoErr.Code != hostapi.CodeNotFound {
		t.Fatalf("error code = %d", protoErr.Code)
	}
	if _, err := c.Resume("does-not-exist"); !errors.As(err, &protoErr) || protoErr.Code != hostapi.CodeNotFound {
		t.Fatalf("resume error = %v", err)
	}
}

func TestSocketPathHandlesLongHome(t *testing.T) {
	long := "/tmp/" + string(make([]byte, 0)) + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := hostpath.SocketPath(long)
	if len(path) > 100 {
		t.Fatalf("socket path %q is %d bytes", path, len(path))
	}
	if path == hostpath.SocketPath("/tmp/short") {
		t.Fatal("different dirs produced the same hashed socket path")
	}
}

func TestStateFileIsReadableWithoutConnecting(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "x"})
	st, err := hostpath.ReadState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.PID == 0 || st.Socket == "" {
		t.Fatalf("state = %+v", st)
	}
	if !hostpath.Probe(st.Socket, time.Second) {
		t.Fatal("probe says the host is not listening")
	}
}
