// Tests for sanitizing untrusted terminal text: the carriage returns, tabs and
// cursor sequences that tool output carries must not reach the frame, while SGR
// colors and intentional layout survive.
package tui

import (
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// TestSanitizeTerminalCarriageReturn verifies a bare CR rewinds the line like a
// terminal would, so a progress bar collapses to its final state rather than
// showing every frame the command drew.
func TestSanitizeTerminalCarriageReturn(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"progress":  {"10%\r50%\r100%", "100%"},
		"two lines": {"a\rb\nc", "b\nc"},
		"crlf":      {"one\r\ntwo", "one\ntwo"},
		"trailing":  {"done\r", "done"},
	}
	for name, tc := range cases {
		if got := sanitizeTerminal(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeTerminal(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// TestSanitizeTerminalDropsCursorSequences is the core guarantee: everything that
// can move the cursor out of the cell the layout reserved for it is removed. A
// surviving ESC [ 2 A paints over the two rows above the card.
func TestSanitizeTerminalDropsCursorSequences(t *testing.T) {
	hostile := []string{
		"\x1b[2A",                // cursor up
		"\x1b[H",                 // cursor home
		"\x1b[K",                 // erase to end of line
		"\x1b[2J",                // clear screen
		"\x1b[?25l",              // hide cursor
		"\x1b]0;title\x07",       // OSC window title
		"\x1b]8;;http://x\x1b\\", // OSC 8 hyperlink
		"\x1bP1;2;3q\x1b\\",      // DCS
		"\x1b(B",                 // charset select
		"\x1b7",                  // save cursor
	}
	for _, seq := range hostile {
		got := sanitizeTerminal("a" + seq + "b")
		if got != "ab" {
			t.Errorf("sanitizeTerminal(%q) = %q, want %q", "a"+seq+"b", got, "ab")
		}
	}
}

// TestSanitizeTerminalKeepsSGR verifies colors survive: they change the pen only,
// so a tool's colored output stays readable instead of turning monochrome.
func TestSanitizeTerminalKeepsSGR(t *testing.T) {
	in := "\x1b[31mred\x1b[0m plain"
	if got := sanitizeTerminal(in); got != in {
		t.Errorf("sanitizeTerminal(%q) = %q, want it unchanged", in, got)
	}
}

// TestSanitizeTerminalTabs verifies tabs expand on eight-column stops, matching
// what the terminal does, so tab-aligned tool output keeps its alignment.
func TestSanitizeTerminalTabs(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"at start":   {"\tx", "        x"},
		"from col 3": {"abc\tx", "abc     x"},
		"aligned":    {"abcdefgh\tx", "abcdefgh        x"},
		"two tabs":   {"\t\tx", "                x"},
	}
	for name, tc := range cases {
		if got := sanitizeTerminal(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeTerminal(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// TestSanitizeTerminalDropsControls verifies the remaining C0 controls and DEL are
// dropped: BEL would beep on every frame, BS can back over the card border.
func TestSanitizeTerminalDropsControls(t *testing.T) {
	if got := sanitizeTerminal("a\x07b\x08c\x7fd\x0be"); got != "abcde" {
		t.Errorf("got %q, want %q", got, "abcde")
	}
}

// TestSanitizeTerminalFastPath verifies ordinary text is returned untouched, which
// is what keeps the per-frame cost at one scan.
func TestSanitizeTerminalFastPath(t *testing.T) {
	in := "plain text\nwith CJK 中文 and emoji 🎉\twith a tab"
	if needsSanitizing("plain text\nwith CJK 中文") {
		t.Error("clean text must not need sanitizing")
	}
	// The tab forces the slow path but nothing else may change.
	if got := sanitizeTerminal(in); strings.ContainsRune(got, '\t') {
		t.Errorf("tab should have been expanded: %q", got)
	}
	clean := "plain text\nwith CJK 中文"
	if got := sanitizeTerminal(clean); got != clean {
		t.Errorf("clean text altered: %q", got)
	}
}

// TestCardRenderSurvivesCapturedTUIFrame renders the kind of payload the bug
// report showed: a captured full-screen program, complete with alt-screen entry,
// cursor addressing, CRLF and its own box borders. Every row of the card must
// still be exactly as wide as the frame, and no cursor-steering byte may survive.
func TestCardRenderSurvivesCapturedTUIFrame(t *testing.T) {
	frame := strings.Join([]string{
		"\x1b[?1049h\x1b[H\x1b[2J",                  // alt screen, home, clear
		"\x1b[1m│ pigo(gpt-4o-mini)  │\x1b[0m",      // the program's own borders
		"progress 10%\rprogress 60%\rprogress 100%", // a progress bar
		"col1\tcol2\tcol3",                          // tab-aligned columns
		"\x1b[2A\x1b[Koverwritten",                  // cursor up + erase
		"\x1b[?1049l",                               // leave alt screen
	}, "\r\n")

	card := newToolCard("c1", "bash", map[string]any{"command": "script -qc pigo /dev/null"})
	card.complete(cardSuccess, parseToolResult(frame), "")
	out := card.render(DefaultTheme(), 70)

	rows := strings.Split(out, "\n")
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

	// The progress bar must show its last state, not every frame.
	if !strings.Contains(out, "progress 100%") {
		t.Errorf("progress bar should collapse to its final state:\n%s", out)
	}
	if strings.Contains(out, "progress 10%") {
		t.Errorf("superseded progress frames should be gone:\n%s", out)
	}
	// Tab expansion keeps the columns readable.
	if !strings.Contains(out, "col1    col2    col3") {
		t.Errorf("tab-aligned columns should stay aligned:\n%s", out)
	}
}

// TestThinkingAndProseAreSanitized verifies the guarantee holds outside tool
// cards too: model text is wrapped through the same path, and a model echoing a
// file with escapes must not be able to draw over the frame.
func TestThinkingAndProseAreSanitized(t *testing.T) {
	got := WrapToWidth("before\x1b[2A\x1b[Kafter", 40)
	if strings.Contains(got, "\x1b[2A") || strings.Contains(got, "\x1b[K") {
		t.Errorf("cursor sequences survived wrapping: %q", got)
	}
	if got != "beforeafter" {
		t.Errorf("got %q, want %q", got, "beforeafter")
	}
}
