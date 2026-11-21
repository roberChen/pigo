// Tests for rendering a manual /rebuild's checkpoint in the TUI. A rebuild is a
// compaction by another route: it collapses the prefix into a checkpoint, so it
// must report the checkout text the same way a live compaction does instead of
// only printing a token-count system line.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestModelRebuildShowsCheckpointCard verifies rebuildDoneMsg with a body adds a
// compaction-family card carrying the rebuilt checkpoint, alongside the status
// line, so an operator can see what /rebuild collapsed the history into.
func TestModelRebuildShowsCheckpointCard(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 20})
	m.running = true

	m = apply(t, m, rebuildDoneMsg{
		summary: "context rebuilt from checkpoint: 40,000 → 6,000 tokens, collapsed 9 messages, kept 2",
		body:    "## Goal\nrebuilt from the persisted checkpoint",
	})
	if m.running {
		t.Error("a finished rebuild should clear the running state")
	}

	card := m.toolCards["rebuild-1"]
	if card == nil {
		t.Fatal("a successful rebuild should add a card")
	}
	if card.toolFamily() != compactionCardName {
		t.Errorf("rebuild card family = %q, want %q", card.toolFamily(), compactionCardName)
	}
	if card.state != cardSuccess {
		t.Errorf("rebuild card state = %v, want cardSuccess", card.state)
	}
	if got := card.headerText(); !strings.Contains(got, "rebuilt from checkpoint") {
		t.Errorf("card header %q should carry the rebuild's own accounting", got)
	}
	if got := responseText(card); !strings.Contains(got, "rebuilt from the persisted checkpoint") {
		t.Errorf("card body should carry the checkpoint text, got %q", got)
	}
	// The status line stays in the transcript as plain text (it is what the REPL
	// prints and what users already recognise), the card adds the body.
	if !strings.Contains(transcriptText(m.transcript), "context rebuilt from checkpoint") {
		t.Errorf("transcript should still show the rebuild status line:\n%s", transcriptText(m.transcript))
	}
}

// TestModelRebuildNoBodyAddsNoCard verifies the no-op and failure branches do not
// invent a card: there is no new checkpoint to show, only the status line (or the
// error), so the transcript should stay as terse as before.
func TestModelRebuildNoBodyAddsNoCard(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 20})

	m = apply(t, m, rebuildDoneMsg{summary: "nothing to rebuild (1,200 tokens, 4 messages)"})
	if len(m.toolCards) != 0 {
		t.Errorf("a no-op rebuild should not add a card, got %d", len(m.toolCards))
	}
	if !strings.Contains(transcriptText(m.transcript), "nothing to rebuild") {
		t.Errorf("no-op rebuild should still report itself:\n%s", transcriptText(m.transcript))
	}

	m = apply(t, m, rebuildDoneMsg{err: errRebuild("no checkpoint")})
	if len(m.toolCards) != 0 {
		t.Errorf("a failed rebuild should not add a card, got %d", len(m.toolCards))
	}
	if !strings.Contains(transcriptText(m.transcript), "rebuild failed") {
		t.Errorf("failed rebuild should report the error:\n%s", transcriptText(m.transcript))
	}
}

// errRebuild is a tiny error type so the test can build a rebuildDoneMsg failure
// without importing errors just for a literal.
type errRebuild string

func (e errRebuild) Error() string { return string(e) }
