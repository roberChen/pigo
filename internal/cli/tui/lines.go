// This file implements the transcript's scroll window: the "viewport" half of
// the log, without the per-frame cost the Bubble Tea viewport pays for it.
//
// bubbles/viewport re-measures its whole content on every SetContent — it walks
// every line, grapheme cluster by grapheme cluster, to find the longest one
// (ansi.StringWidth). The transcript pushes new content on *every streamed
// delta*, so that made one delta cost O(whole transcript bytes): with 400 blocks
// of history a single delta burned ~6ms, and 70% of it was that measurement. The
// transcript already lays its own rows out to the exact width (WrapToWidth, the
// scrollbar column, fitLine), so nothing here needs measuring: rows are taken as
// given, the window only tracks an offset and renders the visible slice.
//
// The API intentionally mirrors the subset of viewport.Model the transcript used
// (Height/SetHeight/Width/SetWidth/SetContent/GetContent/View/YOffset/
// SetYOffset/AtTop/AtBottom/TotalLineCount/GotoBottom/Update), so the scrolling
// semantics — key bindings, wheel delta, offset clamping — stay exactly what they
// were.

package tui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// lineKeys are the scroll bindings, matching bubbles/viewport's DefaultKeyMap so
// behavior does not change for a key the model forwards here.
var lineKeys = struct {
	PageDown, PageUp         key.Binding
	HalfPageDown, HalfPageUp key.Binding
	Down, Up                 key.Binding
}{
	PageDown:     key.NewBinding(key.WithKeys("pgdown", "space", "f")),
	PageUp:       key.NewBinding(key.WithKeys("pgup", "b")),
	HalfPageDown: key.NewBinding(key.WithKeys("d", "ctrl+d")),
	HalfPageUp:   key.NewBinding(key.WithKeys("u", "ctrl+u")),
	Down:         key.NewBinding(key.WithKeys("down", "j")),
	Up:           key.NewBinding(key.WithKeys("up", "k")),
}

// wheelRows is the number of rows one mouse-wheel notch scrolls, the same
// default the viewport used.
const wheelRows = 3

// lineWindow is a fixed-height window over a pre-laid-out list of display rows.
// rows are stored exactly as they should be drawn (already styled and wrapped);
// padding to width and truncation of an over-wide row happen in View, bounded by
// the window height rather than by the length of the log.
type lineWindow struct {
	rows []string

	width  int
	height int

	// yOffset is the index of the first visible row.
	yOffset int
}

// SetLines replaces the window's rows. Unlike viewport.SetContent it neither
// splits nor measures them: the transcript builds rows incrementally and they are
// already one display row each.
func (w *lineWindow) SetLines(rows []string) {
	w.rows = rows
	if max := w.maxYOffset(); w.yOffset > max {
		w.yOffset = max
	}
}

// SetContent splits content into rows. It exists for callers (and tests) that
// have a rendered blob rather than rows; the transcript itself uses SetLines.
func (w *lineWindow) SetContent(content string) {
	if content == "" {
		w.SetLines(nil)
		return
	}
	w.SetLines(strings.Split(content, "\n"))
}

// GetContent joins the rows back into one string, the inverse of SetContent.
func (w lineWindow) GetContent() string { return strings.Join(w.rows, "\n") }

// Height returns the number of visible rows.
func (w lineWindow) Height() int { return w.height }

// SetHeight sets the number of visible rows and keeps the offset in range.
func (w *lineWindow) SetHeight(h int) {
	w.height = h
	if max := w.maxYOffset(); w.yOffset > max {
		w.yOffset = max
	}
}

// Width returns the content width rows are padded/truncated to.
func (w lineWindow) Width() int { return w.width }

// SetWidth sets the content width. The next View pads (or truncates) to it.
func (w *lineWindow) SetWidth(width int) { w.width = width }

// TotalLineCount returns the number of rows in the log. With rows pre-wrapped
// this is simply their count — no soft-wrap arithmetic, so it is O(1), and the
// reflow overflow decision that runs on every delta is free of content cost.
func (w lineWindow) TotalLineCount() int { return len(w.rows) }

// YOffset returns the index of the first visible row.
func (w lineWindow) YOffset() int { return w.yOffset }

// AtTop reports whether the window shows the first row.
func (w lineWindow) AtTop() bool { return w.yOffset <= 0 }

// AtBottom reports whether the window is scrolled to the end of the log.
func (w lineWindow) AtBottom() bool { return w.yOffset >= w.maxYOffset() }

// SetYOffset scrolls to row n, clamped into [0, maxYOffset] so a growing or
// shrinking log can never leave the window past its own content.
func (w *lineWindow) SetYOffset(n int) {
	if n < 0 {
		n = 0
	}
	if max := w.maxYOffset(); n > max {
		n = max
	}
	w.yOffset = n
}

// GotoBottom scrolls to the last page.
func (w *lineWindow) GotoBottom() { w.yOffset = w.maxYOffset() }

// maxYOffset is the largest offset that still shows a full window (or 0 when the
// content fits).
func (w lineWindow) maxYOffset() int {
	if max := len(w.rows) - w.height; max > 0 {
		return max
	}
	return 0
}

// ScrollUp moves the window up by n rows.
func (w *lineWindow) ScrollUp(n int) {
	if n <= 0 {
		return
	}
	w.SetYOffset(w.yOffset - n)
}

// ScrollDown moves the window down by n rows.
func (w *lineWindow) ScrollDown(n int) {
	if n <= 0 {
		return
	}
	w.SetYOffset(w.yOffset + n)
}

// PageUp moves the window up by one window height.
func (w *lineWindow) PageUp() { w.ScrollUp(w.height) }

// PageDown moves the window down by one window height.
func (w *lineWindow) PageDown() { w.ScrollDown(w.height) }

// HalfPageUp moves the window up by half a window height.
func (w *lineWindow) HalfPageUp() { w.ScrollUp(w.height / 2) }

// HalfPageDown moves the window down by half a window height.
func (w *lineWindow) HalfPageDown() { w.ScrollDown(w.height / 2) }

// Update applies the scroll keys and the mouse wheel, mirroring
// bubbles/viewport's default handling: page keys move a page, half-page keys half
// a page, arrows one row, and each wheel notch wheelRows. Horizontal scrolling is
// deliberately not implemented — the transcript wraps every row to the width, so
// there is nothing to scroll sideways to.
func (w *lineWindow) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, lineKeys.PageDown):
			w.PageDown()
		case key.Matches(msg, lineKeys.PageUp):
			w.PageUp()
		case key.Matches(msg, lineKeys.HalfPageDown):
			w.HalfPageDown()
		case key.Matches(msg, lineKeys.HalfPageUp):
			w.HalfPageUp()
		case key.Matches(msg, lineKeys.Down):
			w.ScrollDown(1)
		case key.Matches(msg, lineKeys.Up):
			w.ScrollUp(1)
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			w.ScrollDown(wheelRows)
		case tea.MouseWheelUp:
			w.ScrollUp(wheelRows)
		}
	}
	return nil
}

// View renders the visible rows, each padded or truncated to the content width,
// and pads the bottom with blank rows when the content is shorter than the
// window, so the caller can paint it into a fixed-height region.
func (w lineWindow) View() string {
	if w.width <= 0 || w.height <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow((w.width + 1) * w.height)
	for i := 0; i < w.height; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		row := ""
		if at := w.yOffset + i; at >= 0 && at < len(w.rows) {
			row = w.rows[at]
		}
		b.WriteString(fitLine(row, w.width))
	}
	return b.String()
}

// String names the window for debugging: offset, height and row count.
func (w lineWindow) String() string {
	return "lines[" + strconv.Itoa(w.yOffset) + "+" + strconv.Itoa(w.height) +
		"/" + strconv.Itoa(len(w.rows)) + "]"
}
