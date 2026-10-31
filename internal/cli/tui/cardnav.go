// This file implements picking a tool card in the transcript and expanding or
// collapsing it.
//
// Until now only the most recent card could be toggled (Ctrl+O), so a long
// transcript was effectively read-only: a huge bash log early in the conversation
// stayed truncated forever. A card can now be selected — by clicking it, or by
// walking the transcript with Alt+↑/Alt+↓ — and the same Ctrl+O (or a second click)
// flips it. Selecting a card highlights its border, so it is obvious which card a
// key press will affect.
package tui

// cardIDAtScreenY returns the tool card drawn at screen row y, or "" when the row
// holds no card.
func (m *Model) cardIDAtScreenY(y int) string {
	if t := m.targetAtScreenY(y); t.role == roleTool {
		return t.id
	}
	return ""
}

// targetAtScreenY returns the tool card drawn at screen row y, or the zero
// rowTarget when the row holds nothing collapsible.
// Resolution goes through the map of the frame that was actually painted
// (transcript.paint): the pointer is over a drawn cell, and while a run streams the
// live layout keeps moving under it, so re-deriving the mapping from the current
// offset and block list would attribute the click to whichever block has scrolled
// into that row since. Rows outside the transcript (the input line, the status bar,
// an overlay) never belong to a block.
//
// The layout-derived fallback below only runs before the first frame is drawn,
// when there is no paint map to consult.
func (m *Model) targetAtScreenY(y int) rowTarget {
	if y < 0 || y >= m.transcript.viewportHeight() {
		return rowTarget{}
	}
	if p := m.transcript.paint; p != nil && y < len(p.targets) {
		return p.targets[y]
	}
	row := m.transcript.vp.YOffset() + y
	if row >= m.transcript.vp.TotalLineCount() {
		return rowTarget{} // padding below the last block, not content
	}
	idx, ok := m.transcript.blockAtRow(row)
	if !ok {
		return rowTarget{}
	}
	return m.transcript.rowTarget(idx)
}

// selectCard marks the card with the given id as selected, clearing any previous
// selection. It reports whether the card exists.
func (m *Model) selectCard(id string) bool {
	card, ok := m.toolCards[id]
	if !ok {
		return false
	}
	for cid, c := range m.toolCards {
		if cid != id {
			c.setSelected(false)
		}
	}
	card.setSelected(true)
	m.selectedCardID = id
	m.lastToolCard = card
	return true
}

// clearCardSelection drops the selection highlight.
func (m *Model) clearCardSelection() {
	for _, c := range m.toolCards {
		c.setSelected(false)
	}
	m.selectedCardID = ""
}

// toggleSelectedCard expands or collapses the selected card and scrolls it into
// view, so toggling a card picked with the keyboard shows the result even when the
// card grew past the bottom of the viewport.
func (m *Model) toggleSelectedCard() {
	card, ok := m.toolCards[m.selectedCardID]
	if !ok {
		return
	}
	card.toggleExpanded()
	m.transcript.reflow()
	m.revealCard(card.id)
}

// revealCard scrolls the transcript the least amount needed to show the card.
func (m *Model) revealCard(id string) {
	for i := range m.transcript.blocks {
		if blk := m.transcript.blocks[i]; blk.role == roleTool && blk.card != nil && blk.card.id == id {
			m.transcript.revealBlock(i)
			return
		}
	}
}

// indexTranscriptCards registers every card already in the transcript in the model's
// card index. Replayed cards (a resumed session) are created by seedTranscript and
// would otherwise be invisible to selection: the map only knew about cards started
// during this process.
func (m *Model) indexTranscriptCards() {
	for i := range m.transcript.blocks {
		blk := m.transcript.blocks[i]
		if blk.role == roleTool && blk.card != nil {
			m.toolCards[blk.card.id] = blk.card
		}
	}
}

// cardBlockIndexes lists the transcript blocks that hold tool cards, top to bottom.
func (m *Model) cardBlockIndexes() []int {
	var out []int
	for i, blk := range m.transcript.blocks {
		if blk.role == roleTool && blk.card != nil {
			out = append(out, i)
		}
	}
	return out
}

// moveCardSelection walks the card selection up (-1) or down (+1), wrapping at the
// ends, and scrolls the newly selected card into view. With no selection it starts
// from the last card walking up, or the first walking down.
func (m *Model) moveCardSelection(delta int) bool {
	indexes := m.cardBlockIndexes()
	if len(indexes) == 0 {
		return false
	}
	current := -1
	for i, idx := range indexes {
		if m.transcript.blocks[idx].card.id == m.selectedCardID {
			current = i
			break
		}
	}
	var next int
	switch {
	case current < 0 && delta < 0:
		next = len(indexes) - 1
	case current < 0:
		next = 0
	default:
		next = (current + delta + len(indexes)) % len(indexes)
	}
	card := m.transcript.blocks[indexes[next]].card
	m.selectCard(card.id)
	m.revealCard(card.id)
	return true
}

// handleCardKey routes the card navigation keys. It reports whether the key was
// consumed. Plain ↑/↓ stay with the composer (prompt history), so navigation uses
// Alt+↑/Alt+↓, which no editing action claims.
func (m *Model) handleCardKey(key string) bool {
	switch key {
	case "alt+up", "alt+shift+up":
		return m.moveCardSelection(-1)
	case "alt+down", "alt+shift+down":
		return m.moveCardSelection(1)
	}
	return false
}
