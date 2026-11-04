// This file wires the diagnostic collector (internal/diag) into the TUI.
//
// The TUI owns the terminal from the moment the alt screen is entered. Without
// this, any diagnostic pigo emits while running — a reload rebuilding hooks, an
// MCP server failing to connect, a compaction checkpoint write error, a plugin
// process dying — would be written to stderr behind Bubble Tea's back and land
// in the middle of a frame, corrupting the display. Installing a collector turns
// every such write into a queued line that Update folds into the transcript on
// the UI goroutine.
//
// Diagnostics stay debug-gated, matching the rest of pigo: with PIGO_DEBUG unset
// the collector discards writes, so a normal session is quiet (nothing appears
// in the transcript and nothing reaches the terminal).
package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/diag"
)

// diagMsg carries diagnostics that arrived while the UI was busy (or between
// frames) from the collector goroutine to the UI goroutine. It is the only path
// by which a background diagnostic reaches the screen.
type diagMsg struct {
	lines []string
}

// waitDiag returns a Cmd that blocks until the collector has something queued,
// then delivers it. Update re-arms it after every delivery, so the stream of
// diagnostics keeps flowing without polling. A nil or non-keeping collector
// yields a nil Cmd (nothing can ever arrive), which tea ignores.
func waitDiag(c *diag.Collector) tea.Cmd {
	if c == nil || !c.Keeping() {
		return nil
	}
	return func() tea.Msg {
		<-c.Wait()
		return diagMsg{lines: c.Drain()}
	}
}

// withDiag binds the collector whose writes should be rendered in the
// transcript. Run calls it with the collector it installed; the session-less
// constructor path (tests) leaves it nil, so no drain command is armed.
func (m Model) withDiag(c *diag.Collector) Model {
	m.diags = c
	return m
}

// handleDiag folds a batch of diagnostics into the transcript and re-arms the
// drain command. Each line is a system row, the same shape a reload summary
// uses, so the session's own output never smears the frame.
func (m Model) handleDiag(msg diagMsg) (tea.Model, tea.Cmd) {
	for _, line := range msg.lines {
		m.transcript.addSystem(line)
	}
	m.relayout()
	return m, waitDiag(m.diags)
}
