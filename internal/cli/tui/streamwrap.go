// This file implements the incremental render of the still-streaming assistant
// block: the one block whose text changes on every delta.
//
// A delta only ever appends to the block's text, and greedy wrapping never
// revisits a decision once a row is closed — appending text can extend the last
// row and push the overflow onto following rows, but every row already closed
// stays exactly as it was. So the cursor keeps a byte buffer of the text it has
// seen, the rows it has already closed (wrapped and styled), and the one open row
// the new text lands in: a delta appends to the buffer and marks the new bytes
// pending, and the next layout wraps only those. That turns a delta from "wrap,
// style and copy the whole answer" — ~3ms per delta once an answer reaches
// 64 KiB, i.e. quadratic over the answer, and the reason streaming visibly slowed
// down as a reply grew — into O(delta).
//
// Equivalence with the non-incremental path matters: the rows produced here are
// the rows renderBlock's streaming branch would produce,
// theme.Assistant.Render(WrapToWidth(text, width)) split on newlines. So the wrap
// rule is wrapLine's, copied deliberately — same runeWidth, same "a rune that
// would overflow starts a new row", same hard newlines, and the same allowance
// for a one-rune row to overflow when the rune is wider than the width.
//
// The cursor is fed at ingest (append) but wrapped at layout (sync), because the
// wrap width is decided by reflow — which may narrow by a column for the
// scrollbar — and not by the code that received the delta.
package tui

import (
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// streamRender is the wrap+style cursor of one streaming block.
//
// buf is every byte of the block's text, appended amortized (a delta costs the
// growth of the byte slice, not a copy of the answer). pending is the suffix that
// has not been wrapped yet; it is drained by sync. rows holds the closed rows plus
// the row still being filled (always the last element), and open/openW are that
// row's source text and display width, which is what the wrap decision needs.
//
// sanitized latches once the block carries control bytes. From then on every
// layout re-wraps the whole sanitized text: CR overwrite, TAB expansion and OSC
// removal are whole-string operations, so a suffix cannot be fed in on its own
// and still agree with the finalized render. Assistant prose does not carry
// control bytes, so the fallback costs nothing in the case this exists for.
type streamRender struct {
	width     int
	buf       []byte
	pending   []byte
	rows      []string
	open      []rune
	openW     int
	sanitized bool
	// raw makes the cursor keep plain row text instead of styling each row. A
	// thinking block is laid out by thinkingRows, which only ever emits the handful
	// of rows it shows, so it indents and styles there (inside the style span, the
	// way every other render applies an indent) rather than paying for a style call
	// per closed row that may never be drawn.
	raw bool
	// contentRows is how many rows the block has up to and including its last
	// non-empty one, which is what trimmed() cuts at.
	contentRows int
	// consumed is how many bytes of the caller's text have been ingested, so a
	// caller that hands over growing text (ingest, rowsFor) feeds only the new
	// suffix.
	consumed int
}

// append ingests text without wrapping it. It is the hot path: the byte slice
// grows amortized and nothing else happens until the next sync.
func (s *streamRender) append(text string) {
	if text == "" {
		return
	}
	if needsSanitizing(text) {
		s.sanitized = true
	}
	s.buf = append(s.buf, text...)
	s.pending = append(s.pending, text...)
	s.consumed += len(text)
}

// ingest feeds the part of text the cursor has not seen. It is the entry point
// for a source that resends the whole value each time (reasoning updates arrive as
// a growing partial) as well as for a caller with a delta in hand: either way only
// the new suffix is appended, and nothing is wrapped until the next layout.
func (s *streamRender) ingest(text string) {
	if s.consumed > len(text) {
		// The text was replaced by something shorter rather than appended to —
		// finalizeTurn overwriting the accumulated deltas is the real case. Start
		// over; there is nothing to reuse.
		s.reset()
	}
	if len(text) > s.consumed {
		s.append(text[s.consumed:])
	}
}

// rowsFor ingests text (only the part past what it has already seen) and returns
// the block's display rows at width. It is the whole-text entry point: the TUI
// feeds through append and only this file's tests call it directly.
func (s *streamRender) rowsFor(style lipgloss.Style, text string, width int) []string {
	s.ingest(text)
	return s.sync(style, width)
}

// sync wraps whatever is pending at width and returns the rows. It is the only
// place rows change, so the TUI can call it as often as its layout wants without
// advancing the render.
//
// A width change re-wraps the whole buffer (every row's break moves) and a
// sanitized block re-wraps on every call; both are O(answer) and both are rare
// next to the per-delta case they are not on.
func (s *streamRender) sync(style lipgloss.Style, width int) []string {
	if width <= 0 {
		// Nothing is drawn at this size (the viewport has no width yet), so the
		// wrapping is skipped the way WrapToWidth skips it; the row count still has
		// to describe the text for the height arithmetic.
		return []string{s.styled(style, string(s.buf))}
	}
	switch {
	case s.sanitized:
		s.dropRows()
		s.width = width
		s.feed(sanitizeTerminal(string(s.buf)), style)
		s.pending = s.pending[:0]
	case s.width != width:
		// dropRows already reset pending to the whole buffer; fall through so the
		// default arm feeds it at the new width.
		s.dropRows()
		s.width = width
		fallthrough
	default:
		if len(s.pending) > 0 {
			// Keep an incomplete trailing rune pending; it is not a row yet.
			n := s.feed(string(s.pending), style)
			s.pending = append(s.pending[:0], s.pending[n:]...)
		}
	}
	if len(s.rows) == 0 {
		s.rows = append(s.rows, style.Render(""))
	}
	return s.rows
}

// trimmed returns the rows with the trailing empty ones dropped, which is what the
// whole-text render produces (Split(TrimRight(wrapped, "\n"), "\n")). A streamed
// partial commonly ends on the newline that opens the next row, and that row is
// not part of the block; only the thinking render trims, so this is separate from
// sync's return value. At least one row comes back, matching Split("", "\n").
func (s *streamRender) trimmed() []string {
	if len(s.rows) == 0 {
		return nil
	}
	n := s.contentRows
	if n < 1 {
		n = 1
	}
	if n > len(s.rows) {
		n = len(s.rows)
	}
	return s.rows[:n]
}

// text returns the block's text as accumulated so far. It copies the buffer, so
// it is only for the end of a turn (and for the sanitized rebuild).
func (s *streamRender) text() string { return string(s.buf) }

// len is how many bytes of text the cursor holds.
func (s *streamRender) len() int { return len(s.buf) }

// feed walks src rune by rune, closing a row when the next rune would overflow
// the width (or on a hard newline), then restyles the still-open row. It returns
// how many bytes it consumed.
//
// A streamed chunk can end in the middle of a rune — providers split on byte
// offsets, not rune boundaries — and decoding that tail would append a
// replacement character that no later delta can undo, leaving the live block
// visibly different from the same text rendered whole. So an incomplete
// sequence stays unconsumed in pending until the rest of it arrives.
func (s *streamRender) feed(src string, style lipgloss.Style) int {
	i := 0
	for i < len(src) {
		r, size := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && size == 1 && !utf8.FullRuneInString(src[i:]) {
			break // the rest of this rune is in the next delta
		}
		i += size
		if r == '\n' {
			s.closeRow(style)
			continue
		}
		rw := runeWidth(r)
		if len(s.open) > 0 && s.openW+rw > s.width {
			s.closeRow(style)
		}
		s.open = append(s.open, r)
		s.openW += rw
	}
	s.styleOpen(style)
	return i
}

// closeRow finishes the open row and starts a fresh one.
func (s *streamRender) closeRow(style lipgloss.Style) {
	s.styleOpen(style)
	s.open = s.open[:0]
	s.openW = 0
	s.rows = append(s.rows, style.Render(""))
}

// styleOpen rewrites the last row from the open row's source. Closed rows are
// never touched again, so this — one short string, one style call — is the only
// styling a delta pays for.
func (s *streamRender) styleOpen(style lipgloss.Style) {
	if len(s.rows) == 0 {
		s.rows = append(s.rows, "")
	}
	s.rows[len(s.rows)-1] = s.styled(style, string(s.open))
	if len(s.open) > 0 {
		s.contentRows = len(s.rows)
	}
}

// styled renders one row, or hands back the plain text for a raw cursor.
func (s *streamRender) styled(style lipgloss.Style, row string) string {
	if s.raw {
		return row
	}
	return style.Render(row)
}

// dropRows discards the laid-out rows but keeps the text, so the next sync
// re-wraps from the buffer. It is what a width change and a palette invalidation
// need; the text stays because the turn is still streaming.
func (s *streamRender) dropRows() {
	s.rows = s.rows[:0]
	s.open = s.open[:0]
	s.openW = 0
	s.contentRows = 0
	s.pending = append(s.pending[:0], s.buf...)
}

// rewrap forces the next sync to re-wrap everything without losing the text.
func (s *streamRender) rewrap() { s.dropRows() }

// reset drops the text and the layout. Only the end of a turn (or a shorter
// replacement) may call it: the text is authoritative while streaming. raw
// survives because it describes the block, not the text.
func (s *streamRender) reset() {
	raw := s.raw
	*s = streamRender{}
	s.raw = raw
}
