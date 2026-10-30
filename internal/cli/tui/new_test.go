// Tests for the TUI's /new command: it starts a fresh empty session, keeps the
// configuration that is live now (not the launch-time copy), leaves the previous
// session on disk, and rejects a stray argument.
package tui

import (
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// conversationSession builds a TUI session that already has a turn in it.
func conversationSession(t *testing.T) *runSession {
	t.Helper()
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	s, _, err := newRunSessionWithStore(newTestStore(t), Options{Model: "launch-model", ProviderName: "launch-provider"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	s.agentCtx.Messages = agentcore.MessageList{agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
	}}
	s.persisted = 0 // force the switch to flush this turn
	s.curLeaf = "leaf-1"
	return s
}

// TestNewSessionStartsEmpty verifies /new mints a new session with an empty
// conversation, resets the branch cursor, and reports the new id.
func TestNewSessionStartsEmpty(t *testing.T) {
	s := conversationSession(t)
	oldID := s.header.ID
	m := NewModel(Options{Model: "launch-model"}).withSession(s, nil)

	next, _ := m.newSession("/new")
	got := next.(Model)

	if got.session == nil {
		t.Fatal("the model should still have a session")
	}
	if got.session.header.ID == oldID {
		t.Error("/new should mint a new session id")
	}
	if len(got.session.agentCtx.Messages) != 0 {
		t.Errorf("the new session should be empty, got %d messages", len(got.session.agentCtx.Messages))
	}
	if got.session.curLeaf != "" || got.session.persisted != 0 {
		t.Errorf("branch cursor = (%q,%d), want a fresh start", got.session.curLeaf, got.session.persisted)
	}
	if got.startRunFn == nil || got.live != got.session.live || got.slash != got.session.slash {
		t.Error("the model should be rebound to the new session")
	}
	if !strings.Contains(transcriptText(got.transcript), "new session "+got.session.header.ID) {
		t.Error("the transcript should report the new session id")
	}
	// The previous session is still on disk and loadable.
	if _, _, err := s.store.LoadEntries(oldID); err != nil {
		t.Errorf("the previous session should remain loadable: %v", err)
	}
}

// TestNewSessionKeepsLiveConfiguration verifies /new runs the configuration that is
// in effect now — after a /model switch — instead of the launch-time options.
func TestNewSessionKeepsLiveConfiguration(t *testing.T) {
	s := conversationSession(t)
	s.live.Model = "switched-model"
	s.live.ProviderName = "switched-provider"
	m := NewModel(Options{Model: "launch-model", ProviderName: "launch-provider"}).withSession(s, nil)

	next, _ := m.newSession("/new")
	got := next.(Model)

	if got.session.live.Model != "switched-model" || got.session.live.ProviderName != "switched-provider" {
		t.Errorf("new session runs %s/%s, want the switched config",
			got.session.live.Model, got.session.live.ProviderName)
	}
	if got.session.header.Model != "switched-model" {
		t.Errorf("header model = %q, want the live model", got.session.header.Model)
	}
}

// TestNewSessionRejectsArguments verifies "/new <arg>" is reported as a usage error
// and leaves the running session untouched.
func TestNewSessionRejectsArguments(t *testing.T) {
	s := conversationSession(t)
	oldID := s.header.ID
	m := NewModel(Options{Model: "launch-model"}).withSession(s, nil)

	next, _ := m.newSession("/new hello")
	got := next.(Model)

	if got.session.header.ID != oldID {
		t.Error("a rejected /new must not switch sessions")
	}
	if len(got.session.agentCtx.Messages) == 0 {
		t.Error("a rejected /new must not clear the conversation")
	}
	if !strings.Contains(transcriptText(got.transcript), "usage: /new") {
		t.Error("the transcript should explain the usage")
	}
}

// transcriptText joins the transcript's block texts for assertions.
func transcriptText(tr transcript) string {
	parts := make([]string, 0, len(tr.blocks))
	for _, block := range tr.blocks {
		parts = append(parts, block.text)
	}
	return strings.Join(parts, "\n")
}
