package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/ui"
)

// ansiRE strips SGR escape sequences so tests can inspect the raw text the
// transcript stored, independent of the theme's coloring.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// apply runs one Update tick and returns the concrete Model, failing on an
// unexpected model type. It keeps the streaming tests terse.
func apply(t *testing.T, m tea.Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return got
}

// TestTranscriptStreamingConcat feeds a run of text deltas then a turn end and
// asserts the assistant block accumulates the deltas in order and the joined
// text is rendered in the View.
func TestTranscriptStreamingConcat(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 40, Height: 12})

	m = apply(t, m, textDeltaMsg{delta: "Hello "})
	m = apply(t, m, textDeltaMsg{delta: "world"})
	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		Content: agentcore.ContentList{agentcore.NewTextContent("Hello world")},
	}})

	if n := len(m.transcript.blocks); n != 1 {
		t.Fatalf("block count = %d, want 1 assistant block", n)
	}
	if got := m.transcript.blocks[0]; got.role != roleAssistant || got.text != "Hello world" {
		t.Errorf("assistant block = %+v, want role assistant text %q", got, "Hello world")
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "Hello world") {
		t.Errorf("rendered View missing streamed text; got:\n%s", content)
	}
	// The turn was finalized, so a fresh delta starts a NEW assistant block.
	if m.transcript.activeAssistant != -1 {
		t.Errorf("activeAssistant = %d after turn end, want -1", m.transcript.activeAssistant)
	}
}

// TestTranscriptSurfacesTurnError verifies a turn that ends with stopReason
// error surfaces the provider's error message as a system block rather than
// finalizing an empty turn and returning silently to the prompt. The loop
// delivers request failures (e.g. a 4xx) this way — as a terminal assistant
// message via TurnEndEvent, not as the run's result error — so without the
// StopReason check in the turnEndMsg handler the TUI would show nothing at all.
func TestTranscriptSurfacesTurnError(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 40, Height: 12})

	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		StopReason:   agentcore.StopReasonError,
		ErrorMessage: "upstream 401: 无效的令牌",
	}})

	var sys string
	for _, b := range m.transcript.blocks {
		if b.role == roleSystem {
			sys = b.text
		}
	}
	if !strings.Contains(sys, "error:") || !strings.Contains(sys, "upstream 401: 无效的令牌") {
		t.Errorf("turn error not surfaced; system block = %q", sys)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "upstream 401") {
		t.Errorf("rendered View missing the surfaced error; got:\n%s", content)
	}
}

// TestTranscriptSurfacesAbortedTurn verifies a turn that ends with stopReason
// aborted is flagged rather than returning silently.
func TestTranscriptSurfacesAbortedTurn(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 40, Height: 12})

	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		StopReason: agentcore.StopReasonAborted,
	}})

	var sys string
	for _, b := range m.transcript.blocks {
		if b.role == roleSystem {
			sys = b.text
		}
	}
	if !strings.Contains(sys, "aborted") {
		t.Errorf("aborted turn not surfaced; system block = %q", sys)
	}
}

// TestTranscriptNotesEmptyResponse verifies a clean end_turn that produced no
// content and no tool results is flagged with a note (with a provider-mismatch
// hint) instead of returning silently to the prompt — the shape produced when an
// endpoint accepts the request with a 200 but returns nothing decodable.
func TestTranscriptNotesEmptyResponse(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 40, Height: 12})

	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		StopReason: agentcore.StopReasonEndTurn,
	}})

	var sys string
	for _, b := range m.transcript.blocks {
		if b.role == roleSystem {
			sys = b.text
		}
	}
	if !strings.Contains(sys, "empty response from the model") {
		t.Errorf("empty response not flagged; system block = %q", sys)
	}
}

// TestTranscriptCleanTurnNoNote verifies a normal turn with content does NOT add
// a spurious error/empty system note.
func TestTranscriptCleanTurnNoNote(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 40, Height: 12})

	m = apply(t, m, turnEndMsg{msg: agentcore.AssistantMessage{
		StopReason: agentcore.StopReasonEndTurn,
		Content:    agentcore.ContentList{agentcore.NewTextContent("the answer")},
	}})

	for _, b := range m.transcript.blocks {
		if b.role == roleSystem {
			t.Errorf("clean turn should add no system note, got %q", b.text)
		}
	}
}

// TestTranscriptAutoStick verifies the stick-to-bottom rule: while the viewport
// is at the bottom, new content keeps it pinned there; once the user scrolls up,
// streamed content no longer forces a jump to the bottom — but submitting a new
// turn (addUser) re-arms follow and snaps back to the newest output.
func TestTranscriptAutoStick(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(20, 3) // 3 visible rows

	for i := 0; i < 6; i++ {
		tr.addUser("line")
	}
	if !tr.vp.AtBottom() {
		t.Fatal("transcript should stick to the bottom while at the bottom")
	}

	// More content while pinned keeps it pinned.
	tr.addUser("more")
	if !tr.vp.AtBottom() {
		t.Fatal("new content should keep a bottom-pinned transcript at the bottom")
	}

	// Simulate a user scroll-up through the viewport's key handling.
	tr.update(tea.KeyPressMsg{Code: tea.KeyUp})
	if tr.vp.AtBottom() {
		t.Fatal("scrolling up should move the viewport off the bottom")
	}

	// Streamed content arriving while scrolled up must NOT yank the view back to
	// the bottom — the user is reading history.
	tr.appendDelta("streamed while reading history\nsecond line\nthird line")
	if tr.vp.AtBottom() {
		t.Error("auto-stick should stay paused after the user scrolls up")
	}

	// Submitting a new turn is an explicit action: it re-arms follow and snaps
	// back to the newest output so the reply is never left off-screen.
	tr.addUser("a brand new prompt")
	if !tr.vp.AtBottom() {
		t.Error("submitting a new turn should re-arm auto-scroll to the bottom")
	}
}

// TestTranscriptCJKWrap feeds a long CJK line into a narrow transcript and
// asserts every wrapped line fits the width in display columns (not bytes) and
// that no rune was dropped or split.
func TestTranscriptCJKWrap(t *testing.T) {
	const width = 10
	tr := newTranscript(DefaultTheme())
	tr.setSize(width, 20)

	line := strings.Repeat("你好世界", 5) // 20 CJK runes = 40 display columns
	tr.addUser(line)

	content := tr.vp.GetContent()
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the CJK line to wrap onto multiple rows, got %d line(s)", len(lines))
	}
	for i, ln := range lines {
		if w := ui.Width(ln); w > width {
			t.Errorf("wrapped line %d width = %d columns, want <= %d: %q", i, w, width, stripANSI(ln))
		}
	}
	// No rune was cut or dropped: every source rune survives the wrap.
	if got := strings.Count(stripANSI(content), "你"); got != 5 {
		t.Errorf("counted %d 你 runes after wrap, want 5", got)
	}
}

// TestTranscriptScrollbar verifies the scrollbar policy: the gutter is hidden
// while the content fits (nothing to scroll) and appears only once the content
// overflows. When overflowing, a rounded pill thumb (body "█" with half-block
// caps "▄"/"▀") sits alongside the thin groove "│".
func TestTranscriptScrollbar(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(20, 4) // 4 visible rows

	// Two short lines fit in 4 rows: no scrollbar at all — no thumb, no groove.
	tr.addUser("one")
	tr.addUser("two")
	if tr.overflowing() {
		t.Fatal("transcript should not overflow while content fits")
	}
	fit := stripANSI(tr.view())
	if strings.ContainsAny(fit, "█▄▀│") {
		t.Errorf("expected no scrollbar glyphs while content fits; got:\n%q", fit)
	}

	// Enough lines to exceed 4 rows: now it overflows, thumb shrinks and the
	// groove appears.
	for i := 0; i < 10; i++ {
		tr.addUser("line")
	}
	if !tr.overflowing() {
		t.Fatal("transcript should overflow once content exceeds the viewport")
	}
	view := stripANSI(tr.view())
	if !strings.Contains(view, "▄") || !strings.Contains(view, "▀") {
		t.Errorf("expected a rounded pill thumb (▄ top, ▀ bottom) while overflowing; got:\n%q", view)
	}
	if !strings.Contains(view, "│") {
		t.Errorf("expected a groove │ while overflowing; got:\n%q", view)
	}
	if strings.Contains(view, "░") {
		t.Errorf("scrollbar no longer uses the shaded track ░; got:\n%q", view)
	}
}

// TestTranscriptScrollToRow checks the click/drag mapping: pressing the top of
// the gutter scrolls to the top, the bottom scrolls to the bottom, and it is a
// no-op when the content fits.
func TestTranscriptScrollToRow(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(20, 4)

	// Content fits: dragging must not move a non-scrollable viewport.
	tr.addUser("only line")
	tr.scrollToRow(3)
	if tr.vp.YOffset() != 0 {
		t.Errorf("scrollToRow on non-overflowing viewport moved offset to %d, want 0", tr.vp.YOffset())
	}

	for i := 0; i < 20; i++ {
		tr.addUser("line")
	}
	if !tr.overflowing() {
		t.Fatal("expected overflow after filling the transcript")
	}

	tr.scrollToRow(0)
	if !tr.vp.AtTop() {
		t.Errorf("dragging to row 0 should scroll to the top; YOffset=%d", tr.vp.YOffset())
	}

	tr.scrollToRow(tr.viewportHeight() - 1)
	if !tr.vp.AtBottom() {
		t.Errorf("dragging to the last row should scroll to the bottom; YOffset=%d", tr.vp.YOffset())
	}
}

// TestModelScrollbarDrag drives the model with mouse press/motion/release on the
// scrollbar column and asserts the drag state toggles and the viewport scrolls.
func TestModelScrollbarDrag(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 30, Height: 8})
	for i := 0; i < 40; i++ {
		m.transcript.addUser("line")
	}
	if !m.transcript.overflowing() {
		t.Fatal("expected the transcript to overflow")
	}

	col := m.width - 1
	// Press at the top of the gutter: drag begins and the view jumps to the top.
	m = apply(t, m, tea.MouseClickMsg{X: col, Y: 0, Button: tea.MouseLeft})
	if !m.draggingScrollbar {
		t.Fatal("left press on the scrollbar column should start dragging")
	}
	if !m.transcript.vp.AtTop() {
		t.Errorf("press at row 0 should scroll to top; YOffset=%d", m.transcript.vp.YOffset())
	}

	// Motion to the bottom row while held drags the thumb down.
	m = apply(t, m, tea.MouseMotionMsg{X: col, Y: m.transcript.viewportHeight() - 1, Button: tea.MouseLeft})
	if !m.transcript.vp.AtBottom() {
		t.Errorf("motion to the last row while dragging should scroll to bottom; YOffset=%d", m.transcript.vp.YOffset())
	}

	// Release ends the drag.
	m = apply(t, m, tea.MouseReleaseMsg{X: col, Y: 3, Button: tea.MouseLeft})
	if m.draggingScrollbar {
		t.Error("release should end the scrollbar drag")
	}

	// A press away from the gutter column must not start a drag.
	m = apply(t, m, tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if m.draggingScrollbar {
		t.Error("press off the scrollbar column should not start dragging")
	}
}

// TestTranscriptSetSizeSkipsUnchangedReflow pins the keystroke fast path: the
// model re-runs relayout on every key press, and an unchanged size must not
// re-flow the transcript (which would re-render the whole conversation, Markdown
// included, on each typed rune). A height change still re-flows, and the blocks
// it lays out come from their cached renders rather than being re-rendered.
func TestTranscriptSetSizeSkipsUnchangedReflow(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(30, 6)
	tr.addUser("original")

	// Stand-in for "a newer render exists": the cache is what a reflow would use.
	tr.blocks[0].rendered = "SENTINEL"

	tr.setSize(30, 6) // same width and height: no re-flow
	if content := tr.vp.GetContent(); strings.Contains(content, "SENTINEL") {
		t.Fatalf("an unchanged size re-flowed the transcript; content = %q", content)
	}

	tr.setSize(30, 7) // height changed: re-flow runs and reuses the cached render
	if content := tr.vp.GetContent(); !strings.Contains(content, "SENTINEL") {
		t.Fatalf("a height change should re-flow from the cached renders; content = %q", content)
	}
}

// TestTranscriptReusesCachedBlockRenders verifies the per-block render cache: a
// reflow at the same width reuses each block's cached text (so finalized
// assistant turns are not re-run through glamour), and a width change
// re-renders them because the wrapping and Markdown layout change.
func TestTranscriptReusesCachedBlockRenders(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(30, 10)
	tr.addUser("first")
	tr.addSystem("second")

	tr.blocks[0].rendered = "CACHED-FIRST"
	tr.reflow()
	if content := tr.vp.GetContent(); !strings.Contains(content, "CACHED-FIRST") {
		t.Fatalf("reflow re-rendered an unchanged block instead of reusing its cache; content = %q", content)
	}

	tr.setSize(40, 10)
	content := tr.vp.GetContent()
	if strings.Contains(content, "CACHED-FIRST") {
		t.Fatal("a width change must invalidate the cached render")
	}
	if !strings.Contains(stripANSI(content), "first") {
		t.Fatalf("block content missing after re-render at the new width; content = %q", content)
	}
}

// TestTranscriptScrollbarColumnSticky verifies the scrollbar column is reserved
// once and stays reserved across reflows (re-deciding it each time would render
// every block at both widths on each streamed delta), and is released again when
// the content shrinks back to fitting.
func TestTranscriptScrollbarColumnSticky(t *testing.T) {
	tr := newTranscript(DefaultTheme())
	tr.setSize(20, 4)
	for i := 0; i < 10; i++ {
		tr.addUser("line")
	}
	if !tr.narrowed {
		t.Fatal("an overflowing transcript should reserve a column for the scrollbar")
	}
	if tr.width != 19 {
		t.Fatalf("content width = %d, want 19 (20 minus the scrollbar column)", tr.width)
	}

	tr.reflow()
	tr.reflow()
	if tr.width != 19 {
		t.Fatalf("content width flipped to %d across reflows, want it to stay 19", tr.width)
	}

	tr.blocks = tr.blocks[:1]
	tr.reflow()
	if tr.narrowed || tr.width != 20 {
		t.Fatalf("narrowed=%v width=%d after the content fits again, want false/20", tr.narrowed, tr.width)
	}
}

// TestToolCardCacheInvalidatedOnCompletion verifies a tool card's cached render
// is dropped when the call completes: the card is appended while running and its
// result arrives later, so without the revision bump the transcript would keep
// showing the empty running card.
func TestToolCardCacheInvalidatedOnCompletion(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 20})
	m = apply(t, m, toolStartMsg{id: "c1", name: "bash", input: map[string]any{"command": "ls"}})
	m = apply(t, m, toolEndMsg{id: "c1", ok: true, result: "file-one.txt"})

	if content := stripANSI(m.View().Content); !strings.Contains(content, "file-one.txt") {
		t.Fatalf("completed card result missing from the view; got:\n%s", content)
	}
}

// TestToolCardExpandInvalidatesRender verifies Ctrl+O's expand toggle bumps the
// card revision so the transcript re-renders the card with its full response
// instead of reusing the capped preview.
func TestToolCardExpandInvalidatesRender(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 30})
	m = apply(t, m, toolStartMsg{id: "c1", name: "bash", input: map[string]any{"command": "seq 1 10"}})
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("row-%02d", i))
	}
	m = apply(t, m, toolEndMsg{id: "c1", ok: true, result: strings.Join(lines, "\n")})

	if content := stripANSI(m.View().Content); !strings.Contains(content, "(Ctrl+O for more)") {
		t.Fatalf("collapsed card should advertise the expand hint; got:\n%s", content)
	}
	if content := stripANSI(m.View().Content); strings.Contains(content, "row-10") {
		t.Fatal("collapsed card should cap its response lines")
	}

	m = apply(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if content := stripANSI(m.View().Content); !strings.Contains(content, "row-10") {
		t.Fatalf("expanded card should show every response line; got:\n%s", content)
	}
}

// TestFitLine verifies the scrollbar gutter fit: a row already exactly the
// content width is returned untouched (the per-frame fast path), short rows are
// padded, and over-wide rows are truncated, always to the exact column width.
func TestFitLine(t *testing.T) {
	const width = 10
	if got := fitLine("abcdefghij", width); got != "abcdefghij" {
		t.Errorf("exact-width line was rewritten: %q", got)
	}
	if got := fitLine("abc", width); got != "abc"+"\x1b[0m"+strings.Repeat(" ", 7) {
		t.Errorf("short line = %q, want it reset and padded to %d columns", got, width)
	}
	if got := fitLine(strings.Repeat("x", 30), width); ui.Width(got) != width {
		t.Errorf("over-wide line width = %d, want %d: %q", ui.Width(got), width, got)
	}
	for _, in := range []string{"", "abc", strings.Repeat("界", 20), "\x1b[31mred\x1b[0m"} {
		if got := fitLine(in, width); ui.Width(got) != width {
			t.Errorf("fitLine(%q) width = %d, want %d", in, ui.Width(got), width)
		}
	}
}
