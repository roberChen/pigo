// Regression tests for resuming in the TUI: the active branch is what loads, the
// agent continues from the last compaction checkpoint while the replay shows the
// whole history, and the checkpoint is visible in the transcript.
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// saveCompactedBranchSession writes a session with an abandoned branch and a
// compaction checkpoint on the active one, returning its id:
//
//	root question -> abandoned reply            (sibling branch)
//	root question -> compaction -> kept reply   (active branch)
func saveCompactedBranchSession(t *testing.T, store *session.Store) string {
	t.Helper()
	now := time.Date(2026, 7, 12, 11, 0, 0, 0, time.UTC)
	header := session.SessionHeader{ID: "tui-compacted", CreatedAt: now, UpdatedAt: now}
	if err := store.Save(header, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("root question")}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, entries, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	// Abandoned branch: a reply the user rewound away from.
	if _, err := store.AppendBranch(header, entries[0].ID, agentcore.MessageList{
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("abandoned reply")}},
	}); err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	// Active branch: a compaction checkpoint (covering the root question), then the
	// retained reply and a newer turn.
	if _, err := store.AppendBranch(header, entries[0].ID, agentcore.MessageList{
		agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "summary of the root question", CoveredMessages: 1},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("kept reply")}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("later question")}},
	}); err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	return header.ID
}

// TestResumeLoadsActiveBranchAndTrimsContext verifies a resumed TUI session replays
// the active branch's full history while the agent's context starts at the last
// compaction checkpoint, and that the checkpoint is visible in the transcript.
func TestResumeLoadsActiveBranchAndTrimsContext(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	store := newTestStore(t)
	id := saveCompactedBranchSession(t, store)

	s, history, err := newRunSessionWithStore(store, Options{ResumeID: id, Model: "m", ProviderName: "p"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}

	// The replay is the active branch, without the abandoned sibling.
	var replayed []string
	for _, m := range history {
		switch msg := m.(type) {
		case agentcore.UserMessage:
			replayed = append(replayed, agentcore.ContentToText(msg.Content))
		case agentcore.AssistantMessage:
			replayed = append(replayed, agentcore.ContentToText(msg.Content))
		case agentcore.CompactionMessage:
			replayed = append(replayed, "compaction")
		}
	}
	joined := strings.Join(replayed, "|")
	if strings.Contains(joined, "abandoned") {
		t.Errorf("replay contains the abandoned branch: %s", joined)
	}
	if !strings.Contains(joined, "root question") || !strings.Contains(joined, "kept reply") || !strings.Contains(joined, "later question") {
		t.Errorf("replay should show the active branch: %s", joined)
	}

	// The context starts at the checkpoint: the summarized root question is history,
	// not context.
	if len(s.agentCtx.Messages) != 3 {
		t.Fatalf("context = %d messages, want 3 (checkpoint + 2)", len(s.agentCtx.Messages))
	}
	if _, ok := s.agentCtx.Messages[0].(agentcore.CompactionMessage); !ok {
		t.Errorf("context should start with the checkpoint, got %T", s.agentCtx.Messages[0])
	}
	if s.persisted != len(s.agentCtx.Messages) {
		t.Errorf("persisted = %d, want the context size %d", s.persisted, len(s.agentCtx.Messages))
	}
	if s.curLeaf == "" {
		t.Error("the resumed session should have an active leaf")
	}
	if _, path, err := store.LoadPath(id); err != nil || s.curLeaf != path[len(path)-1].ID {
		t.Errorf("curLeaf = %q, want the path tip (err=%v)", s.curLeaf, err)
	}

	// The transcript explains the gap rather than silently starting mid-conversation.
	tr := newTranscript(DefaultTheme())
	seedTranscript(&tr, history)
	if !strings.Contains(transcriptText(tr), "compacted here") {
		t.Errorf("replay should mark the compaction point:\n%s", transcriptText(tr))
	}
}
