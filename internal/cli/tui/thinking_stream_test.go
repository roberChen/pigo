// This file tests the live thinking block against the whole-text render it
// replaces. A reasoning update resends the whole partial, so the block's cursor
// ingests only the new suffix and lays out only that suffix's rows; the rows it
// produces must be exactly renderThinking's for the same text at the same width,
// otherwise the reasoning a user watches settle into place would be laid out
// differently from the same reasoning replayed from a session file.
//
// The cases are the ones reasoning actually hits plus the ones that broke the
// answer's equivalent test: prose wrapped at arbitrary byte offsets, hard
// newlines (a partial commonly ends on one), CJK and emoji, control bytes that
// force the sanitized fallback, and a width change in the middle of the stream.
package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
)

// wantThinkingRows is the reference: renderThinking over the full text, split into
// display rows.
func wantThinkingRows(theme Theme, blk transcriptBlock, width int) []string {
	return strings.Split(renderThinking(theme, blk, width), "\n")
}

// validPrefix returns s without a trailing incomplete UTF-8 sequence.
//
// This is the one place the two paths intentionally differ, and only for a
// partial that arrived mid-rune: the live cursor withholds the half-arrived rune
// until the rest of it shows up (see streamRender.feed), while the whole-text
// render has no notion of "more coming" and draws each stray byte as a
// replacement character. Comparing against the longest complete prefix is the
// correct reference — it is what the block shows the moment the rune arrives, and
// those replacement characters were never part of the text.
func validPrefix(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	start := len(s) - 1
	for start > 0 && !utf8.RuneStart(s[start]) && len(s)-start <= utf8.UTFMax {
		start--
	}
	if !utf8.FullRuneInString(s[start:]) {
		return s[:start]
	}
	return s
}

// streamingThinking feeds each partial into a live thinking block and compares the
// block's rows against the whole-text render after every one.
func streamingThinking(t *testing.T, text string, cut func(int) []int) {
	t.Helper()
	const width = 44
	tr := newTranscript(DefaultTheme())
	tr.setSize(width, 40)

	// Partial lengths: every prefix length from 1 to len(text), or a sampled set for
	// long inputs, so a break that only lands at certain offsets is still covered.
	var ends []int
	if cut != nil {
		ends = cut(len(text))
	} else {
		for n := 1; n <= len(text); n++ {
			ends = append(ends, n)
		}
	}

	for i, end := range ends {
		if end > len(text) {
			end = len(text)
		}
		partial := text[:end]
		tr.setThinking(partial)

		blk := tr.blocks[tr.activeThinking]
		got := tr.blockRows(tr.activeThinking)
		want := wantThinkingRows(tr.theme, transcriptBlock{role: roleThinking, text: validPrefix(partial)}, tr.width)
		if len(got) != len(want) {
			t.Fatalf("partial %d (%d bytes): %d rows, want %d\ngot:  %q\nwant: %q\npartial: %q",
				i, end, len(got), len(want), got, want, partial)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("partial %d row %d = %q, want %q\npartial: %q", i, j, got[j], want[j], partial)
			}
		}
		// The paint-map arithmetic reads the cached height, so it has to agree with
		// the rows the log got while the block is still streaming.
		if h := tr.blockHeight(tr.activeThinking); h != len(got) {
			t.Fatalf("partial %d: blockHeight = %d, want %d (rows)", i, h, len(got))
		}
		_ = blk
	}
}

// TestThinkingStreamMatchesWholeBlockRender is the core equivalence check for
// reasoning, over the shapes a streamed chain of thought takes.
func TestThinkingStreamMatchesWholeBlockRender(t *testing.T) {
	cases := map[string]string{
		"prose":            "I should check how the transcript lays out its rows before changing anything at all, because a wrong assumption here would show up as flicker.",
		"trailing-newline": "first thought\nsecond thought\n",
		"blank-rows":       "alpha\n\n\nbeta\n",
		"no-trailing-nl":   "alpha\nbeta\ngamma",
		"cjk":              "先看看行窗口是怎么切行的，然后再决定要不要改动它；双宽字符不能被切开。",
		"emoji":            "checking 👀 the emoji width 🧮 against the wrap boundary",
		"control-bytes":    "before\rafter\ttabbed\nplain",
		"exact-wrap":       strings.Repeat("x", 42),
		"leading-newline":  "\nalpha",
		"only-newlines":    "\n\n\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) { streamingThinking(t, text, nil) })
	}
}

// TestThinkingStreamTrimmedLongInput samples the prefixes of a realistic long chain
// of thought, which is the case the optimization exists for: checking every byte of
// a 12 KiB reasoning stream would be quadratic in the test itself.
func TestThinkingStreamTrimmedLongInput(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 12<<10; i++ {
		b.WriteString("considering option ")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(" because it interacts with the retry path; ")
		if i%3 == 0 {
			b.WriteString("\n")
		}
	}
	streamingThinking(t, b.String(), func(n int) []int {
		var ends []int
		for e := 1; e <= n; e += 37 {
			ends = append(ends, e)
		}
		ends = append(ends, n)
		return ends
	})
}

// TestThinkingStreamRenarrows verifies a width change mid-stream re-wraps the whole
// reasoning text instead of keeping rows that were wrapped for the old width.
func TestThinkingStreamRenarrows(t *testing.T) {
	const text = "one two three four five six seven eight nine ten eleven twelve thirteen"
	tr := newTranscript(DefaultTheme())
	tr.setSize(50, 40)

	tr.setThinking(text[:30])
	tr.setSize(24, 40)
	tr.setThinking(text)

	got := tr.blockRows(tr.activeThinking)
	want := wantThinkingRows(tr.theme, transcriptBlock{role: roleThinking, text: text}, tr.width)
	if len(got) != len(want) {
		t.Fatalf("after narrowing: %d rows, want %d\ngot: %q\nwant: %q", len(got), len(want), got, want)
	}
	for j := range want {
		if got[j] != want[j] {
			t.Fatalf("row %d = %q, want %q", j, got[j], want[j])
		}
	}
}

// TestThinkingStreamExpandRelaysOut verifies toggling expansion mid-stream changes
// how many rows the block shows, and that appending afterwards still matches the
// whole-text render of the expanded block.
func TestThinkingStreamExpandRelaysOut(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("reason-")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n")
	}
	text := b.String()

	tr := newTranscript(DefaultTheme())
	tr.setSize(60, 40)
	tr.setThinking(text[:len(text)/2])

	collapsed := tr.blockHeight(tr.activeThinking)
	tr.setThinkingExpanded(tr.blocks[tr.activeThinking].id, true)
	tr.reflow()
	expanded := tr.blockHeight(tr.activeThinking)
	if expanded <= collapsed {
		t.Fatalf("expanded height %d, want more than collapsed %d", expanded, collapsed)
	}

	// Appending after the toggle must still match the expanded whole-text render.
	tr.setThinking(text)
	got := tr.blockRows(tr.activeThinking)
	want := wantThinkingRows(tr.theme, transcriptBlock{role: roleThinking, text: text, expanded: true}, tr.width)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expanded stream diverged\ngot:  %q\nwant: %q", got, want)
	}
}

// TestThinkingSettlesWithItsStreamedText verifies the block hands its body back
// when it settles: a turn end (or a new prompt) must leave the reasoning readable,
// not an empty block whose text only ever lived in the cursor.
func TestThinkingSettlesWithItsStreamedText(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 30})
	m = apply(t, m, thinkingDeltaMsg{text: "weighing the options"})
	m = apply(t, m, thinkingDeltaMsg{text: "weighing the options, second pass"})
	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonEndTurn,
	}})

	blk := m.transcript.blocks[0]
	if blk.role != roleThinking {
		t.Fatalf("first block role = %v, want thinking", blk.role)
	}
	if blk.text != "weighing the options, second pass" {
		t.Errorf("settled text = %q, want the streamed reasoning", blk.text)
	}
	if out := stripANSI(m.View().Content); !strings.Contains(out, "second pass") {
		t.Errorf("settled reasoning not rendered:\n%s", out)
	}
}

// TestAbandonedStreamKeepsItsText verifies a turn abandoned mid-stream (a new
// prompt arrives before the turn end) leaves both live blocks with their bodies
// rather than rendering empty.
func TestAbandonedStreamKeepsItsText(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(60, 30)
	tr.setThinking("a half-finished thought")
	tr.appendDelta("a half-finished answer")
	tr.addUser("next question")

	think := tr.blocks[0]
	if think.role != roleThinking || think.text != "a half-finished thought" {
		t.Errorf("abandoned thinking block = %+v, want its text", think)
	}
	ans := tr.blocks[1]
	if ans.role != roleAssistant || ans.text != "a half-finished answer" {
		t.Errorf("abandoned assistant block = %+v, want its text", ans)
	}
	out := stripANSI(strings.Join(tr.rows, "\n"))
	for _, want := range []string{"a half-finished thought", "a half-finished answer", "next question"} {
		if !strings.Contains(out, want) {
			t.Errorf("assembled log missing %q:\n%s", want, out)
		}
	}
}

// TestStreamingAssistantHeightTracksItsRows verifies a live answer block publishes
// its row count. Clicks, scrolling and the paint map read the cached height rather
// than counting rows, so a live block that left renderHeight at its pre-stream value
// (zero, for a block that has never settled) would make its rows unclickable and
// mis-attribute the click to a neighbour.
func TestStreamingAssistantHeightTracksItsRows(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(40, 30)
	tr.addUser("question")
	for i := 0; i < 5; i++ {
		tr.appendDelta("a line of the answer\n")
	}
	want := len(tr.blockRows(tr.activeAssistant))
	if got := tr.blockHeight(tr.activeAssistant); got != want {
		t.Fatalf("live block height = %d, want %d (its rows)", got, want)
	}
}
