// This file implements reporting a context compaction in the transcript as a
// card: the live one a run emits (CompactionEvent) and the one a resumed session
// replays from its persisted CompactionMessage.
//
// Before this, a compaction left a single "(context compacted)" system line. The
// checkpoint that replaced the summarized turns — the text the model actually
// carries on every later turn — was visible only by opening
// <memoryRoot>/sessions/<id>/checkpoint.md, so an operator could not tell what
// had been kept, and had no way to notice a summary that lost the thread. The
// card reuses the tool-card renderer (bounded preview, Ctrl+O / click to expand)
// because that is where a long, expandable block of text already renders well.
package tui

import (
	"fmt"

	"github.com/roberChen/pigo/internal/agentcore"
)

// addCompactionCard appends the report of a compaction to the transcript, indexes
// it for selection, and points the expansion key at it. A failure is rendered as
// a warn card carrying the reason: the failure is non-fatal, the context is left
// unchanged, but the operator still has to learn that the summarization did not
// happen (US-004).
func (m *Model) addCompactionCard(ev agentcore.CompactionEvent) {
	m.compactionSeq++
	id := fmt.Sprintf("compaction-%d", m.compactionSeq)
	card := newCompactionCard(id, liveCompactionNote(ev), ev.Summary, ev.ErrorMessage != "")
	if ev.ErrorMessage != "" {
		card.response = parseToolResult(ev.ErrorMessage + "\n(context left unchanged)")
	}
	m.toolCards[id] = card
	m.lastToolCard = card
	m.transcript.addToolCard(card)
	m.remoteEcho("\n· context compacted\n")
}

// addRebuildCard appends a card reporting a manual /rebuild's checkpoint: the
// status line as the header note and the checkpoint text as the body. A rebuild
// collapses the prefix into the same kind of checkpoint an automatic compaction
// writes, so it is shown the same way; the accounting differs (a rebuild reuses
// an existing checkpoint rather than summarizing afresh), so the note is the
// rebuild's own summary line rather than the compaction token counts.
func (m *Model) addRebuildCard(note, body string) {
	m.compactionSeq++
	id := fmt.Sprintf("rebuild-%d", m.compactionSeq)
	card := newCompactionCard(id, note, body, false)
	m.toolCards[id] = card
	m.lastToolCard = card
	m.transcript.addToolCard(card)
}

// liveCompactionNote is the header note of a compaction card reported by the run
// loop: the before/after token counts and the summarized/retained message counts
// for a successful compaction, or a plain "failed" for one that did not happen
// (its token counts are unchanged, so printing them would only be confusing).
func liveCompactionNote(ev agentcore.CompactionEvent) string {
	if ev.ErrorMessage != "" {
		return "failed · context left unchanged"
	}
	return compactionNote(ev.TokensBefore, ev.TokensAfter, ev.SummarizedCount, ev.KeptCount)
}

// replayedCompactionCard builds the card for a compaction checkpoint read back
// from a session file. The persisted message records neither the post-compaction
// token count nor the retained count, so the note states what the checkpoint
// stands in for instead of inventing figures, and keeps the explanation a reader
// needs when the turns above it were never seen live.
func replayedCompactionCard(id string, msg agentcore.CompactionMessage) *toolCard {
	note := "earlier turns are summarized for the model"
	if msg.CoveredMessages > 0 {
		note += fmt.Sprintf(" · %d messages", msg.CoveredMessages)
	}
	if userNote := msg.UserInstructions.Note(); userNote != "" {
		note += " · " + userNote
	}
	return newCompactionCard(id, note, msg.Text(), false)
}
