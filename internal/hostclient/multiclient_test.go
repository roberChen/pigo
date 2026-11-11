// This file tests the multi-UI contract of the host split: one host serving
// several clients at once, each owning a different session. A TUI, a Web UI and
// a script all speak the same client API, so these cases are what makes "one
// server, many UIs" safe:
//
//   - sessions are private to the connection that created them;
//   - streaming events, permission questions and cancellation reach only the
//     owner;
//   - a client that disconnects mid-turn neither disturbs the others nor leaves
//     its own turn blocked on a permission question nobody can answer;
//   - concurrent creates do not collide on an id;
//   - host shutdown reaches every client.
//
// The factory is keyed by the model name a session was created with, which lets
// each simulated UI get a different script (reply, delay, tool) from one host.
package hostclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostserver"
	"github.com/smallnest/pigo/internal/runtime"
)

// scriptSpec is one session's behaviour, selected by the model name passed to
// CreateSession.
type scriptSpec struct {
	reply      string
	delay      time.Duration
	tool       string
	chunks     int
	chunkDelay time.Duration
}

// multiFactory builds a different agent per requested model, so concurrent
// sessions on one host do not share a script. specs is read without a lock and
// must not change after the host starts.
type multiFactory struct {
	specs    map[string]scriptSpec
	fallback scriptSpec
}

func (f *multiFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	s, ok := f.specs[spec.Model]
	if !ok {
		s = f.fallback
	}
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = "scripted system prompt"
	}
	seed := append(agentcore.MessageList(nil), spec.Messages...)
	if s.chunks > 0 {
		return &burstAgent{spec: spec, messages: seed, chunks: s.chunks, chunkDelay: s.chunkDelay, reply: s.reply}, nil
	}
	return &scriptedAgent{spec: spec, messages: seed, reply: s.reply, delay: s.delay, tool: s.tool}, nil
}

// burstAgent streams chunks text deltas before its final reply, giving the
// concurrent-stream tests enough events to interleave.
type burstAgent struct {
	mu         sync.Mutex
	spec       hostserver.AgentSpec
	messages   agentcore.MessageList
	chunks     int
	chunkDelay time.Duration
	reply      string
}

func (a *burstAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	a.mu.Lock()
	if prompt != "" {
		a.messages = append(a.messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	a.mu.Unlock()
	for i := 0; i < a.chunks; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(a.chunkDelay):
		}
		if h.OnText != nil {
			h.OnText(fmt.Sprintf("chunk-%05d;", i))
		}
	}
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(a.reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
	}
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.mu.Unlock()
	return &msg, nil
}

func (a *burstAgent) Messages() agentcore.MessageList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(agentcore.MessageList(nil), a.messages...)
}

func (a *burstAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *burstAgent) Model() string        { return a.spec.Model }
func (a *burstAgent) Provider() string     { return "scripted" }
func (a *burstAgent) Close()               {}

// connectAs is connect with a distinct client label, so a status read can tell
// the simulated UIs apart if a failure needs the host's view.
func connectAs(t *testing.T, dir, label string) *Client {
	t.Helper()
	c, err := Connect(Options{Dir: dir, Client: label})
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// mustCreate creates a session and fails the test on error.
func mustCreate(t *testing.T, c *Client, cwd, model, title string) hostapi.SessionInfo {
	t.Helper()
	info, err := c.CreateSession(cwd, model, title)
	if err != nil {
		t.Fatalf("create session (%s): %v", model, err)
	}
	return info
}

// eventSink counts the session ids carried by the session events one client
// received. Cross-talk shows up as an id that is not the client's own.
type eventSink struct {
	mu     sync.Mutex
	ids    map[string]int
	events int
}

func newEventSink() *eventSink { return &eventSink{ids: make(map[string]int)} }

func (s *eventSink) add(env hostapi.Envelope) {
	var ev hostapi.SessionEvent
	if err := env.DecodeParams(&ev); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[ev.SessionID]++
	s.events++
}

func (s *eventSink) snapshot() (map[string]int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.ids))
	for id, n := range s.ids {
		out[id] = n
	}
	return out, s.events
}

// subscribePermissions returns the permission questions this connection
// received. A buffered channel keeps the dispatcher from blocking a test that
// deliberately delays before reading.
func subscribePermissions(c *Client) <-chan hostapi.PermissionRequest {
	ch := make(chan hostapi.PermissionRequest, 8)
	c.OnEvent(hostapi.EventPermission, func(env hostapi.Envelope) {
		var req hostapi.PermissionRequest
		if err := env.DecodeParams(&req); err == nil {
			select {
			case ch <- req:
			default:
			}
		}
	})
	return ch
}

type promptOutcome struct {
	res hostapi.PromptResult
	err error
}

func isCode(err error, code int) bool {
	var protoErr *hostapi.Error
	return errors.As(err, &protoErr) && protoErr.Code == code
}

// TestManyClientsOwnIndependentSessions is the base case: four UIs, four
// sessions, one host. Concurrent turns must return their own replies, and every
// streamed event must carry the receiving client's session id.
func TestManyClientsOwnIndependentSessions(t *testing.T) {
	const n = 4
	specs := make(map[string]scriptSpec, n)
	for i := 0; i < n; i++ {
		specs[fmt.Sprintf("model-%d", i)] = scriptSpec{
			reply:      fmt.Sprintf("reply-%d", i),
			chunks:     40,
			chunkDelay: time.Millisecond,
		}
	}
	dir, _ := startHostWithServer(t, &multiFactory{specs: specs})

	clients := make([]*Client, n)
	sinks := make([]*eventSink, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		clients[i] = connectAs(t, dir, fmt.Sprintf("ui-%d", i))
		sinks[i] = newEventSink()
		clients[i].OnEvent(hostapi.EventSession, sinks[i].add)
		ids[i] = mustCreate(t, clients[i], t.TempDir(), fmt.Sprintf("model-%d", i), fmt.Sprintf("title-%d", i)).ID
	}

	seen := make(map[string]int, n)
	for i, id := range ids {
		if seen[id] != 0 {
			t.Fatalf("clients %d and %d share session id %s", seen[id]-1, i, id)
		}
		seen[id] = i + 1
		list, err := clients[i].ListSessions()
		if err != nil {
			t.Fatalf("ui-%d list: %v", i, err)
		}
		if len(list) != 1 || list[0].ID != id {
			t.Fatalf("ui-%d sees sessions %+v, want only %s", i, list, id)
		}
	}

	// All four turns in flight at once.
	var wg sync.WaitGroup
	results := make([]hostapi.PromptResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = clients[i].Prompt(context.Background(), ids[i], fmt.Sprintf("prompt-%d", i))
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("ui-%d prompt: %v", i, errs[i])
		}
		if want := fmt.Sprintf("reply-%d", i); results[i].Text != want {
			t.Fatalf("ui-%d got %q, want %q", i, results[i].Text, want)
		}
		if results[i].SessionID != ids[i] {
			t.Fatalf("ui-%d result names session %s, want %s", i, results[i].SessionID, ids[i])
		}
		gotIDs, events := sinks[i].snapshot()
		if events == 0 {
			t.Fatalf("ui-%d received no session events", i)
		}
		if len(gotIDs) != 1 || gotIDs[ids[i]] == 0 {
			t.Fatalf("ui-%d received events for foreign sessions: %v (own %s)", i, gotIDs, ids[i])
		}
	}

	status := connectAs(t, dir, "status")
	st, err := status.ServerStatus()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Sessions != n {
		t.Fatalf("host reports %d live sessions, want %d", st.Sessions, n)
	}
}

// TestCrossClientSessionsAreIsolated covers the security boundary between two
// UIs: another client can neither drive nor close a session it does not own, and
// a closed session can be resumed by the next connection (the transcript is the
// hand-off).
func TestCrossClientSessionsAreIsolated(t *testing.T) {
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"plain": {reply: "plain-reply"},
	}})
	owner := connectAs(t, dir, "owner")
	other := connectAs(t, dir, "other")
	info := mustCreate(t, owner, t.TempDir(), "plain", "owned")

	if _, err := other.Prompt(context.Background(), info.ID, "steal"); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign prompt = %v, want forbidden", err)
	}
	if err := other.Cancel(info.ID); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign cancel = %v, want forbidden", err)
	}
	if err := other.Decide(info.ID, "perm-1", true, false); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign decide = %v, want forbidden", err)
	}
	if err := other.CloseSession(info.ID); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign close = %v, want forbidden", err)
	}
	if _, err := other.Resume(info.ID); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign resume = %v, want forbidden", err)
	}
	if list, err := other.ListSessions(); err != nil || len(list) != 0 {
		t.Fatalf("other lists %d sessions (err %v), want none", len(list), err)
	}

	// The owner is untouched by the attempted access.
	res, err := owner.Prompt(context.Background(), info.ID, "hello")
	if err != nil || res.Text != "plain-reply" {
		t.Fatalf("owner prompt = (%q, %v)", res.Text, err)
	}

	// Closing hands the transcript over: any connection may resume it then.
	if err := owner.CloseSession(info.ID); err != nil {
		t.Fatalf("owner close: %v", err)
	}
	rr, err := other.Resume(info.ID)
	if err != nil {
		t.Fatalf("resume after close: %v", err)
	}
	if len(rr.Messages) < 2 {
		t.Fatalf("resumed %d messages, want the persisted turn", len(rr.Messages))
	}
	if err := owner.CloseSession(info.ID); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("previous owner still closes the resumed session: %v", err)
	}
	if _, err := owner.Prompt(context.Background(), info.ID, "again"); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("previous owner still prompts the resumed session: %v", err)
	}
	if _, err := other.Resume(info.ID); !isCode(err, hostapi.CodeBusy) {
		t.Fatalf("resume of an owned session = %v, want busy", err)
	}
	if res, err := other.Prompt(context.Background(), info.ID, "again"); err != nil || res.Text != "plain-reply" {
		t.Fatalf("new owner prompt = (%q, %v)", res.Text, err)
	}
}

// TestPermissionRequestsReachOnlyTheirOwner pins the routing of side-effect
// confirmations: the question goes to the session's owner, another client cannot
// answer it, and "always" is scoped to the session's directory rather than to the
// whole host.
func TestPermissionRequestsReachOnlyTheirOwner(t *testing.T) {
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"tool": {tool: "bash", reply: "allowed"},
	}})
	a := connectAs(t, dir, "ui-a")
	b := connectAs(t, dir, "ui-b")
	cwdA, cwdB := t.TempDir(), t.TempDir()
	aInfo := mustCreate(t, a, cwdA, "tool", "a")
	bInfo := mustCreate(t, b, cwdB, "tool", "b")
	permA := subscribePermissions(a)
	permB := subscribePermissions(b)

	aDone := make(chan promptOutcome, 1)
	go func() {
		res, err := a.Prompt(context.Background(), aInfo.ID, "run")
		aDone <- promptOutcome{res, err}
	}()

	var reqA hostapi.PermissionRequest
	select {
	case reqA = <-permA:
	case <-time.After(10 * time.Second):
		t.Fatal("the owner never received its permission request")
	}
	select {
	case req := <-permB:
		t.Fatalf("another client received a permission question for someone else's session: %+v", req)
	case <-time.After(200 * time.Millisecond):
	}
	if err := b.Decide(aInfo.ID, reqA.RequestID, true, true); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign decide = %v, want forbidden", err)
	}
	if err := a.Decide(aInfo.ID, reqA.RequestID, true, true); err != nil {
		t.Fatalf("owner decide: %v", err)
	}
	outA := <-aDone
	if outA.err != nil || outA.res.Text != "allowed" {
		t.Fatalf("owner turn after approval = (%q, %v)", outA.res.Text, outA.err)
	}

	// "always" was granted for cwdA: the same session's next tool call must not
	// ask again.
	if res, err := a.Prompt(context.Background(), aInfo.ID, "run again"); err != nil || res.Text != "allowed" {
		t.Fatalf("second owner turn = (%q, %v)", res.Text, err)
	}
	select {
	case req := <-permA:
		t.Fatalf("owner was asked again after 'always': %+v", req)
	default:
	}

	// A different session in a different directory still asks, and the question
	// goes to its own owner.
	bDone := make(chan promptOutcome, 1)
	go func() {
		res, err := b.Prompt(context.Background(), bInfo.ID, "run")
		bDone <- promptOutcome{res, err}
	}()
	var reqB hostapi.PermissionRequest
	select {
	case reqB = <-permB:
	case <-time.After(10 * time.Second):
		t.Fatal("the second client never received its permission request")
	}
	select {
	case req := <-permA:
		t.Fatalf("the first client received the second client's question: %+v", req)
	case <-time.After(200 * time.Millisecond):
	}
	if err := b.Decide(bInfo.ID, reqB.RequestID, true, false); err != nil {
		t.Fatalf("second owner decide: %v", err)
	}
	outB := <-bDone
	if outB.err != nil || outB.res.Text != "allowed" {
		t.Fatalf("second client turn = (%q, %v)", outB.res.Text, outB.err)
	}
}

// TestCancelIsOwnerOnlyAndOtherClientsContinue runs a slow turn on one UI while
// another UI works normally: the fast client is unaffected, only the owner may
// cancel, and the aborted turn settles in-band.
func TestCancelIsOwnerOnlyAndOtherClientsContinue(t *testing.T) {
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"slow": {delay: 3 * time.Second, reply: "slow-done"},
		"fast": {reply: "fast-done"},
	}})
	a := connectAs(t, dir, "ui-a")
	b := connectAs(t, dir, "ui-b")
	aInfo := mustCreate(t, a, t.TempDir(), "slow", "a")
	bInfo := mustCreate(t, b, t.TempDir(), "fast", "b")

	aDone := make(chan promptOutcome, 1)
	go func() {
		res, err := a.Prompt(context.Background(), aInfo.ID, "slow")
		aDone <- promptOutcome{res, err}
	}()
	waitFor(t, "the slow turn to be in flight", func() bool {
		list, err := a.ListSessions()
		return err == nil && len(list) == 1 && list[0].Running
	})

	start := time.Now()
	if res, err := b.Prompt(context.Background(), bInfo.ID, "quick"); err != nil || res.Text != "fast-done" {
		t.Fatalf("the other client's turn = (%q, %v)", res.Text, err)
	} else if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the other client's turn took %v while a slow turn was running", elapsed)
	}

	if err := b.Cancel(aInfo.ID); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("foreign cancel = %v, want forbidden", err)
	}
	if err := a.Cancel(bInfo.ID); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("cancel of the other client's idle session = %v, want forbidden", err)
	}
	if err := a.Cancel(aInfo.ID); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	select {
	case out := <-aDone:
		if out.err != nil {
			t.Fatalf("cancelled prompt returned a transport error: %v", out.err)
		}
		if out.res.StopReason != agentcore.StopReasonAborted {
			t.Fatalf("cancelled turn stop reason = %q, want aborted", out.res.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled turn did not settle")
	}
	if list, err := a.ListSessions(); err != nil || len(list) != 1 || list[0].Running {
		t.Fatalf("cancelled session is still running: %+v (err %v)", list, err)
	}
	if res, err := b.Prompt(context.Background(), bInfo.ID, "again"); err != nil || res.Text != "fast-done" {
		t.Fatalf("the other client stopped working: (%q, %v)", res.Text, err)
	}
}

// TestDisconnectMidTurnKeepsTheOtherClientAndResumes is the disconnect case: one
// UI dies while its turn runs. The host owns the turn, so it finishes and
// persists; the other UI never notices, and the session is resumable afterwards.
func TestDisconnectMidTurnKeepsTheOtherClientAndResumes(t *testing.T) {
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"slow": {delay: 2 * time.Second, reply: "slow-done"},
		"fast": {reply: "fast-done"},
	}})
	a := connectAs(t, dir, "ui-a")
	b := connectAs(t, dir, "ui-b")
	aInfo := mustCreate(t, a, t.TempDir(), "slow", "a")
	bInfo := mustCreate(t, b, t.TempDir(), "fast", "b")

	aDone := make(chan promptOutcome, 1)
	go func() {
		res, err := a.Prompt(context.Background(), aInfo.ID, "slow")
		aDone <- promptOutcome{res, err}
	}()
	waitFor(t, "the slow turn to be in flight", func() bool {
		list, err := a.ListSessions()
		return err == nil && len(list) == 1 && list[0].Running
	})
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case out := <-aDone:
		if out.err == nil {
			t.Fatal("the dead client's prompt returned a result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dead client's prompt waiter did not fail")
	}

	// The surviving UI keeps working while the orphaned turn finishes.
	if res, err := b.Prompt(context.Background(), bInfo.ID, "quick"); err != nil || res.Text != "fast-done" {
		t.Fatalf("surviving client's turn = (%q, %v)", res.Text, err)
	}

	c := connectAs(t, dir, "ui-c")
	var rr hostapi.SessionResumeResult
	waitFor(t, "the orphaned session to settle and become resumable", func() bool {
		res, err := c.Resume(aInfo.ID)
		if err != nil {
			return false
		}
		rr = res
		return true
	})
	if len(rr.Messages) != 2 {
		t.Fatalf("resumed %d messages, want the user prompt and the finished assistant turn", len(rr.Messages))
	}
	if list, err := b.ListSessions(); err != nil || len(list) != 1 || list[0].ID != bInfo.ID {
		t.Fatalf("surviving client's session list changed: %+v (err %v)", list, err)
	}
}

// TestDisconnectDuringPermissionDeniesAndFreesTheSession is the dangerous
// disconnect: the client vanishes while its session is waiting for a tool
// confirmation. The host must fail the question (deny), let the turn unwind and
// persist, and make the session resumable again, never leave it "running"
// forever because it is waiting on an answer nobody can send.
func TestDisconnectDuringPermissionDeniesAndFreesTheSession(t *testing.T) {
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"tool": {tool: "bash", reply: "allowed"},
	}})
	a := connectAs(t, dir, "ui-a")
	info := mustCreate(t, a, t.TempDir(), "tool", "a")
	perm := subscribePermissions(a)

	done := make(chan promptOutcome, 1)
	go func() {
		res, err := a.Prompt(context.Background(), info.ID, "run")
		done <- promptOutcome{res, err}
	}()
	select {
	case <-perm:
	case <-time.After(10 * time.Second):
		t.Fatal("no permission question arrived")
	}
	// Do not answer: drop the connection the question was sent to.
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("the dead client's prompt returned a result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dead client's prompt waiter did not fail")
	}

	b := connectAs(t, dir, "ui-b")
	var rr hostapi.SessionResumeResult
	waitFor(t, "the denied turn to settle", func() bool {
		res, err := b.Resume(info.ID)
		if err != nil {
			return false
		}
		rr = res
		return true
	})
	if len(rr.Messages) != 2 {
		t.Fatalf("resumed %d messages, want the denied turn to be persisted", len(rr.Messages))
	}
	// The resumed session is fully drivable; a fresh side-effect tool still needs
	// this connection's answer (the earlier denial granted no trust).
	permB := subscribePermissions(b)
	doneB := make(chan promptOutcome, 1)
	go func() {
		res, err := b.Prompt(context.Background(), info.ID, "again")
		doneB <- promptOutcome{res, err}
	}()
	var reqB hostapi.PermissionRequest
	select {
	case reqB = <-permB:
	case <-time.After(10 * time.Second):
		t.Fatal("the resumed session was not asked about its tool call")
	}
	if err := b.Decide(info.ID, reqB.RequestID, true, false); err != nil {
		t.Fatalf("decide on the resumed session: %v", err)
	}
	outB := <-doneB
	if outB.err != nil || outB.res.Text != "allowed" {
		t.Fatalf("resumed session turn = (%q, %v)", outB.res.Text, outB.err)
	}
}

// TestConcurrentSessionCreatesAreUnique hammers the id allocator: every UI
// creates at once, and no two sessions may share an id (a collision would make
// two live sessions write one transcript).
func TestConcurrentSessionCreatesAreUnique(t *testing.T) {
	const n = 8
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"plain": {reply: "ok"},
	}})
	clients := make([]*Client, n)
	for i := 0; i < n; i++ {
		clients[i] = connectAs(t, dir, fmt.Sprintf("ui-%d", i))
	}

	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info, err := clients[i].CreateSession(t.TempDir(), "plain", fmt.Sprintf("t-%d", i))
			ids[i], errs[i] = info.ID, err
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("ui-%d create: %v", i, errs[i])
		}
		if seen[id] {
			t.Fatalf("duplicate session id %s", id)
		}
		seen[id] = true
		list, err := clients[i].ListSessions()
		if err != nil || len(list) != 1 || list[0].ID != id {
			t.Fatalf("ui-%d sees %+v (err %v), want only %s", i, list, err, id)
		}
	}
	status := connectAs(t, dir, "status")
	if st, err := status.ServerStatus(); err != nil || st.Sessions != n {
		t.Fatalf("host reports %d sessions (err %v), want %d", st.Sessions, err, n)
	}
}

// TestOneClientOwnsSeveralSessions is the "one UI, several sessions" half: a
// tabbed TUI may create more than one session on its connection, and closing one
// must leave the rest untouched.
func TestOneClientOwnsSeveralSessions(t *testing.T) {
	dir, _ := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"one":   {reply: "reply-one"},
		"two":   {reply: "reply-two"},
		"three": {reply: "reply-three"},
	}})
	ui := connectAs(t, dir, "tabbed-ui")
	sink := newEventSink()
	ui.OnEvent(hostapi.EventSession, sink.add)

	one := mustCreate(t, ui, t.TempDir(), "one", "")
	two := mustCreate(t, ui, t.TempDir(), "two", "")
	three := mustCreate(t, ui, t.TempDir(), "three", "")
	if list, err := ui.ListSessions(); err != nil || len(list) != 3 {
		t.Fatalf("list = %d sessions (err %v), want 3", len(list), err)
	}

	for _, tc := range []struct {
		id   string
		want string
	}{{one.ID, "reply-one"}, {two.ID, "reply-two"}, {three.ID, "reply-three"}} {
		res, err := ui.Prompt(context.Background(), tc.id, "q")
		if err != nil || res.Text != tc.want {
			t.Fatalf("prompt %s = (%q, %v), want %q", tc.id, res.Text, err, tc.want)
		}
	}
	gotIDs, _ := sink.snapshot()
	for _, id := range []string{one.ID, two.ID, three.ID} {
		if gotIDs[id] == 0 {
			t.Fatalf("no session events for %s: %v", id, gotIDs)
		}
	}

	if err := ui.CloseSession(two.ID); err != nil {
		t.Fatalf("close two: %v", err)
	}
	list, err := ui.ListSessions()
	if err != nil || len(list) != 2 {
		t.Fatalf("after close, list = %d sessions (err %v), want 2", len(list), err)
	}
	for _, s := range list {
		if s.ID == two.ID {
			t.Fatalf("closed session %s is still listed", two.ID)
		}
	}
	if _, err := ui.Prompt(context.Background(), two.ID, "gone"); !isCode(err, hostapi.CodeForbidden) {
		t.Fatalf("prompt of a closed session = %v, want forbidden", err)
	}
	if res, err := ui.Prompt(context.Background(), one.ID, "still"); err != nil || res.Text != "reply-one" {
		t.Fatalf("remaining session stopped working: (%q, %v)", res.Text, err)
	}
}

// TestHostShutdownNotifiesEveryClient is the end of the shared host: every
// attached UI gets a server_shutdown event with the reason and sees its
// connection close.
func TestHostShutdownNotifiesEveryClient(t *testing.T) {
	dir, srv := startHostWithServer(t, &multiFactory{specs: map[string]scriptSpec{
		"plain": {reply: "ok"},
	}})
	a := connectAs(t, dir, "ui-a")
	b := connectAs(t, dir, "ui-b")
	mustCreate(t, a, t.TempDir(), "plain", "")
	mustCreate(t, b, t.TempDir(), "plain", "")

	shutdownA := make(chan hostapi.ServerShutdownEvent, 1)
	shutdownB := make(chan hostapi.ServerShutdownEvent, 1)
	a.OnEvent(hostapi.EventServerShutdown, func(env hostapi.Envelope) {
		var ev hostapi.ServerShutdownEvent
		if env.DecodeParams(&ev) == nil {
			select {
			case shutdownA <- ev:
			default:
			}
		}
	})
	b.OnEvent(hostapi.EventServerShutdown, func(env hostapi.Envelope) {
		var ev hostapi.ServerShutdownEvent
		if env.DecodeParams(&ev) == nil {
			select {
			case shutdownB <- ev:
			default:
			}
		}
	})

	srv.Shutdown("multi-client shutdown")

	for i, ch := range []chan hostapi.ServerShutdownEvent{shutdownA, shutdownB} {
		select {
		case ev := <-ch:
			if ev.Reason != "multi-client shutdown" {
				t.Fatalf("client %d shutdown reason = %q", i, ev.Reason)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("client %d did not receive server_shutdown", i)
		}
	}
	for i, c := range []*Client{a, b} {
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Fatalf("client %d connection did not close", i)
		}
		if c.ReadErr() == nil {
			t.Fatalf("client %d reports no read error after shutdown", i)
		}
		if _, err := c.ServerStatus(); err == nil {
			t.Fatalf("client %d still talks to the shut-down host", i)
		}
	}
}
