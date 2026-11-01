// This file implements the TUI's interactive /skill and /mcp overlays, plus the
// selected-skills strip that renders directly above the composer. Skills are no
// longer one slash command each: the picker is a multi-select list whose checked
// rows become chips above the input, and the next submitted message carries
// every selected skill's instructions (one-shot, like Claude Code's skill
// selection). /mcp is the multi-select server switch: toggling a row connects or
// disconnects that server immediately and rewires the live tool set, so the very
// next turn advertises exactly the active servers' tools.
package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/cli/prompts"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/runtime"
)

// maxSkillPickerRows caps how many skill rows the overlay shows at once; a
// longer catalog scrolls a window around the selection, like the model picker.
const maxSkillPickerRows = 10

// maxMCPPickerRows caps how many server rows the overlay shows at once.
const maxMCPPickerRows = 10

// pickerHeaderRows is how many lines both overlays draw above their first row:
// a title line and a filter line.
const pickerHeaderRows = 2

// selectedSkill is one committed chip: a skill name plus the argument tokens
// entered in the /skill form (or prefilled by "/skill <name> <args>"). The
// tokens are one-shot, like the selection itself: they expand the skill body
// for exactly the next message and are then dropped.
type selectedSkill struct {
	Name string
	Args []string
}

// skillChipLabel renders one committed skill for the strip and the status
// lines: "commit" when it carries no arguments, "review-it(fix main)" when it
// does.
func skillChipLabel(sel selectedSkill) string {
	if len(sel.Args) == 0 {
		return sel.Name
	}
	return sel.Name + "(" + strings.Join(sel.Args, " ") + ")"
}

// skillPicker is the "/skill" overlay state. chosen is a draft selection: Space
// toggles the highlighted row, Enter commits the draft onto the model (and thus
// the chips), Esc discards it so a canceled picker changes nothing. args keeps
// the committed arguments so reopening the picker (and the argument form it
// leads to) is lossless.
type skillPicker struct {
	theme    Theme
	active   bool
	all      []*runtime.Skill
	filtered []int
	selected int
	filter   string
	chosen   map[string]bool
	args     map[string][]string
}

// newSkillPicker builds an inactive picker bound to the theme used for its rows.
func newSkillPicker(theme Theme) skillPicker { return skillPicker{theme: theme} }

// open shows the picker over the loaded skills, seeding the draft from the
// current selection so reopening never loses what the user already picked. It
// leaves the picker closed when there is nothing to choose from (the caller
// reports that instead).
func (p *skillPicker) open(skills []*runtime.Skill, selected []selectedSkill) {
	if len(skills) == 0 {
		return
	}
	p.all = skills
	p.filter = ""
	p.selected = 0
	p.chosen = make(map[string]bool, len(selected))
	p.args = make(map[string][]string, len(selected))
	for _, sel := range selected {
		p.chosen[sel.Name] = true
		p.args[sel.Name] = sel.Args
	}
	p.active = true
	p.refilter()
}

// close hides the picker and drops its draft.
func (p *skillPicker) close() {
	p.active = false
	p.all = nil
	p.filtered = nil
	p.selected = 0
	p.filter = ""
	p.chosen = nil
	p.args = nil
}

// refilter recomputes the visible rows from the typed filter, matching a skill's
// name or description case-insensitively.
func (p *skillPicker) refilter() {
	p.filtered = p.filtered[:0]
	q := strings.ToLower(p.filter)
	for i, s := range p.all {
		if s == nil {
			continue
		}
		if q == "" ||
			strings.Contains(strings.ToLower(s.Frontmatter.Name), q) ||
			strings.Contains(strings.ToLower(s.Frontmatter.Description), q) {
			p.filtered = append(p.filtered, i)
		}
	}
	if p.selected >= len(p.filtered) {
		p.selected = len(p.filtered) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}
}

// move shifts the highlight by delta rows, wrapping at the ends.
func (p *skillPicker) move(delta int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	p.selected = ((p.selected+delta)%n + n) % n
}

// page moves the highlight by one screenful, stopping at the ends.
func (p *skillPicker) page(direction int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	step := maxSkillPickerRows
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

// toggle flips the draft selection of the highlighted skill.
func (p *skillPicker) toggle() {
	s, ok := p.current()
	if !ok {
		return
	}
	name := s.Frontmatter.Name
	p.chosen[name] = !p.chosen[name]
}

// typeFilter appends typed text and re-filters from the first match.
func (p *skillPicker) typeFilter(s string) {
	p.filter += s
	p.selected = 0
	p.refilter()
}

// backspaceFilter drops the last filter rune (not byte) and re-filters.
func (p *skillPicker) backspaceFilter() {
	runes := []rune(p.filter)
	if len(runes) == 0 {
		return
	}
	p.filter = string(runes[:len(runes)-1])
	p.selected = 0
	p.refilter()
}

// current returns the highlighted skill.
func (p skillPicker) current() (*runtime.Skill, bool) {
	if !p.active || p.selected < 0 || p.selected >= len(p.filtered) {
		return nil, false
	}
	return p.all[p.filtered[p.selected]], true
}

// selection returns the draft's checked skills in catalog order (deduplicated by
// construction, since chosen is a set), carrying the args they were seeded with.
func (p skillPicker) selection() []selectedSkill {
	var out []selectedSkill
	for _, s := range p.all {
		if s != nil && p.chosen[s.Frontmatter.Name] {
			out = append(out, selectedSkill{Name: s.Frontmatter.Name, Args: p.args[s.Frontmatter.Name]})
		}
	}
	return out
}

// rows reports how many terminal rows the overlay occupies.
func (p skillPicker) rows() int {
	if !p.active {
		return 0
	}
	n := len(p.filtered)
	if n > maxSkillPickerRows {
		n = maxSkillPickerRows
	}
	if n == 0 {
		n = 1
	}
	return pickerHeaderRows + n
}

// view renders the overlay: title, filter, then one row per visible skill with a
// checkbox and the highlighted row marked by "›".
func (p skillPicker) view(width int) string {
	if !p.active {
		return ""
	}
	rowWidth := width - 2
	if rowWidth < 1 {
		rowWidth = width
	}
	var b strings.Builder
	b.WriteString(TruncateToWidth(p.theme.Accent.Render("select skills  (↑/↓ move · Space toggle · Enter apply · Esc cancel · type to filter)"), width))
	b.WriteByte('\n')
	filter := p.filter
	if filter == "" {
		filter = "type to filter…"
	} else {
		filter = "/" + filter
	}
	if n := len(p.filtered); n > 0 {
		filter += fmt.Sprintf("  (%d/%d)", p.selected+1, n)
	}
	b.WriteString(p.theme.System.Render(TruncateToWidth("  filter: "+filter, width)))
	if len(p.filtered) == 0 {
		b.WriteByte('\n')
		b.WriteString(p.theme.System.Render(TruncateToWidth("  no matching skill", width)))
		return b.String()
	}
	start, end := pickerWindow(p.selected, len(p.filtered), maxSkillPickerRows)
	for i := start; i < end; i++ {
		s := p.all[p.filtered[i]]
		if s == nil {
			continue
		}
		mark := "[ ]"
		if p.chosen[s.Frontmatter.Name] {
			mark = "[x]"
		}
		label := s.Frontmatter.Name
		if desc := strings.TrimSpace(s.Frontmatter.Description); desc != "" {
			label += "  -  " + firstLineOf(desc)
		}
		if hint := s.ParamHint(); hint != "" {
			label += "  " + hint
		}
		line := TruncateToWidth(mark+" "+label, rowWidth)
		b.WriteByte('\n')
		if i == p.selected {
			b.WriteString(p.theme.Accent.Render("› " + line))
		} else {
			b.WriteString(p.theme.System.Render("  " + line))
		}
	}
	return b.String()
}

// mcpPicker is the "/mcp" overlay state. Unlike the skill picker it has no
// draft: toggling a row applies immediately (the manager connects or disconnects
// the server and the tool set is rewired), so Enter/Esc just dismiss the
// overlay. pending marks servers whose switch is still in flight.
type mcpPicker struct {
	theme    Theme
	active   bool
	all      []mcp.ServerStatus
	filtered []int
	selected int
	filter   string
	pending  map[string]bool
}

func newMCPPicker(theme Theme) mcpPicker { return mcpPicker{theme: theme} }

// open shows the picker over the configured servers.
func (p *mcpPicker) open(statuses []mcp.ServerStatus) {
	if len(statuses) == 0 {
		return
	}
	p.all = statuses
	p.filter = ""
	p.selected = 0
	p.pending = make(map[string]bool)
	p.active = true
	p.refilter()
}

// close hides the picker.
func (p *mcpPicker) close() {
	p.active = false
	p.all = nil
	p.filtered = nil
	p.selected = 0
	p.filter = ""
	p.pending = nil
}

// setStatuses replaces the rows with fresh manager state, keeping the filter,
// cursor position and in-flight markers.
func (p *mcpPicker) setStatuses(statuses []mcp.ServerStatus) {
	name := ""
	if cur, ok := p.current(); ok {
		name = cur.Name
	}
	p.all = statuses
	p.refilter()
	if name != "" {
		for i, idx := range p.filtered {
			if p.all[idx].Name == name {
				p.selected = i
				break
			}
		}
	}
}

// refilter recomputes the visible rows from the typed filter, matching a server
// name case-insensitively.
func (p *mcpPicker) refilter() {
	p.filtered = p.filtered[:0]
	q := strings.ToLower(p.filter)
	for i, st := range p.all {
		if q == "" || strings.Contains(strings.ToLower(st.Name), q) {
			p.filtered = append(p.filtered, i)
		}
	}
	if p.selected >= len(p.filtered) {
		p.selected = len(p.filtered) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}
}

func (p *mcpPicker) move(delta int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	p.selected = ((p.selected+delta)%n + n) % n
}

func (p *mcpPicker) page(direction int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	step := maxMCPPickerRows
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

func (p *mcpPicker) typeFilter(s string) {
	p.filter += s
	p.selected = 0
	p.refilter()
}

func (p *mcpPicker) backspaceFilter() {
	runes := []rune(p.filter)
	if len(runes) == 0 {
		return
	}
	p.filter = string(runes[:len(runes)-1])
	p.selected = 0
	p.refilter()
}

// current returns the highlighted server.
func (p mcpPicker) current() (mcp.ServerStatus, bool) {
	if !p.active || p.selected < 0 || p.selected >= len(p.filtered) {
		return mcp.ServerStatus{}, false
	}
	return p.all[p.filtered[p.selected]], true
}

// rows reports how many terminal rows the overlay occupies.
func (p mcpPicker) rows() int {
	if !p.active {
		return 0
	}
	n := len(p.filtered)
	if n > maxMCPPickerRows {
		n = maxMCPPickerRows
	}
	if n == 0 {
		n = 1
	}
	return pickerHeaderRows + n
}

// view renders the overlay: title, filter, then one row per server with its
// active state, tool count and last connect error.
func (p mcpPicker) view(width int) string {
	if !p.active {
		return ""
	}
	rowWidth := width - 2
	if rowWidth < 1 {
		rowWidth = width
	}
	var b strings.Builder
	b.WriteString(TruncateToWidth(p.theme.Accent.Render("mcp servers  (↑/↓ move · Space toggle · Enter/Esc close · type to filter)"), width))
	b.WriteByte('\n')
	filter := p.filter
	if filter == "" {
		filter = "type to filter…"
	} else {
		filter = "/" + filter
	}
	if n := len(p.filtered); n > 0 {
		filter += fmt.Sprintf("  (%d/%d)", p.selected+1, n)
	}
	b.WriteString(p.theme.System.Render(TruncateToWidth("  filter: "+filter, width)))
	if len(p.filtered) == 0 {
		b.WriteByte('\n')
		b.WriteString(p.theme.System.Render(TruncateToWidth("  no matching server", width)))
		return b.String()
	}
	start, end := pickerWindow(p.selected, len(p.filtered), maxMCPPickerRows)
	for i := start; i < end; i++ {
		st := p.all[p.filtered[i]]
		state := "off"
		if p.pending[st.Name] {
			state = "switching…"
		} else if st.Active {
			state = fmt.Sprintf("on · %d tools", st.Tools)
		}
		mark := "[ ]"
		if st.Active {
			mark = "[x]"
		}
		label := st.Name + "  -  " + state
		if st.Error != "" && !st.Active {
			label += "  (" + firstLineOf(st.Error) + ")"
		}
		line := TruncateToWidth(mark+" "+label, rowWidth)
		b.WriteByte('\n')
		if i == p.selected {
			b.WriteString(p.theme.Accent.Render("› " + line))
		} else {
			b.WriteString(p.theme.System.Render("  " + line))
		}
	}
	return b.String()
}

// pickerWindow returns the [start,end) slice of visible rows, scrolled to keep
// the selection visible in a list taller than max rows.
func pickerWindow(selected, n, maxRows int) (int, int) {
	if n <= maxRows {
		return 0, n
	}
	start := selected - maxRows + 1
	if start < 0 {
		start = 0
	}
	if start > n-maxRows {
		start = n - maxRows
	}
	return start, start + maxRows
}

// firstLineOf keeps a description or error on one line for the compact rows.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// availableSkills returns the skill set the current session was assembled with
// (refreshed by a reload), falling back to the launch options for a session-less
// model (tests, pure construction).
func (m Model) availableSkills() []*runtime.Skill {
	if m.session != nil {
		return m.session.skills
	}
	return m.opts.Skills
}

// mcpStatuses returns the configured MCP servers and their state, or nil when
// no manager is wired.
func (m Model) mcpStatuses() []mcp.ServerStatus {
	if m.session == nil || m.session.mcp == nil {
		return nil
	}
	return m.session.mcp.Statuses()
}

// openSkillPicker replaces the composer buffer with the multi-select skill
// chooser, seeding the draft from the current chips (names and arguments).
func (m Model) openSkillPicker(line string) (tea.Model, tea.Cmd) {
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	m.modelPicker.close()
	m.picker.close()
	m.skillArgForm.close()
	skills := m.availableSkills()
	if len(skills) == 0 {
		m.transcript.addSystem("(no skills loaded; add SKILL.md under ~/.agents/skills/ or .pigo/skills/)")
		m.relayout()
		return m, nil
	}
	m.skillPicker.open(skills, m.selectedSkills)
	m.transcript.addSystem(fmt.Sprintf("select skills from %d loaded — Space toggles, Enter continues to the argument form, Esc cancels", len(skills)))
	m.relayout()
	return m, nil
}

// submitSkillPick hands the picker's draft to the argument form. With nothing
// checked it clears the chips instead of opening an empty form.
func (m Model) submitSkillPick() (tea.Model, tea.Cmd) {
	sels := m.skillPicker.selection()
	m.skillPicker.close()
	if len(sels) == 0 {
		m.setSelectedSkills(nil)
		m.relayout()
		m.transcript.addSystem("no skills selected")
		return m, nil
	}
	return m.openSkillArgs(sels)
}

// skillNamedArgs handles "/skill <name> [args]" in the TUI. The named skill is
// no longer run on the spot: it becomes the only selected skill and opens the
// argument form with the typed args prefilled, so the values can be adjusted
// before the next message carries it. An unknown name reports the catalog
// instead, like the REPL.
func (m Model) skillNamedArgs(line string) (tea.Model, tea.Cmd) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "/skill"))
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return m.openSkillPicker(line)
	}
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	name := fields[0]
	skills := m.availableSkills()
	s := prompts.FindSkill(skills, name)
	if s == nil {
		m.transcript.addSystem(fmt.Sprintf("skill: unknown skill %q\n%s", name, prompts.SkillListing(skills)))
		m.relayout()
		return m, nil
	}
	rest := body
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		rest = strings.TrimSpace(rest[i:])
	} else {
		rest = ""
	}
	var tokens []string
	if rest != "" {
		toks, err := runtime.SplitArgs(rest)
		if err != nil {
			toks = []string{rest}
		}
		tokens = toks
	}
	return m.openSkillArgs([]selectedSkill{{Name: s.Frontmatter.Name, Args: tokens}})
}

// openSkillArgs shows the argument form for sels: one field per template slot
// the selected skills declare (plus a free-text field for skills without any),
// prefilled from the args they already carry.
func (m Model) openSkillArgs(sels []selectedSkill) (tea.Model, tea.Cmd) {
	m.skillPicker.close()
	m.modelPicker.close()
	m.picker.close()
	m.skillArgForm.open(m.availableSkills(), sels)
	m.relayout()
	if !m.skillArgForm.active {
		return m, nil
	}
	m.transcript.addSystem("fill in the skill arguments — Tab/↑/↓ moves, Enter applies to the next message, Esc goes back")
	return m, nil
}

// submitSkillArgs commits the form's values onto the chips and closes the form.
// The chipped skills then ride the next submitted message; the values are what
// ExpandTemplate sees when the body is expanded.
func (m Model) submitSkillArgs() (tea.Model, tea.Cmd) {
	sels := m.skillArgForm.selection()
	m.skillArgForm.close()
	m.setSelectedSkills(sels)
	m.relayout()
	if len(sels) == 0 {
		m.transcript.addSystem("no skills selected")
		return m, nil
	}
	labels := make([]string, len(sels))
	for i, sel := range sels {
		labels[i] = skillChipLabel(sel)
	}
	m.transcript.addSystem("skills selected: " + strings.Join(labels, ", ") + " (they run with your next message)")
	return m, nil
}

// cancelSkillArgs goes back from the argument form to the picker without
// committing, keeping the checked skills and anything already typed.
func (m Model) cancelSkillArgs() (tea.Model, tea.Cmd) {
	sels := m.skillArgForm.selection()
	m.skillArgForm.close()
	m.skillPicker.open(m.availableSkills(), sels)
	m.relayout()
	return m, nil
}

// setSelectedSkills replaces the chip selection and reflows the shell (the strip
// takes a row above the composer while any skill is selected).
func (m *Model) setSelectedSkills(sels []selectedSkill) {
	m.selectedSkills = append(m.selectedSkills[:0], sels...)
}

// clearSelectedSkills drops every chip. It is called after a message carrying
// the skills is submitted (the selection is one-shot).
func (m *Model) clearSelectedSkills() {
	m.selectedSkills = nil
}

// skillChipsRows reports how many rows the selected-skills strip needs, so
// relayout can shrink the transcript by exactly that much. The strip is hidden
// while a skill overlay is open (the overlay already shows the selection).
func (m Model) skillChipsRows() int {
	if len(m.selectedSkills) == 0 || m.skillPicker.active || m.skillArgForm.active || m.running {
		return 0
	}
	return 1
}

// skillChips renders the selected-skills strip shown directly above the input,
// e.g. "⚡ skills: commit · review-it(fix main)   (/skill to change)". It returns
// "" when nothing is selected so the shell layout is unchanged.
func (m Model) skillChips(width int) string {
	if len(m.selectedSkills) == 0 || m.skillPicker.active || m.skillArgForm.active || m.running {
		return ""
	}
	labels := make([]string, len(m.selectedSkills))
	for i, sel := range m.selectedSkills {
		labels[i] = skillChipLabel(sel)
	}
	label := "⚡ skills: " + strings.Join(labels, " · ")
	hint := "  (/skill to change)"
	if width <= 0 {
		return label
	}
	if len([]rune(label))+len([]rune(hint)) > width {
		hint = ""
	}
	line := m.theme.Accent.Render(label)
	if hint != "" {
		line += m.theme.System.Render(hint)
	}
	return TruncateToWidth(line, width)
}

// composeWithSkills prepends every selected skill's instructions (as a
// <skill name="…" args="…"> block) to the prompt. It is what makes a chip
// "trigger" the skill without turning it into a slash command: the model
// receives the text /skill <name> <args> would have produced, with the form's
// values expanded through the same template engine. Unknown names (a skill
// removed by a reload) are skipped; with no usable skill the prompt is returned
// unchanged.
func (m Model) composeWithSkills(prompt string) string {
	if len(m.selectedSkills) == 0 {
		return prompt
	}
	skills := m.availableSkills()
	var b strings.Builder
	used := 0
	for _, sel := range m.selectedSkills {
		s := prompts.FindSkill(skills, sel.Name)
		if s == nil {
			continue
		}
		if used == 0 {
			b.WriteString("The user selected the following skills for this message. Follow their instructions.\n\n")
		}
		b.WriteString(`<skill name="` + escapeAttr(s.Frontmatter.Name) + `"`)
		if len(sel.Args) > 0 {
			b.WriteString(` args="` + escapeAttr(strings.Join(sel.Args, " ")) + `"`)
		}
		b.WriteString(">\n")
		b.WriteString(strings.TrimRight(s.ExpandArgs(sel.Args), "\n"))
		b.WriteString("\n</skill>\n\n")
		used++
	}
	if used == 0 {
		return prompt
	}
	b.WriteString(prompt)
	return b.String()
}

// escapeAttr escapes a value for the <skill args="…"> attribute so a quote or
// angle bracket in an argument cannot break the block's framing.
func escapeAttr(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}

// openMCPPicker replaces the composer buffer with the multi-select server
// switch, showing every configured server and its state.
func (m Model) openMCPPicker(line string) (tea.Model, tea.Cmd) {
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	m.modelPicker.close()
	m.picker.close()
	statuses := m.mcpStatuses()
	if len(statuses) == 0 {
		m.transcript.addSystem("(no MCP servers configured; add [[mcp]] to config.toml or use `pigorc mcp add`)")
		m.relayout()
		return m, nil
	}
	m.mcpPicker.open(statuses)
	m.transcript.addSystem(fmt.Sprintf("switch MCP servers (%d configured) — Space toggles one immediately, Enter/Esc closes", len(statuses)))
	m.relayout()
	return m, nil
}

// toggleMCPAtCursor switches the highlighted server and marks its row as
// switching. The switch itself runs off the tea loop (a stdio server spawn or an
// HTTP handshake can block); mcpToggleDoneMsg applies the result. The model is
// marked running for the duration so a prompt cannot be submitted while the
// switch mutates agentCtx.Tools on the UI goroutine (the run goroutine reads it),
// and so two switches cannot overlap.
func (m Model) toggleMCPAtCursor() (tea.Model, tea.Cmd) {
	st, ok := m.mcpPicker.current()
	if !ok || m.mcpPicker.pending[st.Name] {
		return m, nil
	}
	if m.session == nil || m.session.mcp == nil {
		m.transcript.addSystem("(MCP switching unavailable: no active session)")
		return m, nil
	}
	m.mcpPicker.pending[st.Name] = true
	m.spinner.begin(time.Now(), m.thinkingLabel())
	m.spinner.pin("Switching MCP server")
	m.running = true
	m.relayout()
	return m, tea.Batch(m.toggleMCPCmd(st.Name, !st.Active), m.tickSpinner())
}

// toggleMCPCmd switches one server off the tea loop and reports the result.
func (m Model) toggleMCPCmd(name string, active bool) tea.Cmd {
	mgr := m.session.mcp
	return func() tea.Msg {
		msg := prompts.SetMCPActive(mgr, name, active)
		return mcpToggleDoneMsg{name: name, active: active, message: msg}
	}
}

// runMCPCommand handles the textual "/mcp <name> [on|off]" form: it applies the
// switch asynchronously behind the spinner (connecting can block) and then
// reports the status, mirroring the picker's immediate behavior.
func (m Model) runMCPCommand(line string) (tea.Model, tea.Cmd) {
	if m.session == nil || m.session.mcp == nil {
		return m.runSlashFallback(line)
	}
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	m.spinner.begin(time.Now(), m.thinkingLabel())
	m.spinner.pin("Switching MCP server")
	m.running = true
	m.relayout()
	mgr := m.session.mcp
	args := strings.TrimSpace(strings.TrimPrefix(line, "/mcp"))
	return m, tea.Batch(func() tea.Msg {
		return mcpToggleDoneMsg{message: prompts.ApplyMCPCommand(mgr, args)}
	}, m.tickSpinner())
}

// runSlashFallback resolves a command through the shared registry, for the case
// where the TUI cannot handle it interactively (no session).
func (m Model) runSlashFallback(line string) (tea.Model, tea.Cmd) {
	if m.slash == nil {
		m.transcript.addUser(line)
		m.transcript.addSystem("Slash commands unavailable")
		m.relayout()
		return m, nil
	}
	outcome, err := m.slash.ResolveOutcome(line)
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	if err != nil {
		m.transcript.addSystem(err.Error())
		m.relayout()
		return m, nil
	}
	if outcome.Message != "" {
		m.transcript.addSystem(outcome.Message)
	}
	if outcome.Kind == runtime.SlashAction || outcome.Prompt == "" {
		m.relayout()
		return m, nil
	}
	return m.startPrompt(outcome.Prompt)
}

// mcpToggleDoneMsg reports a finished MCP switch: message is the status line to
// show, name/active identify what changed (empty for the textual form, whose
// status line already carries it). Both forms mark the model running for the
// duration, so the handler always ends the busy state.
type mcpToggleDoneMsg struct {
	name    string
	active  bool
	message string
}
