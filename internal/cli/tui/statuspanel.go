// This file implements the status bar's drawers: clicking the model, thinking,
// skills, mcp, files or context block on the bar opens a list that grows upward
// from the bar, and clicking a row acts on it. It is the mouse counterpart of the
// keyboard overlays (/model, /think, /skill, /mcp): the model and thinking drawers
// switch through the very same code paths those commands use, so a click cannot
// drift from the command. The context drawer is the exception: it explains the
// number the bar shows (budget, reserve, threshold, where the count came from)
// rather than offering a list to pick from.
//
// A drawer holds no copy of the state it shows. Rows are rebuilt from live state
// every time it is drawn, so a /model switch, an MCP reconnect or a new git probe
// is reflected without an invalidation step that could be missed. The only state
// here is which section is open.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/prompts"
	"github.com/roberChen/pigo/internal/cli/ui"
	"github.com/roberChen/pigo/internal/compaction"
)

// maxStatusPanelRows caps how many data rows a drawer shows at once. A longer
// list (the model catalog can hold ~95 presets) scrolls a window around the row in
// effect, so an open drawer never eats the transcript.
const maxStatusPanelRows = 10

// statusPanel is the drawer attached to the status bar: which section is open, or
// sectionNone for none, plus where its window has been scrolled to.
type statusPanel struct {
	section statusSection

	// scroll is the first row drawn, and manual says the user put it there.
	// Until they scroll, the window follows the row in effect (the model in use,
	// the connected server); once they scroll, it stays where they left it so a
	// live list cannot yank the view back while it is being read.
	scroll int
	manual bool
}

// open reports whether a drawer is up.
func (p statusPanel) open() bool { return p.section != sectionNone }

// close hides the drawer and forgets its scroll position.
func (p *statusPanel) close() {
	p.section = sectionNone
	p.resetScroll()
}

// resetScroll hands the window back to the automatic, current-row-centered mode.
func (p *statusPanel) resetScroll() {
	p.scroll = 0
	p.manual = false
}

// toggle opens section's drawer, or closes it when that drawer is already the one
// up, so a second click on the same block dismisses it. Opening a different
// section replaces the current drawer: there is never more than one, and the new
// one starts centered on its value in effect.
func (p *statusPanel) toggle(section statusSection) {
	if p.section == section {
		p.close()
		return
	}
	p.section = section
	p.resetScroll()
}

// statusRowAction is what clicking a drawer row does. The row carries its action
// so a click never has to be re-interpreted from the row's text.
type statusRowAction int

const (
	rowNone        statusRowAction = iota // informational row
	rowSwitchModel                        // value is a model id
	rowSetThinking                        // value is a thinking level
	rowToggleMCP                          // value is a server name
)

// statusPanelRow is one line of a drawer. detail is the secondary text on the
// right, current marks the value in effect (the current model, the thinking level
// in use, a connected server, a skill selected for the next message).
//
// warn paints the detail in the warning color: a row whose number has crossed a
// line the user cares about (the context is over its compaction threshold) says so
// in color as well as in words.
type statusPanelRow struct {
	label   string
	detail  string
	current bool
	warn    bool
	action  statusRowAction
	value   string
}

// thinkingLevels is the ordered set /think accepts, with a one-line gloss for the
// drawer. It mirrors agentcore's enum; this drawer is the only place that has to
// enumerate all of it, so the display order lives here.
var thinkingLevels = []struct {
	level agentcore.ThinkingLevel
	note  string
}{
	{agentcore.ThinkingOff, "no reasoning"},
	{agentcore.ThinkingMinimal, "bare minimum"},
	{agentcore.ThinkingLow, "light"},
	{agentcore.ThinkingMedium, "balanced"},
	{agentcore.ThinkingHigh, "deep"},
	{agentcore.ThinkingXHigh, "deeper"},
	{agentcore.ThinkingMax, "exhaustive"},
}

// currentThinking is the reasoning-effort level in use, or "" when no live config
// is bound (a session-less model built by a test).
func (m Model) currentThinking() string {
	if m.live == nil {
		return ""
	}
	return string(m.live.ThinkingLevel)
}

// statusPanelRows builds the drawer's data rows from live state. An empty result
// is meaningful: the drawer then says there is nothing to show, which is the
// honest answer for a model catalog that could not be assembled or a working tree
// with no changed files.
func (m Model) statusPanelRows() []statusPanelRow {
	switch m.statusPanel.section {
	case sectionModel:
		return m.modelDrawerRows()
	case sectionThinking:
		return m.thinkingDrawerRows()
	case sectionSkills:
		return m.skillDrawerRows()
	case sectionMCP:
		return m.mcpDrawerRows()
	case sectionFiles:
		return m.fileDrawerRows()
	case sectionContext:
		return m.contextDrawerRows()
	default:
		return nil
	}
}

// modelDrawerRows lists the switchable models, the same catalog "/models" prints
// and the /model overlay shows.
func (m Model) modelDrawerRows() []statusPanelRow {
	if m.live == nil {
		return nil
	}
	choices := prompts.ModelChoices(m.live)
	rows := make([]statusPanelRow, 0, len(choices))
	for _, c := range choices {
		// Same "label  ·  provider" shape the /model overlay draws; the marker
		// column replaces that overlay's "(current)" tag.
		label := c.Label
		if label == "" {
			label = c.ID
		}
		if c.Provider != "" {
			label += "  ·  " + c.Provider
		}
		rows = append(rows, statusPanelRow{
			label:   label,
			current: c.ID == m.live.Model,
			action:  rowSwitchModel,
			value:   c.ID,
		})
	}
	return rows
}

// thinkingDrawerRows lists the reasoning-effort levels in ascending order, marking
// the one in effect.
func (m Model) thinkingDrawerRows() []statusPanelRow {
	cur := m.currentThinking()
	rows := make([]statusPanelRow, 0, len(thinkingLevels))
	for _, lvl := range thinkingLevels {
		rows = append(rows, statusPanelRow{
			label:   string(lvl.level),
			detail:  lvl.note,
			current: cur == string(lvl.level),
			action:  rowSetThinking,
			value:   string(lvl.level),
		})
	}
	return rows
}

// skillDrawerRows lists the loaded skills and marks the ones selected for the
// next message. Selection itself stays with /skill (it also collects arguments
// through a form), so these rows are informational: the drawer answers
// "what is active, and what is queued".
func (m Model) skillDrawerRows() []statusPanelRow {
	skills := m.availableSkills()
	chosen := make(map[string]bool, len(m.selectedSkills))
	for _, sel := range m.selectedSkills {
		chosen[sel.Name] = true
	}
	rows := make([]statusPanelRow, 0, len(skills))
	for _, s := range skills {
		if s == nil {
			continue
		}
		rows = append(rows, statusPanelRow{
			label:   s.Frontmatter.Name,
			detail:  firstLineOf(s.Frontmatter.Description),
			current: chosen[s.Frontmatter.Name],
			action:  rowNone,
		})
	}
	return rows
}

// mcpDrawerRows lists every configured MCP server with its connection state and
// tool count; a server that failed to connect carries the manager's last error so
// the drawer explains why it is off.
func (m Model) mcpDrawerRows() []statusPanelRow {
	statuses := m.mcpStatuses()
	rows := make([]statusPanelRow, 0, len(statuses))
	for _, st := range statuses {
		detail := "off"
		if st.Active {
			detail = fmt.Sprintf("on · %d tools", st.Tools)
		}
		if st.Error != "" {
			detail += " · " + firstLineOf(st.Error)
		}
		rows = append(rows, statusPanelRow{
			label:   st.Name,
			detail:  detail,
			current: st.Active,
			action:  rowToggleMCP,
			value:   st.Name,
		})
	}
	return rows
}

// fileDrawerRows lists the changed paths with their line deltas, straight from the
// latest git probe.
func (m Model) fileDrawerRows() []statusPanelRow {
	files := m.statusBar.git.files
	rows := make([]statusPanelRow, 0, len(files))
	for _, f := range files {
		rows = append(rows, statusPanelRow{
			label:  f.path,
			detail: fileDeltaText(f),
			action: rowNone,
		})
	}
	return rows
}

// fileDeltaText renders one file's line delta: "+42 −7", "binary" for a file git
// refuses to count, and "new" for an untracked one (which has no delta at all).
// A staged-but-unmodified file shows its counts as "0 0" only when git reported
// them, so the absence of counts is never dressed up as a zero delta.
func fileDeltaText(f gitFile) string {
	switch {
	case f.binary:
		return "binary"
	case f.code == "??":
		return "new"
	case f.counts:
		return fmt.Sprintf("%s%d %s%d", glyphAdded, f.added, glyphRemoved, f.removed)
	default:
		return ""
	}
}

// contextDrawerRows explains the context readout on the bar. The bar has room for
// one number and its percentage; everything that makes those numbers actionable —
// the budget they are measured against, the reserve auto-compaction keeps free,
// how much room is left before it fires, where the count came from and how many
// checkpoints the history already carries — only fits in a drawer.
//
// The numbers are the loop's, not a second opinion: the window is the budget
// buildConfig hands the loop (runtime compares against window-reserve in
// compaction.ShouldCompact), and the count is the same estimate the bar shows.
func (m Model) contextDrawerRows() []statusPanelRow {
	v := m.ctxMeter.view()
	window := m.contextWindow()
	known := m.contextWindowKnown()

	// used: the headline, and the row the accent marks. It is what the bar's
	// block shows, spelled out.
	used := "not measured yet · the bar shows --"
	if v.known {
		used = fmt.Sprintf("%s tokens · %d%% of the window", humanizeInt(v.tokens), m.contextPercent())
	}
	rows := []statusPanelRow{{label: "used", detail: used, current: true}}

	// window: the denominator. contextWindow() prefers the meter's seeded copy and
	// falls back to the live config, so this row is right both before the first
	// seed and after a model switch moved the budget.
	switch {
	case window <= 0:
		rows = append(rows, statusPanelRow{label: "window", detail: "unknown · auto-compaction is off"})
	case !known:
		rows = append(rows, statusPanelRow{
			label:  "window",
			detail: fmt.Sprintf("%s tokens · fallback (this model reports no window)", humanizeInt(window)),
		})
	default:
		rows = append(rows, statusPanelRow{
			label:  "window",
			detail: fmt.Sprintf("%s tokens · the budget auto-compaction measures against", humanizeInt(window)),
		})
	}

	// remaining and threshold: where the line is and how far away it is. Both are
	// meaningless without a window (ShouldCompact returns false then, so no amount
	// of context would ever trigger it), so they are replaced by one row saying so
	// rather than by figures describing compaction that cannot happen.
	if !known || window <= 0 {
		rows = append(rows, statusPanelRow{
			label:  "remaining",
			detail: "auto-compaction is off · the loop has no context window for this model",
			warn:   true,
		})
		return m.appendContextProvenanceRows(rows)
	}

	reserve := compaction.DefaultCompactionSettings.ReserveTokens
	threshold := window - reserve
	switch {
	case !v.known:
		rows = append(rows, statusPanelRow{
			label:  "remaining",
			detail: fmt.Sprintf("up to %s tokens before auto-compaction", humanizeInt(threshold)),
		})
	case v.tokens > threshold:
		rows = append(rows, statusPanelRow{
			label:  "remaining",
			detail: fmt.Sprintf("over the threshold by %s tokens", humanizeInt(v.tokens-threshold)),
			warn:   true,
		})
	case v.tokens == threshold:
		rows = append(rows, statusPanelRow{label: "remaining", detail: "at the threshold · the next token compacts", warn: true})
	default:
		rows = append(rows, statusPanelRow{
			label:  "remaining",
			detail: fmt.Sprintf("%s tokens until auto-compaction", humanizeInt(threshold-v.tokens)),
		})
	}
	rows = append(rows, statusPanelRow{
		label:  "threshold",
		detail: fmt.Sprintf("%s tokens · the window minus a %s reserve", humanizeInt(threshold), humanizeInt(reserve)),
	})
	return m.appendContextProvenanceRows(rows)
}

// appendContextProvenanceRows adds the two rows that say where the count came from
// and what the history already lost to compaction. They are appended (rather than
// written inline) so both exits of contextDrawerRows end with the same tail.
func (m Model) appendContextProvenanceRows(rows []statusPanelRow) []statusPanelRow {
	rows = append(rows, statusPanelRow{label: "measured", detail: m.contextMeasuredText()})
	detail := "none yet · the history is intact"
	if n := m.ctxMeter.compactions; n > 0 {
		detail = m.contextCompactionText(n)
	}
	return append(rows, statusPanelRow{label: "compactions", detail: detail})
}

// contextMeasuredText describes the meter's breakdown, so the count is never a
// bare number: provider usage, what was appended since, what is still streaming.
func (m Model) contextMeasuredText() string {
	usage, appended, streaming := m.ctxMeter.parts()
	if !m.ctxMeter.measured {
		return "nothing measured yet · counted as text arrives"
	}
	var parts []string
	if usage > 0 {
		parts = append(parts, fmt.Sprintf("%s from provider usage", humanizeInt(usage)))
	}
	if appended > 0 {
		parts = append(parts, fmt.Sprintf("%s appended since", humanizeInt(appended)))
	}
	if streaming > 0 {
		parts = append(parts, fmt.Sprintf("%s streaming", humanizeInt(streaming)))
	}
	if len(parts) == 0 {
		return "empty context · nothing to send yet"
	}
	return strings.Join(parts, " · ")
}

// contextCompactionText renders the compaction row: how many checkpoints the
// conversation carries, the size the last one took it from and to (when the end
// figure is known — a checkpoint replayed from disk only knows the start), and why
// it ran.
func (m Model) contextCompactionText(n int) string {
	last := m.ctxMeter.last
	text := fmt.Sprintf("%d", n)
	switch {
	case m.ctxMeter.hasLast && last.before > 0 && last.after > 0:
		text = fmt.Sprintf("%d · last %s → %s tokens", n, humanizeInt(last.before), humanizeInt(last.after))
	case m.ctxMeter.hasLast && last.before > 0:
		text = fmt.Sprintf("%d · last folded %s tokens away", n, humanizeInt(last.before))
	}
	if m.ctxMeter.hasLast && last.reason != "" {
		text += " (" + last.reason + ")"
	}
	return text
}

// contextSummary is the drawer title's headline: the same readout the bar shows,
// so the drawer visibly explains its own block.
func (m Model) contextSummary() string {
	v := m.ctxMeter.view()
	window := m.contextWindow()
	switch {
	case window <= 0:
		return "window unknown"
	case !v.known:
		return fmt.Sprintf("not measured yet · %s token window", humanizeInt(window))
	default:
		return fmt.Sprintf("%s of %s tokens · %d%%", humanizeInt(v.tokens), humanizeInt(window), m.contextPercent())
	}
}

// contextWindow is the context-token budget the drawer explains: the meter's copy
// once it has been seeded, otherwise the live config's value (the same one
// buildConfig hands the loop). 0 means neither is known.
func (m Model) contextWindow() int {
	if w := m.ctxMeter.window; w > 0 {
		return w
	}
	if m.live != nil {
		return m.live.ContextWindow
	}
	return 0
}

// contextPercent is the utilization to display, computed from the same window the
// drawer explains so its headline and the bar's block cannot disagree.
func (m Model) contextPercent() int {
	window := m.contextWindow()
	v := m.ctxMeter.view()
	if window <= 0 || !v.known {
		return 0
	}
	v.window = window
	v.util = float64(v.tokens) / float64(window)
	return ctxPercent(v)
}

// contextWindowKnown reports whether the live config carries a real context
// window. buildConfig hands the loop this same value, so a false answer means the
// loop has no budget at all and auto-compaction cannot fire — the drawer must not
// present the meter's fallback budget as if the loop used it.
func (m Model) contextWindowKnown() bool {
	return m.live != nil && m.live.ContextWindow > 0
}

// statusPanelCapacity is how many data rows the drawer may draw: at most
// maxStatusPanelRows, and never more than fits above the bar (the header, the bar
// and one more row always stay).
func (m Model) statusPanelCapacity() int {
	capacity := maxStatusPanelRows
	if m.height > 0 {
		if room := m.height - 3; capacity > room {
			capacity = room
		}
	}
	if capacity < 1 {
		capacity = 1
	}
	return capacity
}

// centeredWindow returns the [start,end) slice of rows to draw when the drawer has
// not been scrolled by hand. A list that fits is drawn whole; a longer one scrolls
// a window centered on the current row, so the value in effect is always visible
// even in a 95-model catalog.
func centeredWindow(total, current, capacity int) (start, end int) {
	if total <= 0 || capacity <= 0 {
		return 0, 0
	}
	if total <= capacity {
		return 0, total
	}
	if current < 0 {
		current = 0
	}
	start = current - capacity/2
	if start < 0 {
		start = 0
	}
	if start > total-capacity {
		start = total - capacity
	}
	return start, start + capacity
}

// statusPanelWindow returns the [start,end) slice of rows to draw for the open
// drawer. It is the manual scroll position once the user has scrolled, and the
// current-row-centered window until then. Both paths clamp to the list, so a
// drawer whose rows shrink under it (a reconnect empties the mcp list, the git
// probe returns fewer files) can never draw past its end.
func (m Model) statusPanelWindow(rows []statusPanelRow) (start, end int) {
	capacity := m.statusPanelCapacity()
	if !m.statusPanel.manual {
		return centeredWindow(len(rows), statusPanelCurrentIndex(rows), capacity)
	}
	if capacity <= 0 || len(rows) == 0 {
		return 0, 0
	}
	start = m.statusPanel.scroll
	if maxStart := len(rows) - capacity; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	end = start + capacity
	if end > len(rows) {
		end = len(rows)
	}
	return start, end
}

// scrollStatusPanel moves the drawer window by delta rows, clamping to the list.
// Scrolling switches the drawer to manual mode, so the window stops following the
// current row; a list that already fits ignores it entirely.
func (m *Model) scrollStatusPanel(delta int) {
	rows := m.statusPanelRows()
	capacity := m.statusPanelCapacity()
	if len(rows) <= capacity {
		return
	}
	start, _ := m.statusPanelWindow(rows)
	start += delta
	if maxStart := len(rows) - capacity; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	m.statusPanel.scroll = start
	m.statusPanel.manual = true
}

// scrollStatusPanelTo jumps the drawer window to the first or last row.
func (m *Model) scrollStatusPanelTo(last bool) {
	rows := m.statusPanelRows()
	capacity := m.statusPanelCapacity()
	if len(rows) <= capacity {
		return
	}
	if last {
		m.statusPanel.scroll = len(rows) - capacity
	} else {
		m.statusPanel.scroll = 0
	}
	m.statusPanel.manual = true
}

// statusPanelCurrentIndex returns the row marked current, or -1 when no row is.
func statusPanelCurrentIndex(rows []statusPanelRow) int {
	for i, r := range rows {
		if r.current {
			return i
		}
	}
	return -1
}

// statusPanelDataCount is how many data lines the drawer draws: the window length,
// or 1 for the "nothing to show" line of an empty list.
func (m Model) statusPanelDataCount() int {
	if !m.statusPanel.open() {
		return 0
	}
	rows := m.statusPanelRows()
	if len(rows) == 0 {
		return 1
	}
	start, end := m.statusPanelWindow(rows)
	return end - start
}

// statusPanelHeight is how many terminal rows the open drawer occupies, header
// included, or 0 when it is closed. relayout reserves exactly this many rows and
// view fills exactly this many, both from the same count, so the shell can never
// grow an extra row that would push the bar off screen.
func (m Model) statusPanelHeight() int {
	if !m.statusPanel.open() {
		return 0
	}
	return 1 + m.statusPanelDataCount()
}

// statusPanelHeader is the drawer's title line: what is listed, what a click does,
// and — when the list is windowed — which slice of it is on screen. rows is the
// list being drawn, so the header cannot be built from a different one.
func (m Model) statusPanelHeader(width int, rows []statusPanelRow) string {
	var title string
	switch m.statusPanel.section {
	case sectionModel:
		title = "model · click a row to switch · Esc closes"
	case sectionThinking:
		title = "thinking level · click a row to switch · Esc closes"
	case sectionSkills:
		title = fmt.Sprintf("skills · %d loaded · selected ones ride the next message · Esc closes", len(rows))
	case sectionMCP:
		title = "mcp servers · click a row to connect or disconnect · Esc closes"
	case sectionFiles:
		title = "changed files · " + m.filesSummary()
	case sectionContext:
		title = "context · " + m.contextSummary() + " · Esc closes"
	}
	if start, end := m.statusPanelWindow(rows); len(rows) > end-start {
		title += fmt.Sprintf("  (%d-%d/%d)", start+1, end, len(rows))
	}
	return m.drawerSpansLine(width,
		drawerSpan{text: " ", style: drawerBase()},
		drawerSpan{text: title, style: drawerBase().
			Foreground(lipgloss.Color(drawerAccent(m.statusPanel.section))).
			Bold(true)},
	)
}

// filesSummary is the drawer title's headline: the file count and line totals that
// the bar's files block shows, so the drawer explains its own block.
func (m Model) filesSummary() string {
	files := m.statusBar.git.files
	added, removed, counted := m.statusBar.git.lineTotals()
	if !counted {
		return fmt.Sprintf("%d files", len(files))
	}
	return fmt.Sprintf("%d files · %s%d %s%d", len(files), glyphAdded, added, glyphRemoved, removed)
}

// drawerSpan is one styled piece of a drawer line. Splitting a row into spans is
// what lets the label and its detail carry different weight on the same line.
type drawerSpan struct {
	text  string
	style lipgloss.Style
}

// drawerSpansLine renders spans on the bar background, clipping the line to width
// (a span that does not fit is truncated, later spans are dropped) and padding the
// remainder. Width is measured with ui.Width, so CJK labels and file paths pad
// and clip in display columns rather than runes.
func (m Model) drawerSpansLine(width int, spans ...drawerSpan) string {
	line, used := m.renderDrawerSpans(width, spans)
	return line + barPad(width-used)
}

// renderDrawerSpans paints spans into at most width columns and reports how many
// columns were used. It is the width-budgeting half of drawerSpansLine, for rows
// that reserve columns on the right for something drawn separately.
func (m Model) renderDrawerSpans(width int, spans []drawerSpan) (string, int) {
	if width <= 0 {
		return "", 0
	}
	var b strings.Builder
	used := 0
	for _, sp := range spans {
		if used >= width || sp.text == "" {
			continue
		}
		text := TruncateToWidth(sp.text, width-used)
		b.WriteString(sp.style.Render(text))
		used += ui.Width(text)
	}
	return b.String(), used
}

// drawerBase returns the style every drawer span starts from: the bar background,
// so the drawer and the bar read as one surface.
func drawerBase() lipgloss.Style {
	return lipgloss.NewStyle().Background(lipgloss.Color(sbBarBg))
}

// drawerAccent is the color that ties a drawer to the block that opened it: the
// header and the row in effect use it, so an open list is visibly "the model list"
// or "the mcp list" and not just some rows above the bar.
func drawerAccent(section statusSection) string {
	switch section {
	case sectionModel:
		return sbModelBg
	case sectionThinking:
		return sbThinkBg
	case sectionSkills:
		return sbSkillBg
	case sectionMCP:
		return sbMcpBg
	case sectionFiles:
		return sbFilesBg
	case sectionContext:
		return sbCtxBg
	default:
		return colorAccent
	}
}

// statusPanelRowLine renders one data row in two columns: the label, then its
// detail aligned at labelWidth. The row in effect is marked ●, painted in the
// drawer's accent and bold, so the drawer has exactly one visual focus; the rest
// are readable rather than dim, because a list exists to be read. An action row
// keeps a muted ↵ in the last-but-one column so a click target is distinguishable
// from an informational row at a glance; the hint's columns are reserved BEFORE
// the text is fitted, so a long model id can never clip it away.
func (m Model) statusPanelRowLine(width, labelWidth int, r statusPanelRow) string {
	accent := drawerAccent(m.statusPanel.section)
	mark := " "
	if r.current {
		mark = "●"
	}
	labelStyle := drawerBase().Foreground(lipgloss.Color(colorAssist))
	detailStyle := drawerBase().Foreground(lipgloss.Color(colorGray))
	markStyle := drawerBase().Foreground(lipgloss.Color(colorGray))
	if r.warn {
		detailStyle = drawerBase().Foreground(lipgloss.Color(colorWarn))
	}
	if r.current {
		labelStyle = drawerBase().Foreground(lipgloss.Color(accent)).Bold(true)
		detailStyle = drawerBase().Foreground(lipgloss.Color(accent))
		markStyle = labelStyle
	}

	budget := width
	if r.action != rowNone {
		budget = width - drawerActionWidth
	}
	spans := []drawerSpan{
		{text: " ", style: drawerBase()},
		{text: mark, style: markStyle},
		{text: " ", style: drawerBase()},
		{text: padToWidth(r.label, labelWidth), style: labelStyle},
	}
	if r.detail != "" {
		spans = append(spans,
			drawerSpan{text: "  ", style: drawerBase()},
			drawerSpan{text: r.detail, style: detailStyle},
		)
	}
	line, used := m.renderDrawerSpans(budget, spans)
	if r.action == rowNone {
		return line + barPad(width-used)
	}
	return line + barPad(budget-used) +
		drawerBase().Foreground(lipgloss.Color(colorGray)).Render(glyphAction) + " " +
		barPad(width-budget-drawerActionWidth)
}

// statusPanelLabelWidth is the label column width shared by the rows on screen:
// the widest label in the drawn window, capped so a long model id cannot push the
// detail column off the row. Rows outside the window do not affect it, so
// scrolling a catalog with one very long id does not jitter the other rows.
func (m Model) statusPanelLabelWidth(rows []statusPanelRow, width int) int {
	start, end := m.statusPanelWindow(rows)
	labelWidth := 0
	for _, r := range rows[start:end] {
		if w := ui.Width(r.label); w > labelWidth {
			labelWidth = w
		}
	}
	// Leave the detail column at least half the row: the label says which row this
	// is, the detail says what it is, and neither may starve the other.
	limit := width - drawerRowGutter - drawerActionWidth
	if limit > width/2 {
		limit = width / 2
	}
	if limit < 12 {
		limit = min(12, width)
	}
	if labelWidth > limit {
		labelWidth = limit
	}
	return max(labelWidth, 0)
}

// Drawer row geometry. The gutter is the space + ● marker + space that starts
// every row; the action slot is the ↵ plus one trailing column an action row
// reserves on the right.
const (
	drawerRowGutter   = 3
	drawerActionWidth = 2
)

// padToWidth right-pads s with spaces to width display columns. A string already at
// or past width is returned unchanged: the line renderer clips it.
func padToWidth(s string, width int) string {
	if w := ui.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// statusPanelView renders the open drawer, or "" when none is open. The header
// comes first, then the visible window of data rows; an empty list draws an
// explicit line instead of an empty box.
func (m Model) statusPanelView(width int) string {
	if !m.statusPanel.open() || width <= 0 {
		return ""
	}
	rows := m.statusPanelRows()
	lines := []string{m.statusPanelHeader(width, rows)}
	if len(rows) == 0 {
		lines = append(lines, m.drawerSpansLine(width,
			drawerSpan{text: "   ", style: drawerBase()},
			drawerSpan{text: "nothing to show", style: drawerBase().Foreground(lipgloss.Color(colorGray)).Italic(true)},
		))
		return strings.Join(lines, "\n")
	}
	start, end := m.statusPanelWindow(rows)
	labelWidth := m.statusPanelLabelWidth(rows, width)
	for _, r := range rows[start:end] {
		lines = append(lines, m.statusPanelRowLine(width, labelWidth, r))
	}
	return strings.Join(lines, "\n")
}

// statusPanelRowAt maps a screen row to the drawer row drawn there. It returns
// ok=false for a click outside the drawer's data rows — the header, or anything
// above the drawer — and for a row beyond the list's end when the list is short.
// The mapping is recomputed from the same layout functions the view uses, so a
// click lands on the row the user sees.
func (m Model) statusPanelRowAt(y int) (statusPanelRow, bool) {
	if !m.statusPanel.open() || m.height <= 0 {
		return statusPanelRow{}, false
	}
	rows := m.statusPanelRows()
	if len(rows) == 0 {
		return statusPanelRow{}, false
	}
	height := m.statusPanelHeight()
	top := m.height - 1 - height // the header's screen row
	idx := y - top - 1
	if idx < 0 || idx >= m.statusPanelDataCount() {
		return statusPanelRow{}, false
	}
	start, _ := m.statusPanelWindow(rows)
	i := start + idx
	if i < 0 || i >= len(rows) {
		return statusPanelRow{}, false
	}
	return rows[i], true
}

// activateStatusRow performs what a clicked drawer row offers. A model or thinking
// switch is replayed as the equivalent slash line so the click goes through exactly
// the path "/model <id>" and "/think <level>" use — same provider resolution, same
// validation, same status message — and the drawer closes because the new value is
// now on the bar. An MCP row keeps the drawer open: its state flips in place when
// the switch lands, and connecting several servers in a row is the common case.
func (m Model) activateStatusRow(row statusPanelRow) (tea.Model, tea.Cmd) {
	switch row.action {
	case rowSwitchModel:
		m.statusPanel.close()
		m.relayout()
		return m.runSlash("/model " + row.value)
	case rowSetThinking:
		m.statusPanel.close()
		m.relayout()
		return m.runSlash("/think " + row.value)
	case rowToggleMCP:
		return m.toggleMCPFromDrawer(row.value, !row.current)
	default:
		return m, nil
	}
}
