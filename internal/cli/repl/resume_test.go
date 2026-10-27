package repl

// Tests for /resume (FR-16): listing the stored sessions and loading a chosen
// one into the live REPL session.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// saveResumeSession writes a small two-message session into store under an
// explicit id and update time, so the listing order is deterministic.
func saveResumeSession(t *testing.T, store *session.Store, id string, updated time.Time, prompt, reply string) {
	t.Helper()
	header := session.SessionHeader{
		ID:        id,
		CreatedAt: updated,
		UpdatedAt: updated,
		Model:     "faux",
		Provider:  "faux",
	}
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(prompt)}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(reply)}},
	}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

// TestREPLResumeListsAndLoads drives /resume on an empty argument (list) and
// with a selection (load), asserting the list is newest-first and numbered and
// that loading swaps the live session and replays its transcript.
func TestREPLResumeListsAndLoads(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, store := newTestDeps(t, p)

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	olderID := "20260710-110000-000001"
	newerID := "20260710-120000-000002"
	saveResumeSession(t, store, olderID, base.Add(-time.Hour), "older question", "older answer")
	saveResumeSession(t, store, newerID, base, "newest question", "newest answer")

	var out bytes.Buffer
	input := "/resume\n/resume 2\n/resume 9\n/resume missing-id\n/exit\n"
	if err := runREPL(strings.NewReader(input), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "stored sessions (run /resume <n> to load one):") {
		t.Fatalf("bare /resume did not print the listing:\n%s", got)
	}
	newestLine, olderLine := strings.Index(got, "1. "), strings.Index(got, "2. ")
	if newestLine < 0 || olderLine < 0 || newestLine > olderLine {
		t.Fatalf("listing is not numbered newest-first:\n%s", got)
	}
	if !strings.Contains(got[newestLine:olderLine], "newest question") {
		t.Errorf("row 1 does not describe the newest session:\n%s", got)
	}
	if !strings.Contains(got[olderLine:], "older question") {
		t.Errorf("row 2 does not describe the older session:\n%s", got)
	}

	if !strings.Contains(got, "resumed session "+olderID+" (2 messages)") {
		t.Errorf("/resume 2 did not load the older session:\n%s", got)
	}
	if !strings.Contains(got, "> older question") || !strings.Contains(got, "older answer") {
		t.Errorf("/resume 2 did not replay the resumed transcript:\n%s", got)
	}
	if !strings.Contains(got, `no session "9"`) {
		t.Errorf("out-of-range selection was not reported:\n%s", got)
	}
	if !strings.Contains(got, `no session "missing-id"`) {
		t.Errorf("unknown id was not reported:\n%s", got)
	}
	if p.calls != 0 {
		t.Errorf("/resume must not launch an agent run, got %d calls", p.calls)
	}
}

// TestREPLResumeAcceptsSessionID verifies a full session id selects that session
// directly, without printing the listing first.
func TestREPLResumeAcceptsSessionID(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, store := newTestDeps(t, p)
	id := "20260710-120000-000003"
	saveResumeSession(t, store, id, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), "by id question", "by id answer")

	var out bytes.Buffer
	if err := runREPL(strings.NewReader("/resume "+id+"\n/exit\n"), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "stored sessions") {
		t.Errorf("an id argument should load directly, not list:\n%s", got)
	}
	if !strings.Contains(got, "resumed session "+id) || !strings.Contains(got, "> by id question") {
		t.Errorf("/resume <id> did not load and replay the session:\n%s", got)
	}
}
