package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// This file covers the seam between the modal core (vim.go) and the composer
// (input.go): mode transitions, the key routing split, caret placement in the
// real textarea, and the insert-session undo snapshot. Commands (motions,
// operators) are tested directly against the core in vim_test.go.

// newVimInput builds a composer in its full-screen layout — the only place
// modal editing exists — with room for the rules and the hint bar.
func newVimInput(t *testing.T) input {
	t.Helper()
	in := newInput()
	in.SetWidth(80)
	in.SetFullscreen(true, 24)
	return in
}

func keyMsg(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func escMsg() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyEscape}
}

// send feeds one key and returns the updated editor.
func send(in input, msg tea.KeyPressMsg) input {
	next, _ := in.Update(msg)
	return next
}

func typeIntoInput(in input, s string) input {
	for _, r := range s {
		in = send(in, keyMsg(r))
	}
	return in
}

func (in input) caret() (int, int) { return in.ta.Line(), in.ta.Column() }

func TestInputVimEscEntersNormalAndKeepsBuffer(t *testing.T) {
	in := newVimInput(t)
	in = typeIntoInput(in, "hello world")
	if in.Mode() != vimInsert {
		t.Fatalf("mode = %v, want insert", in.Mode())
	}

	in = send(in, escMsg())
	if in.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal", in.Mode())
	}
	if in.Value() != "hello world" {
		t.Fatalf("Esc changed the buffer: %q", in.Value())
	}
	if row, col := in.caret(); row != 0 || col != 10 {
		t.Fatalf("caret = (%d,%d), want (0,10): Esc steps back one column", row, col)
	}
	if v := in.View(); !strings.Contains(v, "NORMAL") {
		t.Fatalf("normal-mode view does not show the mode label:\n%s", v)
	}
}

func TestInputVimNormalKeysDoNotInsertText(t *testing.T) {
	in := newVimInput(t)
	in = typeIntoInput(in, "abc")
	in = send(in, escMsg())

	// d is an operator, not text: it waits for a motion, so nothing changes yet.
	in = send(in, keyMsg('d'))
	if in.Value() != "abc" {
		t.Fatalf("operator key inserted text: %q", in.Value())
	}
	// An unknown command is swallowed entirely.
	in = send(in, keyMsg('Z'))
	if in.Value() != "abc" {
		t.Fatalf("unmapped normal key inserted text: %q", in.Value())
	}
	// Enter in normal mode must never submit or insert a newline.
	in = send(in, tea.KeyPressMsg{Code: tea.KeyEnter})
	if in.Value() != "abc" {
		t.Fatalf("enter in normal mode changed the buffer: %q", in.Value())
	}
	if in.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal", in.Mode())
	}
}

func TestInputVimMotionAndOperator(t *testing.T) {
	in := newVimInput(t)
	in.SetValue("hello world")
	in = send(in, escMsg())
	if row, col := in.caret(); row != 0 || col != 10 {
		t.Fatalf("caret after SetValue+Esc = (%d,%d), want (0,10)", row, col)
	}

	in = send(in, keyMsg('0'))
	if row, col := in.caret(); row != 0 || col != 0 {
		t.Fatalf("0: caret = (%d,%d), want (0,0)", row, col)
	}
	in = send(in, keyMsg('d'))
	in = send(in, keyMsg('w'))
	if in.Value() != "world" {
		t.Fatalf("dw: value = %q, want %q", in.Value(), "world")
	}
	if row, col := in.caret(); row != 0 || col != 0 {
		t.Fatalf("dw: caret = (%d,%d), want (0,0)", row, col)
	}

	// p pastes the deleted word back after the current character.
	in = send(in, keyMsg('p'))
	if in.Value() != "whello orld" {
		t.Fatalf("p: value = %q, want %q", in.Value(), "whello orld")
	}
}

func TestInputVimInsertSessionUndo(t *testing.T) {
	in := newVimInput(t)
	in = typeIntoInput(in, "abc")
	in = send(in, escMsg())
	in = send(in, keyMsg('u'))
	if in.Value() != "" {
		t.Fatalf("u after an insert session: value = %q, want empty", in.Value())
	}
	if row, col := in.caret(); row != 0 || col != 0 {
		t.Fatalf("undo caret = (%d,%d), want (0,0)", row, col)
	}
	if in.Mode() != vimNormal {
		t.Fatalf("undo left normal mode: %v", in.Mode())
	}
}

func TestInputVimInsertAppendsAndUndoRestoresPrefix(t *testing.T) {
	in := newVimInput(t)
	in.SetValue("ab")
	in = send(in, escMsg()) // caret on 'b' (col 1)
	in = send(in, keyMsg('A'))
	if in.Mode() != vimInsert {
		t.Fatalf("A did not enter insert mode: %v", in.Mode())
	}
	in = typeIntoInput(in, "cd")
	if in.Value() != "abcd" {
		t.Fatalf("A + typing: %q", in.Value())
	}
	in = send(in, escMsg())
	in = send(in, keyMsg('u'))
	if in.Value() != "ab" {
		t.Fatalf("undo of the second insert session: %q, want %q", in.Value(), "ab")
	}
}

func TestInputVimLinewiseOnRealTextarea(t *testing.T) {
	in := newVimInput(t)
	in.SetValue("a\nb\nc")
	in = send(in, escMsg()) // caret {2,0} on the last line
	in = send(in, keyMsg('g'))
	in = send(in, keyMsg('g'))
	if row, col := in.caret(); row != 0 || col != 0 {
		t.Fatalf("gg: caret = (%d,%d), want (0,0)", row, col)
	}
	in = send(in, keyMsg('d'))
	in = send(in, keyMsg('j'))
	if in.Value() != "c" {
		t.Fatalf("dj: value = %q, want %q", in.Value(), "c")
	}
}

func TestInputVimDisabledKeepsPlainBehavior(t *testing.T) {
	in := newInput()
	in.SetWidth(80)
	in = typeIntoInput(in, "abc")
	in = send(in, escMsg())
	if in.Mode() != vimInsert {
		t.Fatalf("mode = %v, want insert when vim is off", in.Mode())
	}
	if in.Value() != "abc" {
		t.Fatalf("value = %q, want %q", in.Value(), "abc")
	}
	// 'x' is a plain character without modal editing.
	in = send(in, keyMsg('x'))
	if in.Value() != "abcx" {
		t.Fatalf("value = %q, want %q", in.Value(), "abcx")
	}
}

func TestInputVimClearResetsMode(t *testing.T) {
	in := newVimInput(t)
	in = typeIntoInput(in, "abc")
	in = send(in, escMsg())
	in.Clear()
	if in.Mode() != vimInsert {
		t.Fatalf("mode after Clear = %v, want insert", in.Mode())
	}
	if in.Value() != "" {
		t.Fatalf("value after Clear = %q", in.Value())
	}
}

func TestInputVimCJK(t *testing.T) {
	in := newVimInput(t)
	in = typeIntoInput(in, "你好 世界")
	if in.Value() != "你好 世界" {
		t.Fatalf("insert CJK: %q", in.Value())
	}
	in = send(in, escMsg()) // caret on 界
	in = send(in, keyMsg('0'))
	in = send(in, keyMsg('x'))
	if in.Value() != "好 世界" {
		t.Fatalf("x over CJK: %q, want %q", in.Value(), "好 世界")
	}
}
