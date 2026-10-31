// Tests for picking a tool card and toggling it: a click on any historical card
// expands or collapses it, a drag stays a text selection, and Alt+↑/Alt+↓ walk the
// selection so the same thing can be done from the keyboard.
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
)

// twoCardModel builds an idle model holding two finished bash cards, sized so both
// are on screen.
func twoCardModel(t *testing.T) Model {
	t.Helper()
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 40})
	m = apply(t, m, toolStartMsg{id: "c1", name: "bash", input: map[string]any{"command": "seq 1 10"}})
	m = apply(t, m, toolEndMsg{id: "c1", ok: true, result: longOutput(10)})
	m = apply(t, m, toolStartMsg{id: "c2", name: "bash", input: map[string]any{"command": "seq 20 30"}})
	m = apply(t, m, toolEndMsg{id: "c2", ok: true, result: longOutput(30)})
	return m
}

// longOutput renders n numbered rows.
func longOutput(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "row-%02d\n", i)
	}
	return b.String()
}

// screenRowOf returns the shell row holding needle, or -1.
func screenRowOf(m Model, needle string) int {
	for i, line := range strings.Split(stripANSI(m.View().Content), "\n") {
		if strings.Contains(line, needle) {
			return i
		}
	}
	return -1
}

// TestClickTogglesHistoricalCard verifies a click on an older card toggles that card
// (not the newest one) and selects it.
func TestClickTogglesHistoricalCard(t *testing.T) {
	m := twoCardModel(t)
	first := m.toolCards["c1"]
	second := m.toolCards["c2"]
	if first.expanded || second.expanded {
		t.Fatal("cards should start collapsed")
	}

	y := screenRowOf(m, "bash(seq 1 10)")
	if y < 0 {
		t.Fatalf("first card header not on screen:\n%s", stripANSI(m.View().Content))
	}
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	// A press alone must not toggle: a drag from here is a text selection.
	if m.toolCards["c1"].expanded {
		t.Fatal("a press should not toggle the card")
	}
	m = apply(t, m, tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})

	if !m.toolCards["c1"].expanded {
		t.Error("clicking the first card should expand it")
	}
	if m.toolCards["c2"].expanded {
		t.Error("clicking the first card must not expand the newest one")
	}
	if m.selectedCardID != "c1" || !m.toolCards["c1"].selected {
		t.Errorf("clicked card should be selected, got %q", m.selectedCardID)
	}

	// A second click collapses it again.
	y = screenRowOf(m, "bash(seq 1 10)")
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	if m.toolCards["c1"].expanded {
		t.Error("a second click should collapse the card")
	}
}

// TestDragDoesNotToggleCard verifies dragging on a card stays a text selection.
func TestDragDoesNotToggleCard(t *testing.T) {
	m := twoCardModel(t)
	y := screenRowOf(m, "bash(seq 1 10)")
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseMotionMsg{X: 20, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 20, Y: y, Button: tea.MouseLeft})

	if m.toolCards["c1"].expanded {
		t.Error("a drag should select text, not toggle the card")
	}
	if m.sel.empty() {
		t.Error("the drag should have produced a text selection")
	}
}

// TestAltArrowsWalkCardSelection verifies Alt+↑/Alt+↓ move the selection through the
// cards, wrapping at the ends, and Ctrl+O then toggles the selected card.
func TestAltArrowsWalkCardSelection(t *testing.T) {
	m := twoCardModel(t)

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	if m.selectedCardID != "c1" {
		t.Fatalf("first Alt+↓ should select the first card, got %q", m.selectedCardID)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	if m.selectedCardID != "c2" {
		t.Fatalf("second Alt+↓ should select the second card, got %q", m.selectedCardID)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	if m.selectedCardID != "c1" {
		t.Errorf("Alt+↓ past the last card should wrap, got %q", m.selectedCardID)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	if m.selectedCardID != "c2" {
		t.Errorf("Alt+↑ should wrap back to the last card, got %q", m.selectedCardID)
	}

	// Ctrl+O targets the selection, not the newest card.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	if m.selectedCardID != "c1" {
		t.Fatalf("selection = %q, want the first card", m.selectedCardID)
	}
	m = apply(t, m, ctrlKey('o'))
	if !m.toolCards["c1"].expanded {
		t.Error("Ctrl+O should toggle the selected card")
	}
	if m.toolCards["c2"].expanded {
		t.Error("Ctrl+O must not toggle a card that is not selected")
	}
	// Only one card wears the selection border.
	if m.toolCards["c2"].selected {
		t.Error("the unselected card should not be highlighted")
	}
}

// TestCtrlOWithoutSelectionTogglesNewest verifies the original shortcut still works
// when nothing is selected.
func TestCtrlOWithoutSelectionTogglesNewest(t *testing.T) {
	m := twoCardModel(t)
	if m.selectedCardID != "" {
		t.Fatalf("no card should be selected initially, got %q", m.selectedCardID)
	}
	m = apply(t, m, ctrlKey('o'))
	if !m.toolCards["c2"].expanded {
		t.Error("Ctrl+O without a selection should toggle the newest card")
	}
}

// TestReplayCardsAreSelectable verifies cards restored by a resume can be toggled
// too: replay must register them in the model's card index.
func TestReplayCardsAreSelectable(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(60, 30)
	seedTranscript(&tr, []agentcore.Message{
		agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content:   agentcore.ContentList{agentcore.NewToolCallContent("call-1", "bash", []byte(`{"command":"ls"}`))},
		},
		agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Content:    agentcore.ContentList{agentcore.NewTextContent(longOutput(20))},
		},
	})
	m := NewModel(Options{})
	m.transcript = tr
	m.indexTranscriptCards()
	if !m.selectCard("call-1") {
		t.Fatal("a replayed card should be selectable")
	}
	m.toggleSelectedCard()
	if !tr.blocks[0].card.expanded {
		t.Error("toggling a replayed card should expand it")
	}
}

// cardRowsOnScreen returns the screen rows a card's frame occupies, derived from
// the rendered shell alone: the header row is located by its unique command, then
// the block is walked outward to its rounded border. Deriving the rows from what is
// drawn (rather than from the transcript's own row arithmetic) is what makes the
// mapping tests meaningful — they compare two independent computations.
// ok is false when the card is only partly on screen (scrolled halfway out of the
// viewport), since then its frame cannot be walked to both corners.
func cardRowsOnScreen(t *testing.T, m Model, header string) (rows []int, ok bool) {
	t.Helper()
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	head := -1
	for i, ln := range lines {
		if strings.Contains(ln, header) {
			head = i
			break
		}
	}
	if head < 0 {
		return nil, false
	}
	// Walk outward from the header to the frame's rounded corners. The card may
	// have any number of body rows between, so this cannot just look one row out.
	top := head
	for top > 0 && !strings.HasPrefix(strings.TrimSpace(lines[top]), "╭") {
		top--
	}
	bottom := head
	for bottom < len(lines)-1 && !strings.HasPrefix(strings.TrimSpace(lines[bottom]), "╰") {
		bottom++
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[top]), "╭") || !strings.HasPrefix(strings.TrimSpace(lines[bottom]), "╰") {
		return nil, false
	}
	for y := top; y <= bottom; y++ {
		rows = append(rows, y)
	}
	return rows, true
}

// TestEveryCardRowMapsToItsCard is the regression test for clicks that missed:
// every row a card draws — top border, header, body, bottom border — must map back
// to that card. The row arithmetic used to count a blank separator before every
// block, but the transcript only draws one before a user turn, so each block after
// the first was attributed a row late: by the third card a click landed on the
// previous card, and the top border of every card after the first was dead.
func TestEveryCardRowMapsToItsCard(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 40})
	m.transcript.addUser("run these")
	m.transcript.reflow()
	for i, name := range []string{"alpha", "beta", "gamma"} {
		id := fmt.Sprintf("c%d", i+1)
		m = apply(t, m, toolStartMsg{id: id, name: "bash", input: map[string]any{"command": "echo " + name}})
		m = apply(t, m, toolEndMsg{id: id, ok: true, result: "done " + name})
	}

	for i, name := range []string{"alpha", "beta", "gamma"} {
		id := fmt.Sprintf("c%d", i+1)
		rows, ok := cardRowsOnScreen(t, m, "echo "+name)
		if !ok || len(rows) < 3 {
			t.Fatalf("card %s should be fully on screen as a framed card", id)
		}
		for _, y := range rows {
			if got := m.cardIDAtScreenY(y); got != id {
				t.Errorf("screen row %d of card %s maps to %q, want %q", y, id, got, id)
			}
		}
	}
}

// TestClickOnCardBottomBorderToggles verifies the fix end to end: clicking the
// bottom border of a card (a row the old arithmetic lost to the next block) toggles
// that card.
func TestClickOnCardBottomBorderToggles(t *testing.T) {
	m := twoCardModel(t)
	rows, ok := cardRowsOnScreen(t, m, "seq 20 30")
	if !ok {
		t.Fatal("the second card should be fully on screen")
	}
	y := rows[len(rows)-1] // the ╰ border row
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	if !m.toolCards["c2"].expanded {
		t.Error("clicking a card's bottom border should toggle that card")
	}
	if m.toolCards["c1"].expanded {
		t.Error("clicking the second card must not toggle the first")
	}
}

// TestClickWithPointerJitterToggles verifies a real click still counts as a click
// when the pointer drifts a cell between press and release. Trackpads do this on
// nearly every click; treating any drift as a drag is why clicking a card "often"
// did nothing (it started a one-cell text selection instead).
func TestClickWithPointerJitterToggles(t *testing.T) {
	m := twoCardModel(t)
	y := screenRowOf(m, "bash(seq 1 10)")
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseMotionMsg{X: 5, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 5, Y: y, Button: tea.MouseLeft})

	if !m.toolCards["c1"].expanded {
		t.Error("a click that jitters one cell should still toggle the card")
	}
	if !m.sel.empty() {
		t.Error("a click should not leave a one-cell text selection behind")
	}
}

// TestClickUsesThePaintedFrame verifies a click is attributed to the frame the user
// is looking at, not to the layout the model happens to hold when the event is
// processed. While a run streams, output appended after a frame was drawn scrolls
// the transcript (follow sticks to the bottom), so by the time the pointer event
// arrives the live layout has moved the rows under the cursor — reading it there
// toggled whichever card had scrolled into that row, which is the "click selects the
// neighbouring card" report.
func TestClickUsesThePaintedFrame(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 20})
	for i, name := range []string{"one", "two", "three"} {
		id := fmt.Sprintf("c%d", i+1)
		m = apply(t, m, toolStartMsg{id: id, name: "bash", input: map[string]any{"command": "echo " + name}})
		m = apply(t, m, toolEndMsg{id: id, ok: true, result: longOutput(4)})
	}

	y := screenRowOf(m, "echo two") // paints the frame the user is over
	if y < 0 {
		t.Fatalf("second card not on screen:\n%s", stripANSI(m.View().Content))
	}
	if got := m.cardIDAtScreenY(y); got != "c2" {
		t.Fatalf("painted row %d maps to %q, want c2", y, got)
	}

	// A streamed turn appends lines below the cards and the viewport follows the
	// bottom, so everything moves up by the rows that arrived.
	m.transcript.follow = true
	m = apply(t, m, textDeltaMsg{delta: strings.Repeat("streamed output line\n", 9)})
	if m.transcript.vp.YOffset() == 0 {
		t.Fatal("the viewport did not move, the test would not exercise a stale frame")
	}
	liveRow := m.transcript.vp.YOffset() + y
	if idx, ok := m.transcript.blockAtRow(liveRow); ok && m.transcript.blocks[idx].card == m.toolCards["c2"] {
		t.Fatalf("test setup: the live layout still puts card c2 on row %d", liveRow)
	}

	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	if !m.toolCards["c2"].expanded {
		t.Error("the click should toggle the card the frame drew on that row (c2)")
	}
	if m.toolCards["c1"].expanded || m.toolCards["c3"].expanded {
		t.Error("the click toggled a card the user never pointed at")
	}
}

// scrolledCardModel builds a transcript taller than the viewport and pages it up,
// so the viewport has a non-zero offset — the state in which a screen row and a
// content row differ.
func scrolledCardModel(t *testing.T) Model {
	t.Helper()
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 16})
	m.transcript.addUser("run these")
	m.transcript.reflow()
	for i, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		id := fmt.Sprintf("c%d", i+1)
		m = apply(t, m, toolStartMsg{id: id, name: "bash", input: map[string]any{"command": "echo " + name}})
		m = apply(t, m, toolEndMsg{id: id, ok: true, result: longOutput(4)})
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.transcript.vp.YOffset() == 0 {
		t.Fatal("the transcript did not scroll, the test would not exercise the offset")
	}
	return m
}

// TestEveryCardRowMapsToItsCardWhenScrolled verifies the row mapping still holds
// with the viewport scrolled, where screen row != content row.
func TestEveryCardRowMapsToItsCardWhenScrolled(t *testing.T) {
	m := scrolledCardModel(t)
	checked := 0
	for i, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		id := fmt.Sprintf("c%d", i+1)
		rows, ok := cardRowsOnScreen(t, m, "echo "+name)
		if !ok {
			continue // partly scrolled out of the viewport
		}
		for _, y := range rows {
			if got := m.cardIDAtScreenY(y); got != id {
				t.Errorf("screen row %d of card %s maps to %q, want %q", y, id, got, id)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no card was visible after scrolling, the test proves nothing")
	}
}

// TestClickBelowTranscriptDoesNotToggleCard verifies a click on the input line or
// the status bar is not attributed to a transcript row. Screen rows past the
// viewport used to resolve against the transcript's content anyway, so clicking the
// prompt of a scrolled session could expand an arbitrary card.
func TestClickBelowTranscriptDoesNotToggleCard(t *testing.T) {
	m := scrolledCardModel(t)
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	h := m.transcript.viewportHeight()
	if h <= 0 || h >= len(lines) {
		t.Fatalf("unexpected layout: viewport %d of %d rows", h, len(lines))
	}
	for _, y := range []int{h, h + 1, len(lines) - 1} {
		if got := m.cardIDAtScreenY(y); got != "" {
			t.Errorf("row %d is outside the transcript but maps to card %q", y, got)
		}
		m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
		m = apply(t, m, tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	}
	for id, card := range m.toolCards {
		if card.expanded {
			t.Errorf("a click below the transcript expanded card %q", id)
		}
	}
	if m.selectedCardID != "" {
		t.Errorf("a click below the transcript must not select a card, got %q", m.selectedCardID)
	}
}

// TestHoverAfterClickDoesNotGrowSelection verifies motion with no button held does
// not extend a finished selection: the drag state, not the mere presence of a
// selection, decides whether the pointer is still selecting text.
func TestHoverAfterClickDoesNotGrowSelection(t *testing.T) {
	m := twoCardModel(t)
	y := screenRowOf(m, "bash(seq 1 10)")
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseMotionMsg{X: 30, Y: y + 2, Button: tea.MouseNone})

	if !m.sel.empty() {
		t.Error("hovering after a click should not paint a text selection")
	}
}

// TestDragPastSlopStillSelectsAndDoesNotToggle verifies a genuine drag is still a
// text selection: the tolerance does not swallow multi-row drags.
func TestDragPastSlopStillSelectsAndDoesNotToggle(t *testing.T) {
	m := twoCardModel(t)
	y := screenRowOf(m, "bash(seq 1 10)")
	m = apply(t, m, tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseMotionMsg{X: 6, Y: y + 3, Button: tea.MouseLeft})
	m = apply(t, m, tea.MouseReleaseMsg{X: 6, Y: y + 3, Button: tea.MouseLeft})

	if m.toolCards["c1"].expanded {
		t.Error("a multi-row drag should not toggle the card")
	}
	if m.sel.empty() {
		t.Error("a multi-row drag should produce a text selection")
	}
}
