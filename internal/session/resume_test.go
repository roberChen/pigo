// Tests for reading a session back as a conversation: the active branch (not every
// abandoned branch in the file), the context that starts at the last compaction
// checkpoint, and persisting a compacted context without deleting the history it
// summarizes.
package session

import (
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// userMsg / assistantMsg build simple messages for fixtures.
func userMsg(text string) agentcore.Message {
	return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}

func assistantMsg(text string) agentcore.Message {
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}

// compactedMsg builds a checkpoint that covers n messages.
func compactedMsg(summary string, covered int) agentcore.Message {
	return agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: summary, CoveredMessages: covered}
}

// newTestStore opens a store rooted at a temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// saveLinear writes a linear session with the given messages and returns its
// header and entries.
func saveLinear(t *testing.T, store *Store, msgs ...agentcore.Message) (SessionHeader, []Entry) {
	t.Helper()
	now := time.Now().UTC()
	header := SessionHeader{ID: NewID(now), CreatedAt: now, UpdatedAt: now}
	if err := store.Save(header, agentcore.MessageList(msgs)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, entries, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	return header, entries
}

// TestLoadPathFollowsActiveBranch verifies a resumed session is the conversation on
// the active branch, not every entry in the file: after a leaf switch the file holds
// two sibling branches, and only the one that was continued belongs to the session.
func TestLoadPathFollowsActiveBranch(t *testing.T) {
	store := newTestStore(t)
	header, entries := saveLinear(t, store, userMsg("hello"), assistantMsg("hi"), userMsg("second"))
	if len(entries) != 3 {
		t.Fatalf("saved %d entries, want 3", len(entries))
	}

	// Continue from the first entry: the old tail (entries[1:]) becomes an abandoned
	// sibling branch, and the new turn is the active one.
	if _, err := store.AppendBranch(header, entries[0].ID, agentcore.MessageList{assistantMsg("branch reply")}); err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	_, all, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("file holds %d entries, want 4 (both branches)", len(all))
	}

	h, path, err := store.LoadPath(header.ID)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	if h.ID != header.ID {
		t.Errorf("LoadPath header = %q, want %q", h.ID, header.ID)
	}
	if len(path) != 2 {
		t.Fatalf("LoadPath returned %d entries, want the 2 on the active branch: %+v", len(path), path)
	}
	if got := agentcore.ContentToText(path[1].Message.(agentcore.AssistantMessage).Content); got != "branch reply" {
		t.Errorf("active branch tail = %q, want the continued reply", got)
	}
	if tip := TipLeaf(all); tip != TipLeaf(path) {
		t.Errorf("TipLeaf = %q, want the last entry written %q", TipLeaf(all), TipLeaf(path))
	}
}

// TestMessagesSinceLastCompaction verifies the resumed context starts at the last
// checkpoint, while an uncompacted conversation is returned whole.
func TestMessagesSinceLastCompaction(t *testing.T) {
	plain := agentcore.MessageList{userMsg("a"), assistantMsg("b")}
	if got := MessagesSinceLastCompaction(plain); len(got) != 2 {
		t.Errorf("uncompacted list = %d messages, want all 2", len(got))
	}

	compacted := agentcore.MessageList{
		userMsg("old"), assistantMsg("old reply"),
		compactedMsg("summary one", 2),
		userMsg("new"), assistantMsg("new reply"),
	}
	got := MessagesSinceLastCompaction(compacted)
	if len(got) != 3 {
		t.Fatalf("context = %d messages, want 3 (checkpoint + 2)", len(got))
	}
	if _, ok := got[0].(agentcore.CompactionMessage); !ok {
		t.Errorf("context should start with the checkpoint, got %T", got[0])
	}

	// A second compaction wins: the context starts at the newest checkpoint.
	double := append(append(agentcore.MessageList{}, compacted...), compactedMsg("summary two", 3), userMsg("later"))
	got = MessagesSinceLastCompaction(double)
	if len(got) != 2 {
		t.Fatalf("context after a second compaction = %d messages, want 2", len(got))
	}
	if cp, ok := got[0].(agentcore.CompactionMessage); !ok || cp.Summary != "summary two" {
		t.Errorf("context should start at the newest checkpoint, got %+v", got[0])
	}
}

// TestAppendCompactedContextKeepsHistory verifies persisting a compacted context
// attaches it to the tree instead of rewriting the file, so the summarized
// conversation stays on disk and the active path is checkpoint → tail → later.
func TestAppendCompactedContextKeepsHistory(t *testing.T) {
	store := newTestStore(t)
	header, entries := saveLinear(t, store, userMsg("one"), assistantMsg("two"), userMsg("three"), assistantMsg("four"))

	// Compaction over the first two messages keeps the last two.
	compacted := agentcore.MessageList{
		compactedMsg("summary of one+two", 2),
		entries[2].Message,
		entries[3].Message,
		userMsg("five"),
	}
	leaf, appended, err := store.AppendCompactedContext(header, TipLeaf(entries), compacted)
	if err != nil {
		t.Fatalf("AppendCompactedContext: %v", err)
	}
	if !appended {
		t.Fatal("a checkpoint with a covered count should be attached, not rewritten")
	}

	_, all, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	// 4 original entries + checkpoint + 3 appended (kept tail + new message).
	if len(all) != 8 {
		t.Fatalf("file holds %d entries, want 8 (history kept): %+v", len(all), all)
	}

	_, path, err := store.LoadPath(header.ID)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	if len(path) != 6 {
		t.Fatalf("active path = %d entries, want 6 (2 summarized + checkpoint + 3): %+v", len(path), path)
	}
	if path[len(path)-1].ID != leaf {
		t.Errorf("path tip = %q, want the returned leaf %q", path[len(path)-1].ID, leaf)
	}
	// The summarized prefix is still the head of the active path, and the checkpoint
	// sits right after it.
	if _, ok := path[2].Message.(agentcore.CompactionMessage); !ok {
		t.Errorf("checkpoint should follow the summarized prefix, got %T at index 2", path[2].Message)
	}
	// The resumed context is the checkpoint + what followed it: the pre-compaction
	// turns are history, not tokens to resend.
	ctx := MessagesSinceLastCompaction(Messages(path))
	if len(ctx) != 4 {
		t.Fatalf("resumed context = %d messages, want 4: %+v", len(ctx), ctx)
	}
	if got := agentcore.ContentToText(ctx[len(ctx)-1].(agentcore.UserMessage).Content); got != "five" {
		t.Errorf("context tail = %q, want the newest message", got)
	}
}

// TestAppendCompactedContextFallsBack verifies the cases where the checkpoint cannot
// be attached (no covered count, or a prefix that is not on the path) report
// "not appended" so the caller rewrites instead of corrupting the tree.
func TestAppendCompactedContextFallsBack(t *testing.T) {
	store := newTestStore(t)
	header, entries := saveLinear(t, store, userMsg("one"), assistantMsg("two"))

	if _, appended, err := store.AppendCompactedContext(header, TipLeaf(entries), agentcore.MessageList{
		compactedMsg("no count", 0), entries[1].Message,
	}); err != nil || appended {
		t.Errorf("a checkpoint without a covered count should not be attached (appended=%v, err=%v)", appended, err)
	}
	// An over-count (a compaction that summarized messages of the turn in progress,
	// never persisted) clamps to the last persisted entry instead of rewriting: those
	// messages were never in the file, so nothing is lost.
	leaf, appended, err := store.AppendCompactedContext(header, TipLeaf(entries), agentcore.MessageList{
		compactedMsg("over-counted", 99), entries[1].Message,
	})
	if err != nil {
		t.Fatalf("over-counted append: %v", err)
	}
	if !appended {
		t.Error("an over-counted checkpoint should clamp and append, not force a rewrite")
	}
	_, path, err := store.LoadPath(header.ID)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	if got := path[len(path)-1].ID; got != leaf {
		t.Errorf("path tip = %q, want the appended leaf %q", got, leaf)
	}
	if _, appended, err := store.AppendCompactedContext(header, TipLeaf(entries), agentcore.MessageList{
		entries[0].Message,
	}); err != nil || appended {
		t.Errorf("a context without a checkpoint should not be attached (appended=%v, err=%v)", appended, err)
	}
}

// TestAppendCompactedContextAfterResume verifies a second compaction attaches at
// the right node. Once a session has been resumed, its context starts at the
// previous checkpoint while the file still holds the summarized history, so the cut
// point must be translated into a position on the on-disk path — otherwise the new
// checkpoint branches off the wrong message and silently drops both the earlier
// checkpoint and the messages between them.
func TestAppendCompactedContextAfterResume(t *testing.T) {
	store := newTestStore(t)
	header, entries := saveLinear(t, store, userMsg("one"), assistantMsg("two"), userMsg("three"), assistantMsg("four"))

	// First compaction: summarize the first two, keep the last two.
	first := agentcore.MessageList{
		compactedMsg("summary one", 2),
		entries[2].Message,
		entries[3].Message,
	}
	leaf, appended, err := store.AppendCompactedContext(header, TipLeaf(entries), first)
	if err != nil || !appended {
		t.Fatalf("first compaction: appended=%v err=%v", appended, err)
	}

	// Resume: the context is checkpoint one + the two kept messages.
	_, path, err := store.LoadPath(header.ID)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	ctx := MessagesSinceLastCompaction(Messages(path))
	if len(ctx) != 3 {
		t.Fatalf("resumed context = %d messages, want 3", len(ctx))
	}

	// Second compaction over the resumed context: compress the checkpoint and the
	// first kept message, keep "four".
	second := agentcore.MessageList{
		compactedMsg("summary two", 2),
		ctx[2],
	}
	leaf2, appended, err := store.AppendCompactedContext(header, leaf, second)
	if err != nil || !appended {
		t.Fatalf("second compaction: appended=%v err=%v", appended, err)
	}

	_, path2, err := store.LoadPath(header.ID)
	if err != nil {
		t.Fatalf("LoadPath after second compaction: %v", err)
	}
	if got := path2[len(path2)-1].ID; got != leaf2 {
		t.Errorf("path tip = %q, want the new leaf %q", got, leaf2)
	}
	ctx2 := MessagesSinceLastCompaction(Messages(path2))
	if len(ctx2) != 2 {
		t.Fatalf("context after the second compaction = %d messages, want 2 (checkpoint + kept)", len(ctx2))
	}
	if cp, ok := ctx2[0].(agentcore.CompactionMessage); !ok || cp.Summary != "summary two" {
		t.Errorf("context should start with the newest checkpoint, got %+v", ctx2[0])
	}
	if got := agentcore.ContentToText(ctx2[1].(agentcore.AssistantMessage).Content); got != "four" {
		t.Errorf("context tail = %q, want the retained message", got)
	}
	// The first checkpoint is still history on the path (nothing was re-parented
	// into a sibling branch), and all original history is still on disk.
	joined := ""
	for _, e := range path2 {
		if m, ok := e.Message.(agentcore.CompactionMessage); ok {
			joined += m.Summary + "|"
		}
	}
	if !strings.Contains(joined, "summary one") {
		t.Errorf("the earlier checkpoint should remain in the history: %s", joined)
	}
	_, all, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(all) < len(entries)+len(first)+len(second) {
		t.Errorf("file holds %d entries, want at least %d", len(all), len(entries)+len(first)+len(second))
	}
}
