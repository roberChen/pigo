// This file implements the interactive model picker of the full-screen TUI: a
// filterable overlay over the same catalog "/models" lists, opened with a bare
// "/model" (or "/models"). Choosing a row performs exactly the switch that
// "/model <id>" performs — both go through prompts.ApplyModelSwitch — so the
// picker cannot drift from the command.
//
// It mirrors the /resume overlay (resume.go): a plain value stored on the Model,
// rendering nothing while inactive (so the shell layout is unchanged when
// closed), owning the keyboard while open, and reserving its rows in relayout.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/cli/prompts"
)

// maxModelPickerRows caps how many model rows the overlay shows at once; a
// longer catalog scrolls a window around the selection so the shell layout
// stays bounded no matter how many presets (or fetched models) exist.
const maxModelPickerRows = 10

// modelPickerHeaderRows is how many lines the overlay draws above its first
// selectable row: a title line and a filter line.
const modelPickerHeaderRows = 2

// modelPicker is the "/model" overlay state: the full catalog, the rows left
// after applying the typed filter, the highlighted row, and the current model
// id (tagged in the list so the user can see where they already are).
type modelPicker struct {
	theme    Theme
	active   bool
	all      []prompts.ModelChoice
	filtered []prompts.ModelChoice
	selected int
	filter   string
	current  string
}

// newModelPicker builds an inactive picker bound to the theme used for its
// rows.
func newModelPicker(theme Theme) modelPicker { return modelPicker{theme: theme} }

// open shows the picker over the given catalog, with the cursor on the current
// model when it is in the list (or the first row otherwise). It is a no-op that
// leaves the picker closed when there is nothing to choose from.
func (p *modelPicker) open(choices []prompts.ModelChoice, current string) {
	if len(choices) == 0 {
		return
	}
	p.all = choices
	p.current = current
	p.filter = ""
	p.selected = 0
	p.active = true
	p.refilter()
	for i, c := range p.filtered {
		if c.ID == current {
			p.selected = i
			break
		}
	}
}

// close hides the picker and drops its catalog.
func (p *modelPicker) close() {
	p.active = false
	p.all = nil
	p.filtered = nil
	p.selected = 0
	p.filter = ""
	p.current = ""
}

// refilter recomputes the visible rows from the typed filter. A row matches on
// its model id, provider name or display label, case-insensitively, so
// "claude", "anthropic" and "fable" all narrow the same list. The selection is
// clamped into range.
func (p *modelPicker) refilter() {
	if p.filter == "" {
		p.filtered = append(p.filtered[:0], p.all...)
	} else {
		q := strings.ToLower(p.filter)
		p.filtered = p.filtered[:0]
		for _, c := range p.all {
			if strings.Contains(strings.ToLower(c.ID), q) ||
				strings.Contains(strings.ToLower(c.Provider), q) ||
				strings.Contains(strings.ToLower(c.Label), q) {
				p.filtered = append(p.filtered, c)
			}
		}
	}
	if p.selected >= len(p.filtered) {
		p.selected = len(p.filtered) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}
}

// moveUp / moveDown cycle the highlighted row, wrapping at the ends so arrow
// navigation is continuous.
func (p *modelPicker) moveUp() { p.move(-1) }

func (p *modelPicker) moveDown() { p.move(1) }

// move shifts the highlight by delta rows, wrapping around the visible list so a
// single ↑ at the top lands on the last row and ↓ at the bottom returns to the
// first. It is the one place the wrap-around rule lives; the paging and
// home/end helpers below are deliberately NOT wrapping, since a page jump that
// teleported to the other end would be disorienting.
func (p *modelPicker) move(delta int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	p.selected = ((p.selected+delta)%n + n) % n
}

// pageUp / pageDown move the highlight by one screenful (the number of rows the
// window shows), stopping at the first / last row instead of wrapping: with a
// 95-model catalog a single Ctrl+U should travel a screen, not a full loop.
func (p *modelPicker) pageUp() { p.page(-1) }

func (p *modelPicker) pageDown() { p.page(1) }

func (p *modelPicker) page(direction int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	step := maxModelPickerRows
	if step > n {
		step = n
	}
	p.selected += direction * step
	if p.selected < 0 {
		p.selected = 0
	}
	if p.selected >= n {
		p.selected = n - 1
	}
}

// moveHome / moveEnd jump to the first / last visible row, which pairs with the
// wrap-around arrows when the catalog is long: Home then ↑ reaches the end in
// two keystrokes without scrolling through 95 presets.
func (p *modelPicker) moveHome() {
	if len(p.filtered) > 0 {
		p.selected = 0
	}
}

func (p *modelPicker) moveEnd() {
	if n := len(p.filtered); n > 0 {
		p.selected = n - 1
	}
}

// typeFilter appends typed text to the filter and re-filters, resetting the
// cursor to the first match so the next Enter picks a visible row.
func (p *modelPicker) typeFilter(s string) {
	p.filter += s
	p.selected = 0
	p.refilter()
}

// backspaceFilter drops the last rune of the filter (not the last byte, so a CJK
// filter never ends in a broken rune) and re-filters.
func (p *modelPicker) backspaceFilter() {
	runes := []rune(p.filter)
	if len(runes) == 0 {
		return
	}
	p.filter = string(runes[:len(runes)-1])
	p.selected = 0
	p.refilter()
}

// currentChoice returns the highlighted choice, or ok=false when the picker is
// inactive or the filter matched nothing.
func (p modelPicker) currentChoice() (prompts.ModelChoice, bool) {
	if !p.active || p.selected < 0 || p.selected >= len(p.filtered) {
		return prompts.ModelChoice{}, false
	}
	return p.filtered[p.selected], true
}

// rows reports how many terminal rows the overlay occupies, so the model can
// reserve that space above the input line during relayout. An empty result set
// still shows the title, filter and "no match" line.
func (p modelPicker) rows() int {
	if !p.active {
		return 0
	}
	n := len(p.filtered)
	if n > maxModelPickerRows {
		n = maxModelPickerRows
	}
	if n == 0 {
		n = 1 // the "no matching model" line
	}
	return modelPickerHeaderRows + n
}

// view renders the overlay: a title line, a filter line, then one row per
// visible choice with the highlighted one marked by a "›" caret. Each row is
// "label  ·  provider" truncated to the width so it never wraps. It returns ""
// when inactive so the model omits the overlay entirely.
func (p modelPicker) view(width int) string {
	if !p.active {
		return ""
	}
	rowWidth := width - 2 // reserve the caret / indent column
	if rowWidth < 1 {
		rowWidth = width
	}
	var b strings.Builder
	b.WriteString(TruncateToWidth(p.theme.Accent.Render("select a model  (↑/↓/PgUp/PgDn/Home/End move · type to filter · Enter switch · Esc cancel)"), width))
	b.WriteByte('\n')
	filter := p.filter
	if filter == "" {
		filter = "type to filter…"
	} else {
		filter = "/" + filter
	}
	// The position readout ("12/95") tells the user how far the ↑/↓ walk has
	// travelled in a catalog that cannot fit on screen, which is otherwise
	// impossible to tell once the window starts scrolling.
	if n := len(p.filtered); n > 0 {
		filter += fmt.Sprintf("  (%d/%d)", p.selected+1, n)
	}
	b.WriteString(p.theme.System.Render(TruncateToWidth("  filter: "+filter, width)))
	if len(p.filtered) == 0 {
		b.WriteByte('\n')
		b.WriteString(p.theme.System.Render(TruncateToWidth("  no matching model", width)))
		return b.String()
	}
	start, end := p.window()
	for i := start; i < end; i++ {
		c := p.filtered[i]
		b.WriteByte('\n')
		label := c.Label
		if label == "" {
			label = c.ID
		}
		if c.Provider != "" {
			label += "  ·  " + c.Provider
		}
		if p.current != "" && c.ID == p.current {
			label += " (current)"
		}
		line := TruncateToWidth(label, rowWidth)
		if i == p.selected {
			b.WriteString(p.theme.Accent.Render("› " + line))
		} else {
			b.WriteString(p.theme.System.Render("  " + line))
		}
	}
	return b.String()
}

// window returns the [start,end) slice of visible choices to display, scrolled
// to keep the selection visible when the list is taller than maxModelPickerRows.
func (p modelPicker) window() (int, int) {
	n := len(p.filtered)
	if n <= maxModelPickerRows {
		return 0, n
	}
	start := p.selected - maxModelPickerRows + 1
	if start < 0 {
		start = 0
	}
	if start > n-maxModelPickerRows {
		start = n - maxModelPickerRows
	}
	return start, start + maxModelPickerRows
}

// openModelPicker replaces the composer buffer with the interactive chooser:
// the invocation is echoed, the catalog is assembled from the live config, and
// the overlay opens on the current model. The REPL keeps its text listing; the
// TUI adds the picker on top of the same registry command.
func (m Model) openModelPicker(line string) (tea.Model, tea.Cmd) {
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	m.picker.close()
	if m.live == nil {
		m.transcript.addSystem("(model picker unavailable: no active run configuration — type /model <id> instead)")
		m.relayout()
		return m, nil
	}
	choices := prompts.ModelChoices(m.live)
	if len(choices) == 0 {
		m.transcript.addSystem("(no preset models to choose from — type /model <id> to switch by id)")
		m.relayout()
		return m, nil
	}
	m.modelPicker.open(choices, m.live.Model)
	m.transcript.addSystem(fmt.Sprintf("choose a model from %d presets — type to filter, Enter to switch, Esc to cancel", len(choices)))
	m.relayout()
	return m, nil
}

// submitModelPick switches to the highlighted model and closes the overlay. The
// switch goes through prompts.ApplyModelSwitch (the /model action), so both
// surfaces resolve providers identically — including the fetched-catalog and
// bare-provider-name special cases. The status bar is refreshed immediately;
// the new model takes effect on the next turn because the run loop reads live.
func (m Model) submitModelPick() (tea.Model, tea.Cmd) {
	choice, ok := m.modelPicker.currentChoice()
	m.modelPicker.close()
	m.relayout()
	if !ok || m.live == nil {
		return m, nil
	}
	m.transcript.addSystem(prompts.ApplyModelSwitch(m.live, choice.ID))
	m.statusBar.SetModel(m.live.Model)
	m.statusBar.SetThinking(string(m.live.ThinkingLevel))
	m.syncContextWindow()
	// Round-trip the prompt so the TUI does not fire image/clipboard reads for a
	// selection; there is nothing to run.
	return m, nil
}
