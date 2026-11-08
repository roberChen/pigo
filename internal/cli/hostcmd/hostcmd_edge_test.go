// This file tests the edge cases of the `pigo serve` / `pigo attach` CLI layer:
// serve's dispatch and flag handling (default/relative/missing --dir, an
// unresolvable state directory, a host that died without cleanup), attach's
// input handling (stdin EOF, blank lines, unknown slash commands, a host dying
// mid-turn, permission answers typed at the prompt) and the renderer's output
// event by event.
//
// It complements hostcmd_test.go: that file walks the happy path end to end,
// this one pins the branches a user only hits when something is unusual. The
// scripted agent, the temp trust store and the `t.Setenv("PIGO_HOME", …)` rule
// are the same, so nothing here reads or writes the developer's real ~/.pigo.
//
// Ctrl-C handling is deliberately not covered: attach only installs the signal
// handler when stdin is os.Stdin, and a buffer or pipe (the only thing a test can
// drive) never is.
//
// The argv-level usage errors that live beside the flag sets (an invalid --cwd,
// --list with a session id, extra positional arguments) are covered in
// cmd/pigo/hostcmd_args_test.go: they never reach this package.
package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostpath"
	"github.com/smallnest/pigo/internal/hostserver"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// ---- helpers ----------------------------------------------------------

// syncBuffer is a bytes.Buffer whose contents may be read while the attacher and
// the client's dispatcher goroutine are still writing to it. A bare bytes.Buffer
// would be a data race here, and these tests have to poll the transcript to know
// when the loop has reached a given prompt.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForOutput blocks until buf contains substr. It replaces a fixed sleep: the
// interesting transitions (a permission question, a shutdown notice) are facts
// about the transcript, not about the clock.
func waitForOutput(t *testing.T, buf *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(buf.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; output so far: %q", substr, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// turnRecorder gives a scripted agent the message bookkeeping the host persists.
// Without it the host writes no session file (appendTail sees an empty tail), so
// the session could not be resumed after a detach — which several tests assert.
type turnRecorder struct {
	mu   sync.Mutex
	msgs agentcore.MessageList
}

func (r *turnRecorder) record(prompt string, msg agentcore.AssistantMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prompt != "" {
		r.msgs = append(r.msgs, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	r.msgs = append(r.msgs, msg)
}

func (r *turnRecorder) Messages() agentcore.MessageList {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(agentcore.MessageList(nil), r.msgs...)
}

// awaitSocket waits until a host is listening on dir.
func awaitSocket(t *testing.T, dir string) {
	t.Helper()
	socket := hostpath.SocketPath(dir)
	deadline := time.Now().Add(10 * time.Second)
	for !hostpath.Probe(socket, 50*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("host never listened on %s", socket)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// backgroundServe is one in-process `pigo serve` run started by a test.
type backgroundServe struct {
	dir         string
	out, errOut *syncBuffer
	finished    chan struct{}
	code        int
}

// serveInBackground runs `pigo serve` in this process. Cleanup stops the host the
// way a user would, so a failing assertion cannot leave a listener (or its socket)
// behind; a test that stops it itself can just wait.
func serveInBackground(t *testing.T, opts ServeOptions) *backgroundServe {
	t.Helper()
	s := &backgroundServe{
		dir:      opts.Dir,
		out:      &syncBuffer{},
		errOut:   &syncBuffer{},
		finished: make(chan struct{}),
	}
	go func() {
		defer close(s.finished)
		s.code = Run(nil, opts, s.out, s.errOut)
	}()
	t.Cleanup(func() {
		select {
		case <-s.finished:
			return
		default:
		}
		var so, se bytes.Buffer
		Run([]string{"stop"}, ServeOptions{Dir: s.dir}, &so, &se)
		select {
		case <-s.finished:
		case <-time.After(10 * time.Second):
			t.Errorf("serve did not exit after stop: %s", s.errOut.String())
		}
	})
	return s
}

// wait blocks until the serve call returns.
func (s *backgroundServe) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-s.finished:
		return s.code
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not exit: %s", s.errOut.String())
		return -1
	}
}

// runningHost is startHost with the server handed back, for the tests that must
// take the host down at a chosen moment instead of waiting for cleanup.
func runningHost(t *testing.T, dir string, f hostserver.Factory, store *session.Store) *hostserver.Server {
	t.Helper()
	if store == nil {
		s, err := session.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("session store: %v", err)
		}
		store = s
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
	go func() { _ = srv.Serve(context.Background()) }()
	t.Cleanup(func() { srv.Shutdown("test cleanup") })
	return srv
}

// leftoverSocket leaves a bound-then-closed unix socket behind, which is the
// inode a SIGKILLed host leaves: the socket path exists but nothing accepts on it.
func leftoverSocket(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	socket := hostpath.SocketPath(dir)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	// Without this a closed listener unlinks its path, which would simulate a
	// clean shutdown rather than a crash.
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return socket
}

// attachPipe drives Attach over a pipe so the test decides when each line
// arrives. A strings.Reader delivers every line before the first turn starts,
// which is exactly the wrong shape for "the user types while a prompt is up".
type attachPipe struct {
	pw          *io.PipeWriter
	out, errOut *syncBuffer
	finished    chan struct{}
	code        int
}

func startPipeAttach(t *testing.T, dir string, mutate func(*AttachOptions)) *attachPipe {
	t.Helper()
	pr, pw := io.Pipe()
	p := &attachPipe{pw: pw, out: &syncBuffer{}, errOut: &syncBuffer{}, finished: make(chan struct{})}
	opts := AttachOptions{Dir: dir}
	if mutate != nil {
		mutate(&opts)
	}
	go func() {
		defer close(p.finished)
		p.code = Attach(opts, pr, p.out, p.errOut)
	}()
	t.Cleanup(func() {
		_ = pw.Close()
		select {
		case <-p.finished:
		case <-time.After(10 * time.Second):
			t.Errorf("attach did not return; output=%q stderr=%q", p.out.String(), p.errOut.String())
		}
	})
	return p
}

func (p *attachPipe) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(p.pw, line+"\n"); err != nil {
		t.Fatalf("write attach stdin: %v", err)
	}
}

func (p *attachPipe) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.finished:
		return p.code
	case <-time.After(10 * time.Second):
		t.Fatalf("attach hung; output=%q stderr=%q", p.out.String(), p.errOut.String())
		return -1
	}
}

// echoFactory replies with the prompt it was handed, which is how the tests prove
// what attach actually forwarded to the host.
type echoFactory struct{}

func (echoFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "echo-model"
	}
	return &echoAgent{spec: spec, turnRecorder: &turnRecorder{}}, nil
}

type echoAgent struct {
	spec hostserver.AgentSpec
	*turnRecorder
}

func (a *echoAgent) Run(_ context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent("echo:" + prompt)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	a.record(prompt, msg)
	if h.OnText != nil {
		h.OnText("echo:" + prompt)
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
	}
	return &msg, nil
}

func (a *echoAgent) SystemPrompt() string { return "echo system prompt" }
func (a *echoAgent) Model() string        { return a.spec.Model }
func (a *echoAgent) Provider() string     { return "scripted" }
func (a *echoAgent) Close()               {}

// gateFactory consults the host's permission gate calls times in a row, which is
// how "always" is proven to cover the later calls in the same session: only the
// first may raise a question. The decisions travel back through the reply text.
type gateFactory struct {
	tool  string
	calls int
}

func (f *gateFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	return &gateAgent{spec: spec, tool: f.tool, calls: f.calls, turnRecorder: &turnRecorder{}}, nil
}

type gateAgent struct {
	spec  hostserver.AgentSpec
	tool  string
	calls int
	*turnRecorder
}

func (a *gateAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	decisions := make([]string, 0, a.calls)
	for i := 0; i < a.calls; i++ {
		dec := a.spec.BeforeToolCall(ctx, agentcore.AgentToolCall{
			Name:      a.tool,
			Arguments: json.RawMessage(`{"command":"ls"}`),
		})
		if dec == nil {
			decisions = append(decisions, "allowed")
			continue
		}
		// A denial ends the sequence: the model would not retry the same call
		// blindly, and the test wants to see the first refusal.
		decisions = append(decisions, "blocked")
		break
	}
	reply := "gates:" + strings.Join(decisions, ",")
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	a.record(prompt, msg)
	a.record(prompt, msg)
	if h.OnText != nil {
		h.OnText(reply)
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
	}
	return &msg, nil
}

func (a *gateAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *gateAgent) Model() string        { return a.spec.Model }
func (a *gateAgent) Provider() string     { return "scripted" }
func (a *gateAgent) Close()               {}

// endingFactory ends a turn with a chosen stop reason, so the tail `attach` prints
// for a turn that failed in-band (or hit the output cap) is covered.
type endingFactory struct {
	reply      string
	stopReason string
	errMsg     string
}

func (f *endingFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	return &endingAgent{spec: spec, turnRecorder: &turnRecorder{}, reply: f.reply, stopReason: f.stopReason, errMsg: f.errMsg}, nil
}

type endingAgent struct {
	spec hostserver.AgentSpec
	*turnRecorder
	reply      string
	stopReason string
	errMsg     string
}

func (a *endingAgent) Run(_ context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	msg := agentcore.AssistantMessage{
		RoleField:    agentcore.RoleAssistant,
		Content:      agentcore.ContentList{agentcore.NewTextContent(a.reply)},
		StopReason:   a.stopReason,
		ErrorMessage: a.errMsg,
	}
	a.record(prompt, msg)
	if h.OnText != nil {
		h.OnText(a.reply)
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
	}
	return &msg, nil
}

func (a *endingAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *endingAgent) Model() string        { return a.spec.Model }
func (a *endingAgent) Provider() string     { return "scripted" }
func (a *endingAgent) Close()               {}

// chatterFactory emits the event kinds a terminal user sees around the text —
// tool lifecycle, compaction, sub-agent progress — through the real host, so the
// runtime-event → wire → renderer path is covered rather than render() alone.
// The order mirrors the loop: a turn ends before compaction runs.
type chatterFactory struct{}

func (chatterFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	return &chatterAgent{spec: spec, turnRecorder: &turnRecorder{}}, nil
}

type chatterAgent struct {
	spec hostserver.AgentSpec
	*turnRecorder
}

func (a *chatterAgent) Run(_ context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	emit := func(ev agentcore.AgentEvent) {
		if h.OnEvent != nil {
			h.OnEvent(ev)
		}
	}
	text := func(s string) {
		if h.OnText != nil {
			h.OnText(s)
		}
	}
	// Text without a trailing newline, then a tool: the renderer must break the
	// line itself or the marker would be glued to the reply.
	text("checking")
	emit(agentcore.ToolExecutionStartEvent{ToolCallID: "t1", ToolName: "read", Args: map[string]any{"path": "a.go"}})
	emit(agentcore.ToolExecutionUpdateEvent{
		ToolCallID:    "t1",
		ToolName:      "read",
		PartialResult: agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("PARTIAL-RESULT-TEXT")}},
	})
	emit(agentcore.ToolExecutionEndEvent{
		ToolCallID: "t1",
		ToolName:   "read",
		Result:     agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	text("done")
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent("checkingdone")},
		StopReason: agentcore.StopReasonEndTurn,
	}
	emit(agentcore.TurnEndEvent{Message: msg})
	emit(agentcore.CompactionEvent{Reason: "threshold", TokensBefore: 1000, TokensAfter: 250, SummarizedCount: 3, KeptCount: 2})
	emit(agentcore.SubAgentProgressEvent{ToolCallID: "s1", Description: "SUBAGENT-DESC", Activity: "Running bash"})
	emit(agentcore.SubAgentEndEvent{ToolCallID: "s1"})
	a.record(prompt, msg)
	return &msg, nil
}

func (a *chatterAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *chatterAgent) Model() string        { return a.spec.Model }
func (a *chatterAgent) Provider() string     { return "scripted" }
func (a *chatterAgent) Close()               {}

// startedFactory wraps another factory and reports when a turn actually begins,
// so "kill the host mid-turn" is a fact rather than a sleep.
type startedFactory struct {
	inner   hostserver.Factory
	started chan struct{}
}

func (f *startedFactory) NewAgent(ctx context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	a, err := f.inner.NewAgent(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &startedAgent{Agent: a, started: f.started}, nil
}

type startedAgent struct {
	hostserver.Agent
	started chan struct{}
	once    sync.Once
}

func (a *startedAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	a.once.Do(func() { close(a.started) })
	return a.Agent.Run(ctx, prompt, h)
}

// sessionEventEnvelope builds the notification the host would push for ev, so the
// renderer can be driven without a socket.
func sessionEventEnvelope(t *testing.T, ev hostapi.SessionEvent) hostapi.Envelope {
	t.Helper()
	params, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal session event: %v", err)
	}
	return hostapi.Envelope{Event: hostapi.EventSession, Params: params}
}

// renderEvents feeds events through a fresh attacher and returns the transcript.
func renderEvents(t *testing.T, evs ...hostapi.SessionEvent) string {
	t.Helper()
	var out bytes.Buffer
	a := newAttacher(nil, &out, io.Discard)
	for _, ev := range evs {
		a.render(sessionEventEnvelope(t, ev))
	}
	return out.String()
}

// ---- serve: dispatch --------------------------------------------------

// TestServeDispatchActionEdges pins the parts of the dispatch contract the happy
// path does not: the message names the offending action *and* the accepted set, a
// misspelled action (wrong case) is not silently accepted, and anything after the
// action word is ignored rather than treated as a second action.
func TestServeDispatchActionEdges(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer

	if code := Run([]string{"Status"}, ServeOptions{Dir: dir}, &out, &errOut); code != 2 {
		t.Fatalf("`serve Status` exit = %d, want 2 (actions are lowercase)", code)
	}
	if out.Len() != 0 {
		t.Fatalf("dispatch error wrote to stdout: %q", out.String())
	}
	msg := errOut.String()
	for _, want := range []string{`"Status"`, "start", "status", "stop"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("dispatch error %q does not mention %q", msg, want)
		}
	}

	// A trailing word after a known action is not a second action: `serve status
	// extra` behaves like `serve status` (which reports no host here).
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status", "extra"}, ServeOptions{Dir: dir}, &out, &errOut); code != 1 {
		t.Fatalf("`serve status extra` exit = %d, want 1 (no host, not a dispatch error)", code)
	}
	if !strings.Contains(out.String(), "no pigo host is running") {
		t.Fatalf("`serve status extra` = %q, want the no-host message", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("`serve status extra` wrote to stderr: %q", errOut.String())
	}
}

// TestServeStopWithoutHost covers the missing half of the no-host contract:
// `stop` must fail loudly (exit 1) on stderr, and must not create a state dir.
func TestServeStopWithoutHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-started")
	var out, errOut bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no running host") {
		t.Fatalf("stderr = %q, want it to say there is no running host", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("failed stop wrote to stdout: %q", out.String())
	}
	// `stop` is a client operation: it must not fabricate a state directory that
	// would make the next `status` look like a crashed host.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stop created %s (stat error %v)", dir, err)
	}
}

// TestServeStatusAfterHostDiesWithoutCleanup is the crash variant of the stale
// state file: the socket *path* still exists (a killed host cannot unlink it), but
// nothing accepts, so `status` must report the host as unresponsive and point at
// `serve stop` rather than claiming it is up or down.
func TestServeStatusAfterHostDiesWithoutCleanup(t *testing.T) {
	dir := t.TempDir()
	socket := leftoverSocket(t, dir)
	if err := hostpath.WriteToken(dir, "tok"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	stale := hostpath.State{PID: 999999, Version: hostapi.Version, StartedAt: time.Now().Add(-time.Minute), Socket: socket}
	if err := hostpath.WriteState(dir, stale); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, ServeOptions{Dir: dir}, &out, &errOut); code != 1 {
		t.Fatalf("status exit = %d, want 1: %q", code, out.String())
	}
	msg := out.String()
	if !strings.Contains(msg, "not responding") || !strings.Contains(msg, "999999") {
		t.Fatalf("status = %q, want it to name the dead pid and say it is not responding", msg)
	}
	if !strings.Contains(msg, "pigo serve stop") {
		t.Fatalf("status = %q, want it to suggest `pigo serve stop`", msg)
	}

	// `stop` cannot clear a host it cannot reach: it fails, and it leaves the
	// leftover files alone instead of silently deleting evidence.
	var stopOut, stopErr bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &stopOut, &stopErr); code != 1 {
		t.Fatalf("stop against a dead host exit = %d, want 1", code)
	}
	if _, err := os.Stat(hostpath.StatePath(dir)); err != nil {
		t.Fatalf("stop removed the stale state file: %v", err)
	}
}

// TestServeStartRecoversFromStaleSocket checks the recovery half: a host killed
// without cleanup must not block the next `serve`, which removes the stale socket
// and republishes a state file naming the new pid.
func TestServeStartRecoversFromStaleSocket(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	dir := t.TempDir()
	socket := leftoverSocket(t, dir)
	if err := hostpath.WriteToken(dir, "stale-token"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if err := hostpath.WriteState(dir, hostpath.State{PID: 999999, Version: hostapi.Version, StartedAt: time.Now(), Socket: socket}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	serve := serveInBackground(t, ServeOptions{Dir: dir, Build: "test", Factory: &scriptedFactory{reply: "ok"}})
	awaitSocket(t, dir)
	waitForOutput(t, serve.out, "listening on")

	state, err := hostpath.ReadState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.PID == 999999 || state.PID != os.Getpid() {
		t.Fatalf("state pid = %d, want this process (%d) after recovery", state.PID, os.Getpid())
	}
	if tok, err := hostpath.ReadToken(dir); err != nil || tok == "stale-token" {
		t.Fatalf("token = %q (%v), want it rotated by the new host", tok, err)
	}

	var statusOut, statusErr bytes.Buffer
	if code := Run([]string{"status"}, ServeOptions{Dir: dir}, &statusOut, &statusErr); code != 0 {
		t.Fatalf("status after recovery exit = %d (%s)", code, statusErr.String())
	}
	if !strings.Contains(statusOut.String(), "live session(s)") {
		t.Fatalf("status = %q, want the live host", statusOut.String())
	}

	var so, se bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &so, &se); code != 0 {
		t.Fatalf("stop exit = %d (%s)", code, se.String())
	}
	if code := serve.wait(t); code != 0 {
		t.Fatalf("serve exited %d: %s", code, serve.errOut.String())
	}
}

// TestServeWithoutResolvableStateDir covers the guard for an environment that
// cannot name a state directory: `serve` must refuse to start, and `status` must
// report no host rather than crashing on an empty path.
func TestServeWithoutResolvableStateDir(t *testing.T) {
	// PIGO_HOME unset *and* no home directory: hostpath.DefaultDir() then returns
	// "" and every command has to degrade instead of panicking.
	t.Setenv("PIGO_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	var out, errOut bytes.Buffer
	if code := Run(nil, ServeOptions{Factory: &scriptedFactory{reply: "x"}}, &out, &errOut); code != 1 {
		t.Fatalf("serve start exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "cannot resolve the state directory") {
		t.Fatalf("stderr = %q, want the state-directory diagnostic", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status"}, ServeOptions{}, &out, &errOut); code != 1 {
		t.Fatalf("status exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "no pigo host is running") {
		t.Fatalf("status = %q, want the no-host message", out.String())
	}
}

// ---- serve: --dir handling --------------------------------------------

// TestServeDirDefaultsToPigoHome verifies the documented default: with no --dir
// the socket, token and state file live directly in $PIGO_HOME, and status/stop
// (which take the same default) find them there.
func TestServeDirDefaultsToPigoHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)

	serve := serveInBackground(t, ServeOptions{Factory: &scriptedFactory{reply: "ok"}})
	awaitSocket(t, home)
	waitForOutput(t, serve.out, "listening on")

	for _, p := range []string{hostpath.SocketPath(home), hostpath.TokenPath(home), hostpath.StatePath(home)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing for the default state dir: %v", p, err)
		}
	}
	if !strings.Contains(serve.out.String(), hostpath.SocketPath(home)) {
		t.Fatalf("startup line %q does not name the default socket %q", serve.out.String(), hostpath.SocketPath(home))
	}

	var statusOut, statusErr bytes.Buffer
	if code := Run([]string{"status"}, ServeOptions{}, &statusOut, &statusErr); code != 0 {
		t.Fatalf("status exit = %d (%s)", code, statusErr.String())
	}
	if !strings.Contains(statusOut.String(), "live session(s)") {
		t.Fatalf("status = %q, want the live host", statusOut.String())
	}

	var so, se bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{}, &so, &se); code != 0 {
		t.Fatalf("stop exit = %d (%s)", code, se.String())
	}
	if code := serve.wait(t); code != 0 {
		t.Fatalf("serve exited %d: %s", code, serve.errOut.String())
	}
}

// TestServeRelativeDir pins that --dir is used as given: a relative path resolves
// against the process working directory for both the server and a later client,
// which is what makes `pigo serve --dir run/host` usable.
func TestServeRelativeDir(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	base := t.TempDir()
	t.Chdir(base)

	serve := serveInBackground(t, ServeOptions{Dir: "run/host", Build: "test", Factory: &scriptedFactory{reply: "ok"}})
	awaitSocket(t, "run/host")
	waitForOutput(t, serve.out, "listening on")

	abs := filepath.Join(base, "run", "host")
	for _, p := range []string{hostpath.SocketPath(abs), hostpath.TokenPath(abs), hostpath.StatePath(abs)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing for a relative --dir: %v", p, err)
		}
	}

	var so, se bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: "run/host"}, &so, &se); code != 0 {
		t.Fatalf("stop with a relative --dir exit = %d (%s)", code, se.String())
	}
	if code := serve.wait(t); code != 0 {
		t.Fatalf("serve exited %d: %s", code, serve.errOut.String())
	}
}

// TestServeDirCreatedWhenMissing documents the real behaviour of a --dir that does
// not exist yet: Listen creates it (0700) instead of failing, so a first run needs
// no mkdir.
func TestServeDirCreatedWhenMissing(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "nested", "host-state")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("test setup: %s already exists", dir)
	}

	serve := serveInBackground(t, ServeOptions{Dir: dir, Build: "test", Factory: &scriptedFactory{reply: "ok"}})
	awaitSocket(t, dir)
	waitForOutput(t, serve.out, "listening on")

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("--dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir mode = %o, want 700 (the token lives here)", perm)
	}
	if _, err := os.Stat(hostpath.SocketPath(dir)); err != nil {
		t.Fatalf("socket missing in the created dir: %v", err)
	}

	var so, se bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &so, &se); code != 0 {
		t.Fatalf("stop exit = %d (%s)", code, se.String())
	}
	if code := serve.wait(t); code != 0 {
		t.Fatalf("serve exited %d: %s", code, serve.errOut.String())
	}
}

// ---- attach: input handling -------------------------------------------

// TestAttachImmediateEOFDetaches is the Ctrl-D-at-an-empty-prompt case: no line at
// all must detach with exit 0 (and the "> " prompt must not loop forever). A
// session that never ran a turn writes no file, so resuming it is expected to fail
// — the detach itself is what this test pins.
func TestAttachImmediateEOFDetaches(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "never used"})

	out, errOut := &syncBuffer{}, &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Attach(AttachOptions{Dir: dir}, strings.NewReader(""), out, errOut)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("attach hung on immediate EOF; stdout=%q stderr=%q", out.String(), errOut.String())
	}
	stdout := out.String()
	if !strings.Contains(stdout, "detached from ") {
		t.Fatalf("output = %q, want a detach line", stdout)
	}
	if strings.Contains(stdout, "never used") {
		t.Fatalf("no turn was prompted but the reply was rendered: %q", stdout)
	}
	if got := strings.Count(stdout, "\n> "); got != 1 {
		t.Fatalf("prompt printed %d times, want 1 (EOF must not re-prompt): %q", got, stdout)
	}
	id := sessionIDFrom(t, stdout)
	_, resumeErr, code := attachAndRead(t, dir, "/exit\n", func(o *AttachOptions) { o.SessionID = id })
	if code != 1 || !strings.Contains(resumeErr, "no such session") {
		t.Fatalf("resuming a turn-less session = (%d, %q), want exit 1 and 'no such session'", code, resumeErr)
	}
}

// TestAttachIgnoresBlankLines checks that whitespace-only input never becomes a
// prompt: an empty turn would burn a model call and (worse) would look to the user
// like the client swallowed their input.
func TestAttachIgnoresBlankLines(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "REPLY-MARKER"})

	stdout, stderr, code := attachAndRead(t, dir, "\n   \n\t\n \t \n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "REPLY-MARKER") {
		t.Fatalf("a blank line started a turn: %q", stdout)
	}
	// One prompt per line, all four blanks plus /exit.
	if got := strings.Count(stdout, "\n> "); got != 5 {
		t.Fatalf("prompt count = %d, want 5 (one per input line): %q", got, stdout)
	}
	if !strings.Contains(stdout, "detached from ") {
		t.Fatalf("output = %q, want a detach line", stdout)
	}
}

// TestAttachExitCommandVersusEOF pins the three ways out: /exit and /quit detach
// immediately, EOF detaches at end of input, and all three keep the session on
// disk (a detach is not a delete).
func TestAttachExitCommandVersusEOF(t *testing.T) {
	dir := t.TempDir()
	host := runningHost(t, dir, &echoFactory{}, nil)

	cases := []struct {
		name  string
		stdin string
		// wantTurn reports whether the input should have produced a reply.
		wantTurn bool
	}{
		{name: "exit-command", stdin: "hi\n/exit\n", wantTurn: true},
		{name: "quit-alias", stdin: "hi\n/quit\n", wantTurn: true},
		{name: "eof", stdin: "hi\n", wantTurn: true},
		{name: "eof-without-turn", stdin: "", wantTurn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := attachAndRead(t, host.Dir(), tc.stdin, nil)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, errOut)
			}
			if got := strings.Contains(out, "echo:hi"); got != tc.wantTurn {
				t.Fatalf("turn rendered = %v, want %v: %q", got, tc.wantTurn, out)
			}
			if !strings.Contains(out, "detached from ") {
				t.Fatalf("output = %q, want a detach line", out)
			}
			id := sessionIDFrom(t, out)
			if !tc.wantTurn {
				// Nothing was persisted, so there is nothing to resume.
				return
			}
			// A turn ran, so the detach must not have discarded the history: a new
			// connection can resume it and sees the prompt and the reply.
			resumeOut, resumeErr, rcode := attachAndRead(t, host.Dir(), "/exit\n", func(o *AttachOptions) { o.SessionID = id })
			if rcode != 0 {
				t.Fatalf("resume %s after detach exit = %d (%s)", id, rcode, resumeErr)
			}
			if !strings.Contains(resumeOut, "resumed "+id) {
				t.Fatalf("session %s did not survive the detach: %q", id, resumeOut)
			}
			if !strings.Contains(resumeOut, "2 messages") {
				t.Fatalf("resume %s did not replay the turn: %q", id, resumeOut)
			}
		})
	}
}

// TestAttachPassesUnknownSlashCommandAsPrompt confirms the client only intercepts
// the three commands it implements (/exit, /quit, /sessions): everything else is
// prompt text, so `/model` or a future command still reaches the model instead of
// silently vanishing.
func TestAttachPassesUnknownSlashCommandAsPrompt(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &echoFactory{})

	stdout, stderr, code := attachAndRead(t, dir, "/frobnicate with args\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "echo:/frobnicate with args") {
		t.Fatalf("unknown slash command was not forwarded as the prompt: %q", stdout)
	}

	// /sessions is handled locally and must never reach the agent.
	out2, err2, code2 := attachAndRead(t, dir, "/sessions\n/exit\n", nil)
	if code2 != 0 {
		t.Fatalf("exit = %d, stderr = %q", code2, err2)
	}
	if strings.Contains(out2, "echo:/sessions") {
		t.Fatalf("/sessions was forwarded to the model: %q", out2)
	}
	if !strings.Contains(out2, "echo-model") || !strings.Contains(out2, sessionIDFrom(t, out2)) {
		t.Fatalf("/sessions did not list the connection's session: %q", out2)
	}
}

// TestAttachListWithNoSessions covers the empty case of --list on a host where this
// connection has created nothing: it must exit 0 with a message, and it must not
// create a session just to list them.
func TestAttachListWithNoSessions(t *testing.T) {
	dir := t.TempDir()
	storeDir := t.TempDir()
	store, err := session.NewStore(storeDir)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	startHost(t, dir, &scriptedFactory{reply: "x"}, func(c *hostserver.Config) { c.Store = store })

	out, errOut, code := attachAndRead(t, dir, "", func(o *AttachOptions) { o.List = true })
	if code != 0 {
		t.Fatalf("--list exit = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "attached to pigo host") {
		t.Fatalf("--list did not print the handshake banner: %q", out)
	}
	if !strings.Contains(out, "no sessions on this connection") {
		t.Fatalf("--list = %q, want the empty-session message", out)
	}
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			t.Fatalf("--list created session %s", e.Name())
		}
	}
	if strings.Contains(out, "detached from ") {
		t.Fatalf("--list kept the attach loop running: %q", out)
	}
}

// TestAttachHostDiesMidTurn shuts the host down while a turn is in flight. The
// client must report the loss, stop prompting, and exit 0 (the session is already
// persisted host-side, so a detach loses nothing) instead of hanging on a socket
// nobody will answer. Which of the two wake-ups reports it — the queued shutdown
// event or the connection error — is a race, so the assertion is on the transcript
// saying the host is gone, not on the exact channel it was reported through.
func TestAttachHostDiesMidTurn(t *testing.T) {
	dir := t.TempDir()
	storeDir := t.TempDir()
	store, err := session.NewStore(storeDir)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	started := make(chan struct{})
	srv := runningHost(t, dir, &startedFactory{
		inner:   &scriptedFactory{reply: "slow reply", delay: 500 * time.Millisecond},
		started: started,
	}, store)

	p := startPipeAttach(t, dir, nil)
	waitForOutput(t, p.out, "session ")
	p.send(t, "please run")

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatalf("the turn never started: %q", p.out.String())
	}
	srv.Shutdown("host died mid-turn")

	code := p.wait(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (a lost host is a detach, not a usage error)", code)
	}
	stdout := p.out.String()
	if !strings.Contains(stdout, "host is gone; detaching") {
		t.Fatalf("transcript = %q, want the shutdown notice and detach", stdout)
	}
	if !strings.Contains(stdout, "detached from ") {
		t.Fatalf("transcript = %q, want a detach line", stdout)
	}
	if strings.Contains(stdout, "slow reply") {
		t.Fatalf("a reply that was never delivered was rendered: %q", stdout)
	}

	// The orphaned turn still finishes host-side and persists itself; waiting for
	// that keeps the store's temp dir quiet while the test's cleanups run.
	deadline := time.Now().Add(10 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(storeDir, "*.jsonl"))
		if len(matches) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the aborted turn was never persisted in %s", storeDir)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAttachShutdownEventStopsTheLoop is the graceful counterpart: the host
// announces server_shutdown before it closes. The renderer must print the reason,
// and the loop must not prompt a host that is going away.
func TestAttachShutdownEventStopsTheLoop(t *testing.T) {
	dir := t.TempDir()
	srv := runningHost(t, dir, &scriptedFactory{reply: "x"}, nil)

	p := startPipeAttach(t, dir, nil)
	waitForOutput(t, p.out, "session ")
	srv.Shutdown("maintenance window")
	waitForOutput(t, p.out, "host is shutting down: maintenance window")

	// stdin is still open: the loop must notice the dead connection by itself
	// instead of waiting at the prompt for input that will never be answered.
	code := p.wait(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(p.out.String(), "host is gone; detaching") {
		t.Fatalf("transcript = %q, want the loop to detach instead of prompting again", p.out.String())
	}
}

// TestAttachStdinReadErrorDetaches covers a line the client cannot read at all (the
// scanner's 1 MiB token cap, i.e. pasting a very large blob). The error is
// reported, and because stdin is unusable from then on the client must detach —
// hanging on a reader nobody will ever write to is the failure this pins.
func TestAttachStdinReadErrorDetaches(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "x"})

	huge := strings.Repeat("x", 1<<20+1024) + "\n/exit\n"
	out, errOut := &syncBuffer{}, &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Attach(AttachOptions{Dir: dir}, strings.NewReader(huge), out, errOut)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("attach hung on a stdin read error; stderr=%q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "read input") {
		t.Fatalf("stderr = %q, want the read error reported", errOut.String())
	}
	if !strings.Contains(out.String(), "detached from ") {
		t.Fatalf("output = %q, want a detach line", out.String())
	}
}

// TestAttachPermissionEOFDuringTurnDenies covers the fail-closed path with no user
// left to ask: the turn asks for permission after stdin already hit EOF. The host
// must be answered (deny) so the turn can settle, and the client must detach
// instead of waiting for input that will never come.
func TestAttachPermissionEOFDuringTurnDenies(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "done", tool: "bash"})

	out, errOut := &syncBuffer{}, &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// EOF immediately after the prompt: the question arrives with no reader.
		Attach(AttachOptions{Dir: dir}, strings.NewReader("run ls\n"), out, errOut)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("attach hung with a permission request and no stdin; stdout=%q stderr=%q", out.String(), errOut.String())
	}
	stdout := out.String()
	if !strings.Contains(stdout, "wants to run outside a trusted directory") {
		t.Fatalf("permission question not shown: %q", stdout)
	}
	if !strings.Contains(stdout, "denied") {
		t.Fatalf("silence did not deny the tool: %q", stdout)
	}
	if strings.Contains(stdout, "allowed") {
		t.Fatalf("a missing answer widened the tool's permission: %q", stdout)
	}
}

// ---- attach: permission interaction ----------------------------------

// TestAttachAnswersPermissionTypedAtPrompt drives the other half of the permission
// handshake: the user types the answer *while the question is on screen* (the
// permSig branch), rather than ahead of it. hostcmd_test.go covers the typed-ahead
// (queued) branch; together they cover both ways a reply can reach the host.
func TestAttachAnswersPermissionTypedAtPrompt(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "done", tool: "bash"})

	p := startPipeAttach(t, dir, nil)
	waitForOutput(t, p.out, "session ")
	p.send(t, "run ls")
	// The question is on screen before the answer is typed, so this exercises the
	// permSig branch (the question arrives first) rather than the queued one.
	waitForOutput(t, p.out, "allow? [y]es / [a]lways / [N]o:")
	p.send(t, "y")
	waitForOutput(t, p.out, "allowed once")
	p.send(t, "/exit")

	if code := p.wait(t); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.Count(p.out.String(), "wants to run outside a trusted directory"); got != 1 {
		t.Fatalf("permission question asked %d times, want 1: %q", got, p.out.String())
	}
}

// TestAttachAlwaysApproveSkipsLaterPrompts proves "a" is session-scoped: the same
// session's next side-effect call must not ask again, and both calls must reach
// the tool gate allowed.
func TestAttachAlwaysApproveSkipsLaterPrompts(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &gateFactory{tool: "bash", calls: 2})

	stdout, stderr, code := attachAndRead(t, dir, "run twice\na\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if got := strings.Count(stdout, "wants to run outside a trusted directory"); got != 1 {
		t.Fatalf("permission asked %d times, want 1 after 'a': %q", got, stdout)
	}
	if !strings.Contains(stdout, "allowed for this session") {
		t.Fatalf("'a' was not reported as always: %q", stdout)
	}
	if !strings.Contains(stdout, "gates:allowed,allowed") {
		t.Fatalf("second call was not allowed: %q", stdout)
	}
}

// TestAttachUnexpectedPermissionInputDenies pins fail-closed parsing: anything
// that is not an explicit yes/always — here a plausible-looking "maybe" — denies.
func TestAttachUnexpectedPermissionInputDenies(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &gateFactory{tool: "bash", calls: 1})

	stdout, stderr, code := attachAndRead(t, dir, "run ls\nmaybe\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "denied") {
		t.Fatalf("'maybe' did not deny: %q", stdout)
	}
	if strings.Contains(stdout, "gates:allowed") {
		t.Fatalf("'maybe' allowed the tool: %q", stdout)
	}
	// case-insensitive spelling of a real yes is still a yes (README contract).
	out2, err2, code2 := attachAndRead(t, dir, "run ls\nYES\n/exit\n", nil)
	if code2 != 0 {
		t.Fatalf("exit = %d, stderr = %q", code2, err2)
	}
	if !strings.Contains(out2, "allowed once") {
		t.Fatalf("'YES' was not accepted: %q", out2)
	}
}

// ---- rendering -------------------------------------------------------

// TestRenderMultiDeltaReplyNotDuplicated is the anti-duplication contract: deltas
// arrive as increments and are printed verbatim, so "hel"+"lo "+"world" is exactly
// "hello world" — no repeated prefix (the runtime already computed the delta) and
// no inserted marker.
func TestRenderMultiDeltaReplyNotDuplicated(t *testing.T) {
	got := renderEvents(t,
		hostapi.SessionEvent{Type: hostapi.SessionEventTextDelta, Text: "hel"},
		hostapi.SessionEvent{Type: hostapi.SessionEventTextDelta, Text: "lo "},
		hostapi.SessionEvent{Type: hostapi.SessionEventTextDelta, Text: "world"},
		hostapi.SessionEvent{Type: hostapi.SessionEventAgentStart},
		hostapi.SessionEvent{Type: hostapi.SessionEventTurnEnd},
	)
	if want := "hello world\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	if n := strings.Count(got, "hello world"); n != 1 {
		t.Fatalf("reply rendered %d times: %q", n, got)
	}
}

// TestRenderToolLifecycleLines pins the tool transcript: a marker owns its line
// (never glued to a text delta that lacks a trailing newline), tool_end reports the
// outcome, and tool_update — a partial result — is not printed, so the CLI does not
// interleave half-finished tool output into the reply.
func TestRenderToolLifecycleLines(t *testing.T) {
	got := renderEvents(t,
		hostapi.SessionEvent{Type: hostapi.SessionEventTextDelta, Text: "checking"},
		hostapi.SessionEvent{Type: hostapi.SessionEventToolStart, ToolCallID: "t1", ToolName: "read"},
		hostapi.SessionEvent{Type: hostapi.SessionEventToolUpdate, ToolCallID: "t1", ToolName: "read", Result: "PARTIAL-RESULT-TEXT"},
		hostapi.SessionEvent{Type: hostapi.SessionEventToolEnd, ToolCallID: "t1", ToolName: "read"},
		hostapi.SessionEvent{Type: hostapi.SessionEventToolStart, ToolCallID: "t2", ToolName: "write"},
		hostapi.SessionEvent{Type: hostapi.SessionEventToolEnd, ToolCallID: "t2", ToolName: "write", IsError: true},
		hostapi.SessionEvent{Type: hostapi.SessionEventTurnEnd},
	)
	want := "checking\n  ⚙ read\n  ⚙ read → ok\n  ⚙ write\n  ⚙ write → error\n"
	if got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	if strings.Contains(got, "PARTIAL-RESULT-TEXT") {
		t.Fatalf("tool_update leaked a partial result into the transcript: %q", got)
	}
}

// TestRenderThinkingMarkerOnce checks thinking is a "still working" marker, not
// text: it appears once per uninterrupted stretch, and it never interrupts a reply
// that is already mid-line.
func TestRenderThinkingMarkerOnce(t *testing.T) {
	got := renderEvents(t,
		hostapi.SessionEvent{Type: hostapi.SessionEventThinking, Text: "hmm"},
		hostapi.SessionEvent{Type: hostapi.SessionEventThinking, Text: "more"},
		hostapi.SessionEvent{Type: hostapi.SessionEventTextDelta, Text: "answer"},
		hostapi.SessionEvent{Type: hostapi.SessionEventThinking, Text: "again"},
		hostapi.SessionEvent{Type: hostapi.SessionEventTurnEnd},
	)
	if want := "… answer\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	if strings.Contains(got, "hmm") || strings.Contains(got, "again") {
		t.Fatalf("thinking text was rendered verbatim: %q", got)
	}
}

// TestRenderCompactionLine covers the compaction marker on its own line (the loop
// ends a turn before compacting, which is what makes the line safe) and pins that
// sub-agent progress carries no line in the line client: attach renders what a
// terminal user needs, and a sub-agent row is a full-UI affordance.
func TestRenderCompactionLine(t *testing.T) {
	got := renderEvents(t,
		hostapi.SessionEvent{Type: hostapi.SessionEventTextDelta, Text: "text"},
		hostapi.SessionEvent{Type: hostapi.SessionEventTurnEnd},
		hostapi.SessionEvent{Type: hostapi.SessionEventCompaction, TokensBefore: 1000, TokensAfter: 200, SummarizedCount: 4, KeptCount: 2},
		hostapi.SessionEvent{Type: hostapi.SessionEventSubAgent, ToolCallID: "s1", Description: "SUBAGENT-DESC", Activity: "Running bash"},
		hostapi.SessionEvent{Type: hostapi.SessionEventSubAgentDone, ToolCallID: "s1"},
	)
	if want := "text\n  [compacted 1000 → 200 tokens]\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

// TestAttachRendersInBandTurnOutcome covers the tail of a turn that did not simply
// end: a provider failure is reported in-band (the turn is persisted, so it is not a
// transport error) together with its stop reason, and a truncated turn shows why it
// stopped. Both are what tells a user the answer is incomplete.
func TestAttachRendersInBandTurnOutcome(t *testing.T) {
	t.Run("failed-turn", func(t *testing.T) {
		dir := t.TempDir()
		startHost(t, dir, &endingFactory{reply: "partial answer", stopReason: agentcore.StopReasonError, errMsg: "provider exploded"})

		stdout, stderr, code := attachAndRead(t, dir, "go\n/exit\n", nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		if !strings.Contains(stdout, "partial answer") {
			t.Fatalf("partial text of a failed turn was dropped: %q", stdout)
		}
		if !strings.Contains(stdout, "[error: provider exploded]") {
			t.Fatalf("failure reason not reported: %q", stdout)
		}
		// The stop reason is printed as well, so a reader does not have to infer it.
		if !strings.Contains(stdout, "\n[error]\n") {
			t.Fatalf("stop reason not reported: %q", stdout)
		}
		if stderr != "" {
			t.Fatalf("an in-band failure was reported as a transport error: %q", stderr)
		}
	})

	t.Run("truncated-turn", func(t *testing.T) {
		dir := t.TempDir()
		startHost(t, dir, &endingFactory{reply: "cut sh", stopReason: agentcore.StopReasonLength})

		stdout, stderr, code := attachAndRead(t, dir, "go\n/exit\n", nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr)
		}
		if !strings.Contains(stdout, "[length]") {
			t.Fatalf("truncated turn not reported: %q", stdout)
		}
		if strings.Contains(stdout, "[error") {
			t.Fatalf("a truncated turn was reported as an error: %q", stdout)
		}
	})
}

// TestRenderFullTranscriptThroughHost runs the same rendering concerns through the
// real host: runtime events → EventMapper → socket → renderer. It is the only test
// that proves the wire carries the tool lifecycle and compaction to `attach` at all.
func TestRenderFullTranscriptThroughHost(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &chatterFactory{})

	stdout, stderr, code := attachAndRead(t, dir, "go\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		// The marker broke the line itself: "checking" has no trailing newline.
		"checking\n  ⚙ read\n",
		"  ⚙ read → ok\n",
		// Text arriving after the tool lines continues on a fresh line.
		"  ⚙ read → ok\ndone",
		"  [compacted 1000 → 250 tokens]\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("transcript missing %q: %q", want, stdout)
		}
	}
	if strings.Contains(stdout, "PARTIAL-RESULT-TEXT") {
		t.Fatalf("tool_update leaked into the wire transcript: %q", stdout)
	}
	if strings.Contains(stdout, "SUBAGENT-DESC") {
		t.Fatalf("sub-agent progress leaked into the line client: %q", stdout)
	}
	if strings.Contains(stdout, "checkingchecking") {
		t.Fatalf("text was duplicated: %q", stdout)
	}
}

// ---- daemon ----------------------------------------------------------

// TestServeDaemonRefusedWhenHostAlreadyRunning covers a second `serve --daemon`
// against a live host. The child cannot bind, so the parent must report the
// failure (exit 1) from the child's exit status rather than claim readiness, and
// it must leave the running host's state files untouched.
func TestServeDaemonRefusedWhenHostAlreadyRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon test re-execs the test binary")
	}
	// The child inherits the environment; point PIGO_HOME at a temp dir so its
	// fallback session store and trust file never touch the real ~/.pigo.
	t.Setenv("PIGO_HOME", t.TempDir())
	dir := t.TempDir()

	runningHost(t, dir, &scriptedFactory{reply: "original"}, nil)
	awaitSocket(t, dir)
	original, err := hostpath.ReadToken(dir)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}

	var out, errOut bytes.Buffer
	code := Run(nil, ServeOptions{
		Dir:     dir,
		Daemon:  true,
		Build:   "test",
		Factory: &scriptedFactory{reply: "second"},
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("second daemon exit = %d, want 1 (stdout=%q stderr=%q)", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "listening on") {
		t.Fatalf("the refused daemon claimed readiness: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "daemon exited during startup") {
		t.Fatalf("stderr = %q, want the startup-failure report", errOut.String())
	}

	// The log is where the child's own diagnostic lands; it must name the reason.
	logData, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if !strings.Contains(string(logData), "already running") {
		t.Fatalf("daemon log = %q, want the already-running diagnostic", logData)
	}

	// State integrity: the original host still owns the socket, token and state.
	if tok, err := hostpath.ReadToken(dir); err != nil || tok != original {
		t.Fatalf("token = %q (%v), want the running host's token unchanged", tok, err)
	}
	// The host runs in this process, so the state file must still name it.
	state, err := hostpath.ReadState(dir)
	if err != nil || state.PID != os.Getpid() {
		t.Fatalf("state = %+v (%v), want the running host's pid %d", state, err, os.Getpid())
	}
	var statusOut, statusErr bytes.Buffer
	if c := Run([]string{"status"}, ServeOptions{Dir: dir}, &statusOut, &statusErr); c != 0 {
		t.Fatalf("status after the refused daemon exit = %d (%s)", c, statusErr.String())
	}
	attachOut, attachErr, attachCode := attachAndRead(t, dir, "hi\n/exit\n", nil)
	if attachCode != 0 {
		t.Fatalf("the refused daemon disturbed the running host: %d (%s)", attachCode, attachErr)
	}
	if !strings.Contains(attachOut, "original") {
		t.Fatalf("the running host no longer serves its session: %q", attachOut)
	}
}

// TestDaemonChildMarkerServesInForeground is the positive half of the re-fork
// guard: under PIGO_HOST_DAEMON_CHILD=1 a --daemon request must serve in this
// process. It asserts the two observable facts of "no fork": the request returns
// (it listens and can be stopped) and no server.log was created, since the fork
// path is the only writer of that file.
func TestDaemonChildMarkerServesInForeground(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv(daemonChildEnv, "1")
	dir := t.TempDir()

	serve := serveInBackground(t, ServeOptions{
		Dir:     dir,
		Daemon:  true,
		Build:   "test",
		Factory: &scriptedFactory{reply: "x"},
	})
	awaitSocket(t, dir)
	waitForOutput(t, serve.out, "listening on")

	if _, err := os.Stat(filepath.Join(dir, "server.log")); !os.IsNotExist(err) {
		t.Fatalf("the daemon child forked instead of serving (server.log exists, stat error %v)", err)
	}

	var so, se bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &so, &se); code != 0 {
		t.Fatalf("stop exit = %d (%s)", code, se.String())
	}
	if code := serve.wait(t); code != 0 {
		t.Fatalf("foreground serve exited %d: %s", code, serve.errOut.String())
	}
}

// TestNewFactoryResolvesThinkingLevel covers the assembly seam `pigo serve` uses:
// the CLI's string flags become a factory, and an unusable --thinking-level is a
// hard error (a daemon that silently ignored it would serve at the wrong effort).
func TestNewFactoryResolvesThinkingLevel(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	// Keep the layered config lookup inside a temp dir: the project layer is
	// ./.pigo/config.json, and the developer's real one must not leak in.
	t.Chdir(t.TempDir())

	if _, err := NewFactory(FactoryOptions{ThinkingLevel: "hyper"}); err == nil {
		t.Fatal("an invalid thinking level was accepted")
	} else if !strings.Contains(err.Error(), "thinkingLevel") {
		t.Fatalf("error = %q, want it to name the bad setting", err)
	}

	f, err := NewFactory(FactoryOptions{Model: "m", Provider: "p", ThinkingLevel: "high"})
	if err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	if f.Model != "m" || f.Provider != "p" {
		t.Fatalf("factory = %+v, want the model and provider carried through", f)
	}
	if f.Thinking == "" {
		t.Fatalf("thinking level was not resolved: %+v", f)
	}
}

// Not covered: startDaemon's readiness timeout. Forcing it needs a child that
// listens but never writes the ready file (e.g. --ready pointing into a non-empty
// directory), which costs the full 10s deadline per run and deliberately leaves a
// live child behind. The failure path that matters — a child that exits during
// startup — is covered by TestServeDaemonRefusedWhenHostAlreadyRunning above.
