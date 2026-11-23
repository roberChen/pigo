// This file tests the client's SetModel wrapper against a real host over a real
// socket. The point is the round trip and the error mapping a UI depends on: a
// successful switch reports the budget the host resolved, and the refusals a
// caller must render (busy, forbidden, unsupported) arrive as protocol errors
// with the documented codes rather than as transport failures.
package hostclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/hostapi"
	"github.com/roberChen/pigo/internal/hostserver"
)

// switchClientAgent is scriptedAgent plus the ModelSwitcher capability, so the
// client-side round trip can be exercised without a provider.
type switchClientAgent struct {
	*scriptedAgent

	mu       sync.Mutex
	model    string
	provider string
}

func (a *switchClientAgent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *switchClientAgent) Provider() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

func (a *switchClientAgent) SetModel(model string) (hostserver.ModelSwitch, error) {
	a.mu.Lock()
	a.model = model
	a.provider = "switchable"
	a.mu.Unlock()
	return hostserver.ModelSwitch{
		Model:         model,
		Provider:      "switchable",
		ContextWindow: 200000,
		InputBudget:   180000,
		TriggerPoint:  144000,
		TriggerSource: "ladder",
	}, nil
}

// switchableFactory builds switchClientAgents over the scripted reply.
type switchableFactory struct {
	reply string
	delay time.Duration
}

func (f *switchableFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	base := &scriptedAgent{
		spec:     spec,
		messages: append(agentcore.MessageList(nil), spec.Messages...),
		reply:    f.reply,
		delay:    f.delay,
	}
	return &switchClientAgent{scriptedAgent: base, model: spec.Model, provider: "scripted"}, nil
}

// TestSetModelRoundTrip checks the client wrapper end to end: the switch lands,
// the host's resolved budget comes back, and the session keeps running.
func TestSetModelRoundTrip(t *testing.T) {
	dir := startHost(t, &switchableFactory{reply: "ok"})
	c := connect(t, dir)

	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Prompt(context.Background(), sid.ID, "first"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	res, err := c.SetModel(sid.ID, "big-model")
	if err != nil {
		t.Fatalf("set model: %v", err)
	}
	if res.Session.ID != sid.ID {
		t.Fatalf("result session = %q, want %q", res.Session.ID, sid.ID)
	}
	if res.Session.Model != "big-model" || res.Session.Provider != "switchable" {
		t.Fatalf("session metadata = %+v", res.Session)
	}
	// The budget is the host's answer, surfaced unchanged.
	if res.ContextWindow != 200000 || res.InputBudget != 180000 || res.TriggerPoint != 144000 {
		t.Fatalf("budget = %+v", res)
	}
	if res.TriggerSource != "ladder" {
		t.Fatalf("trigger source = %q", res.TriggerSource)
	}

	// The session is still usable and still reports the new model.
	sessions, err := c.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Model != "big-model" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if _, err := c.Prompt(context.Background(), sid.ID, "second"); err != nil {
		t.Fatalf("prompt after switch: %v", err)
	}
}

// TestSetModelBusyWhilePromptRuns proves a mid-turn switch is refused with the
// code a UI can special-case, rather than silently racing the loop.
func TestSetModelBusyWhilePromptRuns(t *testing.T) {
	dir := startHost(t, &switchableFactory{reply: "slow", delay: 5 * time.Second})
	c := connect(t, dir)
	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Prompt(context.Background(), sid.ID, "long")
	}()
	waitFor(t, "the session to report running", func() bool {
		sessions, err := c.ListSessions()
		return err == nil && len(sessions) == 1 && sessions[0].Running
	})

	_, err = c.SetModel(sid.ID, "other")
	wantCode(t, err, hostapi.CodeBusy)

	if err := c.Cancel(sid.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	<-done
}

// TestSetModelForbiddenForAnotherClient proves connection isolation covers the
// switch: a second client cannot retarget someone else's session.
func TestSetModelForbiddenForAnotherClient(t *testing.T) {
	dir := startHost(t, &switchableFactory{reply: "ok"})
	owner := connect(t, dir)
	other := connect(t, dir)

	sid, err := owner.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = other.SetModel(sid.ID, "hijack")
	wantCode(t, err, hostapi.CodeForbidden)

	sessions, err := owner.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Model != "scripted-model" {
		t.Fatalf("forbidden switch took effect: %+v", sessions)
	}
}

// TestSetModelUnsupportedByHost proves the wrapper reports a host whose agent
// cannot switch, instead of pretending the change happened.
func TestSetModelUnsupportedByHost(t *testing.T) {
	dir := startHost(t, &scriptedFactory{reply: "ok"})
	c := connect(t, dir)
	sid, err := c.CreateSession(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = c.SetModel(sid.ID, "other")
	wantCode(t, err, hostapi.CodeUnavailable)
}
