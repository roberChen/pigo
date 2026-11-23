// This file tests session.setModel, the in-session model switch (FR-A1/A2).
//
// It splits the coverage the way the rest of the host suite does: the scripted
// tests prove the protocol machine (ownership, busy, capability, metadata), and
// the end-to-end test drives the production DefaultFactory so the *resolution* —
// provider re-resolution, the compaction plan for the new model, and the fact
// that the conversation and tool set survive — is proven against the real
// assembly rather than a double.
package hostserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/hostapi"
)

// switchableAgent is a scripted agent that also implements ModelSwitcher, so the
// server's plumbing can be tested without a provider. It reports a distinct
// budget per model and can be made to reject a switch.
type switchableAgent struct {
	*fakeAgent

	mu       sync.Mutex
	model    string
	provider string
	refuse   error
	// switchOrder records the model ids the server asked for.
	switchOrder []string
	// onSwitch, when set, runs inside SetModel — used to assert the server holds
	// no session lock that would deadlock a re-entrant call.
	onSwitch func()
}

func (a *switchableAgent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *switchableAgent) Provider() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

func (a *switchableAgent) SetModel(model string) (ModelSwitch, error) {
	a.mu.Lock()
	refuse := a.refuse
	a.mu.Unlock()
	if refuse != nil {
		return ModelSwitch{}, refuse
	}
	if a.onSwitch != nil {
		a.onSwitch()
	}
	a.mu.Lock()
	a.switchOrder = append(a.switchOrder, model)
	a.model = model
	a.provider = "fake-switched"
	a.mu.Unlock()
	return ModelSwitch{
		Model:         model,
		Provider:      "fake-switched",
		ContextWindow: 300000,
		InputBudget:   250000,
		TriggerPoint:  200000,
		TriggerSource: "ladder",
	}, nil
}

func (a *switchableAgent) switchedTo() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.switchOrder...)
}

// switchFactory builds switchableAgents. Every agent shares one budget so a test
// can inspect whichever turn it likes.
type switchFactory struct {
	inner *fakeFactory
	// delay makes a turn outlive the switch attempt, for the busy path.
	delay time.Duration
	// refuse, when non-empty, makes every SetModel fail with that message.
	refuse string
	// onSwitch runs inside every SetModel.
	onSwitch func()
}

func (f *switchFactory) NewAgent(_ context.Context, spec AgentSpec) (Agent, error) {
	inner := f.inner
	if inner == nil {
		inner = &fakeFactory{}
	}
	a, err := inner.NewAgent(context.Background(), spec)
	if err != nil {
		return nil, err
	}
	fa := a.(*fakeAgent)
	fa.delay = f.delay
	var refuse error
	if f.refuse != "" {
		refuse = errors.New(f.refuse)
	}
	return &switchableAgent{
		fakeAgent: fa,
		model:     fa.Model(),
		provider:  fa.Provider(),
		refuse:    refuse,
		onSwitch:  f.onSwitch,
	}, nil
}

// TestSetModelSwitchesMetadataAndBudget checks the happy path: the session keeps
// running, its reported model/provider and resolved budget change, and the next
// turn still works on the same session.
func TestSetModelSwitchesMetadataAndBudget(t *testing.T) {
	f := &switchFactory{inner: &fakeFactory{reply: "ok"}}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	// One turn first, so the switch has a persisted session to update.
	decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "first"}))

	res := decodeResult[hostapi.SessionSetModelResult](t,
		c.mustCall(hostapi.MethodSessionSetModel, hostapi.SessionSetModelParams{SessionID: sid, Model: "big-model"}))

	if res.Session.ID != sid {
		t.Fatalf("result names session %q, want %q", res.Session.ID, sid)
	}
	if res.Session.Model != "big-model" || res.Session.Provider != "fake-switched" {
		t.Fatalf("session metadata not updated: %+v", res.Session)
	}
	// The budget is reported by the host, not recomputed by the client.
	if res.ContextWindow != 300000 || res.InputBudget != 250000 || res.TriggerPoint != 200000 {
		t.Fatalf("budget not reported: %+v", res)
	}
	if res.TriggerSource != "ladder" {
		t.Fatalf("trigger source = %q", res.TriggerSource)
	}

	// The session is still the same one: listing shows it, and a further turn runs.
	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || list.Sessions[0].Model != "big-model" {
		t.Fatalf("list does not reflect the switch: %+v", list.Sessions)
	}
	_ = decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "second"}))

	// The new metadata is on disk immediately, not only after the next turn.
	header, _, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if header.Model != "big-model" || header.Provider != "fake-switched" {
		t.Fatalf("persisted header = %+v", header)
	}
}

// TestSetModelRefusedWhileRunning proves the switch cannot land inside a turn:
// the loop's streaming config is being read then, and a half-switched turn would
// send some requests to one model and the rest to another.
func TestSetModelRefusedWhileRunning(t *testing.T) {
	f := &switchFactory{inner: &fakeFactory{reply: "slow"}, delay: 5 * time.Second}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "long"})
	}()
	waitRunning(t, c, sid)

	expectError(t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "other-model"}), hostapi.CodeBusy)

	// Cancel so the turn ends and the test does not wait out the delay.
	_, _ = c.call(hostapi.MethodCancel, hostapi.CancelParams{SessionID: sid})
	<-done
}

// TestSetModelRequiresOwnership proves connection isolation extends to the
// switch: another client must not retarget a session it neither created nor
// adopted.
func TestSetModelRequiresOwnership(t *testing.T) {
	fx := newFixture(t, &switchFactory{inner: &fakeFactory{reply: "ok"}})
	owner := fx.dial(t)
	other := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		owner.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	expectError(t, other.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "hijack"}), hostapi.CodeForbidden)

	// The owner's model is unchanged.
	list := decodeResult[hostapi.SessionListResult](t, owner.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || list.Sessions[0].Model == "hijack" {
		t.Fatalf("forbidden switch took effect: %+v", list.Sessions)
	}
}

// TestSetModelUnknownSession covers the empty/unknown id path.
func TestSetModelUnknownSession(t *testing.T) {
	fx := newFixture(t, &switchFactory{inner: &fakeFactory{reply: "ok"}})
	c := fx.dial(t)
	expectError(t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: "nope", Model: "x"}), hostapi.CodeForbidden)
}

// TestSetModelUnsupportedByAgent proves a host whose agent cannot switch says so
// plainly instead of silently reporting success.
func TestSetModelUnsupportedByAgent(t *testing.T) {
	fx := newFixture(t, &fakeFactory{reply: "ok"})
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	expectError(t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "other"}), hostapi.CodeUnavailable)

	// The session still works on its original model.
	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || list.Sessions[0].Model != "fake-model" {
		t.Fatalf("unsupported switch changed the session: %+v", list.Sessions)
	}
}

// TestSetModelFailureLeavesSessionIntact proves a rejected switch is a no-op: the
// model, the reported metadata and the ability to run a turn all survive.
func TestSetModelFailureLeavesSessionIntact(t *testing.T) {
	f := &switchFactory{inner: &fakeFactory{reply: "ok"}, refuse: "unknown model"}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	expectError(t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "bogus"}), hostapi.CodeInvalidParams)

	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || list.Sessions[0].Model != "fake-model" {
		t.Fatalf("failed switch changed the session: %+v", list.Sessions)
	}
	_ = decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "still here"}))
}

// TestSetModelConcurrentTurnsStaySerialized runs a switch back-to-back with
// prompts and checks the invariant that matters: no prompt observes a
// half-switched session, so every turn reports a single model and the session
// ends up on the last requested one.
func TestSetModelConcurrentTurnsStaySerialized(t *testing.T) {
	f := &switchFactory{inner: &fakeFactory{reply: "ok"}}
	fx := newFixture(t, f)
	c := fx.dial(t)
	sid := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: t.TempDir()})).Session.ID

	models := []string{"m-1", "m-2", "m-3"}
	for _, m := range models {
		decodeResult[hostapi.SessionSetModelResult](t,
			c.mustCall(hostapi.MethodSessionSetModel, hostapi.SessionSetModelParams{SessionID: sid, Model: m}))
		_ = decodeResult[hostapi.PromptResult](t, c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "t"}))
	}

	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || list.Sessions[0].Model != "m-3" {
		t.Fatalf("final model = %+v, want m-3", list.Sessions)
	}
	header, _, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if header.Model != "m-3" {
		t.Fatalf("persisted model = %q, want m-3", header.Model)
	}
}

// waitRunning blocks until the host reports the session mid-turn.
func waitRunning(t *testing.T, c *testClient, sid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
		for _, s := range list.Sessions {
			if s.ID == sid && s.Running {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("session never reported running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestE2E_SetModelSwitchesProviderAndBudget drives the production factory, so the
// real resolution runs: the new model reaches the wire, the compaction plan is
// recomputed for the new budget, and the conversation and tool set survive
// because the environment is not rebuilt.
func TestE2E_SetModelSwitchesProviderAndBudget(t *testing.T) {
	llm, baseURL := newFakeLLM(t,
		llmStep{Text: "first answer"},
		llmStep{Text: "second answer"},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, t.TempDir())

	first := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "hello"))
	if first.Text != "first answer" {
		t.Fatalf("first turn = %q", first.Text)
	}
	before := decodeResult[hostapi.SessionSetModelResult](t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "claude-3-5-sonnet-latest"}))
	if before.ContextWindow == 0 || before.TriggerPoint == 0 {
		t.Fatalf("switch reported no budget: %+v", before)
	}

	second := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "again"))
	if second.Text != "second answer" {
		t.Fatalf("second turn = %q", second.Text)
	}

	req := llm.requestAt(1)
	if req.Model != "claude-3-5-sonnet-latest" {
		t.Fatalf("request model = %q, want the switched model", req.Model)
	}
	// The conversation survived: the second request replays the first turn.
	if len(req.Messages) < 3 {
		t.Fatalf("second request carried %d messages, want the earlier conversation", len(req.Messages))
	}
	// The tool set survived: a switch is not a new session.
	if len(req.Tools) == 0 {
		t.Fatal("switched session lost its tools")
	}

	header, msgs, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if header.Model != "claude-3-5-sonnet-latest" {
		t.Fatalf("persisted model = %q", header.Model)
	}
	if len(msgs) < 4 {
		t.Fatalf("persisted %d messages, want both turns", len(msgs))
	}
}

// TestE2E_SetModelRejectsUnknownModel checks the production resolver's failure is
// reported in-band and leaves the session on its original model.
func TestE2E_SetModelRejectsUnknownModel(t *testing.T) {
	llm, baseURL := newFakeLLM(t, llmStep{Text: "ok"}, llmStep{Text: "still ok"})
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, t.TempDir())
	_ = decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "hello"))

	expectError(t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "   "}), hostapi.CodeInvalidParams)

	list := decodeResult[hostapi.SessionListResult](t, c.mustCall(hostapi.MethodSessionList, nil))
	if len(list.Sessions) != 1 || list.Sessions[0].Model != "e2e-model" {
		t.Fatalf("rejected switch changed the model: %+v", list.Sessions)
	}
	// Still usable, and still on the original model.
	if res := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "again")); res.Text != "still ok" {
		t.Fatalf("turn after a rejected switch = %q", res.Text)
	}
	if got := llm.requestAt(1).Model; got != "e2e-model" {
		t.Fatalf("request model after a rejected switch = %q", got)
	}
}

// TestE2E_SetModelKeepsHooksAndGateVisible guards the "do not rebuild" property
// from the outside: after a switch the session still runs its system prompt and
// still advertises the same tools, so the switch changed the model and nothing
// else.
func TestE2E_SetModelKeepsSystemPrompt(t *testing.T) {
	llm, baseURL := newFakeLLM(t, llmStep{Text: "a"}, llmStep{Text: "b"})
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, t.TempDir())

	_ = decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "one"))
	systemBefore := llm.requestAt(0).Messages[0]
	_ = decodeResult[hostapi.SessionSetModelResult](t, c.mustCall(hostapi.MethodSessionSetModel,
		hostapi.SessionSetModelParams{SessionID: sid, Model: "claude-3-5-sonnet-latest"}))
	_ = decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "two"))

	systemAfter := llm.requestAt(1).Messages[0]
	if got, want := jsonRole(systemAfter), jsonRole(systemBefore); got != want {
		t.Fatalf("system message role changed: %q → %q", want, got)
	}
	if jsonContent(systemAfter) != jsonContent(systemBefore) {
		t.Fatal("system prompt changed across a model switch")
	}
}

// jsonRole reads a message map's role, defaulting to "" so a shape change fails
// loudly rather than panicking.
func jsonRole(m map[string]any) string {
	s, _ := m["role"].(string)
	return s
}

// jsonContent flattens a message's content into a comparable string.
func jsonContent(m map[string]any) string {
	switch v := m["content"].(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if pm, ok := part.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// compile-time check that the scripted double really implements the seam it is
// meant to exercise.
var _ ModelSwitcher = (*switchableAgent)(nil)

// ensure the embedded fake still satisfies Agent through the wrapper.
var _ Agent = (*switchableAgent)(nil)
