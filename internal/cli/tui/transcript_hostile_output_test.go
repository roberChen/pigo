// End-to-end frame integrity: hostile tool output must not be able to draw over
// the TUI. This drives the real event path (toolStartMsg/toolEndMsg) and inspects
// the rendered View, because the corruption the bug report showed was a frame
// defect - the border of a card broke and stray box characters appeared in
// columns that belong to other blocks - not a string the card held wrongly.
package tui

import (
	"strings"
	"testing"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
)

// capturedTUIFrame is what a bash tool returns after running a full-screen
// program through a pty: an alt-screen handshake, cursor addressing, CRLF line
// ends, a progress bar, tab-aligned columns and the program's own box borders.
const capturedTUIFrame = "\x1b[?1049h\x1b[H\x1b[2J" +
	"\x1b[1m│ pigo(gpt-4o-mini)  │\x1b[0m\r\n" +
	"\x1b[38;5;42m/btw\x1b[0m - side question\r\n" +
	"progress 10%\rprogress 60%\rprogress 100%\r\n" +
	"name\tvalue\r\n" +
	"\x1b[2A\x1b[Koverwritten\r\n" +
	"\x1b[?1049l"

// TestViewSurvivesHostileToolOutput feeds a pty capture through the tool event
// path and asserts the drawn frame keeps its geometry: every row is the same
// width as the frame, no control rune leaks, and the sequences that could move
// the cursor are gone.
func TestViewSurvivesHostileToolOutput(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 100, Height: 40})
	m = apply(t, m, toolStartMsg{id: "b1", name: "bash", input: map[string]any{"command": "script -qc pigo /dev/null"}})
	m = apply(t, m, toolEndMsg{id: "b1", ok: true, result: capturedTUIFrame})

	view := m.View().Content

	// The payload's own escape sequences must not reach the frame. (The renderer
	// legitimately emits SGR; these are the steering and mode sequences.)
	for _, seq := range []string{"\x1b[?1049h", "\x1b[?1049l", "\x1b[2A", "\x1b[2J", "\x1b[K", "\x1b[H"} {
		if strings.Contains(view, seq) {
			t.Errorf("payload sequence %q survived into the view", seq)
		}
	}

	rows := strings.Split(view, "\n")
	if len(rows) == 0 {
		t.Fatal("empty view")
	}
	want := ansi.StringWidth(rows[0])
	for i, row := range rows {
		if w := ansi.StringWidth(row); w != want {
			t.Errorf("row %d is %d cells wide, want %d: %q", i, w, want, row)
		}
		for _, r := range row {
			if unicode.IsControl(r) && r != '\x1b' {
				t.Fatalf("row %d leaks control rune %q: %q", i, r, row)
			}
		}
	}

	plain := stripANSI(view)
	// The progress bar shows its final state; superseded frames are gone.
	if !strings.Contains(plain, "progress 100%") {
		t.Errorf("progress bar should keep its final state:\n%s", plain)
	}
	if strings.Contains(plain, "progress 10%") {
		t.Errorf("superseded progress frames should be gone:\n%s", plain)
	}
	// Tab-expanded columns stay aligned and readable.
	if !strings.Contains(plain, "name    value") {
		t.Errorf("tab-aligned output should stay aligned:\n%s", plain)
	}
	// The tool's own borders are preserved as text inside the card, which is the
	// point: they are content now, not instructions to the terminal.
	if !strings.Contains(plain, "│ pigo(gpt-4o-mini)  │") {
		t.Errorf("tool output text should still be shown:\n%s", plain)
	}
}
