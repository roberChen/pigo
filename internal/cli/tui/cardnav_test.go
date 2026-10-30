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
