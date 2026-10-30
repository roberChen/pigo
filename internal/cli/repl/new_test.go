// Tests for the REPL's /new command: a fresh session id, an empty conversation, the
// session-scoped bookkeeping reset, the outgoing turn flushed to disk, and
// SessionStart re-dispatched for the new session.
package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/session"
)

// newSessionDeps builds a REPL session mid-conversation: a persisted turn, a branch
// cursor, a side thread and some telemetry, so /new has everything to reset.
func newSessionDeps(t *testing.T) *replDeps {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := time.Now().UTC()
	projectDir := t.TempDir()
	header := session.SessionHeader{
		ID:           "old-session",
		CreatedAt:    now,
		UpdatedAt:    now,
		Model:        "launch-model",
		Provider:     "launch-provider",
		SystemPrompt: "system prompt",
		Cwd:          projectDir,
	}
	msgs := agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, entries, err := store.LoadEntries(header.ID)
	if err != nil || len(entries) == 0 {
		t.Fatalf("LoadEntries: %v (%d entries)", err, len(entries))
	}
	return &replDeps{
		store:     store,
		header:    header,
		agentCtx:  &agentcore.AgentContext{SystemPrompt: "system prompt", Messages: msgs},
		live:      &cli.LiveConfig{Model: "live-model", ProviderName: "live-provider"},
		cwd:       projectDir,
		persisted: len(entries),
		curLeaf:   entries[len(entries)-1].ID,
		lastBtw:   &agentcore.AgentContext{},
		telemetry: cli.NewTelemetryHolder(),
		hookDeps:  run.HookDeps{SessionID: "old-session", ProjectDir: projectDir},
	}
}

// TestRunNewStartsEmptySession verifies /new mints a new session, empties the
// conversation and resets the session-scoped bookkeeping while keeping the system
// prompt and the live model/provider.
func TestRunNewStartsEmptySession(t *testing.T) {
	deps := newSessionDeps(t)
	var out bytes.Buffer
	runNew(&out, deps, "/new")

	if deps.header.ID == "old-session" {
		t.Error("/new should mint a new session id")
	}
	if deps.header.CreatedAt.IsZero() {
		t.Error("the new session should carry a creation time")
	}
	if len(deps.agentCtx.Messages) != 0 {
		t.Errorf("the new session should be empty, got %d messages", len(deps.agentCtx.Messages))
	}
	if deps.agentCtx.SystemPrompt != "system prompt" {
		t.Errorf("system prompt = %q, want it preserved", deps.agentCtx.SystemPrompt)
	}
	if deps.header.Model != "live-model" || deps.header.Provider != "live-provider" {
		t.Errorf("header records %s/%s, want the live model/provider", deps.header.Model, deps.header.Provider)
	}
	if deps.header.Cwd == "" {
		t.Error("the new session should keep the launch directory")
	}
	if deps.persisted != 0 || deps.curLeaf != "" {
		t.Errorf("branch cursor = (%d,%q), want a fresh start", deps.persisted, deps.curLeaf)
	}
	if deps.lastBtw != nil {
		t.Error("the side thread belongs to the previous conversation and should be dropped")
	}
	if deps.hookDeps.SessionID != deps.header.ID {
		t.Errorf("hook session id = %q, want the new session id", deps.hookDeps.SessionID)
	}
	if !strings.Contains(out.String(), "new session "+deps.header.ID) {
		t.Errorf("output should name the new session:\n%s", out.String())
	}
	// The outgoing session is still on disk and resumable.
	if _, _, err := deps.store.LoadEntries("old-session"); err != nil {
		t.Errorf("the previous session should remain loadable: %v", err)
	}
}

// TestRunNewRedispatchesSessionStart verifies the new session is announced to hooks
// (a launch dispatches SessionStart too), so context-injecting hooks run for it.
func TestRunNewRedispatchesSessionStart(t *testing.T) {
	deps := newSessionDeps(t)
	logPath := filepath.Join(t.TempDir(), "hooks.log")
	script := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	deps.dispatcher = run.BuildDispatcher(hooks.HookSet{
		"SessionStart": {{Hooks: []hooks.HookConfig{{Command: script}}}},
	}, deps.hookDeps)

	var out bytes.Buffer
	runNew(&out, deps, "/new")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("hook did not run: %v", err)
	}
	if !strings.Contains(string(data), deps.header.ID) {
		t.Errorf("hook input should carry the new session id %q, got:\n%s", deps.header.ID, data)
	}
}

// TestRunNewRejectsArguments verifies a stray argument is reported instead of
// silently starting a session, and that nothing was reset.
func TestRunNewRejectsArguments(t *testing.T) {
	deps := newSessionDeps(t)
	var out bytes.Buffer
	runNew(&out, deps, "/new hello")

	if deps.header.ID != "old-session" {
		t.Error("a rejected /new must not switch sessions")
	}
	if len(deps.agentCtx.Messages) == 0 {
		t.Error("a rejected /new must not clear the conversation")
	}
	if !strings.Contains(out.String(), "usage: /new") {
		t.Errorf("output should explain the usage:\n%s", out.String())
	}
}
