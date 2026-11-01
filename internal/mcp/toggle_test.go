package mcp

// Tests for the runtime server switch (/mcp): Enable spawns and registers a
// server that started disabled, Disable unregisters and closes it, failures are
// recorded in Statuses instead of taking the session down, and the manager's
// dirty flag reports when the live tool set changed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/smallnest/pigo/internal/agenttool"
)

// switchManager connects a manager with two servers (one connected, one
// disabled) plus a third that fails to spawn, and returns it with a registry it
// is bound to, mirroring the TUI session assembly.
func switchManager(t *testing.T) (*Manager, *agenttool.ToolRegistry, *int) {
	t.Helper()
	original := transportFactory
	t.Cleanup(func() { transportFactory = original })

	var mu sync.Mutex
	spawned := 0
	transportFactory = func(cfg ServerConfig, _ io.Writer) (Transport, error) {
		if cfg.Name == "broken" {
			return nil, fmt.Errorf("cannot start")
		}
		f := newFakeTransport()
		f.handlers["initialize"] = func(json.RawMessage) (any, error) {
			return map[string]any{"protocolVersion": ProtocolVersion}, nil
		}
		f.handlers["tools/list"] = func(json.RawMessage) (any, error) {
			return map[string]any{"tools": []map[string]any{{"name": "run"}}}, nil
		}
		f.mu.Lock()
		f.calls = append(f.calls, cfg.Name)
		f.mu.Unlock()
		mu.Lock()
		spawned++
		mu.Unlock()
		return f, nil
	}

	m := Connect(context.Background(), []ServerConfig{
		{Name: "good", Command: "x"},
		{Name: "off", Command: "x", Disabled: true},
		{Name: "broken", Command: "x", Disabled: true},
	}, io.Discard)
	t.Cleanup(func() { _ = m.Close() })
	reg := agenttool.NewToolRegistry()
	for _, tool := range m.Tools() {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("seed registry: %v", err)
		}
	}
	m.BindRegistry(reg)
	return m, reg, &spawned
}

// TestManagerStatusesListEveryServer verifies Statuses covers connected and
// disabled servers in configuration order, with their tool counts.
func TestManagerStatusesListEveryServer(t *testing.T) {
	m, _, _ := switchManager(t)
	got := m.Statuses()
	if len(got) != 3 {
		t.Fatalf("statuses = %+v, want 3 entries", got)
	}
	if !got[0].Active || got[0].Tools != 1 {
		t.Errorf("good: %+v, want active with 1 tool", got[0])
	}
	if got[1].Active || got[1].Tools != 0 {
		t.Errorf("off: %+v, want inactive", got[1])
	}
	if got[2].Active {
		t.Errorf("broken: %+v, want inactive", got[2])
	}
	// A server that started disabled is never spawned, so there is no error to
	// report until /mcp actually tries to enable it.
	if got[2].Error != "" {
		t.Errorf("broken error = %q, want empty before the first attempt", got[2].Error)
	}
}

// TestManagerEnableDisableServer walks the toggle: Enable connects and registers
// the server's tools, Disable unregisters and releases them, and Refresh reports
// the change exactly once so the loop re-advertises the tool set.
func TestManagerEnableDisableServer(t *testing.T) {
	m, reg, spawned := switchManager(t)
	if m.Refresh(context.Background()) {
		t.Fatal("a fresh connect should not report a tool-set change")
	}

	if err := m.Enable(context.Background(), "off"); err != nil {
		t.Fatalf("Enable(off): %v", err)
	}
	if *spawned != 2 {
		t.Errorf("spawned = %d, want 2 (good + off)", *spawned)
	}
	if !m.Refresh(context.Background()) {
		t.Error("Refresh should report the Enable change")
	}
	if m.Refresh(context.Background()) {
		t.Error("Refresh should clear the dirty flag")
	}
	if got := toolNames(m.Tools()); len(got) != 2 || !strings.Contains(strings.Join(got, ","), "mcp__off__run") {
		t.Errorf("tools after Enable = %v", got)
	}
	if _, ok := reg.Get("mcp__off__run"); !ok {
		t.Error("Enable should register the server's tools in the bound registry")
	}
	for _, st := range m.Statuses() {
		if st.Name == "off" && (!st.Active || st.Tools != 1) {
			t.Errorf("off status after Enable = %+v", st)
		}
	}

	m.Disable("good")
	if !m.Refresh(context.Background()) {
		t.Error("Refresh should report the Disable change")
	}
	if got := toolNames(m.Tools()); len(got) != 1 || !strings.Contains(got[0], "mcp__off__run") {
		t.Errorf("tools after Disable = %v", got)
	}
	if _, ok := reg.Get("mcp__good__run"); ok {
		t.Error("Disable should unregister the server's tools")
	}
	// Disabling an already-off server is a no-op, not an error.
	m.Disable("good")
}

// TestManagerEnableReportsFailure verifies a server that cannot connect leaves
// the session usable and surfaces the error in its status.
func TestManagerEnableReportsFailure(t *testing.T) {
	m, reg, _ := switchManager(t)
	err := m.Enable(context.Background(), "broken")
	if err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("Enable(broken) error = %v, want the spawn failure", err)
	}
	// Nothing connected, so the live tool set is unchanged: no dirty flag.
	if m.Refresh(context.Background()) {
		t.Error("a failed Enable should not report a tool-set change")
	}
	for _, st := range m.Statuses() {
		if st.Name == "broken" {
			if st.Active {
				t.Error("broken server must not be active")
			}
			if !strings.Contains(st.Error, "cannot start") {
				t.Errorf("broken status error = %q", st.Error)
			}
		}
	}
	if _, ok := reg.Get("mcp__broken__run"); ok {
		t.Error("a failed Enable must not register tools")
	}
	if err := m.Enable(context.Background(), "nope"); err == nil {
		t.Error("Enable on an unknown server should error")
	}
}

// TestManagerEnableDisabledAtLaunchNoOp verifies enabling a server that is
// already active is harmless and does not spawn a second client.
func TestManagerEnableDisabledAtLaunchNoOp(t *testing.T) {
	m, _, spawned := switchManager(t)
	if err := m.Enable(context.Background(), "good"); err != nil {
		t.Fatalf("Enable(active): %v", err)
	}
	if *spawned != 1 {
		t.Errorf("spawned = %d, want 1 (no duplicate client)", *spawned)
	}
}

// TestManagerToolFilterHoldsForRuntimeEnable verifies the tool boundary
// (--allowed-tools/--disallowed-tools) is applied to tools discovered through
// Enable and through Tools()/BindRegistry, so switching a server on cannot
// widen a boundary that filtered the launch-time set.
func TestManagerToolFilterHoldsForRuntimeEnable(t *testing.T) {
	m, reg, _ := switchManager(t)
	// The assembly installs the policy after Connect, before AttachMCP binds the
	// registry; mimic that order.
	m.SetToolFilter(func(name string) bool { return name != "mcp__good__run" })
	if got := toolNames(m.Tools()); len(got) != 0 {
		t.Fatalf("Tools() must honor the filter, got %v", got)
	}
	if _, ok := reg.Get("mcp__good__run"); !ok {
		t.Fatal("precondition: the seed registry holds the tool")
	}
	// A fresh registry bound now (AttachMCP's order) only gets permitted tools.
	reg2 := agenttool.NewToolRegistry()
	m.BindRegistry(reg2)
	if _, ok := reg2.Get("mcp__good__run"); ok {
		t.Error("BindRegistry must not register a filtered-out tool")
	}

	// Enabling another server respects the same predicate: only its permitted
	// tools are exposed and registered.
	if err := m.Enable(context.Background(), "off"); err != nil {
		t.Fatalf("Enable(off): %v", err)
	}
	got := toolNames(m.Tools())
	if len(got) != 1 || !strings.Contains(got[0], "mcp__off__run") {
		t.Fatalf("Tools() after Enable = %v, want only mcp__off__run", got)
	}
	if _, ok := reg2.Get("mcp__off__run"); !ok {
		t.Error("permitted tool should be registered on Enable")
	}
	if _, ok := reg2.Get("mcp__good__run"); ok {
		t.Error("filtered tool must stay unregistered")
	}
}
