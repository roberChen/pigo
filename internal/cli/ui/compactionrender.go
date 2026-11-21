// This file implements the plain-text rendering of a context compaction, shared
// by the REPL's automatic compaction notice and its /compact and /rebuild
// commands. The TUI renders the same information as a card instead (see
// internal/cli/tui/compactioncard.go); both surfaces read the checkpoint text off
// the CompactionEvent so what an operator sees is the body the model actually
// carries on every later turn, not just a token count.
package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/roberChen/pigo/internal/agentcore"
)

// compactionBodyLines caps the checkpoint body printed to a terminal. A summary is
// an LLM-authored document of a few hundred lines at worst; past this the report
// would bury the conversation it is describing, and the full text stays on disk in
// the session's checkpoint.md.
const compactionBodyLines = 30

// RenderCompaction prints a compaction report to out: the failure reason when the
// compaction did not happen (the context is unchanged in that case), or the
// before/after accounting followed by the checkpoint body indented beneath it.
func RenderCompaction(out io.Writer, ev agentcore.CompactionEvent) {
	if ev.ErrorMessage != "" {
		fmt.Fprintf(out, "compaction failed: %s (context left unchanged)\n", ev.ErrorMessage)
		return
	}
	RenderCompactionSummary(out, CompactionNote(ev.TokensBefore, ev.TokensAfter, ev.SummarizedCount, ev.KeptCount), ev.Summary)
}

// RenderCompactionSummary prints a compaction/summary pair: the note as the
// leading line, then body indented two columns and capped to a preview. An empty
// body prints the note alone, so a caller that has no checkpoint text (an older
// event, a no-op) still reports the accounting.
func RenderCompactionSummary(out io.Writer, note, body string) {
	fmt.Fprintf(out, "%s\n", note)
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return
	}
	lines := strings.Split(body, "\n")
	hidden := 0
	if len(lines) > compactionBodyLines {
		hidden = len(lines) - compactionBodyLines
		lines = lines[:compactionBodyLines]
	}
	for _, ln := range lines {
		fmt.Fprintf(out, "  %s\n", Colorize(Enabled(), Dim, ln))
	}
	if hidden > 0 {
		fmt.Fprintf(out, "  %s\n", Colorize(Enabled(), Dim,
			fmt.Sprintf("… %d more lines (the full checkpoint is in the session's checkpoint.md)", hidden)))
	}
}

// CompactionNote renders a compaction's accounting as one line: the token drop and
// the summarized/retained message counts, omitting the counts when they are zero (a
// no-op or a rebuild recovered from a checkpoint does not always know them).
func CompactionNote(before, after, summarized, kept int) string {
	note := fmt.Sprintf("compacted: %d → %d tokens", before, after)
	if summarized > 0 {
		note += fmt.Sprintf(", summarized %d messages", summarized)
	}
	if kept > 0 {
		note += fmt.Sprintf(", kept %d", kept)
	}
	return note
}
