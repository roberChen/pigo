// Regression tests for two defects in how a compaction checkpoint is shown:
// clicking one checkpoint's expand marker jumped the view to a different
// checkpoint, and the checkpoint body was drawn as raw Markdown source.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/ui"
)

// resumedModel builds a model holding a replayed session that already contains
// one compaction checkpoint, the shape /resume produces. filler turns are appended
// after it so the transcript overflows the viewport, which is what makes a wrong
// reveal visible: with everything on screen, scrolling to the wrong block is a
// no-op and the bug hides.
func resumedModel(t *testing.T, width, height int, filler int) Model {
	t.Helper()
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: width, Height: height})
	history := []agentcore.Message{
		// Exactly one block precedes the checkpoint, so the replayed card lands on
		// "compaction-1" — the id the live counter hands to the first compaction
		// after a resume.
		agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent("first request")},
		},
		agentcore.CompactionMessage{Summary: "## Goal\nreplayed checkpoint"},
	}
	for i := 0; i < filler; i++ {
		history = append(history,
			agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent("follow-up request")},
			},
			agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content:   agentcore.ContentList{agentcore.NewTextContent("answer line one\nanswer line two")},
			},
		)
	}
	m.lastToolCard = seedTranscript(&m.transcript, history)
	m.indexTranscriptCards()
	m.relayout()
	return m
}

// TestCompactionCardIDsAreUniqueAcrossReplayAndLive is the root cause of the
// "clicking one summary jumps to another" bug. A card id is the identity of a
// transcript entry: selecting, toggling and revealing all resolve through
// m.toolCards, so two blocks sharing an id make a click act on one card and
// scroll to the other. A resumed session used to number its replayed checkpoints
// off the block index while the live counter restarted at 1, so the first
// compaction after a resume could take an id the replay had already used.
func TestCompactionCardIDsAreUniqueAcrossReplayAndLive(t *testing.T) {
	m := resumedModel(t, 80, 24, 0)
	m = apply(t, m, compactionMsg{ev: agentcore.CompactionEvent{
		TokensBefore: 40_000,
		TokensAfter:  6_000,
		Summary:      "## Goal\nlive checkpoint",
	}})

	seen := make(map[string]int)
	for _, blk := range m.transcript.blocks {
		if blk.card == nil {
			continue
		}
		seen[blk.card.id]++
		if n := seen[blk.card.id]; n > 1 {
			t.Fatalf("card id %q is used by %d transcript blocks; ids must be unique", blk.card.id, n)
		}
	}
	if len(seen) < 2 {
		t.Fatalf("expected the replayed and live checkpoints, got ids %v", seen)
	}
}

// TestClickingCheckpointTogglesThatCheckpoint is the user-visible symptom: the
// click must expand the block that was clicked, and must not scroll the view to
// a different checkpoint.
func TestClickingCheckpointTogglesThatCheckpoint(t *testing.T) {
	// Enough filler that the transcript scrolls: the replayed checkpoint sits at
	// the top, out of view, and the live one is what the reader is looking at.
	m := resumedModel(t, 80, 20, 20)
	m = apply(t, m, compactionMsg{ev: agentcore.CompactionEvent{
		TokensBefore: 40_000,
		TokensAfter:  6_000,
		Summary:      "## Goal\nlive checkpoint",
	}})
	m.transcript.vp.GotoBottom()

	live := m.lastToolCard
	if live == nil {
		t.Fatal("live compaction card was not recorded")
	}
	offsetBefore := m.transcript.vp.YOffset()
	if offsetBefore == 0 {
		t.Fatalf("transcript should be scrolled for this test to mean anything")
	}

	// The click map is filled by the frame that was painted, so draw one before
	// asking which row the header landed on.
	_ = m.transcript.view()
	row := -1
	for y, target := range m.transcript.paint.targets {
		if target.role == roleTool && target.id == live.id {
			row = y
			break
		}
	}
	if row < 0 {
		t.Fatalf("live checkpoint header is not painted in the viewport:\n%s", stripANSI(m.transcript.view()))
	}
	m = apply(t, m, tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: row})
	m = apply(t, m, tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 1, Y: row})

	if !live.expanded {
		t.Errorf("clicking the checkpoint should expand it")
	}
	for _, blk := range m.transcript.blocks {
		if blk.card == nil || blk.card == live {
			continue
		}
		if blk.card.expanded {
			t.Errorf("another checkpoint (%s) was expanded instead", blk.card.id)
		}
	}
	// "Jumped to another summary" is a viewport move: the clicked block must not
	// push the view away from where it was. This is the assertion that reproduces
	// the report — with the replayed checkpoint numbered into the live id space and
	// revealCard matching on the id alone, the reveal found the older block and
	// scrolled the transcript from the bottom to the top.
	if got, want := m.transcript.vp.YOffset(), offsetBefore; got != want {
		t.Errorf("click scrolled the transcript from row %d to %d", want, got)
	}
}

// TestCompactionBodyIsMarkdownRendered verifies the checkpoint is laid out as
// Markdown instead of being drawn as its source. The summarizer writes a fixed
// Markdown shape, so the markers used to reach the screen literally ("## Goal",
// "**bold**"), which is the one block an operator reads to judge a compaction.
func TestCompactionBodyIsMarkdownRendered(t *testing.T) {
	previous := markdownStyled
	markdownStyled = func() bool { return true }
	t.Cleanup(func() { markdownStyled = previous })
	// The renderer cache is keyed by width and by dark/light; both are unchanged
	// here, but a stale renderer built before this test cannot exist because the
	// cache is only filled by renderMarkdown.

	card := newCompactionCard("compaction-1", "40,000 → 6,000 tokens",
		"## Goal\nship the checkpoint\n\n- **first** item\n- second item", false)

	out := card.render(DefaultTheme(), 60)
	plain := stripANSI(out)

	// Markdown rewriting, in the shape glamour applies it: the list marker is
	// converted and the emphasis markers are consumed. (The heading keeps its
	// "## " prefix because glamour's standard styles render h2+ that way — the
	// same look an assistant reply gets in this transcript.)
	if strings.Contains(plain, "- **first**") {
		t.Errorf("list/bold markers should not reach the screen as written:\n%s", plain)
	}
	if strings.Contains(plain, "**first**") {
		t.Errorf("bold markers should be consumed by the renderer:\n%s", plain)
	}
	if !strings.Contains(plain, "• first item") {
		t.Errorf("list should be laid out with a bullet glyph:\n%s", plain)
	}
	if !strings.Contains(plain, "ship the checkpoint") {
		t.Errorf("heading text lost:\n%s", plain)
	}
	if !strings.Contains(plain, "first item") {
		t.Errorf("list item text lost:\n%s", plain)
	}
	// The body is still conversation text under its header: every row is indented
	// so it cannot be misread as a new block.
	for i, row := range strings.Split(plain, "\n") {
		if i == 0 || row == "" {
			continue
		}
		if !strings.HasPrefix(row, "  ") {
			t.Errorf("row %d is not indented under the header: %q", i, row)
		}
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("Markdown styling produced no escape sequences")
	}
}

// TestCompactionMarkdownKeepsBudgetAndHint verifies the Markdown path still obeys
// the block's row budget and still says how much is hidden: rendering first and
// budgeting afterwards is what keeps a long checkpoint from flooding the
// transcript.
func TestCompactionMarkdownKeepsBudgetAndHint(t *testing.T) {
	previous := markdownStyled
	markdownStyled = func() bool { return true }
	t.Cleanup(func() { markdownStyled = previous })

	var body strings.Builder
	body.WriteString("## Goal\n")
	for i := 0; i < 60; i++ {
		body.WriteString("\n- item line\n")
	}
	card := newCompactionCard("compaction-1", "40,000 → 6,000 tokens", body.String(), false)

	collapsed := strings.Split(stripANSI(card.render(DefaultTheme(), 60)), "\n")
	var bodyRows []string
	for _, row := range collapsed[1:] { // skip the header
		if strings.Contains(row, "Ctrl+O") {
			continue
		}
		bodyRows = append(bodyRows, row)
	}
	if len(bodyRows) > cardCompactionLines {
		t.Errorf("collapsed body rendered %d rows, budget is %d:\n%s",
			len(bodyRows), cardCompactionLines, strings.Join(collapsed, "\n"))
	}
	if !strings.Contains(strings.Join(collapsed, "\n"), "Ctrl+O") {
		t.Errorf("a capped body should say it can be opened:\n%s", collapsed)
	}

	card.toggleExpanded()
	expanded := stripANSI(card.render(DefaultTheme(), 60))
	if len(expanded) <= len(strings.Join(collapsed, "\n")) {
		t.Errorf("expanding should reveal more (%d vs %d bytes)",
			len(strings.Join(collapsed, "\n")), len(expanded))
	}
	if n := strings.Count(expanded, "\n") + 1; n > cardExpandedLines+1 {
		t.Errorf("expanded body rendered %d rows, ceiling is %d", n, cardExpandedLines)
	}
}

// TestCompactionMarkdownFallsBackWithoutTerminalStyling verifies the degraded
// path still fits the block: with styling off (pipes, tests) the source is wrapped
// as plain text rather than passed through, or a long paragraph would overflow the
// column and paint over the scrollbar.
func TestCompactionMarkdownFallsBackWithoutTerminalStyling(t *testing.T) {
	card := newCompactionCard("compaction-1", "40,000 → 6,000 tokens",
		"## Goal\n"+strings.Repeat("word ", 40), false)

	for _, row := range strings.Split(stripANSI(card.render(DefaultTheme(), 40)), "\n") {
		if ui.Width(row) > 40 {
			t.Errorf("row wider than the block (%d cols): %q", ui.Width(row), row)
		}
	}
}
