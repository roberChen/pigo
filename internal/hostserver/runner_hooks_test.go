// This file tests the host's hook wiring (#424, #425): a host session must fire
// the same observer hook points (SessionEnd/PreCompact) as the REPL, TUI and
// headless drivers. The seam is chainEventObserver, because the alternative —
// passing a nil event handler to run.InstallDriverHooks — silently drops those
// two hook points for every host session and is invisible at runtime.
package hostserver

import (
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/runtime"
)

// TestChainEventObserverKeepsCallerHandlerFirst pins the ordering contract: the
// driver's own handler owns the protocol event stream and runs before the hook
// observer, and both see the same event.
func TestChainEventObserverKeepsCallerHandlerFirst(t *testing.T) {
	var order []string
	h := runtime.StreamHandler{OnEvent: func(agentcore.AgentEvent) {
		order = append(order, "caller")
	}}
	ev := agentcore.TurnStartEvent{}

	got := chainEventObserver(h, func(agentcore.AgentEvent) {
		order = append(order, "hook")
	})
	if got.OnEvent == nil {
		t.Fatal("chained handler is nil")
	}
	got.OnEvent(ev)

	if len(order) != 2 || order[0] != "caller" || order[1] != "hook" {
		t.Fatalf("call order = %v, want [caller hook]", order)
	}
}

// TestChainEventObserverNilObserverIsIdentity is the no-hooks fast path: with no
// observer the handler must be returned untouched, so a run without hooks
// behaves exactly as it did before hooks existed (FR-18).
func TestChainEventObserverNilObserverIsIdentity(t *testing.T) {
	called := false
	h := runtime.StreamHandler{OnEvent: func(agentcore.AgentEvent) { called = true }}

	got := chainEventObserver(h, nil)
	got.OnEvent(agentcore.TurnStartEvent{})
	if !called {
		t.Fatal("caller handler was not preserved")
	}
}

// TestChainEventObserverWithoutCallerHandler covers the other half of the chain:
// a caller that installs no OnEvent still receives hook events.
func TestChainEventObserverWithoutCallerHandler(t *testing.T) {
	var seen int
	got := chainEventObserver(runtime.StreamHandler{}, func(agentcore.AgentEvent) { seen++ })
	if got.OnEvent == nil {
		t.Fatal("chained handler is nil with no caller handler")
	}
	got.OnEvent(agentcore.TurnStartEvent{})
	if seen != 1 {
		t.Fatalf("observer calls = %d, want 1", seen)
	}
}
