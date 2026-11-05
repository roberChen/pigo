package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file implements the modal (vim-style) editing core behind the TUI
// composer. It is deliberately a pure state machine over a plain text buffer:
// the key handler never touches the textarea, so every command — motions,
// operators, counts, registers, undo — is unit-testable without a terminal.
// input.go owns the translation between this core and the bubbles textarea
// (caret positions, buffer replacement, insert mode).
//
// Why a modal layer at all: the composer is where long prompts are written, and
// a textarea navigated only with arrow keys is a poor place to revise a
// paragraph. Normal mode gives the composer the motions and operators vim users
// already know (gg/G to jump, dd/yy/p to move blocks, u to undo) without giving
// up the multi-line insert experience the TUI already had.
//
// Scope: insert and normal modes; hjkl, word and line motions; d/c/y operators
// with counts; x/X/D/C/s/S/Y; i/a/I/A/o/O; p/P; r; J; ~; u; :q to leave. Visual
// mode, search (/, n), macros and text objects (ciw) are intentionally out of
// scope and are documented as such in README, so the limit is known rather than
// surprising.

// vimMode is the composer's editing mode while modal editing is enabled.
type vimMode uint8

const (
	// vimInsert is ordinary text entry: keys reach the textarea with vim's
	// meaning, so Enter breaks the line like it does in vim. The composer is an
	// editor, not a form: sending is the inline layout's Enter (leave full screen
	// with Ctrl+E or :q first), which is also what stops a vim user from firing a
	// half-written prompt with the Enter they pressed to start a new paragraph.
	vimInsert vimMode = iota
	// vimNormal is command mode, entered with Esc.
	vimNormal
)

// label is the name painted on the composer's top rule.
func (m vimMode) label() string {
	if m == vimNormal {
		return "NORMAL"
	}
	return "INSERT"
}

// vimCursor is a logical caret position: row indexes the line slice, col is a
// rune index into that line. In normal mode col points AT a character (0..len-1,
// and 0 on an empty line). In insert mode col may equal len — the position just
// past the last character.
type vimCursor struct{ row, col int }

// vimBuffer is the composer content as lines. It is the core's only input and
// output; the textarea value is the joined form.
type vimBuffer struct {
	lines []string
}

// newVimBuffer splits text into lines. The empty buffer is one empty line, which
// matches textarea.Value() ("" for an empty editor) and keeps every motion
// well-defined at (0,0).
func newVimBuffer(text string) vimBuffer {
	if text == "" {
		return vimBuffer{lines: []string{""}}
	}
	return vimBuffer{lines: strings.Split(text, "\n")}
}

// text joins the lines back into the editor value.
func (b vimBuffer) text() string { return strings.Join(b.lines, "\n") }

// line returns row r, or "" out of range.
func (b vimBuffer) line(r int) string {
	if r < 0 || r >= len(b.lines) {
		return ""
	}
	return b.lines[r]
}

// lastRow is the index of the final line (always >= 0).
func (b vimBuffer) lastRow() int { return len(b.lines) - 1 }

// total is the buffer length in runes, counting the separating newlines.
func (b vimBuffer) total() int {
	n := len(b.lines) - 1
	for _, l := range b.lines {
		n += utf8.RuneCountInString(l)
	}
	return n
}

// clampNormal keeps a normal-mode caret on an existing character.
func (b vimBuffer) clampNormal(p vimCursor) vimCursor {
	if p.row < 0 {
		p.row = 0
	}
	if p.row > b.lastRow() {
		p.row = b.lastRow()
	}
	if p.col < 0 {
		p.col = 0
	}
	n := utf8.RuneCountInString(b.line(p.row))
	if p.col > n-1 {
		p.col = n - 1
	}
	if p.col < 0 {
		p.col = 0
	}
	return p
}

// clampInsert keeps an insertion caret within the line, allowing col == len.
func (b vimBuffer) clampInsert(p vimCursor) vimCursor {
	if p.row < 0 {
		p.row = 0
	}
	if p.row > b.lastRow() {
		p.row = b.lastRow()
	}
	if p.col < 0 {
		p.col = 0
	}
	if n := utf8.RuneCountInString(b.line(p.row)); p.col > n {
		p.col = n
	}
	return p
}

// offsetOf converts a logical position to an absolute rune offset into text().
// A col past the end of a line (allowed for motion targets such as $) overshoots
// into the following line; callers clamp when they need a real caret.
func (b vimBuffer) offsetOf(p vimCursor) int {
	if p.row < 0 {
		return 0
	}
	off := 0
	for i := 0; i < p.row && i < len(b.lines); i++ {
		off += utf8.RuneCountInString(b.lines[i]) + 1
	}
	return off + max(p.col, 0)
}

// posOf converts an absolute rune offset back to (row, col).
func (b vimBuffer) posOf(off int) vimCursor {
	if off <= 0 {
		return vimCursor{}
	}
	row := 0
	for row < len(b.lines) {
		n := utf8.RuneCountInString(b.lines[row]) + 1
		if off < n {
			return vimCursor{row: row, col: off}
		}
		off -= n
		row++
	}
	last := b.lastRow()
	return vimCursor{row: last, col: utf8.RuneCountInString(b.line(last))}
}

// vimUndo is one undo/redo entry: the full buffer plus caret at that point.
// Prompts are small (kilobytes), so snapshotting the whole string beats keeping
// a command log and is far less error-prone.
type vimUndo struct {
	text   string
	cursor vimCursor
}

// vimState is the persistent part of normal mode: the count/operator being
// assembled, the unnamed register, and the undo stacks. The buffer itself always
// comes fresh from the textarea, so the state never goes stale.
type vimState struct {
	count       int    // count prefix being typed (0 = none)
	op          rune   // pending operator: 'd', 'c', 'y' (0 = none)
	opCount     int    // count typed before the operator (>= 1 once pending)
	prefix      rune   // pending multi-key prefix: 'g', 'f', 'F', 't', 'T', 'r'
	register    string // unnamed register contents
	regLinewise bool   // register holds whole lines
	undo        []vimUndo
	redo        []vimUndo
}

// vimOutcome is what one normal-mode key produced.
type vimOutcome struct {
	handled  bool      // the key was consumed (normal mode swallows everything)
	changed  bool      // value holds a new buffer
	value    string    // new buffer text when changed
	cursor   vimCursor // caret after the command (normal-mode position)
	insert   bool      // switch to insert mode
	insertAt vimCursor // insertion caret when insert is true
	leave    bool      // leave the full-screen composer (:q)
}

// noop is the common "key consumed, nothing to do" outcome.
func noop(cur vimCursor) vimOutcome { return vimOutcome{handled: true, cursor: cur} }

// take consumes and returns the count prefix (at least 1).
func (s *vimState) take() int {
	n := s.count
	s.count = 0
	if n < 1 {
		return 1
	}
	return n
}

// reset clears a partially typed command (Esc, or a prefix given an impossible
// follow-up key).
func (s *vimState) reset() {
	s.count = 0
	s.op = 0
	s.opCount = 0
	s.prefix = 0
}

// pushUndo records b/cur before a mutation and drops the redo stack, matching
// vim: a new change invalidates the redo branch.
func (s *vimState) pushUndo(b vimBuffer, cur vimCursor) {
	s.pushUndoText(b.text(), cur)
}

// pushUndoText is the same for callers that already hold the text (the input
// layer records a whole insert session this way).
func (s *vimState) pushUndoText(text string, cur vimCursor) {
	s.undo = append(s.undo, vimUndo{text: text, cursor: cur})
	if len(s.undo) > vimUndoLimit {
		s.undo = s.undo[len(s.undo)-vimUndoLimit:]
	}
	s.redo = nil
}

// vimUndoLimit bounds the snapshot history so a long editing session cannot grow
// the composer unboundedly.
const vimUndoLimit = 200

// key feeds one key press to the core. key is the Bubble Tea key string (e.g.
// "j", "esc", "left", "space"); text is the message's Text field, used by
// commands that wait for a literal character (f, t, r).
func (s *vimState) key(key, text string, b vimBuffer, cur vimCursor) vimOutcome {
	cur = b.clampNormal(cur)

	if key == "esc" || key == "ctrl+[" {
		s.reset()
		return noop(cur)
	}

	// A pending prefix (g / f / F / t / T / r) consumes this key whatever it is.
	if s.prefix != 0 {
		return s.applyPrefix(key, text, b, cur)
	}

	// Counts. "0" is a motion unless a count is already being assembled.
	if r, ok := vimPrintableRune(key, text); ok && r >= '0' && r <= '9' &&
		(r != '0' || s.count > 0) {
		s.count = s.count*10 + int(r-'0')
		return noop(cur)
	}

	// While an operator is pending, this key names its motion (or repeats the
	// operator, which means linewise).
	if s.op != 0 {
		return s.applyOperatorMotion(key, text, b, cur)
	}

	switch key {
	case "h", "left", "backspace":
		return s.moveChar(-s.take(), b, cur)
	case "l", "right", "space":
		return s.moveChar(s.take(), b, cur)
	case "j", "down":
		return s.moveLine(s.take(), b, cur)
	case "k", "up":
		return s.moveLine(-s.take(), b, cur)
	case "0":
		return noop(b.clampNormal(vimCursor{row: cur.row}))
	case "^":
		return noop(b.clampNormal(vimCursor{row: cur.row, col: firstNonBlank(b.line(cur.row))}))
	case "$", "end":
		return noop(vimCursor{row: cur.row, col: max(0, utf8.RuneCountInString(b.line(cur.row))-1)})
	case "w", "W":
		return s.moveWord(1, false, b, cur)
	case "b", "B":
		return s.moveWord(-1, false, b, cur)
	case "e", "E":
		return s.moveWord(1, true, b, cur)
	case "f", "F", "t", "T":
		s.prefix = rune(key[0])
		return noop(cur)
	case ":":
		// An ex command line. Only :q is implemented: the composer has no file to
		// write and no buffer to abandon, so ":q" is the documented way back to the
		// inline layout (Ctrl+E is the shortcut).
		s.prefix = ':'
		return noop(cur)
	case "G":
		row := b.lastRow()
		if s.count > 0 {
			row = min(s.count-1, b.lastRow())
		}
		s.count = 0
		return noop(vimCursor{row: row})
	case "g":
		s.prefix = 'g'
		return noop(cur)
	case "i":
		return enterInsert(vimCursor{row: cur.row, col: cur.col})
	case "a":
		return enterInsert(vimCursor{row: cur.row, col: cur.col + 1})
	case "I":
		return enterInsert(vimCursor{row: cur.row, col: firstNonBlank(b.line(cur.row))})
	case "A":
		return enterInsert(vimCursor{row: cur.row, col: utf8.RuneCountInString(b.line(cur.row))})
	case "o":
		return s.openLine(1, b, cur)
	case "O":
		return s.openLine(-1, b, cur)
	case "d", "c", "y":
		s.op = rune(key[0])
		s.opCount = s.take()
		return noop(cur)
	case "x":
		return s.deleteChars(1, b, cur)
	case "X":
		return s.deleteChars(-1, b, cur)
	case "D":
		return s.applyOp('d', cur, vimCursor{row: cur.row, col: utf8.RuneCountInString(b.line(cur.row))}, false, b)
	case "C":
		return s.applyOp('c', cur, vimCursor{row: cur.row, col: utf8.RuneCountInString(b.line(cur.row))}, false, b)
	case "Y":
		return s.yankLines(s.take(), b, cur)
	case "s":
		return s.changeChars(s.take(), b, cur)
	case "S":
		return s.applyLinewise('c', b, cur, cur.row, min(cur.row+s.take()-1, b.lastRow()))
	case "p", "P":
		return s.paste(key == "p", b, cur)
	case "r":
		s.prefix = 'r'
		return noop(cur)
	case "J":
		return s.joinLines(s.take(), b, cur)
	case "~":
		return s.toggleCase(s.take(), b, cur)
	case "u":
		return s.doUndo(b, cur)
	}

	// Unknown normal-mode keys (and printable characters not mapped above) are
	// swallowed: in normal mode they must never leak into the buffer.
	return noop(cur)
}

// applyPrefix resolves a multi-key command.
func (s *vimState) applyPrefix(key, text string, b vimBuffer, cur vimCursor) vimOutcome {
	p := s.prefix
	s.prefix = 0

	switch p {
	case 'g':
		if key != "g" {
			s.reset()
			return noop(cur)
		}
		n := s.take()
		row := 0
		if n > 1 {
			row = min(n-1, b.lastRow())
		}
		if s.op != 0 {
			op := s.op
			s.reset()
			// dgg/dGy are linewise: rows from the caret to the target.
			return s.applyLinewise(op, b, cur, min(cur.row, row), max(cur.row, row))
		}
		return noop(vimCursor{row: row})

	case 'f', 'F', 't', 'T':
		r, ok := vimPrintableRune(key, text)
		if !ok {
			s.reset()
			return noop(cur)
		}
		n := s.take() * max(s.opCount, 1)
		target, ok := findChar(stripRunes(b.line(cur.row)), cur.col, r, p, n)
		if !ok {
			s.reset()
			return noop(cur)
		}
		tp := vimCursor{row: cur.row, col: target}
		if s.op != 0 {
			op := s.op
			s.reset()
			// t/T stop one character short, so the span is inclusive up to the
			// cursor position vim would leave behind.
			return s.applyOp(op, cur, tp, true, b)
		}
		s.reset()
		return noop(b.clampNormal(tp))

	case ':':
		// An unsupported ex command is dropped, exactly like an unknown normal-mode
		// key: the draft is never touched by a mistyped command.
		s.reset()
		if r, ok := vimPrintableRune(key, text); ok && r == 'q' {
			return vimOutcome{handled: true, cursor: cur, leave: true}
		}
		return noop(cur)

	case 'r':
		r, ok := vimPrintableRune(key, text)
		if !ok || r == '\n' {
			s.reset()
			return noop(cur)
		}
		return s.replaceRune(r, s.take(), b, cur)
	}

	s.reset()
	return noop(cur)
}

// applyOperatorMotion resolves a motion while an operator is pending.
func (s *vimState) applyOperatorMotion(key, text string, b vimBuffer, cur vimCursor) vimOutcome {
	// g and f/t are themselves prefixes; they persist across the next key.
	switch key {
	case "g", "f", "F", "t", "T", ":":
		s.prefix = rune(key[0])
		return noop(cur)
	}
	if key == string(s.op) {
		// dd / cc / yy with counts: whole lines.
		n := max(s.opCount, 1) * s.take()
		op := s.op
		s.reset()
		return s.applyLinewise(op, b, cur, cur.row, min(cur.row+n-1, b.lastRow()))
	}

	motionN := s.take()
	n := max(s.opCount, 1) * motionN

	// vim's cw is ce when the cursor is on a non-blank: changing a word must not
	// swallow the whitespace after it (the cursor is left where the word ended).
	if s.op == 'c' && (key == "w" || key == "W") &&
		classAt(stripRunes(b.line(cur.row)), cur.col) != 0 {
		key = "e"
	}

	var (
		target    vimCursor
		inclusive bool
		linewise  bool
		ok        = true
	)
	switch key {
	case "h", "left", "backspace":
		target = vimCursor{row: cur.row, col: max(0, cur.col-n)}
	case "l", "right", "space":
		// A target past the line end is allowed: dl on the last character
		// deletes it (the span is clamped to the line's end offset).
		target = vimCursor{row: cur.row, col: min(cur.col+n, utf8.RuneCountInString(b.line(cur.row)))}
	case "j", "down":
		target = vimCursor{row: min(cur.row+n, b.lastRow()), col: cur.col}
		linewise = true
	case "k", "up":
		target = vimCursor{row: max(cur.row-n, 0), col: cur.col}
		linewise = true
	case "G":
		row := b.lastRow()
		if n > 1 {
			row = min(n-1, b.lastRow())
		}
		target = vimCursor{row: row, col: cur.col}
		linewise = true
	case "0":
		target = vimCursor{row: cur.row}
	case "^":
		target = vimCursor{row: cur.row, col: firstNonBlank(b.line(cur.row))}
	case "$":
		target = vimCursor{row: cur.row, col: utf8.RuneCountInString(b.line(cur.row))}
	case "w", "W":
		target = b.posOf(wordForward(stripRunes(b.text()), b.offsetOf(cur), n))
		if n == 1 && target.row > cur.row {
			// A single dw/yw on the last word of a line stops at the line end
			// instead of eating the newline (vim's documented behavior).
			target = vimCursor{row: cur.row, col: utf8.RuneCountInString(b.line(cur.row))}
		}
	case "b", "B":
		target = b.posOf(wordBack(stripRunes(b.text()), b.offsetOf(cur), n))
	case "e", "E":
		target = b.posOf(wordEnd(stripRunes(b.text()), b.offsetOf(cur), n))
		inclusive = true
	default:
		ok = false
	}

	if !ok {
		s.reset()
		return noop(cur)
	}
	op := s.op
	s.reset()
	if linewise {
		return s.applyLinewise(op, b, cur, min(cur.row, target.row), max(cur.row, target.row))
	}
	return s.applyOp(op, cur, target, inclusive, b)
}

// applyOp performs op over the span between cur and target.
func (s *vimState) applyOp(op rune, cur, target vimCursor, inclusive bool, b vimBuffer) vimOutcome {
	rs := []rune(b.text())
	sp := charwiseSpan(b, cur, target, inclusive)
	if sp.end <= sp.start {
		return noop(cur)
	}
	s.setRegister(string(rs[sp.start:sp.end]), false)

	switch op {
	case 'y':
		return noop(b.clampNormal(b.posOf(sp.start)))
	case 'd', 'c':
		s.pushUndo(b, cur)
		nb := newVimBuffer(string(rs[:sp.start]) + string(rs[sp.end:]))
		pos := nb.clampNormal(nb.posOf(sp.start))
		if op == 'c' {
			return vimOutcome{handled: true, changed: nb.text() != b.text(), value: nb.text(),
				insert: true, insertAt: nb.clampInsert(nb.posOf(sp.start)), cursor: pos}
		}
		return vimOutcome{handled: true, changed: true, value: nb.text(), cursor: pos}
	}
	return noop(cur)
}

// applyLinewise performs a whole-line operator over rows r1..r2.
func (s *vimState) applyLinewise(op rune, b vimBuffer, cur vimCursor, r1, r2 int) vimOutcome {
	r1 = max(0, min(r1, b.lastRow()))
	r2 = max(r1, min(r2, b.lastRow()))

	linewiseText := strings.Join(b.lines[r1:r2+1], "\n")
	if op == 'y' {
		s.setRegister(linewiseText, true)
		return noop(b.clampNormal(vimCursor{row: r1, col: firstNonBlank(b.line(r1))}))
	}

	s.pushUndo(b, cur)
	s.setRegister(linewiseText, true)

	if op == 'c' {
		lines := make([]string, 0, len(b.lines)-(r2-r1))
		lines = append(lines, b.lines[:r1]...)
		lines = append(lines, "")
		lines = append(lines, b.lines[r2+1:]...)
		nb := vimBuffer{lines: lines}
		return vimOutcome{handled: true, changed: true, value: nb.text(),
			insert: true, insertAt: vimCursor{row: r1}, cursor: vimCursor{row: r1}}
	}

	rs := []rune(b.text())
	start := b.offsetOf(vimCursor{row: r1})
	end := b.offsetOf(vimCursor{row: r2, col: utf8.RuneCountInString(b.line(r2))})
	if r2 < b.lastRow() {
		end++ // include the newline terminating the last deleted line
	} else if start > 0 {
		// Deleting the final line must also consume the newline before it,
		// otherwise the buffer would end in a blank line vim does not have.
		start--
	}
	nb := newVimBuffer(string(rs[:start]) + string(rs[end:]))
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		cursor: nb.clampNormal(nb.posOf(start))}
}

// charwiseSpan converts two caret positions into an exclusive rune-offset span.
// Backward ranges run from the target to the caret; forward ranges end one past
// the target when the motion is inclusive.
func charwiseSpan(b vimBuffer, cur, target vimCursor, inclusive bool) vimSpan {
	a := b.offsetOf(cur)
	z := b.offsetOf(target)
	if z < a {
		return vimSpan{start: z, end: a}
	}
	end := z
	if inclusive {
		end++
	}
	if end < a {
		end = a
	}
	if total := b.total(); end > total {
		end = total
	}
	return vimSpan{start: a, end: end}
}

// vimSpan is an absolute rune-offset range [start,end).
type vimSpan struct {
	start, end int
}

// setRegister stores yanked/deleted text in the unnamed register.
func (s *vimState) setRegister(text string, linewise bool) {
	s.register = text
	s.regLinewise = linewise
}

// moveChar moves within the current line only, like h/l.
func (s *vimState) moveChar(delta int, b vimBuffer, cur vimCursor) vimOutcome {
	return noop(b.clampNormal(vimCursor{row: cur.row, col: cur.col + delta}))
}

// moveLine moves by whole logical lines, preserving the column when it fits.
func (s *vimState) moveLine(delta int, b vimBuffer, cur vimCursor) vimOutcome {
	return noop(b.clampNormal(vimCursor{row: cur.row + delta, col: cur.col}))
}

// moveWord moves by word. end selects the "end of word" variant (e/E).
func (s *vimState) moveWord(dir int, end bool, b vimBuffer, cur vimCursor) vimOutcome {
	n := s.take()
	rs := []rune(b.text())
	off := b.offsetOf(cur)
	for i := 0; i < n; i++ {
		switch {
		case end:
			off = wordEnd(rs, off, 1)
		case dir > 0:
			off = wordForward(rs, off, 1)
		default:
			off = wordBack(rs, off, 1)
		}
	}
	return noop(b.clampNormal(b.posOf(off)))
}

// deleteChars implements x (dir 1) and X (dir -1): count characters on the
// current line, never crossing a line break.
func (s *vimState) deleteChars(dir int, b vimBuffer, cur vimCursor) vimOutcome {
	n := s.take()
	line := stripRunes(b.line(cur.row))
	var from, to int
	if dir > 0 {
		from, to = cur.col, min(cur.col+n, len(line))
	} else {
		from, to = max(0, cur.col-n), cur.col
	}
	if from >= to {
		return noop(cur)
	}
	s.pushUndo(b, cur)
	s.setRegister(string(line[from:to]), false)
	lines := append([]string{}, b.lines...)
	lines[cur.row] = string(append(append([]rune{}, line[:from]...), line[to:]...))
	nb := vimBuffer{lines: lines}
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		cursor: nb.clampNormal(vimCursor{row: cur.row, col: from})}
}

// changeChars implements s: delete n characters then insert.
func (s *vimState) changeChars(n int, b vimBuffer, cur vimCursor) vimOutcome {
	lineLen := utf8.RuneCountInString(b.line(cur.row))
	target := vimCursor{row: cur.row, col: min(cur.col+n, lineLen)}
	return s.applyOp('c', cur, target, false, b)
}

// yankLines implements Y: yank n whole lines.
func (s *vimState) yankLines(n int, b vimBuffer, cur vimCursor) vimOutcome {
	return s.applyLinewise('y', b, cur, cur.row, min(cur.row+n-1, b.lastRow()))
}

// openLine implements o (dir 1) and O (dir -1): insert an empty line and enter
// insert mode there.
func (s *vimState) openLine(dir int, b vimBuffer, cur vimCursor) vimOutcome {
	s.pushUndo(b, cur)
	at := cur.row + 1
	if dir < 0 {
		at = cur.row
	}
	lines := make([]string, 0, len(b.lines)+1)
	lines = append(lines, b.lines[:at]...)
	lines = append(lines, "")
	lines = append(lines, b.lines[at:]...)
	nb := vimBuffer{lines: lines}
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		insert: true, insertAt: vimCursor{row: at}, cursor: vimCursor{row: at}}
}

// paste implements p/P from the unnamed register.
func (s *vimState) paste(after bool, b vimBuffer, cur vimCursor) vimOutcome {
	if s.register == "" && !s.regLinewise {
		return noop(cur)
	}
	s.pushUndo(b, cur)

	if s.regLinewise {
		reg := strings.Split(s.register, "\n")
		at := cur.row
		if after {
			at = cur.row + 1
		}
		lines := make([]string, 0, len(b.lines)+len(reg))
		lines = append(lines, b.lines[:at]...)
		lines = append(lines, reg...)
		lines = append(lines, b.lines[at:]...)
		nb := vimBuffer{lines: lines}
		return vimOutcome{handled: true, changed: true, value: nb.text(),
			cursor: vimCursor{row: at, col: firstNonBlank(reg[0])}}
	}

	rs := []rune(b.text())
	at := b.offsetOf(cur)
	if after && len(stripRunes(b.line(cur.row))) > 0 {
		at++
	}
	nb := newVimBuffer(string(rs[:at]) + s.register + string(rs[at:]))
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		cursor: nb.clampNormal(nb.posOf(at))}
}

// replaceRune implements r: replace n characters with r, on the current line.
func (s *vimState) replaceRune(r rune, n int, b vimBuffer, cur vimCursor) vimOutcome {
	line := stripRunes(b.line(cur.row))
	end := cur.col + n
	if cur.col >= len(line) || end > len(line) {
		return noop(cur)
	}
	s.pushUndo(b, cur)
	out := append([]rune{}, line...)
	for i := cur.col; i < end; i++ {
		out[i] = r
	}
	lines := append([]string{}, b.lines...)
	lines[cur.row] = string(out)
	nb := vimBuffer{lines: lines}
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		cursor: vimCursor{row: cur.row, col: end - 1}}
}

// joinLines implements J: join n lines (at least the current and the next).
func (s *vimState) joinLines(n int, b vimBuffer, cur vimCursor) vimOutcome {
	if cur.row >= b.lastRow() {
		return noop(cur)
	}
	rows := min(max(n, 2)-1, b.lastRow()-cur.row)
	s.pushUndo(b, cur)
	lines := append([]string{}, b.lines...)
	joined := lines[cur.row]
	col := utf8.RuneCountInString(joined)
	for i := 0; i < rows; i++ {
		next := strings.TrimLeft(lines[cur.row+1], " \t")
		if joined != "" || next != "" {
			joined += " "
		}
		joined += next
		lines = append(lines[:cur.row+1], lines[cur.row+2:]...)
	}
	lines[cur.row] = joined
	nb := vimBuffer{lines: lines}
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		cursor: nb.clampNormal(vimCursor{row: cur.row, col: col})}
}

// toggleCase implements ~: swap the case of n characters and advance.
func (s *vimState) toggleCase(n int, b vimBuffer, cur vimCursor) vimOutcome {
	line := stripRunes(b.line(cur.row))
	if cur.col >= len(line) {
		return noop(cur)
	}
	end := min(cur.col+n, len(line))
	s.pushUndo(b, cur)
	out := append([]rune{}, line...)
	for i := cur.col; i < end; i++ {
		out[i] = toggleRune(out[i])
	}
	lines := append([]string{}, b.lines...)
	lines[cur.row] = string(out)
	nb := vimBuffer{lines: lines}
	return vimOutcome{handled: true, changed: true, value: nb.text(),
		cursor: nb.clampNormal(vimCursor{row: cur.row, col: end})}
}

// doUndo pops the undo stack. Redo is not bound to a key yet, but the stack is
// maintained so adding one needs no new bookkeeping.
func (s *vimState) doUndo(b vimBuffer, cur vimCursor) vimOutcome {
	if len(s.undo) == 0 {
		return noop(cur)
	}
	last := s.undo[len(s.undo)-1]
	s.undo = s.undo[:len(s.undo)-1]
	s.redo = append(s.redo, vimUndo{text: b.text(), cursor: cur})
	nb := newVimBuffer(last.text)
	return vimOutcome{handled: true, changed: true, value: last.text,
		cursor: nb.clampNormal(last.cursor)}
}

// enterInsert builds an outcome that switches to insert mode at p.
func enterInsert(p vimCursor) vimOutcome {
	return vimOutcome{handled: true, insert: true, insertAt: p}
}

// ---- pure rune helpers -----------------------------------------------------

// stripRunes converts a line to runes.
func stripRunes(s string) []rune { return []rune(s) }

// vimPrintableRune reports the literal character a key press carries. Space has
// no Text in Bubble Tea, so it is special-cased; modifier and control keys
// report ok=false.
func vimPrintableRune(key, text string) (rune, bool) {
	if key == "space" {
		return ' ', true
	}
	if text == "" {
		return 0, false
	}
	rs := []rune(text)
	if len(rs) != 1 {
		return 0, false
	}
	return rs[0], true
}

// vimClass groups characters the way vim's word motions do: 0 space, 1 keyword
// (letters, digits, underscore — CJK letters included), 2 other punctuation.
func vimClass(r rune) int {
	if unicode.IsSpace(r) {
		return 0
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return 1
	}
	return 2
}

// classAt is vimClass for an absolute offset (past the end counts as space).
func classAt(rs []rune, off int) int {
	if off < 0 || off >= len(rs) {
		return 0
	}
	return vimClass(rs[off])
}

// wordForward returns the offset of the next word start, n words away.
func wordForward(rs []rune, off, n int) int {
	for i := 0; i < n; i++ {
		if off >= len(rs) {
			break
		}
		if cl := classAt(rs, off); cl != 0 {
			for off < len(rs) && classAt(rs, off) == cl {
				off++
			}
		}
		for off < len(rs) && classAt(rs, off) == 0 {
			off++
		}
	}
	return off
}

// wordBack returns the offset of the previous word start, n words away.
func wordBack(rs []rune, off, n int) int {
	for i := 0; i < n; i++ {
		if off > 0 {
			off--
		}
		for off > 0 && classAt(rs, off) == 0 {
			off--
		}
		cl := classAt(rs, off)
		if cl == 0 {
			continue
		}
		for off > 0 && classAt(rs, off-1) == cl {
			off--
		}
	}
	return off
}

// wordEnd returns the offset of the end of the next word, n words away.
func wordEnd(rs []rune, off, n int) int {
	if len(rs) == 0 {
		return 0
	}
	for i := 0; i < n; i++ {
		if off < len(rs)-1 {
			off++
		}
		for off < len(rs) && classAt(rs, off) == 0 {
			off++
		}
		if off >= len(rs) {
			return len(rs) - 1
		}
		cl := classAt(rs, off)
		for off+1 < len(rs) && classAt(rs, off+1) == cl {
			off++
		}
	}
	return off
}

// findChar implements f/F/t/T on the current line: key selects the direction and
// whether the cursor lands on ('f'/'F') or just before ('t'/'T') the target,
// repeated n times.
func findChar(line []rune, col int, target, key rune, n int) (int, bool) {
	pos := col
	for i := 0; i < n; i++ {
		found := -1
		switch key {
		case 'f', 't':
			for j := pos + 1; j < len(line); j++ {
				if line[j] == target {
					found = j
					break
				}
			}
		case 'F', 'T':
			for j := pos - 1; j >= 0; j-- {
				if line[j] == target {
					found = j
					break
				}
			}
		}
		if found < 0 {
			return 0, false
		}
		switch key {
		case 't':
			pos = found - 1
		case 'T':
			pos = found + 1
		default:
			pos = found
		}
	}
	if pos == col {
		return 0, false
	}
	return pos, true
}

// firstNonBlank is the column of the first non-space rune on a line (^ and I).
func firstNonBlank(line string) int {
	for i, r := range []rune(line) {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return 0
}

// toggleRune flips the case of a single rune.
func toggleRune(r rune) rune {
	switch {
	case unicode.IsUpper(r):
		return unicode.ToLower(r)
	case unicode.IsLower(r):
		return unicode.ToUpper(r)
	}
	return r
}
