package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
)

// This file benchmarks the transcript hot paths that a long conversation turns
// into a typing/streaming problem: re-flowing the whole log on every keystroke
// and every streamed token. The per-block render cache and the unchanged-size
// fast path in setSize keep both costs flat as the log grows — run with
// `go test ./internal/cli/tui -bench Transcript` to see them.

// benchTurns is the number of finished user/assistant turns the benchmarks
// pre-load. It is large enough that re-rendering every turn per update is
// visible in the numbers.
const benchTurns = 20

func benchTranscriptWithTurns(b *testing.B, turns int) *transcript {
	b.Helper()
	tr := newTranscript(DefaultTheme())
	tr.setSize(100, 30)
	body := strings.Repeat("A paragraph of the answer with **bold** and `code`.\n\n", 12)
	for i := range turns {
		tr.addUser(fmt.Sprintf("question %d", i))
		tr.appendDelta(body)
		tr.finalizeTurn(agentcore.AssistantMessage{
			Content: agentcore.ContentList{agentcore.NewTextContent(body)},
		})
	}
	return &tr
}

func benchUpdate(b *testing.B, m Model, msg tea.Msg) Model {
	b.Helper()
	next, _ := m.Update(msg)
	got, ok := next.(Model)
	if !ok {
		b.Fatalf("Update returned %T, want tui.Model", next)
	}
	return got
}

// BenchmarkTranscriptReflow measures a full re-flow of a long transcript, the
// work that used to run on every keystroke.
func BenchmarkTranscriptReflow(b *testing.B) {
	tr := benchTranscriptWithTurns(b, benchTurns)
	for b.Loop() {
		tr.reflow()
	}
}

// BenchmarkTranscriptAppendDelta measures one streamed token landing in a long
// transcript while an answer of a realistic size is in flight. Only the
// streaming block is re-rendered; every finished turn is served from its cached
// render. The streaming block is rewound after each op so the measurement stays
// bounded instead of growing the block by one token per iteration.
func BenchmarkTranscriptAppendDelta(b *testing.B) {
	tr := benchTranscriptWithTurns(b, benchTurns)
	tr.addUser("streaming")
	answer := strings.Repeat("A streamed sentence of the current answer. ", 200)
	tr.appendDelta(answer)
	blk := &tr.blocks[tr.activeAssistant]
	for b.Loop() {
		tr.appendDelta("token ")
		blk.text = blk.text[:len(answer)]
	}
}

// BenchmarkModelKeystroke measures a typed character end to end (Update + View)
// in an idle model holding a long conversation. The composer is reset after each
// op so the measured frame renders a normal one-character draft.
func BenchmarkModelKeystroke(b *testing.B) {
	m := benchUpdate(b, NewModel(Options{}), tea.WindowSizeMsg{Width: 100, Height: 30})
	body := strings.Repeat("A paragraph of the answer with **bold** and `code`.\n\n", 12)
	for i := range benchTurns {
		m = benchUpdate(b, m, textDeltaMsg{delta: fmt.Sprintf("question %d", i)})
		m = benchUpdate(b, m, turnEndMsg{msg: agentcore.AssistantMessage{
			Content: agentcore.ContentList{agentcore.NewTextContent(body)},
		}})
		m = benchUpdate(b, m, textDeltaMsg{delta: "streaming"})
	}
	key := tea.KeyPressMsg{Code: 'a', Text: "a"}
	for b.Loop() {
		m = benchUpdate(b, m, key)
		_ = m.View()
		m.input.Clear()
	}
}
