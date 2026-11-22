// This file tests the incremental streaming render (streamwrap.go) against the
// non-incremental path it replaced: whatever sequence of deltas a turn arrives
// in, the rows must be exactly the rows renderBlock produces for the same text
// at the same width. That equivalence is the whole safety argument for the
// optimization — if it breaks, streamed text would settle into a different layout
// than the finalized block, and rows above the cursor would have been drawn wrong
// for the rest of the turn.
package tui

import (
	"strings"
	"testing"
)

// wantStreamRows is the reference implementation: the streaming branch of
// renderBlock, split into display rows. Kept here rather than calling renderBlock
// so the test does not silently follow a change to it; if the two ever diverge,
// this test is where the divergence becomes visible.
//
// Trailing spaces are trimmed because lipgloss pads every row of a multi-line
// render to the widest row, while the cursor styles one row at a time and does
// not know the final width. That padding never reaches the screen — the line
// window pads every row to the content width as it paints (fitLine), and the
// painted-frame test below pins that. What matters here is the split: the same
// runes on the same rows, with the same styling.
func wantStreamRows(theme Theme, text string, width int) []string {
	rows := strings.Split(theme.Assistant.Render(WrapToWidth(text, width)), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return rows
}

// checkStreamMatches feeds deltas through the cursor and compares against the
// reference render after every single one.
func checkStreamMatches(t *testing.T, theme Theme, width int, deltas []string) {
	t.Helper()
	var cur streamRender
	var text strings.Builder
	for i, delta := range deltas {
		text.WriteString(delta)
		got := cur.rowsFor(theme.Assistant, text.String(), width)
		want := wantStreamRows(theme, text.String(), width)
		if len(got) != len(want) {
			t.Fatalf("delta %d: %d rows, want %d\ngot:  %q\nwant: %q", i, len(got), len(want), got, want)
		}
		for j := range want {
			if trimRow(got[j]) != want[j] {
				t.Fatalf("delta %d row %d = %q, want %q\ntext: %q", i, j, trimRow(got[j]), want[j], text.String())
			}
		}
	}
}

// TestStreamRenderHoldsBackSplitRunes verifies a delta that ends in the middle of a
// rune does not bake a replacement character into the block. Providers split their
// stream on byte offsets, so this happens with any CJK or emoji text, and the
// replacement character used to stay on screen for the rest of the turn because
// nothing re-decodes a row that is already closed.
func TestStreamRenderHoldsBackSplitRunes(t *testing.T) {
	const text = "see 先 and 👀 done"
	theme := DefaultTheme()
	var cur streamRender
	for i := 1; i <= len(text); i++ {
		prefix := text[:i]
		got := cur.rowsFor(theme.Assistant, prefix, 40)
		want := wantStreamRows(theme, validPrefix(prefix), 40)
		if len(got) != len(want) {
			t.Fatalf("prefix %d (%q): %d rows, want %d\ngot: %q\nwant: %q", i, prefix, len(got), len(want), got, want)
		}
		for j := range want {
			if trimRow(got[j]) != want[j] {
				t.Fatalf("prefix %d row %d = %q, want %q", i, j, trimRow(got[j]), want[j])
			}
		}
		if joined := strings.Join(got, ""); strings.Contains(joined, "\uFFFD") {
			t.Fatalf("prefix %d (%q): a half-arrived rune was drawn as a replacement char: %q", i, prefix, joined)
		}
	}
}

// TestStreamRenderMatchesWholeBlockRender is the core equivalence check, over
// the cases a streamed answer actually hits: word-wrapped prose split at
// arbitrary byte offsets, hard newlines, CJK (double-width) runes, combining
// marks and emoji, and a delta that lands exactly on the wrap boundary.
func TestStreamRenderMatchesWholeBlockRender(t *testing.T) {
	theme := DefaultTheme()

	t.Run("prose split every few bytes", func(t *testing.T) {
		answer := strings.Repeat("the quick brown fox jumps over the lazy dog ", 12)
		var deltas []string
		for i := 0; i < len(answer); i += 3 {
			deltas = append(deltas, answer[i:min(i+3, len(answer))])
		}
		checkStreamMatches(t, theme, 20, deltas)
	})

	t.Run("one rune at a time", func(t *testing.T) {
		answer := "hello world\nsecond line\nthird"
		var deltas []string
		for _, r := range answer {
			deltas = append(deltas, string(r))
		}
		checkStreamMatches(t, theme, 7, deltas)
	})

	t.Run("hard newlines only", func(t *testing.T) {
		checkStreamMatches(t, theme, 30, []string{"a\n", "b\n", "c\n", "\n", "d"})
	})

	t.Run("cjk and emoji are measured in cells", func(t *testing.T) {
		answer := "会议纪要：讨论了性能优化方案 🚀 and then some latin text to wrap"
		var deltas []string
		for _, r := range answer {
			deltas = append(deltas, string(r))
		}
		checkStreamMatches(t, theme, 12, deltas)
	})

	t.Run("delta lands on the wrap boundary", func(t *testing.T) {
		// Width 10, one rune per delta: every boundary is exercised.
		answer := strings.Repeat("abcdefghij", 5)
		var deltas []string
		for _, r := range answer {
			deltas = append(deltas, string(r))
		}
		checkStreamMatches(t, theme, 10, deltas)
	})

	t.Run("width narrower than a rune", func(t *testing.T) {
		checkStreamMatches(t, theme, 1, []string{"会", "议", "\n", "ab", "c"})
	})

	t.Run("large delta", func(t *testing.T) {
		checkStreamMatches(t, theme, 40, []string{strings.Repeat("x", 500), "y", strings.Repeat("z\n", 10)})
	})

	t.Run("empty and whitespace", func(t *testing.T) {
		checkStreamMatches(t, theme, 20, []string{"", " ", "  ", "\t", "x"})
	})
}

// TestStreamRenderSanitizesControlBytes verifies a delta carrying control bytes
// re-wraps from the whole sanitized text instead of being fed through: CR
// overwrite and TAB expansion are whole-string operations, so a chunked feed
// would disagree with the finalized render.
func TestStreamRenderSanitizesControlBytes(t *testing.T) {
	theme := DefaultTheme()
	// A progress-bar style payload: three CR-separated states, then tabs, then a
	// cursor-up sequence that must be dropped entirely.
	deltas := []string{
		"progress 10%\r",
		"progress 60%\r",
		"progress 100%\n",
		"name\tvalue\n",
		"\x1b[2Aoverwritten\n",
	}
	checkStreamMatches(t, theme, 24, deltas)
}

// TestStreamRenderRecoversFromReset verifies the two invalidation cases a live
// turn really hits: the provider sending a shorter text than before (a
// replacement, not an append) and a width change mid-stream. Both must re-wrap
// from scratch rather than keep rows from the old text or width.
func TestStreamRenderRecoversFromReset(t *testing.T) {
	theme := DefaultTheme()
	var cur streamRender

	grown := strings.Repeat("abcdefghij", 6)
	cur.rowsFor(theme.Assistant, grown, 10)

	// Shorter text: the cursor must discard the rows it built for the long one.
	shorter := "short"
	got := cur.rowsFor(theme.Assistant, shorter, 10)
	want := wantStreamRows(theme, shorter, 10)
	if trimRows(got) != trimRows(want) {
		t.Fatalf("after a shorter text: got %q, want %q", got, want)
	}

	// Width change: same text, new wrapping.
	got = cur.rowsFor(theme.Assistant, shorter, 3)
	want = wantStreamRows(theme, shorter, 3)
	if trimRows(got) != trimRows(want) {
		t.Fatalf("after a width change: got %q, want %q", got, want)
	}

	// reset drops everything so the next call starts clean.
	cur.reset()
	got = cur.rowsFor(theme.Assistant, "abc", 10)
	if len(got) != 1 {
		t.Fatalf("after reset: %d rows, want 1 (%q)", len(got), got)
	}
}

// TestStreamRenderNonPositiveWidth pins the degenerate case the viewport draws
// nothing for: no wrapping, one row per hard newline, no panic on a zero width.
func TestStreamRenderNonPositiveWidth(t *testing.T) {
	var cur streamRender
	for _, width := range []int{0, -5} {
		got := cur.rowsFor(DefaultTheme().Assistant, "a\nb", width)
		if len(got) != 1 {
			t.Fatalf("width %d: %d rows, want 1 (unwrapped)", width, len(got))
		}
	}
}

// trimRow drops the padding lipgloss adds to a multi-line render (see
// wantStreamRows).
func trimRow(row string) string { return strings.TrimRight(row, " ") }

// trimRows is trimRow over a slice, joined so the comparison reads in one line.
func trimRows(rows []string) string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = trimRow(row)
	}
	return strings.Join(out, "\n")
}

// TestStreamRenderPaintsIdenticallyToWholeBlockRender closes the gap the trimmed
// comparison leaves open: it drives a real transcript, streams an answer in
// chunks, and compares the *painted* frame — padding included — against a frame
// whose rows came from the reference whole-block render. Both frames go through
// the line window, so this is what the user actually sees.
func TestStreamRenderPaintsIdenticallyToWholeBlockRender(t *testing.T) {
	theme := DefaultTheme()
	answer := strings.Repeat("streamed prose that wraps at the content width ", 8)

	// The frame a chunked stream produces.
	streamed := newTranscript(theme)
	streamed.setSize(40, 50)
	for i := 0; i < len(answer); i += 7 {
		streamed.appendDelta(answer[i:min(i+7, len(answer))])
	}

	// The reference: the same text rendered whole, then placed in a window of the
	// same size and width.
	ref := lineWindow{width: 40, height: 50}
	ref.SetLines(wantStreamRows(theme, answer, 40))
	ref.GotoBottom()

	if got, want := streamed.vp.View(), ref.View(); got != want {
		t.Fatalf("painted frames differ\ngot:  %q\nwant: %q", got, want)
	}
}
