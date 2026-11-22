package tui

// Benchmarks for the streaming render path: what one streamed delta costs when
// the transcript already holds a long conversation. They exist so the
// incremental layout in transcript.go can be checked against the naive
// re-render-everything version it replaced (benchstat on two revisions), and so
// a future change that reintroduces a whole-transcript pass per delta shows up
// as a regression rather than as a vague "the TUI feels slow again".

import (
	"fmt"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
)

// benchParagraph is a deterministic ~60-character line, so block sizes are
// stable across runs and revisions.
const benchParagraph = "the quick brown fox jumps over the lazy dog, twice over."

// seedLongTranscript builds a transcript of blocks blockCount finalized turns,
// each lines lines long, at width 100 — roughly the shape of a half-hour
// conversation. The width is applied first so the seeded blocks render through
// the normal cache path.
func seedLongTranscript(b testing.TB, blockCount, lines int) *transcript {
	b.Helper()
	tr := newTranscript(DefaultTheme())
	tr.setSize(100, 40)
	for i := 0; i < blockCount; i++ {
		body := strings.Repeat(benchParagraph+"\n", lines)
		tr.addSystem(fmt.Sprintf("turn %d\n%s", i, body))
	}
	tr.reflow()
	return &tr
}

// BenchmarkAppendDelta measures one streamed delta into a long transcript: the
// per-delta cost the user experiences as "streaming got slower".
func BenchmarkAppendDelta(b *testing.B) {
	for _, blocks := range []int{10, 100, 400} {
		b.Run(fmt.Sprintf("blocks=%d", blocks), func(b *testing.B) {
			tr := seedLongTranscript(b, blocks, 20)
			tr.blocks = append(tr.blocks, transcriptBlock{role: roleAssistant})
			tr.activeAssistant = len(tr.blocks) - 1
			stale := tr.vp.GetContent()
			b.ReportAllocs()
			b.SetBytes(int64(len(stale)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr.appendDelta("a chunk of streamed words ")
			}
		})
	}
}

// BenchmarkFinalizeTurn measures the turn-finalization reflow (the streaming
// block switches from plain text to Markdown).
func BenchmarkFinalizeTurn(b *testing.B) {
	tr := seedLongTranscript(b, 200, 20)
	streamed := strings.Repeat("markdown looking text with *emphasis* and `code` here. ", 200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tr.blocks = append(tr.blocks, transcriptBlock{role: roleAssistant, text: streamed})
		tr.activeAssistant = len(tr.blocks) - 1
		tr.reflow()
		b.StartTimer()
		tr.finalizeTurn(agentcore.AssistantMessage{Content: agentcore.ContentList{agentcore.NewTextContent(streamed)}})
	}
}

// BenchmarkAppendDeltaLongAnswer measures one delta into a long answer that is
// still streaming: the block's own render is re-run from scratch on every delta,
// so this isolates the per-delta cost that grows with the answer (not with the
// history).
func BenchmarkAppendDeltaLongAnswer(b *testing.B) {
	for _, answer := range []int{2 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("answer=%dKiB", answer>>10), func(b *testing.B) {
			tr := newTranscript(DefaultTheme())
			tr.setSize(100, 40)
			tr.addUser("stream a long answer")
			blk := strings.Repeat(benchParagraph, answer/len(benchParagraph))
			tr.blocks = append(tr.blocks, transcriptBlock{role: roleAssistant, text: blk})
			tr.activeAssistant = len(tr.blocks) - 1
			tr.reflow()
			b.ReportAllocs()
			b.SetBytes(int64(len(blk)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr.appendDelta("a chunk of streamed words ")
			}
		})
	}
}

// BenchmarkSetThinking measures one reasoning update into a live thinking block.
// Unlike the answer, reasoning arrives as a growing partial, so the block is
// handed the whole chain of thought each time and only the new suffix may cost
// anything: the benchmark checks that a delta is flat in the reasoning's length
// rather than re-wrapping (and re-capping) all of it.
func BenchmarkSetThinking(b *testing.B) {
	for _, size := range []int{2 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("thinking=%dKiB", size>>10), func(b *testing.B) {
			tr := newTranscript(DefaultTheme())
			tr.setSize(100, 40)
			tr.addUser("reason about it")
			// The provider accumulates the partial and resends all of it, so build it
			// the way it does — a Builder, whose String() does not copy. Concatenating
			// here instead would charge the benchmark's own O(n) copy to the block.
			var partial strings.Builder
			partial.WriteString(strings.Repeat(benchParagraph, size/len(benchParagraph)))
			tr.setThinking(partial.String())
			b.ReportAllocs()
			b.SetBytes(int64(partial.Len()))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				partial.WriteString("and another consideration ")
				tr.setThinking(partial.String())
			}
		})
	}
}
