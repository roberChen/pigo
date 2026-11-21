// Tests for the plain-text compaction report shared by the REPL's automatic
// compaction notice and its /compact and /rebuild commands.
package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
)

// TestRenderCompactionPrintsBody verifies a successful compaction prints the
// accounting and the checkpoint body: the whole point is that the summary which
// replaced the conversation is visible without opening checkpoint.md.
func TestRenderCompactionPrintsBody(t *testing.T) {
	var out bytes.Buffer
	RenderCompaction(&out, agentcore.CompactionEvent{
		Reason:          "threshold",
		TokensBefore:    90_000,
		TokensAfter:     12_000,
		SummarizedCount: 14,
		KeptCount:       3,
		Summary:         "## Goal\nthe summary the model now carries",
	})
	got := out.String()
	for _, want := range []string{"90000 → 12000 tokens", "summarized 14 messages", "kept 3", "the summary the model now carries"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}

// TestRenderCompactionFailure verifies a failed compaction reports the reason and
// says the context was left unchanged, instead of printing an unchanged token
// range that reads like a successful no-op.
func TestRenderCompactionFailure(t *testing.T) {
	var out bytes.Buffer
	RenderCompaction(&out, agentcore.CompactionEvent{
		Reason:       "threshold",
		TokensBefore: 90_000,
		TokensAfter:  90_000,
		ErrorMessage: "summarizer down",
	})
	got := out.String()
	if !strings.Contains(got, "compaction failed: summarizer down") {
		t.Errorf("failure not reported: %q", got)
	}
	if !strings.Contains(got, "context left unchanged") {
		t.Errorf("failure should say the context is unchanged: %q", got)
	}
}

// TestRenderCompactionSummaryCapsBody verifies a long checkpoint is printed as a
// bounded preview with a pointer to the file, so it cannot bury the conversation
// it describes.
func TestRenderCompactionSummaryCapsBody(t *testing.T) {
	body := strings.Repeat("line\n", compactionBodyLines+7)
	var out bytes.Buffer
	RenderCompactionSummary(&out, "compacted: 1 → 2 tokens", body)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if want := compactionBodyLines + 2; len(lines) != want {
		t.Errorf("report rows = %d, want %d (note + capped body + omission marker)", len(lines), want)
	}
	if !strings.Contains(out.String(), "7 more lines") {
		t.Errorf("omission marker should say how much is hidden:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "checkpoint.md") {
		t.Errorf("omission marker should point at the full checkpoint:\n%s", out.String())
	}
}

// TestCompactionNoteOmitsUnknownCounts verifies zero counts (a no-op rebuild, or a
// recovered checkpoint with no recorded count) are left out rather than printed as
// "summarized 0 messages".
func TestCompactionNoteOmitsUnknownCounts(t *testing.T) {
	if got, want := CompactionNote(100, 40, 0, 0), "compacted: 100 → 40 tokens"; got != want {
		t.Errorf("CompactionNote = %q, want %q", got, want)
	}
	if got, want := CompactionNote(100, 40, 5, 2), "compacted: 100 → 40 tokens, summarized 5 messages, kept 2"; got != want {
		t.Errorf("CompactionNote = %q, want %q", got, want)
	}
}
