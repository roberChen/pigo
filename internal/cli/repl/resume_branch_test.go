// Regression tests for resuming and compacting a session: a resume must follow the
// session's active branch and continue from the last compaction checkpoint, replay
// must render assistant replies as Markdown like a live turn, and compacting must
// keep the history it summarizes on disk instead of rewriting the file.
package repl

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/session"
)

// saveBranchSession writes a four-message session, then branches from its first
// entry so the file holds two sibling branches: the abandoned tail (messages 2 and
// 3) and the active continuation ("branch reply").
func saveBranchSession(t *testing.T, store *session.Store) session.SessionHeader {
	t.Helper()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	header := session.SessionHeader{ID: "branch-session", CreatedAt: now, UpdatedAt: now, Model: "faux", Provider: "faux"}
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("root question")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("abandoned reply")}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("abandoned follow-up")}},
	}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, entries, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if _, err := store.AppendBranch(header, entries[0].ID, agentcore.MessageList{
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("branch reply")}},
	}); err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	return header
}

// TestREPLResumeFollowsActiveBranch verifies /resume loads the branch the session
// was left on: the replay shows it, the abandoned sibling branch does not leak into
// the conversation, and the next turn continues from the active leaf.
func TestREPLResumeFollowsActiveBranch(t *testing.T) {
	p := &replProvider{reply: "fresh reply"}
	deps, store := newTestDeps(t, p)
	header := saveBranchSession(t, store)

	var out bytes.Buffer
	input := "/resume " + header.ID + "\nnext question\n/exit\n"
	if err := runREPL(strings.NewReader(input), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "> root question") || !strings.Contains(got, "branch reply") {
		t.Fatalf("the replay should show the active branch:\n%s", got)
	}
	if strings.Contains(got, "abandoned reply") || strings.Contains(got, "abandoned follow-up") {
		t.Errorf("a resumed session must not replay an abandoned branch:\n%s", got)
	}

	// The turn that followed the resume descends from the active leaf, so the active
	// path is root → branch reply → new turn, with the abandoned tail still on disk
	// as a sibling.
	_, path, err := store.LoadPath(header.ID)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	var texts []string
	for _, e := range path {
		texts = append(texts, messageText(e.Message))
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "abandoned") {
		t.Errorf("the continued path contains the abandoned branch: %s", joined)
	}
	if !strings.Contains(joined, "branch reply") || !strings.Contains(joined, "next question") {
		t.Errorf("the continued path should be the active branch plus the new turn: %s", joined)
	}
}

// messageText flattens a message's text for assertions.
func messageText(m agentcore.Message) string {
	switch msg := m.(type) {
	case agentcore.UserMessage:
		return agentcore.ContentToText(msg.Content)
	case agentcore.AssistantMessage:
		return agentcore.ContentToText(msg.Content)
	case agentcore.CompactionMessage:
		return "compaction:" + msg.Summary
	default:
		return ""
	}
}

// TestPersistKeepsHistoryAfterCompaction verifies the persistence step after a
// compaction appends the compacted context instead of rewriting the session, so the
// summarized conversation survives on disk while the resumed context starts at the
// checkpoint.
func TestPersistKeepsHistoryAfterCompaction(t *testing.T) {
	deps, store := newTestDeps(t, &replProvider{reply: "x"})
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	header := session.SessionHeader{ID: "compact-session", CreatedAt: now, UpdatedAt: now}
	msgs := agentcore.MessageList{
		userText("one"), assistantText("two"), userText("three"), assistantText("four"),
	}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, entries, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}

	// What the loop leaves behind after compacting the first two messages.
	deps.header = header
	deps.agentCtx.Messages = agentcore.MessageList{
		compactionText("summary of one+two", 2),
		entries[2].Message,
		entries[3].Message,
	}
	deps.persisted = len(entries)
	deps.curLeaf = entries[len(entries)-1].ID

	var out bytes.Buffer
	cli.PersistTurn(&out, &deps)
	if strings.Contains(out.String(), "save failed") {
		t.Fatalf("persist failed:\n%s", out.String())
	}
	if deps.persisted != 3 {
		t.Errorf("persisted = %d, want the compacted context size (3)", deps.persisted)
	}

	_, all, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(all) != 7 {
		t.Fatalf("file holds %d entries, want 7 (4 history + checkpoint + 2 kept): %+v", len(all), all)
	}
	if !strings.Contains(strings.Join(entryTexts(all), "|"), "three") {
		t.Error("the summarized history should still be on disk")
	}

	ctx := session.MessagesSinceLastCompaction(session.Messages(mustLoadPath(t, store, header.ID)))
	if len(ctx) != 3 {
		t.Fatalf("resumed context = %d messages, want 3 (checkpoint + 2 kept)", len(ctx))
	}
	if _, ok := ctx[0].(agentcore.CompactionMessage); !ok {
		t.Errorf("resumed context should start with the checkpoint, got %T", ctx[0])
	}

	// A later persist descends from the appended tip, so nothing is orphaned.
	deps.agentCtx.Messages = append(deps.agentCtx.Messages, userText("five"))
	cli.PersistTurn(&out, &deps)
	path := mustLoadPath(t, store, header.ID)
	if got := messageText(path[len(path)-1].Message); got != "five" {
		t.Errorf("active path tip = %q, want the new message", got)
	}
	if deps.curLeaf != path[len(path)-1].ID {
		t.Errorf("curLeaf = %q, want the path tip %q", deps.curLeaf, path[len(path)-1].ID)
	}
}

// TestReplayTranscriptRendersMarkdown verifies replayed assistant replies go through
// the Markdown renderer, exactly like a live turn (the resume path used to print the
// raw source).
func TestReplayTranscriptRendersMarkdown(t *testing.T) {
	original := replayMarkdown
	replayMarkdown = func(src string) string { return "[md]" + src }
	t.Cleanup(func() { replayMarkdown = original })

	var out bytes.Buffer
	replayTranscript(&out, agentcore.MessageList{
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("**bold** and `code`")}},
	})
	if !strings.Contains(out.String(), "[md]**bold** and `code`") {
		t.Errorf("replay did not render Markdown:\n%s", out.String())
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Error("replay should end the block with a newline")
	}
}

// TestReplayTranscriptMarksCompaction verifies a replayed checkpoint is visible, so
// a resumed session explains the gap instead of silently starting mid-conversation.
func TestReplayTranscriptMarksCompaction(t *testing.T) {
	var out bytes.Buffer
	replayTranscript(&out, agentcore.MessageList{
		userText("old question"), assistantText("old reply"),
		compactionText("summary", 2),
		userText("new question"),
	})
	got := out.String()
	if !strings.Contains(got, "compacted here") {
		t.Errorf("replay should mark the compaction point:\n%s", got)
	}
	if !strings.Contains(got, "> old question") || !strings.Contains(got, "> new question") {
		t.Errorf("replay should show history on both sides of the checkpoint:\n%s", got)
	}
}

// mustLoadPath loads a session's active path, failing the test on error.
func mustLoadPath(t *testing.T, store *session.Store, id string) []session.Entry {
	t.Helper()
	_, path, err := store.LoadPath(id)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	return path
}

// entryTexts flattens the messages of entries for assertions.
func entryTexts(entries []session.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = messageText(e.Message)
	}
	return out
}

// userText / assistantText / compactionText build fixture messages.
func userText(s string) agentcore.Message {
	return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(s)}}
}

func assistantText(s string) agentcore.Message {
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(s)}}
}

func compactionText(summary string, covered int) agentcore.Message {
	return agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: summary, CoveredMessages: covered}
}
