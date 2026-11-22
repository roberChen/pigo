// Tests for rendering a context compaction in the transcript. A compaction is
// reported as borderless context text rather than as a bordered tool card,
// because the checkpoint is not the record of an event — it is the conversation's
// own content, carried by the model in place of the turns it replaced.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
)

// borderRunes are the rounded-frame characters a bordered card draws its box
// with. A context block must use none of them.
const borderRunes = "╭╮╰╯│─"

// assertNoBorder fails when out holds any box-drawing character.
func assertNoBorder(t *testing.T, out string) {
	t.Helper()
	if i := strings.IndexAny(stripANSI(out), borderRunes); i >= 0 {
		t.Errorf("context block drew a border at %q:\n%s", string(stripANSI(out)[i]), stripANSI(out))
	}
}

// TestCompactionCardHasNoBorder verifies a compaction checkpoint renders without
// a frame: it is conversation content, so drawing it as a boxed tool result made
// it read like a log entry sitting beside the dialogue instead of part of it.
func TestCompactionCardHasNoBorder(t *testing.T) {
	card := newCompactionCard("compaction-1",
		"40,000 → 6,000 tokens · summarized 9 · kept 2",
		"## Goal\nship the borderless checkpoint\n\n## Next Steps\n1. run the tests", false)

	out := card.render(DefaultTheme(), 60)
	assertNoBorder(t, out)

	plain := stripANSI(out)
	// The header still carries the accounting, and the body is still indented
	// under it, so the block reads as a labeled section rather than a card.
	if !strings.Contains(plain, "40,000 → 6,000 tokens") {
		t.Errorf("header should keep the token accounting:\n%s", plain)
	}
	if !strings.Contains(plain, "ship the borderless checkpoint") {
		t.Errorf("body should keep the checkpoint text:\n%s", plain)
	}
}

// TestCompactionCardHeaderIsConversationScoped verifies the header says what
// happened to the conversation ("context compacted") rather than presenting the
// block as a tool named "compaction" with a success glyph.
func TestCompactionCardHeaderIsConversationScoped(t *testing.T) {
	card := newCompactionCard("compaction-1", "40,000 → 6,000 tokens", "## Goal\nx", false)
	plain := stripANSI(card.render(DefaultTheme(), 60))

	if !strings.Contains(plain, "context compacted") {
		t.Errorf("header should name the event, not the tool:\n%s", plain)
	}
	for _, unwanted := range []string{"✓", "compaction(40,000"} {
		if strings.Contains(plain, unwanted) {
			t.Errorf("header %q should not contain %q", plain, unwanted)
		}
	}
}

// TestCompactionCardHeaderWrapKeepsGutter verifies a header longer than the width
// wraps under the gutter instead of continuing at column zero, where the
// continuation would read as the start of a new block.
func TestCompactionCardHeaderWrapKeepsGutter(t *testing.T) {
	card := newCompactionCard("compaction-1",
		"123,456 → 6,000 tokens · summarized 42 · kept 7 · standing instructions · 3 verbatim requests",
		"## Goal\nx", false)

	rows := strings.Split(stripANSI(card.render(DefaultTheme(), 40)), "\n")
	if len(rows) < 2 {
		t.Fatalf("expected the header to wrap at width 40, got %d rows:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	for i, row := range rows {
		if i == 0 {
			continue
		}
		// Every row after the first is either a continuation of the header (two
		// spaces of gutter) or an indented body row. Neither may start at column 0.
		if row != "" && !strings.HasPrefix(row, "  ") {
			t.Errorf("row %d starts at column zero: %q\n%s", i, row, strings.Join(rows, "\n"))
		}
	}
}

// TestCompactionCardExpandedDoesNotDuplicateNote verifies expanding shows more of
// the checkpoint and nothing else. The card's only "argument" is the note already
// spelled out in the header, so re-listing it as "note: …" was pure duplication.
func TestCompactionCardExpandedDoesNotDuplicateNote(t *testing.T) {
	body := "## Goal\nx"
	for i := 0; i < 40; i++ {
		body += "\nline " + strings.Repeat("y", 3)
	}
	card := newCompactionCard("compaction-1", "40,000 → 6,000 tokens", body, false)

	collapsed := stripANSI(card.render(DefaultTheme(), 60))
	card.toggleExpanded()
	expanded := stripANSI(card.render(DefaultTheme(), 60))

	if strings.Count(expanded, "note:") > 0 {
		t.Errorf("expanded block should not repeat the header note:\n%s", expanded)
	}
	if len(expanded) <= len(collapsed) {
		t.Errorf("expanding should reveal more of the body (collapsed %d bytes, expanded %d)", len(collapsed), len(expanded))
	}
	// The expand marker flips so the block says it can be collapsed again.
	if !strings.Contains(expanded, "▾") {
		t.Errorf("expanded block should carry the open marker:\n%s", expanded)
	}
}

// TestCompactionCardSelectedIsVisible verifies selection still has a visible
// effect now that there is no border to recolor. Card navigation (Alt+↑/↓) can
// still pick this block, and an invisible selection is indistinguishable from
// none.
func TestCompactionCardSelectedIsVisible(t *testing.T) {
	card := newCompactionCard("compaction-1", "40,000 → 6,000 tokens", "## Goal\nx", false)
	plain := card.render(DefaultTheme(), 60) // styled, not stripped
	card.setSelected(true)
	selected := card.render(DefaultTheme(), 60)

	if stripANSI(plain) != stripANSI(selected) {
		t.Errorf("selection should not change the block's text")
	}
	if plain == selected {
		t.Errorf("selection should change the block's styling, but the renders are identical")
	}
	if !strings.Contains(selected, "\x1b[4m") && !strings.Contains(selected, ";4m") {
		t.Errorf("selection should underline the header, got %q", selected)
	}
}

// TestCompactionWarnCardShowsReason verifies a failed compaction still reports
// itself: there is no checkpoint to show, the context is unchanged, and the
// operator has to learn that the summarization did not happen.
func TestCompactionWarnCardShowsReason(t *testing.T) {
	card := newCompactionCard("compaction-1", "failed · context left unchanged",
		"provider returned 529", true)

	out := card.render(DefaultTheme(), 60)
	assertNoBorder(t, out)
	plain := stripANSI(out)
	if !strings.Contains(plain, "provider returned 529") {
		t.Errorf("warn block should carry the failure reason:\n%s", plain)
	}
}

// TestCompactionMessageRendersWithoutBorder covers the live path end to end: the
// event the loop emits reaches the transcript as a borderless context block.
func TestCompactionMessageRendersWithoutBorder(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	m = apply(t, m, compactionMsg{ev: agentcore.CompactionEvent{
		Reason:       "threshold",
		TokensBefore: 40_000,
		TokensAfter:  6_000,
		Summary:      "## Goal\nborderless",
	}})

	out := stripANSI(m.transcript.view())
	assertNoBorder(t, out)
	if !strings.Contains(out, "context compacted") {
		t.Errorf("transcript should report the compaction:\n%s", out)
	}
}
