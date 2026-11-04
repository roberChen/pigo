// Tests for the TUI's diagnostic sink: a collector's lines land in the
// transcript on the UI goroutine, the drain command is re-armed, and a quiet
// collector never wires a command that could block.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/diag"
)

// TestWaitDiagInertWhenNotKeeping verifies the two cases where there is nothing
// to drain (no collector, or a discarding one) yield a nil Cmd, so Bubble Tea
// never blocks a goroutine on a channel that cannot be signalled.
func TestWaitDiagInertWhenNotKeeping(t *testing.T) {
	if cmd := waitDiag(nil); cmd != nil {
		t.Error("a nil collector should arm no drain command")
	}
	if cmd := waitDiag(diag.Collect(false)); cmd != nil {
		t.Error("a non-keeping collector should arm no drain command")
	}
}

// TestHandleDiagRendersIntoTranscript verifies the diagnostics a background
// goroutine emitted appear as system rows in the transcript — the whole point of
// the redirect: the message is visible without writing to the terminal.
func TestHandleDiagRendersIntoTranscript(t *testing.T) {
	collector := diag.Collect(true)
	m := NewModel(Options{Model: "test-model"}).withDiag(collector)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	got, cmd := next.Update(diagMsg{lines: []string{"pigo: hooks disabled", "pigo: mcp server down"}})
	model := got.(Model)

	view := model.View().Content
	for _, want := range []string{"hooks disabled", "mcp server down"} {
		if !strings.Contains(view, want) {
			t.Errorf("transcript should show %q:\n%s", want, view)
		}
	}
	// The drain must be re-armed, or the next diagnostic would sit queued forever.
	if cmd == nil {
		t.Error("handling diagnostics should re-arm the drain command")
	}
}

// TestDiagMsgArrivesFromCollector verifies the end-to-end shape: a write to the
// installed sink is delivered by the drain Cmd as a diagMsg carrying that line.
func TestDiagMsgArrivesFromCollector(t *testing.T) {
	collector := diag.Collect(true)
	collector.Write([]byte("pigo: checkpoint not written\n"))

	cmd := waitDiag(collector)
	if cmd == nil {
		t.Fatal("a keeping collector should arm a drain command")
	}
	msg, ok := cmd().(diagMsg)
	if !ok {
		t.Fatalf("drain returned %T, want diagMsg", cmd())
	}
	if len(msg.lines) != 1 || !strings.Contains(msg.lines[0], "checkpoint not written") {
		t.Errorf("lines = %q, want the written diagnostic", msg.lines)
	}
	// Draining clears the queue, so the next Cmd blocks rather than repeating it.
	if rest := collector.Drain(); len(rest) != 0 {
		t.Errorf("queue should be empty after delivery, got %q", rest)
	}
}
