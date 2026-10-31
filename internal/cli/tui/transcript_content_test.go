// Tests for the transcript's per-content display rules: a user turn carries its
// "▌ you" gutter and is capped, reasoning renders as a dim capped block, and both
// replay and live streaming produce the same shapes.
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// TestUserTurnHasGutter verifies a human turn is visually marked and its wrapped
// continuation rows keep the gutter, so input never reads like model output.
func TestUserTurnHasGutter(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(40, 20)
	tr.addUser("first line\nsecond line")

	out := stripANSI(tr.vp.GetContent())
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("rendered %d rows, want a label row + 2 lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "▌ you") {
		t.Errorf("label row = %q, want a ▌ you marker", lines[0])
	}
	for _, ln := range lines[1:] {
		if !strings.HasPrefix(ln, "▌ ") {
			t.Errorf("content row %q is missing the gutter", ln)
		}
	}
	if !strings.Contains(lines[1], "first line") || !strings.Contains(lines[2], "second line") {
		t.Errorf("user text not preserved:\n%s", out)
	}
}

// TestUserTurnCapsLongInput verifies a huge paste (or pasted file) cannot flood the
// transcript: the cap keeps the first rows and reports how many were hidden.
func TestUserTurnCapsLongInput(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(40, 60)
	var b strings.Builder
	for i := 0; i < userPreviewLines+5; i++ {
		b.WriteString("line-")
		b.WriteByte(byte('a' + i))
		b.WriteByte('\n')
	}
	tr.addUser(b.String())

	out := stripANSI(tr.vp.GetContent())
	last := "line-" + string(byte('a'+userPreviewLines+4)) // the final source line
	if strings.Contains(out, last) {
		t.Errorf("input past the cap should not render (%q found):\n%s", last, out)
	}
	if !strings.Contains(out, "more lines") {
		t.Errorf("a capped input should say how much is hidden:\n%s", out)
	}
}

// TestThinkingRendersCapped verifies reasoning content is displayed as a labelled,
// capped, styled block so a long chain of thought does not push the answer away —
// and that the cap keeps the *newest* rows, because that is what the model is
// reasoning about while it streams.
func TestThinkingRendersCapped(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(40, 40)
	var b strings.Builder
	for i := 0; i < thinkingPreviewLines+4; i++ {
		b.WriteString("thought-")
		b.WriteByte(byte('a' + i))
		b.WriteByte('\n')
	}
	tr.setThinking(b.String())
	tr.endThinking()

	out := stripANSI(tr.vp.GetContent())
	if !strings.Contains(out, "thinking") {
		t.Errorf("thinking block should be labelled:\n%s", out)
	}
	if !strings.Contains(out, "thought-j") {
		t.Errorf("thinking block should show its newest lines:\n%s", out)
	}
	if strings.Contains(out, "thought-a") {
		t.Errorf("thinking block should hide its oldest lines:\n%s", out)
	}
	if !strings.Contains(out, "earlier lines") {
		t.Errorf("capped thinking should report how many lines are hidden:\n%s", out)
	}
	if !strings.Contains(out, "click to expand") {
		t.Errorf("capped thinking should advertise that it opens on click:\n%s", out)
	}
}

// TestThinkingExpandedShowsWholeChain verifies a thinking block opens and closes:
// expanded, the rows the default tail view hid — the opening of the chain of
// thought — become visible; collapsed again, it is back to the newest rows.
func TestThinkingExpandedShowsWholeChain(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(40, 40)
	var b strings.Builder
	for i := 0; i < thinkingPreviewLines+4; i++ {
		fmt.Fprintf(&b, "thought-%c\n", rune('a'+i))
	}
	tr.setThinking(b.String())
	tr.endThinking()
	if n := countBlocks(tr, roleThinking); n != 1 {
		t.Fatalf("thinking blocks = %d, want 1", n)
	}
	if out := stripANSI(tr.vp.GetContent()); strings.Contains(out, "thought-a") {
		t.Fatalf("a collapsed thinking block should hide its oldest rows:\n%s", out)
	}

	tr.setThinkingExpanded(tr.blocks[0].id, true)
	tr.reflow()
	out := stripANSI(tr.vp.GetContent())
	if !strings.Contains(out, "thought-a") || !strings.Contains(out, "thought-j") {
		t.Errorf("an expanded thinking block should show the whole chain of thought:\n%s", out)
	}
	if strings.Contains(out, "click to expand") {
		t.Errorf("an expanded thinking block should not offer to expand:\n%s", out)
	}

	tr.setThinkingExpanded(tr.blocks[0].id, false)
	tr.reflow()
	if out := stripANSI(tr.vp.GetContent()); strings.Contains(out, "thought-a") {
		t.Errorf("collapsing should hide the oldest rows again:\n%s", out)
	}
}

// TestThinkingStreamsThenAnswer verifies the live path: reasoning updates replace one
// block (the provider resends the whole partial), the answer then streams into its own
// block, and the turn end closes both.
func TestThinkingStreamsThenAnswer(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 30})
	m = apply(t, m, thinkingDeltaMsg{text: "first consideration"})
	m = apply(t, m, thinkingDeltaMsg{text: "first consideration, second thought"})
	m = apply(t, m, textDeltaMsg{delta: "the answer"})
	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent("the answer")},
		StopReason: agentcore.StopReasonEndTurn,
	}})

	if n := countBlocks(m.transcript, roleThinking); n != 1 {
		t.Fatalf("thinking blocks = %d, want exactly 1 (updates replace)", n)
	}
	if n := countBlocks(m.transcript, roleAssistant); n != 1 {
		t.Fatalf("assistant blocks = %d, want 1", n)
	}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "second thought") {
		t.Errorf("latest reasoning not shown:\n%s", out)
	}
	if !strings.Contains(out, "the answer") {
		t.Errorf("answer not shown:\n%s", out)
	}
	if m.transcript.activeThinking != -1 {
		t.Error("the turn end should close the thinking block")
	}
}

// TestReplaySeedsThinking verifies a resumed conversation shows the reasoning that
// was persisted with the reply, capped like a live block.
func TestReplaySeedsThinking(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(60, 40)
	seedTranscript(&tr, []agentcore.Message{
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{
			agentcore.NewThinkingContent("weighing the options"),
			agentcore.NewTextContent("here is the answer"),
		}},
	})
	if n := countBlocks(tr, roleThinking); n != 1 {
		t.Fatalf("replayed thinking blocks = %d, want 1", n)
	}
	out := stripANSI(tr.vp.GetContent())
	if !strings.Contains(out, "weighing the options") {
		t.Errorf("replayed reasoning missing:\n%s", out)
	}
	if !strings.Contains(out, "here is the answer") {
		t.Errorf("replayed answer missing:\n%s", out)
	}
}

// TestBridgeEmitsThinking verifies the event bridge turns a thinking update into a
// thinkingDeltaMsg (and carries the accumulated text, not a delta).
func TestBridgeEmitsThinking(t *testing.T) {
	ch := newEventChan()
	h := newStreamHandler(ch, nil)
	partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{
		agentcore.NewThinkingContent("thinking hard"),
	}}
	h.OnEvent(agentcore.MessageUpdateEvent{Message: partial, AssistantMessageEvent: provider.StreamThinkingEvent{Partial: partial}})

	select {
	case msg := <-ch:
		td, ok := msg.(thinkingDeltaMsg)
		if !ok {
			t.Fatalf("bridge emitted %T, want thinkingDeltaMsg", msg)
		}
		if td.text != "thinking hard" {
			t.Errorf("thinking text = %q, want the accumulated partial", td.text)
		}
	default:
		t.Fatal("bridge emitted nothing for a thinking update")
	}
}

// countBlocks counts transcript blocks with the given role.
func countBlocks(tr transcript, role blockRole) int {
	n := 0
	for _, b := range tr.blocks {
		if b.role == role {
			n++
		}
	}
	return n
}
