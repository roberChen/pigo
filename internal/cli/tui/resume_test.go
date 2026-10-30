package tui

// Tests for the /resume session picker (FR-16): listing stored sessions in an
// overlay and loading a chosen one into the live TUI session.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// saveResumeSession writes a small two-message session under an explicit id and
// update time so the listing order is deterministic.
func saveResumeSession(t *testing.T, store *session.Store, id string, updated time.Time, prompt, reply string) {
	t.Helper()
	header := session.SessionHeader{
		ID:        id,
		CreatedAt: updated,
		UpdatedAt: updated,
		Model:     "test-model",
		Provider:  "test-provider",
	}
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(prompt)}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(reply)}},
	}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

// resumeTestModel builds a TUI model bound to the given stored session, sized so
// overlays and the transcript render normally.
func resumeTestModel(t *testing.T, store *session.Store, resumeID string) Model {
	t.Helper()
	s, history, err := newRunSessionWithStore(store, Options{ResumeID: resumeID})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	m := NewModel(Options{}).withSession(s, history)
	return apply(t, m, tea.WindowSizeMsg{Width: 90, Height: 24})
}

// TestResumePickerListsAndLoads opens the picker with a bare /resume, checks the
// rows are newest-first, moves the selection, and loads the highlighted session:
// the live session, transcript and picker state must all follow.
func TestResumePickerListsAndLoads(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	olderID := "20260710-110000-000001"
	newerID := "20260710-120000-000002"
	saveResumeSession(t, store, olderID, base.Add(-time.Hour), "older question", "older answer")
	saveResumeSession(t, store, newerID, base, "newest question", "newest answer")

	m := resumeTestModel(t, store, newerID)

	next, cmd := m.runSlash("/resume")
	m = next.(Model)
	if cmd == nil {
		t.Fatal("bare /resume should schedule a session-list load")
	}
	msg := cmd()
	if _, ok := msg.(sessionListMsg); !ok {
		t.Fatalf("session load returned %T, want sessionListMsg", msg)
	}
	m = apply(t, m, msg)

	if !m.picker.active {
		t.Fatalf("picker should be open after the list loads; transcript:\n%s", stripANSI(m.View().Content))
	}
	if got := m.picker.rows(); got != 2 {
		t.Errorf("picker rows = %d, want 2", got)
	}
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "newest question") || !strings.Contains(content, "older question") {
		t.Fatalf("picker overlay should describe both sessions:\n%s", content)
	}
	if !strings.Contains(content, "(current)") {
		t.Errorf("the session already loaded should be tagged in the picker:\n%s", content)
	}

	// Row 1 is the newest session; move down to the older one and load it.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.picker.active {
		t.Error("picker should close once a session is loaded")
	}
	if m.session == nil || m.session.header.ID != olderID {
		t.Fatalf("live session = %v, want %s", m.session, olderID)
	}
	content = stripANSI(m.View().Content)
	if !strings.Contains(content, "older question") {
		t.Fatalf("resumed conversation is not in the transcript:\n%s", content)
	}
	if strings.Contains(content, "newest question") {
		t.Errorf("the previous session's conversation should be cleared:\n%s", content)
	}
	if !strings.Contains(content, "resumed session "+olderID+" (2 messages)") {
		t.Errorf("resume outcome not reported:\n%s", content)
	}
}

// TestResumeByID verifies "/resume <id>" switches straight to that session
// without opening the picker.
func TestResumeByID(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	targetID := "20260710-130000-000003"
	saveResumeSession(t, store, targetID, base, "target question", "target answer")
	currentID := "20260710-140000-000004"
	saveResumeSession(t, store, currentID, base.Add(time.Hour), "current question", "current answer")

	m := resumeTestModel(t, store, currentID)
	next, _ := m.runSlash("/resume " + targetID)
	m = next.(Model)

	if m.picker.active {
		t.Error("an id argument should not open the picker")
	}
	if m.session.header.ID != targetID {
		t.Fatalf("live session = %s, want %s", m.session.header.ID, targetID)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "target question") {
		t.Fatalf("resumed conversation is not in the transcript:\n%s", content)
	}

	// An unknown selection is reported instead of switching.
	next, _ = m.runSlash("/resume no-such-session")
	m = next.(Model)
	if m.session.header.ID != targetID {
		t.Errorf("an unknown id must not change the session, got %s", m.session.header.ID)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "no session") {
		t.Errorf("unknown selection not reported:\n%s", content)
	}
}

// TestResumePickerIsModal verifies the overlay swallows editing keys and that Esc
// dismisses it without touching the composer.
func TestResumePickerIsModal(t *testing.T) {
	store := newTestStore(t)
	id := "20260710-120000-000005"
	saveResumeSession(t, store, id, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), "q", "a")
	m := resumeTestModel(t, store, id)

	m.picker.open([]session.SessionSummary{{Header: session.SessionHeader{ID: id}, Preview: "q"}}, "")
	m = apply(t, m, tea.WindowSizeMsg{Width: 90, Height: 24})

	m = apply(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := m.input.Value(); got != "" {
		t.Errorf("typing while the picker is open changed the composer: %q", got)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.picker.active {
		t.Error("Esc should dismiss the picker")
	}
	if m.session.header.ID != id {
		t.Errorf("Esc must not switch sessions, got %s", m.session.header.ID)
	}
}

// TestResumeWithoutSession checks a session-less model reports that resume is
// unavailable instead of panicking.
func TestResumeWithoutSession(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 16})
	next, cmd := m.runSlash("/resume")
	m = next.(Model)
	if cmd != nil {
		t.Error("a session-less model should not schedule a load")
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "resume unavailable") {
		t.Errorf("missing the unavailable notice:\n%s", content)
	}
}

// TestResumePickerKeepsShellHeight verifies the overlay's rows are reserved out of
// the transcript height, so the shell keeps exactly Height rows while the picker
// is open (the same invariant the slash menu and sub-agent panel uphold).
func TestResumePickerKeepsShellHeight(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 20})
	base := m.transcript.viewportHeight()

	sums := make([]session.SessionSummary, 4)
	for i := range sums {
		sums[i] = session.SessionSummary{Header: session.SessionHeader{ID: fmt.Sprintf("s%d", i)}, Preview: "p"}
	}
	m.picker.open(sums, "")
	m.relayout()
	if got := m.transcript.viewportHeight(); got != base-4 {
		t.Errorf("transcript height with the picker open = %d, want %d (base %d - 4)", got, base-4, base)
	}
	if got := strings.Count(m.View().Content, "\n"); got != 19 {
		t.Errorf("newline count = %d, want 19 (20 rows)", got)
	}
}

// TestResumeNotesRecordedProject verifies resuming a session recorded in another
// directory warns that the tools keep operating on the launch directory, so a
// surprising "file not found" after a switch is explained up front.
func TestResumeNotesRecordedProject(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	elsewhereID := "20260710-120000-000010"
	header := session.SessionHeader{
		ID:        elsewhereID,
		CreatedAt: base,
		UpdatedAt: base,
		Cwd:       "/elsewhere/other-project",
	}
	if err := store.Save(header, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("other question")}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	currentID := "20260710-130000-000011"
	saveResumeSession(t, store, currentID, base.Add(time.Hour), "current question", "current answer")

	m := resumeTestModel(t, store, currentID)
	next, _ := m.runSlash("/resume " + elsewhereID)
	m = next.(Model)

	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "recorded in /elsewhere/other-project") {
		t.Fatalf("missing the cross-project note:\n%s", content)
	}
	if !strings.Contains(content, "tools still run in") {
		t.Fatalf("note should name the directory tools run in:\n%s", content)
	}
}

// TestResumeRendersToolCards verifies a restored conversation shows the tool
// calls as the same rich cards a live run produced, in the rendered view: the
// call's arguments in the header, its response tree, and the success mark. The
// last replayed card is also wired for Ctrl+O.
func TestResumeRendersToolCards(t *testing.T) {
	store := newTestStore(t)
	id := saveSession(t, store, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("inspect main.go")}},
		agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("Reading it."), agentcore.NewToolCallContent("call-1", "read", []byte(`{"path":"main.go"}`))},
			StopReason: agentcore.StopReasonToolUse,
		},
		agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: "call-1",
			ToolName:   "read",
			Content:    agentcore.ContentList{agentcore.NewTextContent("package main")},
		},
	})

	m := resumeTestModel(t, store, id)
	content := stripANSI(m.View().Content)
	for _, want := range []string{"read(main.go)", "inspect main.go", "package main", "✓"} {
		if !strings.Contains(content, want) {
			t.Fatalf("restored view is missing %q:\n%s", want, content)
		}
	}
	if m.lastToolCard == nil || m.lastToolCard.name != "read" {
		t.Fatalf("lastToolCard = %+v, want the replayed read card", m.lastToolCard)
	}
}
