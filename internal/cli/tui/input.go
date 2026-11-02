package tui

import (
	"math"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// This file implements the prompt input field of the full-screen TUI (US-007,
// FR-11/13/14). It wraps charm.land/bubbles/v2/textarea into a small `input`
// component so the model can embed a real multi-line editor instead of the
// throwaway string buffer the skeleton shipped with.
//
// Why textarea rather than a hand-rolled buffer: textarea edits by grapheme /
// rune, so CJK and emoji are inserted and deleted whole. This is exactly the
// class of bug the old REPL input had — it keyed on byte length (len==1) and
// silently dropped the trailing bytes of every multi-byte rune. We deliberately
// delegate all character handling to textarea and never touch bytes ourselves.
//
// Shift+Enter inserts a newline so the editor is a true multi-line composer;
// plain Enter submits (intercepted by the model, never reaching textarea). The
// default InsertNewline binding (Enter) is therefore rebound to Shift+Enter. See
// model.handleKey.
//
// The editor has two layouts: inline (the normal composer, sized to its content)
// and full screen (Ctrl+E), where it takes over the terminal for long-form
// writing. Full screen is always a modal, vim-style editor — it is not a toggle
// or an option — and it paints a key-hint bar so the commands are discoverable
// without leaving the screen. The inline composer keeps the plain emacs-style
// editing it always had, so typing a prompt never puts anyone in normal mode by
// accident.

// maxInputRows caps how tall the editor grows as the user adds lines. Past this
// the buffer keeps growing but textarea scrolls its own viewport, so the shell's
// row accounting stays bounded and the transcript never collapses to nothing.
const maxInputRows = 6

// unboundedContentRows disables textarea's content cap while keeping its
// viewport cap. The two are not the same thing: MaxHeight clamps the visible
// height, but textarea also treats it as a buffer limit — atContentLimit()
// refuses every InsertNewline once the value holds MaxHeight logical lines
// (bubbles/v2/textarea, "legacy MaxHeight behavior"). Left alone that silently
// turns the composer read-only past 6 lines: the newline is dropped and the next
// words are appended to the last row, so several lines collapse onto one.
// Setting MaxContentHeight to any positive value hands the content guard to the
// visual-row path instead, where this sentinel is far beyond what the buffer can
// hold — so MaxHeight caps only the viewport, as intended.
const unboundedContentRows = math.MaxInt

// setEditorRows caps how many rows the editor shows without capping what it
// holds. Every place that sizes the textarea must go through it: setting
// MaxHeight on its own reintroduces the content cap described above.
func setEditorRows(ta *textarea.Model, rows int) {
	ta.MaxHeight = rows
	ta.MaxContentHeight = unboundedContentRows
}

// input is the prompt editor. It embeds a textarea.Model and exposes just the
// surface the root model needs: value/clear, focus/blur (input is blurred while
// a run is in flight so keystrokes never corrupt an in-flight prompt), a width
// setter driven by tea.WindowSizeMsg, and a render string for View.
type input struct {
	ta textarea.Model
	// width is the full editor width (terminal columns) last set via SetWidth. It
	// is the span of the top/bottom rules drawn around the editor (Claude-Code
	// style), kept separately because textarea's own Width() reports only the
	// inner text area (prompt column excluded).
	width int

	// fullscreen reports that the composer is in its full-screen editing layout.
	// Modal editing belongs to that layout: it is always on there and always off
	// inline, which is why there is no separate toggle.
	fullscreen bool
	// fullRows is the number of terminal rows reserved for the whole full-screen
	// composer (rules, text area and hint bar). It is set by the model from the
	// terminal height and refreshed on resize.
	fullRows int
	// mode is the current editing mode; it only means anything while fullscreen.
	mode vimMode
	// vim is the normal-mode command state (counts, operator, register, undo).
	vim vimState
	// insertStart is the caret where the current insert session began, used as
	// the undo restore point. insertUndoPushed records that the session's
	// pre-edit snapshot is already on the undo stack, so a burst of typing is a
	// single `u`, like vim.
	insertStart      vimCursor
	insertUndoPushed bool
}

// newInput builds a focused editor. It starts one row tall and grows with the
// buffer (up to maxInputRows) as the user inserts newlines with Enter. The
// buffer itself is unbounded; beyond maxInputRows textarea scrolls internally.
func newInput() input {
	ta := textarea.New()
	ta.Prompt = "> "
	ta.Placeholder = "Type a message… (Enter to send, Shift+Enter for newline, Ctrl+E full screen)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// Let the textarea own its own height: DynamicHeight grows/shrinks it to the
	// content between MinHeight (1) and MaxHeight (maxInputRows), and — critically
	// — fixes the viewport scroll offset in the same pass. Doing it manually (an
	// after-the-fact SetHeight in syncHeight) left a stale scroll offset: inserting
	// a newline scrolled the cursor into view while the editor was still 1 row
	// tall, pushing the first line off the top, and the later SetHeight never
	// scrolled it back — so a two-line buffer rendered as two blank lines.
	ta.MinHeight = 1
	setEditorRows(&ta, maxInputRows)
	ta.DynamicHeight = true
	// textarea.New starts at defaultHeight (6). DynamicHeight only recomputes on
	// edits, so pin the empty editor to one row up front — otherwise the shell
	// would reserve six rows before the user has typed anything.
	ta.SetHeight(1)
	// Rebind InsertNewline from its default (Enter) to the newline keys, since
	// plain Enter is the model's submit key (handleKey intercepts it before
	// textarea sees it). Shift+Enter is the primary, advertised binding: Bubble
	// Tea v2 already enables the Kitty keyboard protocol's disambiguate flag
	// (flag 1) on every View, so capable terminals (kitty, ghostty, wezterm,
	// recent iTerm2) report Shift+Enter as a distinct CSI-u sequence rather than
	// a bare CR. Crucially this is flag 1, NOT flag 8 (ReportAllKeysAsEscapeCodes)
	// — flag 8 broke IME / CJK input because it strips associated text, whereas
	// flag 1 only disambiguates special keys and leaves text entry untouched.
	// On terminals without the protocol (macOS Terminal.app, tmux by default)
	// Shift+Enter arrives byte-identical to Enter and would submit, so Ctrl+J (a
	// literal LF, always distinct from Enter's CR) and Alt+Enter (ESC-prefixed,
	// always distinct) are kept as silent fallbacks — a newline is guaranteed to
	// work everywhere. All three split the line at the cursor and keep typed text.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j", "alt+enter"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// Draw the cursor into the rendered string: the model composes View as a
	// plain string rather than driving textarea's real cursor reporting.
	ta.SetVirtualCursor(true)
	// Drop the default cursor-line background highlight so the composer is framed
	// only by the top/bottom rules (see View), matching Claude Code — no fill.
	styles := ta.Styles()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Blurred.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(styles)
	ta.Focus()
	return input{ta: ta}
}

// Update forwards a message (typically a key press) to the underlying textarea
// and returns the updated component. The model calls this only for keys it does
// not intercept itself (submit / interrupt / quit); subclasses of key handling
// are:
//
//   - inline layout: everything goes to textarea, exactly as before.
//   - full screen, insert mode: textarea handles the key, except Esc which
//     switches to normal mode without touching the buffer.
//   - full screen, normal mode: the key is a command for the vim core and never
//     reaches textarea, so plain letters cannot leak into the buffer.
//
// Height is owned by textarea's DynamicHeight (see newInput); buffer
// replacements through SetValue recompute it, so there is nothing to re-sync
// here.
func (in input) Update(msg tea.Msg) (input, tea.Cmd) {
	if in.fullscreen {
		if kp, ok := msg.(tea.KeyPressMsg); ok {
			if in.mode == vimNormal {
				return in.updateNormal(kp)
			}
			if k := kp.String(); k == "esc" || k == "ctrl+[" {
				// Esc from insert mode steps the caret back one column like vim,
				// and must never submit or quit.
				in.enterNormal()
				return in, nil
			}
		}
	}

	before := in.ta.Value()
	var cmd tea.Cmd
	in.ta, cmd = in.ta.Update(msg)
	// Record the insert session's starting point once, at the first change, so a
	// burst of typing is undone in one step.
	if in.fullscreen && in.mode == vimInsert && !in.insertUndoPushed && in.ta.Value() != before {
		in.vim.pushUndoText(before, in.insertStart)
		in.insertUndoPushed = true
	}
	return in, cmd
}

// updateNormal runs one key through the vim core and applies the result to the
// textarea.
func (in input) updateNormal(msg tea.KeyPressMsg) (input, tea.Cmd) {
	// Modified keys (Alt/Ctrl/Super) are not normal-mode commands; swallow them
	// so Alt+d cannot be mistaken for `d`. Ctrl+[ is handled as Esc by the core.
	if msg.Mod.Contains(tea.ModAlt) || msg.Mod.Contains(tea.ModCtrl) || msg.Mod.Contains(tea.ModSuper) {
		return in, nil
	}

	b := newVimBuffer(in.ta.Value())
	out := in.vim.key(msg.String(), msg.Text, b, in.normalCursor())
	if out.changed {
		in.ta.SetValue(out.value)
	}
	if out.insert {
		in.mode = vimInsert
		in.insertUndoPushed = false
		in.insertStart = out.insertAt
		in.setCursor(out.insertAt)
		return in, nil
	}
	in.setCursor(out.cursor)
	return in, nil
}

// normalCursor reports the caret as a normal-mode position (on a character).
func (in input) normalCursor() vimCursor {
	row, col := in.ta.Line(), in.ta.Column()
	return newVimBuffer(in.ta.Value()).clampNormal(vimCursor{row: row, col: col})
}

// enterNormal switches from insert to normal mode, stepping the caret back one
// column when it sits past the last character (vim's Esc behavior).
func (in *input) enterNormal() {
	b := newVimBuffer(in.ta.Value())
	row, col := in.ta.Line(), in.ta.Column()
	if col > 0 {
		col--
	}
	in.mode = vimNormal
	in.vim.reset()
	in.setCursor(b.clampNormal(vimCursor{row: row, col: col}))
}

// setCursor places the textarea caret at a logical position. textarea exposes
// only relative movement (CursorUp/Down) plus column setting, so vertical
// movement walks the rows; that is fine for interactive prompts and keeps us off
// the unexported row field.
func (in *input) setCursor(p vimCursor) {
	p = newVimBuffer(in.ta.Value()).clampInsert(p)
	in.moveCursor(p.row, p.col)
}

// cursorMoveLimit bounds the row walk in moveCursor so a pathological caret
// request can never spin.
const cursorMoveLimit = 100000

// moveCursor moves the caret to (row, col), using one CursorUp per logical line
// from the top. textarea's CursorDown may stay on the same logical line when a
// wrapped line spans several visual rows, so the loop is bounded and keyed on
// Line() rather than assuming one call equals one row.
func (in *input) moveCursor(row, col int) {
	if row < 0 {
		row = 0
	}
	if col < 0 {
		col = 0
	}
	in.ta.MoveToBegin()
	for i := 0; in.ta.Line() < row && i < cursorMoveLimit; i++ {
		in.ta.CursorDown()
	}
	in.ta.SetCursorColumn(col)
	// SetCursorColumn does not scroll; re-applying the height repositions the
	// viewport around the (now moved) caret.
	in.ta.SetHeight(in.ta.Height())
}

// SetFullscreen switches the composer between its inline layout and the
// full-screen editing layout, where modal editing is always active. rows is the
// number of terminal rows the full-screen composer may occupy (rules, text area
// and hint bar); it is ignored when leaving. The draft, caret and undo history
// are preserved, so Ctrl+E twice loses nothing.
func (in *input) SetFullscreen(on bool, rows int) {
	if in.fullscreen == on {
		if on {
			in.setFullRows(rows)
		}
		return
	}
	in.fullscreen = on
	if on {
		// Entering is always an insert session: the user asked for room to write,
		// not for a mode switch they did not request. Esc (or an empty hint) then
		// takes them to normal mode.
		in.mode = vimInsert
		in.vim.reset()
		in.insertUndoPushed = false
		in.ta.DynamicHeight = false
		in.setFullRows(rows)
		return
	}
	// Leaving always returns to plain, inline editing: modal keys must never
	// outlive the full-screen layout.
	in.mode = vimInsert
	in.vim = vimState{}
	in.insertUndoPushed = false
	in.ta.DynamicHeight = true
	in.ta.MinHeight = 1
	setEditorRows(&in.ta, maxInputRows)
	in.resyncInlineHeight()
}

// setFullRows sizes the full-screen text area to the rows left over after the
// two rules and the hint bar.
func (in *input) setFullRows(rows int) {
	in.fullRows = max(rows, fullscreenMinRows)
	// Two rows go to the rules around the text area, one or two to the hint bar.
	visible := max(in.fullRows-2-hintHeight(in.width), 1)
	setEditorRows(&in.ta, visible)
	in.ta.SetHeight(visible)
}

// resyncInlineHeight recomputes the inline editor height from the buffer it now
// holds. SetWidth is the documented way to make textarea redo its DynamicHeight
// pass: unlike SetHeight it counts *visual* rows, so soft-wrapped lines and the
// viewport's scroll offset both come out right. Returning from full screen used
// to call SetHeight(LineCount) instead, which undercounted wrapped lines and
// left a stale scroll offset behind.
func (in *input) resyncInlineHeight() {
	in.ta.SetWidth(in.width)
}

// SetFullHeight re-sizes an already full-screen composer after a terminal
// resize. It is a no-op while inline.
func (in *input) SetFullHeight(rows int) {
	if in.fullscreen {
		in.setFullRows(rows)
	}
}

// Fullscreen reports whether the composer is in its full-screen editing layout
// (and therefore modal).
func (in input) Fullscreen() bool { return in.fullscreen }

// Mode reports the current editing mode.
func (in input) Mode() vimMode { return in.mode }

// Height reports the current visible row count of the editor so the model can
// reserve that many rows in its View layout. It includes the two rule rows (top
// and bottom) drawn around the textarea. In the full-screen layout it is the
// height the model handed over: rules, text area and the hint bar.
func (in input) Height() int {
	if in.fullscreen {
		return in.fullRows
	}
	return in.ta.Height() + 2
}

// Value returns the current buffer contents, including any embedded newlines.
func (in input) Value() string { return in.ta.Value() }

// SetValue replaces the buffer contents and moves the cursor to the end. It is
// used by slash autocomplete (Tab) to complete the buffer to the chosen command.
func (in *input) SetValue(s string) {
	in.ta.SetValue(s)
}

// Clear empties the buffer, resets the cursor to the start and returns the
// composer to its inline layout. A cleared composer is always a fresh insert
// session: prompting again should start in insert mode even if the previous
// draft was left in normal mode, and a submitted full-screen draft must hand the
// screen back to the transcript.
func (in *input) Clear() {
	in.ta.Reset()
	in.mode = vimInsert
	in.vim = vimState{}
	in.insertStart = vimCursor{}
	in.insertUndoPushed = false
	in.leaveFullscreen()
}

// leaveFullscreen drops the full-screen layout without touching the draft (Clear
// removes the draft itself). It is the unguarded half of SetFullscreen(false).
func (in *input) leaveFullscreen() {
	if !in.fullscreen {
		return
	}
	in.fullscreen = false
	in.ta.DynamicHeight = true
	in.ta.MinHeight = 1
	setEditorRows(&in.ta, maxInputRows)
	in.resyncInlineHeight()
}

// Focus enables editing and returns the cursor-blink Cmd.
func (in *input) Focus() tea.Cmd { return in.ta.Focus() }

// Blur disables editing (used while a run is in flight).
func (in *input) Blur() { in.ta.Blur() }

// Focused reports whether the editor currently accepts input.
func (in input) Focused() bool { return in.ta.Focused() }

// Line reports the zero-based index of the line the cursor is on, and LineCount
// the total number of lines in the buffer. The model uses them to decide whether
// ↑/↓ should walk the prompt history (caret on the first / last line) or move the
// caret within a multi-line draft.
func (in input) Line() int      { return in.ta.Line() }
func (in input) LineCount() int { return in.ta.LineCount() }

// SetWidth resizes the editor to the terminal width so wrapping and the prompt
// column line up with the rest of the shell.
func (in *input) SetWidth(w int) {
	if w < 0 {
		w = 0
	}
	in.width = w
	in.ta.SetWidth(w)
}

// View renders the editor to a string for embedding in the model's View. The
// textarea is framed with a top and bottom rule (no side borders) in the muted
// gray, mirroring Claude Code's composer — a pair of horizontal lines rather
// than a background fill. The rules span the full editor width.
//
// In the full-screen layout a hint bar sits below the rules: the mode name plus
// the keys that move, edit and leave the editor. It is the only affordance
// telling a user why typing has stopped inserting text (normal mode) and how to
// get out, so it must never be hidden and must fit narrow terminals.
func (in input) View() string {
	style := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, true, false).
		BorderForeground(lipgloss.Color(colorGray))
	if in.width > 0 {
		style = style.Width(in.width)
	}
	view := style.Render(in.ta.View())
	if !in.fullscreen {
		return view
	}
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color(colorGray))
	var b strings.Builder
	for _, line := range in.hintLines() {
		b.WriteByte('\n')
		b.WriteString(hint.Render(truncateWidth(line, in.width)))
	}
	return view + b.String()
}

// hintHeight is the number of rows the hint bar occupies for a given terminal
// width: the full two-line cheatsheet on a normal terminal, and only its first
// line when the terminal is too narrow to say anything useful twice.
func hintHeight(width int) int {
	if width > 0 && width < hintTwoLineWidth {
		return 1
	}
	return hintRows
}

// hintRows is the maximum number of hint lines under the full-screen editor.
const hintRows = 2

// hintTwoLineWidth is the narrowest terminal that still gets the second hint
// line; below it the extra line would be truncated to noise.
const hintTwoLineWidth = 60

// fullscreenMinRows keeps a full-screen composer usable even on a tiny terminal
// (rules + at least one editing row + hint bar).
const fullscreenMinRows = 4

// hintLines returns the cheat sheet shown under the full-screen editor. It is
// deliberately ASCII and mode-aware: insert mode advertises how to reach normal
// mode, normal mode advertises the commands and how to get back.
func (in input) hintLines() []string {
	leave := "ctrl+e leave full screen"
	if in.mode == vimNormal {
		// The cheat sheet stays on one row: what does not fit an 80-column
		// terminal is dropped rather than wrapped, and the optional tail only
		// appears when there is room for it.
		sheet := "h j k l w b e move · 0 ^ $ gg G jump · dd cc yy lines · x X del · p P paste"
		if in.width >= len(sheet)+len(" · f t find") {
			sheet += " · f t find"
		}
		lines := []string{
			"-- " + in.mode.label() + " --  i a o insert · u undo · " + leave,
			sheet,
		}
		if width := in.width; width > 0 && width < hintTwoLineWidth {
			return lines[:1]
		}
		return lines
	}
	lines := []string{
		"-- " + in.mode.label() + " --  esc normal mode · " + leave,
		"enter send · shift+enter newline · ctrl+v paste image",
	}
	if width := in.width; width > 0 && width < hintTwoLineWidth {
		return lines[:1]
	}
	return lines
}

// truncateWidth cuts a plain string to at most width terminal cells, marking the
// cut with an ellipsis so a clipped hint reads as "there is more" instead of as a
// broken word. Hints are single-width ASCII, so rune count is cell count; the
// guard keeps a zero or unknown width from truncating everything away.
func truncateWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(rs[:width-1]) + "…"
}

// clampInt bounds v to [lo, hi].
func clampInt(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
