package tui

import "testing"

// This file tests the modal editing core (vim.go) as a pure state machine. The
// input layer is covered separately in input_vim_test.go; here the textarea, the
// terminal and the render loop are all out of the picture, so a failure points
// straight at a motion/operator bug.

// vimHarness drives the core the way input.go does: apply the outcome to the
// buffer and carry the caret forward.
type vimHarness struct {
	s   vimState
	b   vimBuffer
	cur vimCursor
}

func newVimHarness(text string, cur vimCursor) *vimHarness {
	h := &vimHarness{b: newVimBuffer(text)}
	h.cur = h.b.clampNormal(cur)
	return h
}

// key feeds one key. Single-rune keys carry their own text (as Bubble Tea does);
// named keys such as "esc" or "space" carry none.
func (h *vimHarness) key(k string) vimOutcome {
	text := k
	if len([]rune(k)) != 1 {
		text = ""
	}
	out := h.s.key(k, text, h.b, h.cur)
	if out.changed {
		h.b = newVimBuffer(out.value)
	}
	if out.insert {
		h.cur = h.b.clampInsert(out.insertAt)
	} else {
		h.cur = h.b.clampNormal(out.cursor)
	}
	return out
}

func (h *vimHarness) keys(ks ...string) *vimHarness {
	for _, k := range ks {
		h.key(k)
	}
	return h
}

func TestVimBufferOffsetsRoundTrip(t *testing.T) {
	for _, text := range []string{"", "abc", "a\nb", "你好\n世界", "x\n\n😀y\n"} {
		b := newVimBuffer(text)
		for row := 0; row <= b.lastRow(); row++ {
			for col := 0; col <= len([]rune(b.line(row))); col++ {
				p := vimCursor{row: row, col: col}
				if got := b.posOf(b.offsetOf(p)); got != p {
					t.Fatalf("%q: posOf(offsetOf(%v)) = %v", text, p, got)
				}
			}
		}
	}
}

func TestVimNormalCursorStaysOnCharacter(t *testing.T) {
	b := newVimBuffer("abc\n\n你好")
	cases := []struct{ in, want vimCursor }{
		{vimCursor{0, 9}, vimCursor{0, 2}},   // past end of "abc"
		{vimCursor{1, 1}, vimCursor{1, 0}},   // empty line
		{vimCursor{2, 5}, vimCursor{2, 1}},   // past end of "你好"
		{vimCursor{-1, -4}, vimCursor{0, 0}}, // out of range
	}
	for _, c := range cases {
		if got := b.clampNormal(c.in); got != c.want {
			t.Errorf("clampNormal(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestVimMotions(t *testing.T) {
	const text = "foo bar\nbaz qux"
	cases := []struct {
		name string
		cur  vimCursor
		keys []string
		want vimCursor
	}{
		{"l", vimCursor{0, 0}, []string{"l"}, vimCursor{0, 1}},
		{"h clamps at 0", vimCursor{0, 0}, []string{"h"}, vimCursor{0, 0}},
		{"l clamps at EOL", vimCursor{0, 6}, []string{"l"}, vimCursor{0, 6}},
		{"3l", vimCursor{0, 0}, []string{"3", "l"}, vimCursor{0, 3}},
		{"j keeps column", vimCursor{0, 4}, []string{"j"}, vimCursor{1, 4}},
		{"k", vimCursor{1, 2}, []string{"k"}, vimCursor{0, 2}},
		{"j clamps column", vimCursor{1, 6}, []string{"k"}, vimCursor{0, 6}},
		{"w", vimCursor{0, 0}, []string{"w"}, vimCursor{0, 4}},
		{"2w crosses line", vimCursor{0, 0}, []string{"2", "w"}, vimCursor{1, 0}},
		{"e", vimCursor{0, 0}, []string{"e"}, vimCursor{0, 2}},
		{"b", vimCursor{1, 0}, []string{"b"}, vimCursor{0, 4}},
		{"0", vimCursor{0, 5}, []string{"0"}, vimCursor{0, 0}},
		{"^", vimCursor{1, 3}, []string{"^"}, vimCursor{1, 0}},
		{"$", vimCursor{0, 0}, []string{"$"}, vimCursor{0, 6}},
		{"G", vimCursor{0, 0}, []string{"G"}, vimCursor{1, 0}},
		{"gg", vimCursor{1, 5}, []string{"g", "g"}, vimCursor{0, 0}},
		{"2G", vimCursor{0, 0}, []string{"2", "G"}, vimCursor{1, 0}},
		{"f", vimCursor{0, 0}, []string{"f", "o"}, vimCursor{0, 1}},
		{"2f", vimCursor{0, 0}, []string{"2", "f", "o"}, vimCursor{0, 2}},
		{"t", vimCursor{0, 0}, []string{"t", "b"}, vimCursor{0, 3}},
		{"F", vimCursor{0, 6}, []string{"F", "o"}, vimCursor{0, 2}},
		{"T", vimCursor{0, 6}, []string{"T", "b"}, vimCursor{0, 5}},
		{"unknown key is swallowed", vimCursor{0, 2}, []string{"Z"}, vimCursor{0, 2}},
		{"esc cancels count", vimCursor{0, 0}, []string{"3", "esc", "l"}, vimCursor{0, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newVimHarness(text, c.cur)
			h.keys(c.keys...)
			if h.cur != c.want {
				t.Fatalf("cursor = %v, want %v", h.cur, c.want)
			}
		})
	}
}

func TestVimWordMotionsIncludeCJK(t *testing.T) {
	h := newVimHarness("你好 世界abc", vimCursor{})
	h.keys("w")
	if h.cur != (vimCursor{0, 3}) {
		t.Fatalf("w over CJK: cursor = %v, want {0 3}", h.cur)
	}
	h.keys("e")
	if h.cur != (vimCursor{0, 7}) {
		t.Fatalf("e over CJK: cursor = %v, want {0 7}", h.cur)
	}
	h.keys("b")
	if h.cur != (vimCursor{0, 3}) {
		t.Fatalf("b over CJK: cursor = %v, want {0 3}", h.cur)
	}
}

func TestVimOperators(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		cur      vimCursor
		keys     []string
		want     string
		wantCur  vimCursor
		wantIns  bool
		insertAt vimCursor
	}{
		{"dw", "abc def", vimCursor{0, 0}, []string{"d", "w"}, "def", vimCursor{0, 0}, false, vimCursor{}},
		{"de", "abc def", vimCursor{0, 0}, []string{"d", "e"}, " def", vimCursor{0, 0}, false, vimCursor{}},
		{"d$", "abc", vimCursor{0, 0}, []string{"d", "$"}, "", vimCursor{0, 0}, false, vimCursor{}},
		{"dl on last char", "abc", vimCursor{0, 2}, []string{"d", "l"}, "ab", vimCursor{0, 1}, false, vimCursor{}},
		{"dh at col 0 is a no-op", "abc", vimCursor{0, 0}, []string{"d", "h"}, "abc", vimCursor{0, 0}, false, vimCursor{}},
		{"d0", "abc def", vimCursor{0, 4}, []string{"d", "0"}, "def", vimCursor{0, 0}, false, vimCursor{}},
		{"2dw", "a b c d", vimCursor{0, 0}, []string{"2", "d", "w"}, "c d", vimCursor{0, 0}, false, vimCursor{}},
		{"dd middle", "a\nb\nc", vimCursor{1, 0}, []string{"d", "d"}, "a\nc", vimCursor{1, 0}, false, vimCursor{}},
		{"dd last line", "a\nb", vimCursor{1, 0}, []string{"d", "d"}, "a", vimCursor{0, 0}, false, vimCursor{}},
		{"dd only line", "a", vimCursor{0, 0}, []string{"d", "d"}, "", vimCursor{0, 0}, false, vimCursor{}},
		{"2dd", "a\nb\nc", vimCursor{0, 0}, []string{"2", "d", "d"}, "c", vimCursor{0, 0}, false, vimCursor{}},
		{"dj", "a\nb\nc", vimCursor{0, 0}, []string{"d", "j"}, "c", vimCursor{0, 0}, false, vimCursor{}},
		{"dG", "a\nb\nc", vimCursor{1, 0}, []string{"d", "G"}, "a", vimCursor{0, 0}, false, vimCursor{}},
		{"dgg", "a\nb\nc", vimCursor{1, 0}, []string{"d", "g", "g"}, "c", vimCursor{0, 0}, false, vimCursor{}},
		{"df", "abc def", vimCursor{0, 0}, []string{"d", "f", "c"}, " def", vimCursor{0, 0}, false, vimCursor{}},
		{"dt", "abc def", vimCursor{0, 0}, []string{"d", "t", "c"}, "c def", vimCursor{0, 0}, false, vimCursor{}},
		{"df no match is a no-op", "abc", vimCursor{0, 0}, []string{"d", "f", "z"}, "abc", vimCursor{0, 0}, false, vimCursor{}},
		{"x", "abc", vimCursor{0, 1}, []string{"x"}, "ac", vimCursor{0, 1}, false, vimCursor{}},
		{"3x", "abcdef", vimCursor{0, 1}, []string{"3", "x"}, "aef", vimCursor{0, 1}, false, vimCursor{}},
		{"x at EOL", "a", vimCursor{0, 0}, []string{"x"}, "", vimCursor{0, 0}, false, vimCursor{}},
		{"X", "abc", vimCursor{0, 1}, []string{"X"}, "bc", vimCursor{0, 0}, false, vimCursor{}},
		{"X at col 0 is a no-op", "abc", vimCursor{0, 0}, []string{"X"}, "abc", vimCursor{0, 0}, false, vimCursor{}},
		{"3X", "abcdef", vimCursor{0, 3}, []string{"3", "X"}, "def", vimCursor{0, 0}, false, vimCursor{}},
		{"D", "abc\ndef", vimCursor{0, 1}, []string{"D"}, "a\ndef", vimCursor{0, 0}, false, vimCursor{}},
		{"dd then p reorders", "a\nb", vimCursor{0, 0}, []string{"d", "d", "p"}, "b\na", vimCursor{1, 0}, false, vimCursor{}},
		{"yy then p duplicates", "a\nb", vimCursor{0, 0}, []string{"y", "y", "p"}, "a\na\nb", vimCursor{1, 0}, false, vimCursor{}},
		{"yy then P", "a\nb", vimCursor{0, 0}, []string{"y", "y", "P"}, "a\na\nb", vimCursor{0, 0}, false, vimCursor{}},
		{"Y then p", "a\nb", vimCursor{1, 0}, []string{"Y", "p"}, "a\nb\nb", vimCursor{2, 0}, false, vimCursor{}},
		{"yw then p appends", "ab", vimCursor{0, 0}, []string{"y", "w", "p"}, "aabb", vimCursor{0, 1}, false, vimCursor{}},
		{"cw", "abc def", vimCursor{0, 0}, []string{"c", "w"}, " def", vimCursor{0, 0}, true, vimCursor{0, 0}},
		{"cc", "a\nb\nc", vimCursor{1, 0}, []string{"c", "c"}, "a\n\nc", vimCursor{1, 0}, true, vimCursor{1, 0}},
		{"C", "abc", vimCursor{0, 1}, []string{"C"}, "a", vimCursor{0, 1}, true, vimCursor{0, 1}},
		{"s", "abc", vimCursor{0, 1}, []string{"s"}, "ac", vimCursor{0, 1}, true, vimCursor{0, 1}},
		{"2s", "abcd", vimCursor{0, 1}, []string{"2", "s"}, "ad", vimCursor{0, 1}, true, vimCursor{0, 1}},
		{"S", "a\nb", vimCursor{0, 0}, []string{"S"}, "\nb", vimCursor{0, 0}, true, vimCursor{0, 0}},
		{"r", "abc", vimCursor{0, 1}, []string{"r", "z"}, "azc", vimCursor{0, 1}, false, vimCursor{}},
		{"3r", "abcdef", vimCursor{0, 1}, []string{"3", "r", "z"}, "azzzef", vimCursor{0, 3}, false, vimCursor{}},
		{"r past EOL is a no-op", "ab", vimCursor{0, 1}, []string{"3", "r", "z"}, "ab", vimCursor{0, 1}, false, vimCursor{}},
		{"J", "a\n   b", vimCursor{0, 0}, []string{"J"}, "a b", vimCursor{0, 1}, false, vimCursor{}},
		{"3J", "a\nb\nc\nd", vimCursor{0, 0}, []string{"3", "J"}, "a b c\nd", vimCursor{0, 1}, false, vimCursor{}},
		{"~", "aBc", vimCursor{0, 0}, []string{"~"}, "ABc", vimCursor{0, 1}, false, vimCursor{}},
		{"3~", "abcd", vimCursor{0, 0}, []string{"3", "~"}, "ABCd", vimCursor{0, 3}, false, vimCursor{}},
		{"o", "a\nb", vimCursor{0, 0}, []string{"o"}, "a\n\nb", vimCursor{1, 0}, true, vimCursor{1, 0}},
		{"O", "a\nb", vimCursor{1, 0}, []string{"O"}, "a\n\nb", vimCursor{1, 0}, true, vimCursor{1, 0}},
		{"a appends", "ab", vimCursor{0, 0}, []string{"a"}, "ab", vimCursor{0, 1}, true, vimCursor{0, 1}},
		{"A end of line", "ab\ncd", vimCursor{0, 0}, []string{"A"}, "ab\ncd", vimCursor{0, 2}, true, vimCursor{0, 2}},
		{"I first non-blank", "  ab", vimCursor{0, 3}, []string{"I"}, "  ab", vimCursor{0, 2}, true, vimCursor{0, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newVimHarness(c.text, c.cur)
			var out vimOutcome
			for _, k := range c.keys {
				out = h.key(k)
			}
			if h.b.text() != c.want {
				t.Fatalf("text = %q, want %q", h.b.text(), c.want)
			}
			if out.insert != c.wantIns {
				t.Fatalf("insert = %v, want %v", out.insert, c.wantIns)
			}
			if c.wantIns {
				if out.insertAt != c.insertAt {
					t.Fatalf("insertAt = %v, want %v", out.insertAt, c.insertAt)
				}
				return
			}
			if h.cur != c.wantCur {
				t.Fatalf("cursor = %v, want %v", h.cur, c.wantCur)
			}
		})
	}
}

func TestVimUndoRestoresTextAndCursor(t *testing.T) {
	h := newVimHarness("abc def", vimCursor{0, 4})
	h.keys("x")
	if h.b.text() != "abc ef" {
		t.Fatalf("after x: %q", h.b.text())
	}
	h.keys("u")
	if h.b.text() != "abc def" {
		t.Fatalf("after u: %q", h.b.text())
	}
	if h.cur != (vimCursor{0, 4}) {
		t.Fatalf("undo cursor = %v, want {0 4}", h.cur)
	}
	// Undo with an empty stack is a no-op.
	h.s.undo = nil
	h.keys("u")
	if h.b.text() != "abc def" {
		t.Fatalf("undo on empty stack changed text: %q", h.b.text())
	}
}

func TestVimCountsClearAfterUse(t *testing.T) {
	h := newVimHarness("abcdef", vimCursor{})
	h.keys("2", "l", "l")
	if h.cur != (vimCursor{0, 3}) {
		t.Fatalf("2l then l: cursor = %v, want {0 3}", h.cur)
	}
}

func TestVimKeepsBufferForUnmappedKeys(t *testing.T) {
	for _, text := range []string{"", "abc", "a\nb"} {
		h := newVimHarness(text, vimCursor{})
		for _, k := range []string{"z", "Q", "enter", "backspace", "tab", "left", "up", "ctrl+c"} {
			out := h.key(k)
			if !out.handled {
				t.Fatalf("%q on %q: key was not consumed", k, text)
			}
			if out.changed {
				t.Fatalf("%q on %q: buffer changed", k, text)
			}
		}
	}
}

func TestVimEmptyBufferMotionsAreSafe(t *testing.T) {
	h := newVimHarness("", vimCursor{})
	for _, k := range []string{"h", "j", "k", "l", "w", "b", "e", "0", "^", "$", "x", "X", "D", "dd", "dw", "p", "~", "J", "u", "G", "g", "g"} {
		h.key(k)
		if h.b.text() != "" {
			t.Fatalf("key %q produced text %q from an empty buffer", k, h.b.text())
		}
		if h.cur != (vimCursor{}) {
			t.Fatalf("key %q moved the caret to %v", k, h.cur)
		}
	}
}
