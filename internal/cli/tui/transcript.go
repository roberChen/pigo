package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/smallnest/pigo/internal/agentcore"
)

// This file implements the scrolling transcript region of the full-screen TUI
// (US-005, SPEC 5.1 transcript, FR-5/FR-10). The transcript owns a
// viewport.Model and an ordered list of rendered blocks (user / assistant /
// system turns). Streaming assistant text arrives as textDeltaMsg values that
// append to the current assistant block; turnEndMsg finalizes it. Content is
// re-flowed through the viewport with theme.WrapToWidth at the live width so CJK
// and emoji never split mid-rune. Tool cards are a later node (#389); this file
// leaves a clean seam (system lines) without building cards.

// blockRole distinguishes the three transcript block kinds so each renders with
// its own theme style.
type blockRole int

const (
	roleUser blockRole = iota
	roleAssistant
	roleSystem
	roleTool
	roleThinking
	// roleBanner is the startup logo + config splash. Its text is pre-rendered
	// (already colored, already laid out) and emitted verbatim, so reflow neither
	// wraps it nor overrides its colors with a role style.
	roleBanner
)

// transcriptBlock is one rendered turn in the transcript. text is the raw
// (unstyled, unwrapped) message body; the role selects the theme style and any
// prefix applied at render time. For roleTool blocks text is unused and card
// points at the live tool card (#389); the pointer lets a later toolEndMsg /
// Ctrl+O mutate the card in place and have it re-render on the next reflow.
//
// A roleThinking block carries a stable id and its own expanded flag: a click on
// the block flips it, and the clicked row has to name the block rather than its
// index, because the paint map outlives the frame that was drawn. expanded is read
// by renderBlock, so it works for blocks replayed from a session too.
//
// rendered caches the block's rendered text so a reflow re-renders only what
// changed. A finalized assistant turn goes through glamour (~1ms per turn), and
// re-running every turn on each streamed delta or keystroke is what made the
// TUI lag once a conversation grew. renderOK is false for a block that was
// never rendered and is cleared whenever the block's text changes;
// renderWidth/renderRev record what the cache is valid for — the content width
// the text was wrapped to and, for a tool card, the card revision it was
// rendered from (see renderRev). renderHeight is the cached row count of that
// render, so the row arithmetic behind clicks and the paint map never has to
// re-scan a long transcript.
type transcriptBlock struct {
	role     blockRole
	text     string
	card     *toolCard
	id       string // roleThinking blocks only: the id a click targets
	expanded bool   // roleThinking blocks only: show the whole chain of thought

	rendered     string
	renderWidth  int
	renderRev    int
	renderHeight int
	renderOK     bool
}

// transcript is the scrolling message log. It wraps a viewport.Model and keeps
// the source blocks so it can re-flow on width changes. activeAssistant indexes
// the assistant block currently receiving streaming deltas, or -1 when no turn
// is streaming.
type transcript struct {
	vp    viewport.Model
	theme Theme

	// totalWidth is the full width the transcript may occupy (terminal columns
	// minus any chrome the model reserves). width (below) is the content width
	// the blocks actually wrap to: it equals totalWidth when the content fits, or
	// totalWidth-1 when it overflows and a scrollbar column must be held back.
	// reflow recomputes width from totalWidth on every content change, so the bar
	// column appears/disappears correctly even as a run streams in new lines.
	totalWidth int

	// width is the content width (terminal columns) the blocks wrap to. It is
	// separate from the viewport's own width so reflow measurements stay stable
	// even before the first size message.
	width int

	blocks          []transcriptBlock
	activeAssistant int
	// activeThinking indexes the thinking block currently receiving streamed
	// reasoning deltas, or -1 when none is streaming. The block keeps its text after
	// the turn ends; only the live index is reset.
	activeThinking int

	// follow is the stick-to-bottom intent: while true, every reflow snaps the
	// viewport to the newest line so streamed output stays visible. It is set
	// when the user submits a turn and cleared when they scroll up to read
	// history (re-armed when they scroll back to the bottom). Tracking intent
	// explicitly — rather than sampling viewport.AtBottom() inside reflow — keeps
	// auto-scroll correct across height changes (setSize resizes the viewport
	// before reflow runs, which would make an AtBottom() sample read false).
	follow bool

	// narrowed records that the last reflow held one column back for the
	// scrollbar because the content overflowed. Sticky across reflows so a
	// scrolling transcript keeps laying its blocks out at the narrow width
	// instead of re-rendering the whole log at both widths on every delta (the
	// per-block render cache only pays off when the width is stable). reflow
	// clears it once the content fits again.
	narrowed bool

	// paint is the click map of the most recently painted frame: the collapsible
	// block (tool card or thinking block) drawn at each visible screen row. A click
	// resolves against it rather than against the live layout, because the transcript
	// keeps changing between two frames — while a run streams, text appended after a
	// frame was drawn moves every row under the cursor, and reading the live layout on
	// click would toggle the block that *now* sits at that row instead of the one the
	// user clicked. It is a pointer so the copies Model.Update makes of the
	// transcript share the one record View writes.
	paint *paintRows

	// thinkSeq numbers thinking blocks so each one has an id to be clicked by.
	// Blocks are only ever appended, so an id stays unique and stable for the life
	// of the transcript.
	thinkSeq int
}

// paintRows is the row → clickable-block map of one painted frame: targets[y] is
// the block drawn at visible transcript row y, or the zero value for a row that
// toggles nothing.
type paintRows struct{ targets []rowTarget }

// rowTarget names the block a painted row belongs to. role separates a tool card
// from a thinking block (nothing else is clickable) and id names the block within
// that kind.
type rowTarget struct {
	role blockRole
	id   string
}

// newTranscript builds an empty transcript with the given theme. The viewport
// starts zero-sized; the model drives setSize from the first tea.WindowSizeMsg.
func newTranscript(theme Theme) transcript {
	vp := viewport.New()
	return transcript{
		vp:              vp,
		theme:           theme,
		activeAssistant: -1,
		activeThinking:  -1,
		paint:           &paintRows{},
	}
}

// setSize resizes the transcript's viewport and re-flows the blocks to the new
// width. A non-positive dimension is clamped to zero so the viewport never sees
// a negative extent. width is the total space available; reflow decides whether
// to spend one column on the scrollbar based on whether the content overflows.
//
// An unchanged size is a no-op: the model calls relayout on every keystroke and
// menu change, and re-flowing there is pure waste because neither the content
// nor the width changed (the input row/height accounting, not the transcript,
// is what moved).
func (t *transcript) setSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	if width == t.totalWidth && height == t.vp.Height() {
		return
	}
	t.totalWidth = width
	t.vp.SetHeight(height)
	t.reflow()
}

// invalidateRenders drops every block's cached render so the next reflow
// re-renders all of them. It is needed when something other than the block's
// own text changes its output — today only the Markdown palette
// (SetMarkdownDark) — since the per-block cache is keyed on width and card
// revision alone.
func (t *transcript) invalidateRenders() {
	for i := range t.blocks {
		t.blocks[i].renderOK = false
	}
}

// addUser appends a user turn and closes any streaming assistant block, then
// re-flows. Submitting a prompt is an explicit action where the user always
// wants to see their new turn and the response that follows, so it re-arms
// follow: the viewport snaps to the bottom even if the user had scrolled up
// (e.g. reading the startup banner) — otherwise the streamed reply would
// accumulate off-screen and look like nothing happened. Subsequent streaming
// deltas keep the bottom via follow, which the user can pause by scrolling up.
func (t *transcript) addUser(text string) {
	t.blocks = append(t.blocks, transcriptBlock{role: roleUser, text: text})
	t.activeAssistant = -1
	t.activeThinking = -1
	t.follow = true
	t.reflow()
}

// newThinking builds a thinking block with a fresh id, so a click can name the
// block it toggles after the log has grown past the frame the user clicked.
func (t *transcript) newThinking(text string) transcriptBlock {
	t.thinkSeq++
	return transcriptBlock{
		role: roleThinking,
		text: text,
		id:   fmt.Sprintf("think-%d", t.thinkSeq),
	}
}

// setThinking writes the accumulated reasoning text of the current turn into the
// streaming thinking block (creating it on the first delta). The provider sends the
// whole partial each time, so the block is replaced rather than appended to; the
// rendered view caps it (see renderBlock), so a long chain of thought cannot push
// the answer off the screen.
func (t *transcript) setThinking(text string) {
	if t.activeThinking < 0 {
		t.blocks = append(t.blocks, t.newThinking(text))
		t.activeThinking = len(t.blocks) - 1
	} else {
		t.blocks[t.activeThinking].text = text
		t.blocks[t.activeThinking].renderOK = false
	}
	t.reflow()
}

// setThinkingExpanded sets the expanded state of the thinking block with the given
// id and reports whether that block exists. The render cache is dropped because the
// expanded flag is not part of the revision the cache is keyed on.
func (t *transcript) setThinkingExpanded(id string, expanded bool) bool {
	for i := range t.blocks {
		blk := &t.blocks[i]
		if blk.role != roleThinking || blk.id != id {
			continue
		}
		blk.expanded = expanded
		blk.renderOK = false
		return true
	}
	return false
}

// endThinking closes the streaming thinking block. The text stays in the
// transcript (capped) so the reasoning remains readable after the turn.
func (t *transcript) endThinking() {
	t.activeThinking = -1
}

// addSystem appends a system / meta notice (used for run lifecycle and other
// inline notes).
func (t *transcript) addSystem(text string) {
	t.blocks = append(t.blocks, transcriptBlock{role: roleSystem, text: text})
	t.reflow()
}

// addBanner appends a pre-rendered splash block (startup logo + config). It is
// emitted verbatim by renderBlock, so its colors and horizontal layout survive
// reflow untouched.
func (t *transcript) addBanner(text string) {
	t.blocks = append(t.blocks, transcriptBlock{role: roleBanner, text: text})
	t.reflow()
}

// addToolCard appends a rich tool-call card (#389) as an ordered block so it
// renders inline in the transcript. The card is held by pointer, so a later
// state change (toolEndMsg) or expand toggle (Ctrl+O) followed by reflow
// re-renders it in place.
func (t *transcript) addToolCard(c *toolCard) {
	t.blocks = append(t.blocks, transcriptBlock{role: roleTool, card: c})
	t.reflow()
}

// appendDelta grows the current assistant block by delta, creating the block on
// the first delta of a turn. The re-flow auto-sticks to the bottom when the user
// has not scrolled up.
func (t *transcript) appendDelta(delta string) {
	if t.activeAssistant < 0 {
		t.blocks = append(t.blocks, transcriptBlock{role: roleAssistant})
		t.activeAssistant = len(t.blocks) - 1
	}
	blk := &t.blocks[t.activeAssistant]
	blk.text += delta
	blk.renderOK = false
	t.reflow()
}

// finalizeTurn closes the streaming assistant block. When the final message
// carries text it becomes the block's authoritative body (covering turns that
// arrive without incremental deltas); otherwise the accumulated deltas stand.
func (t *transcript) finalizeTurn(msg agentcore.AssistantMessage) {
	t.endThinking()
	text := agentcore.ContentToText(msg.Content)
	if t.activeAssistant >= 0 {
		if text != "" {
			t.blocks[t.activeAssistant].text = text
		}
		// The streaming block was rendered as plain text; drop that render so the
		// finalized block is laid out as Markdown on this reflow.
		t.blocks[t.activeAssistant].renderOK = false
	} else if text != "" {
		t.blocks = append(t.blocks, transcriptBlock{role: roleAssistant, text: text})
	}
	t.activeAssistant = -1
	t.reflow()
}

// update forwards a message (typically a key press or scroll) to the viewport so
// PgUp/PgDn/arrow scrolling works, then re-syncs the follow intent: scrolling up
// off the bottom pauses auto-scroll, and scrolling back to the bottom re-arms it.
func (t *transcript) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	t.vp, cmd = t.vp.Update(msg)
	t.follow = t.vp.AtBottom()
	return cmd
}

// scrollToRow positions the viewport so the scrollbar thumb aligns with the
// given viewport row y (0-based). It is the inverse of the thumb-position math
// in scrollbar(): pressing or dragging on row y maps that row to the matching
// scroll offset, so clicking the gutter jumps there and dragging the thumb
// tracks the cursor. It is a no-op when the content fits (nothing to scroll).
func (t *transcript) scrollToRow(y int) {
	h := t.vp.Height()
	if h <= 0 {
		return
	}
	total := t.vp.TotalLineCount()
	if total <= h {
		return
	}
	thumb := h * h / total
	if thumb < 2 {
		thumb = 2
	}
	if thumb > h {
		thumb = h
	}
	span := h - thumb // rows the thumb top can occupy
	if span <= 0 {
		return
	}
	// Center the grab on the thumb: aim its top at y minus half its body so the
	// cursor sits roughly mid-thumb, then clamp into the track.
	top := y - thumb/2
	if top < 0 {
		top = 0
	}
	if top > span {
		top = span
	}
	maxOff := total - h
	t.vp.SetYOffset(top * maxOff / span)
	t.follow = t.vp.AtBottom()
}

// viewportHeight reports the number of visible transcript rows, so the model can
// tell whether a mouse Y falls within the scrollable region.
func (t transcript) viewportHeight() int { return t.vp.Height() }

// overflowing reports whether the transcript has more content than fits in the
// viewport, i.e. there is history to scroll. relayout uses this to reserve the
// scrollbar column only when scrolling is possible, and view uses it to decide
// whether to attach the thumb at all.
func (t transcript) overflowing() bool {
	return t.vp.Height() > 0 && t.vp.TotalLineCount() > t.vp.Height()
}

// view renders the current visible slice of the transcript. When the content
// overflows the viewport a one-column vertical scrollbar is drawn down the right
// edge (FR-10): each viewport row is normalized to exactly the content width
// before the scrollbar cell is appended, so the bar sits flush against the
// terminal's right edge and a dangling SGR from Markdown rendering can never
// bleed into (and hide) the bar column. When everything fits there is nothing to
// scroll, so no bar is drawn and the viewport uses the full width (relayout
// releases the reserved column in that case).
func (t transcript) view() string {
	t.recordPaint()
	if !t.overflowing() {
		return t.vp.View()
	}

	bar := strings.Split(t.scrollbar(), "\n")
	body := strings.Split(t.vp.View(), "\n")

	var b strings.Builder
	for i := range bar {
		if i > 0 {
			b.WriteByte('\n')
		}
		if i < len(body) && t.width > 0 {
			b.WriteString(fitLine(body[i], t.width))
		}
		b.WriteString(bar[i])
	}
	return b.String()
}

// fitLine clamps one viewport row to exactly width display columns so the
// scrollbar cell after it lands in the rightmost column: over-wide lines are
// truncated ANSI-aware, short ones padded. The narrow path keeps a line that is
// already the right width as-is — the viewport pads its own rows to the content
// width, so this is the common case and avoids re-laying every row through
// lipgloss on each frame, which cost ~10µs per row. Padding is preceded by a
// reset so a dangling SGR from Markdown rendering cannot color it (and through
// it the bar).
func fitLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(line)
	if w == width {
		return line
	}
	if w > width {
		line = ansi.Truncate(line, width, "")
		w = ansi.StringWidth(line)
		if w >= width {
			return line
		}
	}
	return line + "\x1b[0m" + strings.Repeat(" ", width-w)
}

// recordPaint captures which tool card, if any, the frame about to be drawn puts
// on each visible row. Click resolution reads this map instead of re-deriving the
// layout from live state (cardIDAtScreenY), because between two frames the
// transcript can append streamed output and scroll: the live layout is then a few
// rows ahead of what the user's pointer is pointing at, and a click would toggle a
// neighbouring card. The map walks the blocks once, which the cached row counts
// make cheap, and stops as soon as it passes the visible window.
func (t transcript) recordPaint() {
	if t.paint == nil {
		return
	}
	h := t.vp.Height()
	if h <= 0 {
		t.paint.targets = nil
		return
	}
	targets := t.paint.targets
	if cap(targets) < h {
		targets = make([]rowTarget, h)
	} else {
		targets = targets[:h]
		clear(targets)
	}

	offset := t.vp.YOffset()
	at := 0
	for i := range t.blocks {
		if i > 0 {
			at += t.separatorRows(i)
		}
		if at-offset >= h {
			break // the rest of the log is below the viewport
		}
		height := t.blockHeight(i)
		if target := t.rowTarget(i); target.id != "" {
			for y := max(at-offset, 0); y < at-offset+height && y < h; y++ {
				targets[y] = target
			}
		}
		at += height
	}
	t.paint.targets = targets
}

// rowTarget names the block at index i when it is one a click can toggle: a tool
// card, or a thinking block that is actually collapsible. A thinking block whose
// whole chain of thought already fits the preview is left out, so clicking it does
// not flip a chevron that reveals nothing.
func (t *transcript) rowTarget(i int) rowTarget {
	blk := &t.blocks[i]
	switch {
	case blk.card != nil:
		return rowTarget{role: roleTool, id: blk.card.id}
	case blk.role == roleThinking && blk.id != "" && t.thinkingCollapsible(i):
		return rowTarget{role: roleThinking, id: blk.id}
	}
	return rowTarget{}
}

// thinkingCollapsible reports whether thinking block i has rows the default view
// hides — that is, whether a click on it would change anything. It reads the same
// row counts the paint map just computed, so the check costs nothing; a block that
// is expanded always counts, so it can be collapsed again.
func (t *transcript) thinkingCollapsible(i int) bool {
	blk := &t.blocks[i]
	if blk.expanded {
		return true
	}
	// Collapsed, the block renders its header plus at most thinkingPreviewLines
	// rows; the elision note adds exactly one row when rows were dropped.
	return t.blockHeight(i) > thinkingPreviewLines+1
}

// scrollbar renders the one-column vertical scrollbar the height of the
// viewport. A proportional thumb marks the visible window and its position marks
// the scroll offset, so scrolling up through history moves the thumb; the
// remaining rows draw a thin groove (│). The thumb is drawn as a capsule like
// the macOS system scrollbar: a lower-half block ▄ caps the top and an upper-half
// block ▀ caps the bottom (their filled halves sit on the inner edges so the
// outer ends taper to rounded), with the full block █ filling the body rows
// between the caps. The thumb is never shorter than three rows, so the capsule
// always shows a body between its two rounded caps rather than collapsing to a
// flat blob. When the content fits (no overflow) the capsule fills the full
// height.
func (t transcript) scrollbar() string {
	h := t.vp.Height()
	if h <= 0 {
		return ""
	}
	total := t.vp.TotalLineCount()
	thumb := h
	pos := 0
	if total > h {
		thumb = h * h / total
		// Keep the capsule shape (rounded cap + body + rounded cap) by never
		// letting the thumb shrink below three rows; clamp down to the viewport
		// height when it is shorter than that.
		if thumb < 3 {
			thumb = 3
		}
		if thumb > h {
			thumb = h
		}
		maxOff := total - h
		off := t.vp.YOffset()
		if off > maxOff {
			off = maxOff
		}
		if maxOff > 0 {
			pos = off * (h - thumb) / maxOff
		}
	}
	var b strings.Builder
	for i := 0; i < h; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch {
		case i < pos || i >= pos+thumb:
			b.WriteString(t.theme.ScrollTrack.Render("│"))
		case thumb >= 2 && i == pos:
			b.WriteString(t.theme.ScrollThumb.Render("▄"))
		case thumb >= 2 && i == pos+thumb-1:
			b.WriteString(t.theme.ScrollThumb.Render("▀"))
		default:
			b.WriteString(t.theme.ScrollThumb.Render("█"))
		}
	}
	return b.String()
}

// reflow re-renders the blocks to the current width and pushes the joined
// content into the viewport. When the follow intent is set it snaps to the
// bottom so new content auto-scrolls; otherwise the offset is preserved so
// reading history is not interrupted. follow is tracked in update/scrollToRow
// (user scroll) and addUser (new turn) rather than sampled here, because setSize
// resizes the viewport before reflow runs and an AtBottom() sample would misread.
//
// Width is decided here rather than in setSize so it stays correct as a run
// streams in new lines (which reach reflow via appendDelta/finalizeTurn, not
// setSize): when the content overflows the viewport one column is handed back to
// the scrollbar and the blocks are laid out at totalWidth-1, otherwise the full
// width is used and view() draws no bar. Narrowing is sticky (see
// transcript.narrowed): a scrolling transcript would otherwise have to render
// every block at both widths on each delta, defeating the per-block render
// cache. A narrower width never reduces the line count, so a transcript that
// still fits at totalWidth-1 fits at the full width too and the column is
// released.
func (t *transcript) reflow() {
	if t.narrowed && t.totalWidth > 0 {
		t.layout(t.totalWidth - 1)
		if t.vp.TotalLineCount() > t.vp.Height() {
			return
		}
		// The content fits again at the narrow width, so it also fits at the full
		// width: release the scrollbar column and re-lay.
		t.narrowed = false
		t.layout(t.totalWidth)
		return
	}

	t.layout(t.totalWidth)
	if t.totalWidth > 0 && t.vp.TotalLineCount() > t.vp.Height() {
		t.narrowed = true
		t.layout(t.totalWidth - 1)
	}
}

// layout renders the blocks at width, hands the result to the viewport, and
// re-arms the follow intent. Callers own the narrowing decision.
func (t *transcript) layout(width int) {
	t.width = width
	t.vp.SetWidth(width)
	t.vp.SetContent(t.renderAll())
	if t.follow {
		t.vp.GotoBottom()
	}
}

// renderAll joins every block, rendered to the current content width, into the
// transcript body string. Consecutive turns are separated by a blank line before
// a new user turn so requests read as visually distinct.
func (t *transcript) renderAll() string {
	var b strings.Builder
	for i := range t.blocks {
		if i > 0 {
			b.WriteByte('\n')
			if t.blocks[i].role == roleUser {
				b.WriteByte('\n')
			}
		}
		b.WriteString(t.blockView(i))
	}
	return b.String()
}

// blockView returns block i's rendered text, reusing its cached render when
// neither its content nor the content width changed. The still-streaming block
// is never cached: it changes on every delta and renders as plain text, so a
// cache there would only add bookkeeping.
func (t *transcript) blockView(i int) string {
	if i < 0 || i >= len(t.blocks) {
		return ""
	}
	if t.renderCurrent(i) {
		return t.blocks[i].rendered
	}
	blk := &t.blocks[i]
	streaming := i == t.activeAssistant
	out := renderBlock(t.theme, *blk, t.width, streaming)
	blk.rendered = out
	blk.renderWidth = t.width
	blk.renderRev = t.renderRev(i)
	blk.renderHeight = strings.Count(out, "\n") + 1
	blk.renderOK = true
	return out
}

// renderRev is the revision block i's render is keyed on beyond the content width:
// the tool card's rev for a card, or -1 while the block is still streaming. A
// streaming block renders as plain text and is re-styled as Markdown once it ends,
// so the streaming state is part of the key — otherwise the plain render would
// outlive the turn. Every text change clears renderOK, so the text itself needs no
// revision number.
func (t *transcript) renderRev(i int) int {
	if i == t.activeAssistant {
		return -1
	}
	if blk := &t.blocks[i]; blk.card != nil {
		return blk.card.rev
	}
	return 0
}

// renderCurrent reports whether block i's cached render is valid for the current
// width and revision, i.e. blockView would hand back the cached text.
func (t *transcript) renderCurrent(i int) bool {
	return t.blocks[i].renderOK &&
		t.blocks[i].renderWidth == t.width &&
		t.blocks[i].renderRev == t.renderRev(i)
}

// thinkingPreviewLines caps how much of a thinking block the transcript shows.
// Reasoning can be arbitrarily long and is not the answer, so it is budgeted
// (and clickable to reveal more). A user turn is deliberately *not* budgeted:
// see renderUserTurn.
const (
	thinkingPreviewLines = 6
	// thinkingExpandedLines is the ceiling once a thinking block is opened. It
	// matches a tool card's ceiling: reasoning can run to thousands of rows, and
	// rendering all of it on a click would stall the frame for content the session
	// file already holds in full.
	thinkingExpandedLines = 400
)

// renderBlock wraps a block's text to width and applies the role's theme style.
// Wrapping happens on the raw text (measured in display columns via WrapToWidth)
// before styling so ANSI escapes never confuse the width math and no double-width
// rune is split. A finalized assistant block is rendered as Markdown (fix #3,
// mirroring the REPL's turn-end render); the still-streaming block
// (streaming==true) stays plain text because Markdown can only be laid out once
// the whole block is known. A user turn carries a "▌ you" gutter so the human's
// input is impossible to confuse with model output, and thinking blocks render dim,
// labeled and capped.
func renderBlock(theme Theme, blk transcriptBlock, width int, streaming bool) string {
	if blk.role == roleTool && blk.card != nil {
		return blk.card.render(theme, width)
	}
	switch blk.role {
	case roleBanner:
		return blk.text
	case roleUser:
		return renderUserTurn(theme, blk.text, width)
	case roleSystem:
		return theme.System.Render(WrapToWidth(blk.text, width))
	case roleThinking:
		return renderThinking(theme, blk, width)
	default:
		if streaming {
			return theme.Assistant.Render(WrapToWidth(blk.text, width))
		}
		return renderMarkdown(blk.text, width)
	}
}

// renderUserTurn renders a human turn behind a "▌ you" gutter: the label on the
// first row, then one bar-prefixed row per wrapped line. The gutter is what
// visually separates the human's words from the model's, which matters most in a
// transcript where both are plain text and a pasted block can look like an answer.
//
// The turn is rendered in full, however long it is. It used to be capped at a
// dozen rows with a "… N more lines (see /export)" marker, which hid exactly the
// content the human had just supplied and forced a detour through /export to read
// their own paste back. Thinking blocks and tool output remain budgeted because
// they are model/program output rather than the user's own input.
func renderUserTurn(theme Theme, text string, width int) string {
	bar := theme.User.Render("▌")
	inner := width - 2 // the gutter takes a bar plus a space
	if inner < 1 {
		inner = 1
	}
	var b strings.Builder
	b.WriteString(bar + " " + theme.Accent.Render("you"))
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for _, row := range strings.Split(WrapToWidth(line, inner), "\n") {
			b.WriteString("\n" + bar + " " + theme.User.Render(row))
		}
	}
	return b.String()
}

// renderThinking renders a reasoning block: a dim header, the newest
// thinkingPreviewLines wrapped rows, and — when rows are hidden — a marker saying
// how many and that the block opens on click. The tail is what matters: a chain of
// thought grows forward, so the last rows are what the model is reasoning about
// right now, while the opening rows are the part it has already left behind. A
// click expands the block to thinkingExpandedLines rows (still anchored on the
// newest, since that is the row the reader is on) and a second click collapses it;
// the session file keeps the reasoning in full.
func renderThinking(theme Theme, blk transcriptBlock, width int) string {
	inner := width - 2
	if inner < 1 {
		inner = 1
	}
	rows := strings.Split(strings.TrimRight(WrapToWidth(blk.text, inner), "\n"), "\n")
	limit := thinkingPreviewLines
	if blk.expanded {
		limit = thinkingExpandedLines
	}
	truncated := len(rows) > limit
	header := "✻ thinking"
	if truncated || blk.expanded {
		header = "▸ thinking"
		if blk.expanded {
			header = "▾ thinking"
		}
	}
	shown, hidden := rows, 0
	if truncated {
		shown, hidden = rows[len(rows)-limit:], len(rows)-limit
	}

	var b strings.Builder
	b.WriteString(theme.System.Render(header))
	if hidden > 0 {
		note := fmt.Sprintf("  … %d earlier lines · click to expand", hidden)
		if blk.expanded {
			note = fmt.Sprintf("  … %d earlier lines not shown (see /export)", hidden)
		}
		b.WriteString("\n" + theme.System.Render(note))
	}
	for _, row := range shown {
		b.WriteString("\n" + theme.System.Render("  "+row))
	}
	return b.String()
}

// blockAtRow maps a transcript content row (0-based, counting from the top of the
// rendered log) to the block drawn there. It mirrors renderAll's separator rules, so
// a mouse click can be attributed to the card under the cursor.
func (t *transcript) blockAtRow(row int) (int, bool) {
	if row < 0 {
		return 0, false
	}
	for i := range t.blocks {
		start := t.blockStartRow(i)
		if row < start {
			return 0, false
		}
		if row < start+t.blockHeight(i) {
			return i, true
		}
	}
	return 0, false
}

// blockStartRow returns the content row where block i begins, counting the blank
// line(s) renderAll inserts between blocks. A block owns its own rows, and a row
// break between two blocks costs no extra row, so only the *blank* separators add
// to the offset.
func (t *transcript) blockStartRow(i int) int {
	at := 0
	for j := 0; j < i && j < len(t.blocks); j++ {
		at += t.blockHeight(j) + t.separatorRows(j+1)
	}
	return at
}

// blockHeight is the number of rows block i renders to. The count is cached with
// the block's render, so the row arithmetic behind clicks and the paint map does
// not re-scan every block of a long transcript.
func (t *transcript) blockHeight(i int) int {
	if i < 0 || i >= len(t.blocks) {
		return 0
	}
	if t.renderCurrent(i) {
		return t.blocks[i].renderHeight
	}
	return strings.Count(t.blockView(i), "\n") + 1
}

// separatorRows is how many blank rows precede block i in the rendered log.
// renderAll writes one newline before every block, which only terminates the
// previous block's last row, plus a second newline before a user turn, which is the
// one row that actually reads as blank. Counting the terminating newline as a blank
// row (the original off-by-one) shifted every block after the first one row late per
// separator, so clicks landed on a neighbouring card or nowhere at all.
func (t *transcript) separatorRows(i int) int {
	if i <= 0 || i >= len(t.blocks) {
		return 0
	}
	if t.blocks[i].role == roleUser {
		return 1
	}
	return 0
}

// revealBlock scrolls the viewport the least amount needed to show block i
// completely (or its first row when it is taller than the viewport), and pauses
// follow so the transcript does not snap back to the bottom on the next reflow.
func (t *transcript) revealBlock(i int) {
	if i < 0 || i >= len(t.blocks) {
		return
	}
	h := t.vp.Height()
	if h <= 0 {
		return
	}
	start := t.blockStartRow(i)
	height := t.blockHeight(i)
	offset := t.vp.YOffset()
	switch {
	case start < offset:
		t.vp.SetYOffset(start)
	case start+height > offset+h:
		target := start + height - h
		if target < start {
			target = start // taller than the viewport: show its top
		}
		t.vp.SetYOffset(target)
	default:
		return
	}
	t.follow = false
}
