// This file implements sanitizing untrusted terminal text before it is drawn
// into the TUI frame.
//
// Tool output is not plain text. A command that ran a full-screen program (a
// pty capture, a progress bar, `git diff --color`) hands us carriage returns,
// tabs, SGR colors and, worse, cursor movement and erase sequences. Drawing
// those verbatim into the frame lets the payload move the cursor out of the
// cell the layout reserved for it: the border of the surrounding card breaks,
// and an `ESC [ 2 A` or `ESC [ K` paints over rows that belong to other blocks.
// Width math cannot catch this - an escape sequence occupies zero cells, so a
// line measures as fitting while the terminal draws it somewhere else entirely.
//
// So content passes through here first: SGR colors survive (they only change
// the pen, never the cursor), carriage returns and tabs are interpreted the way
// a terminal would, and everything else that can steer the cursor is dropped.
// The invariant afterwards is that the string only moves the pen: it can be
// placed at a known column and stay there.
package tui

import (
	"strings"
	"unicode/utf8"
)

// tabStop is the terminal's tab interval. Tabs become spaces up to the next
// multiple of eight columns so tab-aligned output (Makefiles, YAML, diffs) keeps
// the alignment the author intended instead of collapsing to one column.
const tabStop = 8

// sanitizeTerminal returns s with terminal control sequences reduced to what is
// safe to draw at a fixed position: SGR colors are kept, CR/TAB are expanded to
// their visual result, and cursor movement, erasing, OSC and stray C0 controls
// are removed. Content without any control byte is returned as-is, so the common
// case costs one scan and no allocation.
func sanitizeTerminal(s string) string {
	if !needsSanitizing(s) {
		return s
	}
	buf := make([]byte, 0, len(s))
	lineStart := 0 // offset in buf where the current line begins, for CR
	col := 0       // display column the cursor would be at, for TAB stops

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\x1b':
			seq, n := readEscape(s[i:])
			if isSGR(seq) {
				buf = append(buf, seq...)
			}
			i += n

		case c == '\n':
			buf = append(buf, '\n')
			i++
			lineStart = len(buf)
			col = 0

		case c == '\r':
			switch {
			case i+1 < len(s) && s[i+1] == '\n':
				// CRLF is just a line break; the LF case below emits it.
				i++
				continue
			case i+1 == len(s):
				// A trailing CR rewinds the cursor but nothing follows to overwrite
				// the line, so the text stays on screen. This is also what CRLF
				// content looks like once a caller split the text on "\n": the CR
				// is all that remains of the terminator, and treating it as an
				// erase would blank every line of the tool's output.
				i++
				continue
			}
			// A bare CR in the middle rewinds the cursor, so what follows is drawn
			// over the line. A progress bar therefore ends up showing its last
			// state instead of every frame the command drew.
			buf = buf[:lineStart]
			col = 0
			i++

		case c == '\t':
			n := tabStop - col%tabStop
			buf = append(buf, strings.Repeat(" ", n)...)
			col += n
			i++

		case c < 0x20 || c == 0x7f:
			// Other C0 controls (BEL, BS, VT, FF, ESC-less SO/SI) and DEL carry no
			// text and can move or reflow the cursor: drop them.
			i++

		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				// Invalid UTF-8: skip the byte rather than emitting a replacement
				// rune whose width the terminal and we would disagree on.
				i++
				continue
			}
			buf = append(buf, s[i:i+size]...)
			col += runeWidth(r)
			i += size
		}
	}
	return string(buf)
}

// needsSanitizing reports whether s holds a byte sanitizeTerminal would change.
// Newlines are fine; everything else below 0x20, plus DEL and ESC, is not.
func needsSanitizing(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; (b < 0x20 && b != '\n') || b == 0x7f {
			return true
		}
	}
	return false
}

// readEscape returns the escape sequence at the start of s and how many bytes it
// occupies, so the caller can skip it. An unterminated sequence swallows the
// rest of the string: the terminal would otherwise keep waiting for its final
// byte and eat the frame that follows.
func readEscape(s string) (string, int) {
	if len(s) < 2 {
		return s, len(s)
	}
	switch s[1] {
	case '[': // CSI: ESC [ parameters intermediates final
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e { // final byte
				return s[:i+1], i + 1
			}
		}
		return s, len(s)
	case ']': // OSC: terminated by BEL or ST (ESC \); often sets the title
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return s[:i+1], i + 1
			}
			if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
				return s[:i+2], i + 2
			}
		}
		return s, len(s)
	case 'P', 'X', '^', '_': // DCS/SOS/PM/APC: string sequences, ST-terminated
		for i := 2; i < len(s); i++ {
			if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
				return s[:i+2], i + 2
			}
		}
		return s, len(s)
	}
	// Two-byte escapes (ESC 7, ESC =, ESC c) and charset selection (ESC ( B).
	if len(s) >= 3 && (s[1] == '(' || s[1] == ')' || s[1] == '#' || s[1] == '%') {
		return s[:3], 3
	}
	return s[:2], 2
}

// isSGR reports whether seq is a Select Graphic Rendition sequence - the only
// kind that changes the pen without moving the cursor, and therefore the only
// kind safe to pass through.
func isSGR(seq string) bool {
	return len(seq) >= 3 && strings.HasPrefix(seq, "\x1b[") && seq[len(seq)-1] == 'm'
}
