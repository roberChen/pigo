package tui

// Tests for the status bar's drawers (statuspanel.go): a click on the model,
// thinking, skills, mcp or files block opens the list hinged to the bar, its rows
// reserve height in the shell, a row click performs the same switch the matching
// slash command performs, and Esc/toggle/second click closes it again.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/builtinskills"
	"github.com/roberChen/pigo/internal/cli"
	"github.com/roberChen/pigo/internal/cli/ui"
	"github.com/roberChen/pigo/internal/mcp"
	"github.com/roberChen/pigo/internal/runtime"
)

// statusPanelTestModel builds a sized model with a live config and the git probe
// already applied, so the bar has every clickable block.
func statusPanelTestModel(t *testing.T, startModel string) Model {
	t.Helper()
	m := NewModel(Options{Model: startModel, ProviderName: "openrouter", ThinkingLevel: agentcore.ThinkingMedium, Skills: skillFixture()})
	m = apply(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m.statusBar.SetGit(gitInfoMsg{branch: "main", ok: true, files: []gitFile{
		{path: "a.go", code: " M", added: 4, removed: 1, counts: true},
		{path: "b.go", code: "??"},
	}})
	m.relayout()
	return m
}

// barColumnFor finds a column on the status bar whose block opens section. It
// goes through the bar's own hit map, so the test clicks exactly where the UI
// says the block is.
func barColumnFor(t *testing.T, m Model, section statusSection) int {
	t.Helper()
	width := m.contentWidth()
	for x := 0; x < width; x++ {
		if got, ok := m.statusBar.sectionAt(x, width, m.statusPanel.section); ok && got == section {
			return x
		}
	}
	t.Fatalf("no column opens section %v", section)
	return 0
}

// clickBar sends a left press on the status bar's block for section.
func clickBar(t *testing.T, m Model, section statusSection) Model {
	t.Helper()
	x := barColumnFor(t, m, section)
	return apply(t, m, tea.MouseClickMsg{X: x, Y: m.height - 1, Button: tea.MouseLeft})
}

// drawerTopRow is the screen row of the open drawer's header, as the view and the
// hit test compute it.
func drawerTopRow(m Model) int { return m.height - 1 - m.statusPanelHeight() }

// TestStatusBarClickOpensAndTogglesDrawer verifies a click on the model block opens
// its drawer and a second click on the same block dismisses it.
func TestStatusBarClickOpensAndTogglesDrawer(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")

	m = clickBar(t, m, sectionModel)
	if !m.statusPanel.open() || m.statusPanel.section != sectionModel {
		t.Fatalf("click should open the model drawer, got %v", m.statusPanel.section)
	}
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "model · click a row to switch") {
		t.Errorf("drawer header not rendered:\n%s", content)
	}

	m = clickBar(t, m, sectionModel)
	if m.statusPanel.open() {
		t.Error("a second click on the same block should close the drawer")
	}
}

// TestStatusBarClickSwitchesSections verifies clicking another block replaces the
// open drawer rather than stacking one on top of the other.
func TestStatusBarClickSwitchesSections(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = clickBar(t, m, sectionModel)
	m = clickBar(t, m, sectionThinking)
	if m.statusPanel.section != sectionThinking {
		t.Errorf("section = %v, want thinking", m.statusPanel.section)
	}
	if got := m.statusPanelHeight(); got != 1+len(thinkingLevels) {
		t.Errorf("drawer height = %d, want header + %d levels", got, len(thinkingLevels))
	}
}

// TestStatusBarDrawerReservesRows checks the drawer shrinks the transcript by
// exactly the rows it draws, so the bar stays on the last line of the shell.
func TestStatusBarDrawerReservesRows(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	before := m.transcript.viewportHeight()

	m = clickBar(t, m, sectionThinking)
	height := m.statusPanelHeight()
	if height <= 0 {
		t.Fatal("the thinking drawer should occupy rows")
	}
	if got := m.transcript.viewportHeight(); got != before-height {
		t.Errorf("transcript rows = %d, want %d (was %d, drawer %d)", got, before-height, before, height)
	}

	rows := strings.Split(stripANSI(m.View().Content), "\n")
	if len(rows) != m.height {
		t.Errorf("shell painted %d rows, want %d", len(rows), m.height)
	}
	if !strings.Contains(rows[m.height-1], "pigo") {
		t.Errorf("status bar is not the last row: %q", rows[m.height-1])
	}
	if !strings.Contains(rows[drawerTopRow(m)], "thinking level") {
		t.Errorf("drawer header is not directly above the bar: %q", rows[drawerTopRow(m)])
	}
}

// TestStatusBarDrawerEscCloses verifies Esc dismisses the drawer without quitting.
func TestStatusBarDrawerEscCloses(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = clickBar(t, m, sectionModel)

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.statusPanel.open() {
		t.Fatal("Esc should close the open drawer")
	}
	if m.quitting {
		t.Fatal("Esc on a drawer must not quit the program")
	}
	if got := m.transcript.viewportHeight(); got == 0 {
		t.Error("closing the drawer should give the transcript its rows back")
	}
}

// TestStatusBarDrawerSwitchesModel verifies clicking a model row switches the live
// model through the same path "/model <id>" uses and closes the drawer.
func TestStatusBarDrawerSwitchesModel(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = clickBar(t, m, sectionModel)

	rows := m.statusPanelRows()
	target := -1
	for i, r := range rows {
		if r.action == rowSwitchModel && !r.current {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("the catalog should hold a model other than the current one")
	}
	start, _ := m.statusPanelWindow(rows)
	if target < start || target >= start+m.statusPanelDataCount() {
		t.Fatalf("row %d is outside the drawn window starting at %d", target, start)
	}
	y := drawerTopRow(m) + 1 + (target - start)

	next := apply(t, m, tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
	if next.statusPanel.open() {
		t.Error("switching a model should close the drawer")
	}
	if next.live.Model != rows[target].value {
		t.Errorf("live.Model = %q, want %q", next.live.Model, rows[target].value)
	}
	if next.statusBar.model != rows[target].value {
		t.Errorf("the bar should show the new model immediately, got %q", next.statusBar.model)
	}
	if content := stripANSI(next.View().Content); !strings.Contains(content, "model switched to "+rows[target].value) {
		t.Errorf("switch outcome not reported:\n%s", content)
	}
}

// TestStatusBarDrawerSwitchesThinking verifies clicking a thinking row sets the
// level the bar shows.
func TestStatusBarDrawerSwitchesThinking(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = clickBar(t, m, sectionThinking)

	rows := m.statusPanelRows()
	target := -1
	for i, r := range rows {
		if r.value == string(agentcore.ThinkingLow) {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("the drawer should list the low level")
	}
	y := drawerTopRow(m) + 1 + target

	next := apply(t, m, tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
	if got := next.live.ThinkingLevel; got != agentcore.ThinkingLow {
		t.Errorf("live thinking = %q, want low", got)
	}
	if next.statusBar.thinking != string(agentcore.ThinkingLow) {
		t.Errorf("bar thinking = %q, want low", next.statusBar.thinking)
	}
}

// TestStatusBarClickOnBarOutsideBlockStartsNoSelection verifies a press on a
// display-only block is swallowed: it neither opens a drawer nor begins a text
// selection that a later release would turn into a click.
func TestStatusBarClickOnBarOutsideBlockStartsNoSelection(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	before := m.sel

	m = apply(t, m, tea.MouseClickMsg{X: 0, Y: m.height - 1, Button: tea.MouseLeft})
	if m.statusPanel.open() {
		t.Error("the app badge should not open a drawer")
	}
	if m.sel.active != before.active || m.sel.dragging != before.dragging {
		t.Error("a press on the bar must not start a text selection")
	}
}

// TestStatusBarDrawerRowsOutsideAreIgnored verifies a press above the drawer never
// resolves to a row (it belongs to the transcript).
func TestStatusBarDrawerRowsOutsideAreIgnored(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = clickBar(t, m, sectionThinking)

	if _, ok := m.statusPanelRowAt(drawerTopRow(m)); ok {
		t.Error("the header row is not a data row")
	}
	if _, ok := m.statusPanelRowAt(drawerTopRow(m) - 1); ok {
		t.Error("a row above the drawer is not a data row")
	}
	if _, ok := m.statusPanelRowAt(m.height - 2); !ok {
		t.Error("the last data row should be clickable")
	}
}

// TestStatusBarDrawerEmptyListShowsNote verifies a section with nothing to list
// draws an explicit line instead of an empty box.
func TestStatusBarDrawerEmptyListShowsNote(t *testing.T) {
	m := NewModel(Options{Model: "openai/gpt-4o"})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.statusPanel.section = sectionSkills
	m.relayout()

	if m.statusPanelHeight() != 2 {
		t.Errorf("empty drawer height = %d, want header + note", m.statusPanelHeight())
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "nothing to show") {
		t.Errorf("empty drawer should say so:\n%s", content)
	}
}

// TestStatusBarDrawerMarksBuiltinSkills verifies a built-in skill row says so, in
// the row's detail and in the drawer title's count. The distinction is the whole
// point: a built-in lives read-only in the binary, so a same-named disk copy is only
// used when [skills] override_builtin lists it — a user editing that copy needs to
// see which kind the loaded skill is.
func TestStatusBarDrawerMarksBuiltinSkills(t *testing.T) {
	m := NewModel(Options{Skills: []*runtime.Skill{
		{Frontmatter: runtime.SkillFrontmatter{Name: "pigorc", Description: "configure pigo"},
			Path: builtinskills.VirtualPath("pigorc", "SKILL.md")},
		{Frontmatter: runtime.SkillFrontmatter{Name: "mine", Description: "my own skill"},
			Path: "/home/u/.agents/skills/mine/SKILL.md"},
	}})
	m.statusPanel.section = sectionSkills
	rows := m.statusPanelRows()
	if len(rows) != 2 {
		t.Fatalf("skill rows = %d, want 2", len(rows))
	}
	if want := "built-in · configure pigo"; rows[0].detail != want {
		t.Errorf("built-in row detail = %q, want %q", rows[0].detail, want)
	}
	if rows[1].detail != "my own skill" {
		t.Errorf("disk row detail = %q, want the bare description", rows[1].detail)
	}

	m = apply(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m.relayout()
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "1 built-in, 1 from disk") {
		t.Errorf("skills drawer title should count both kinds:\n%s", content)
	}
	if !strings.Contains(content, "built-in · configure pigo") {
		t.Errorf("the built-in marker is not on screen:\n%s", content)
	}
}

// TestStatusBarDrawerSkillsAndFilesRows verifies the informational drawers list what
// the bar counts: the loaded skills and the changed paths with their deltas.
func TestStatusBarDrawerSkillsAndFilesRows(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")

	m.statusPanel.section = sectionSkills
	rows := m.statusPanelRows()
	if len(rows) != len(skillFixture()) {
		t.Fatalf("skill rows = %d, want %d", len(rows), len(skillFixture()))
	}
	if rows[0].label != "commit" || rows[0].detail != "write a commit message" {
		t.Errorf("skill row = %+v", rows[0])
	}

	m.statusPanel.section = sectionFiles
	rows = m.statusPanelRows()
	if len(rows) != 2 {
		t.Fatalf("file rows = %d, want 2", len(rows))
	}
	if want := glyphAdded + "4 " + glyphRemoved + "1"; rows[0].detail != want {
		t.Errorf("counted file detail = %q, want %q", rows[0].detail, want)
	}
	if rows[1].detail != "new" {
		t.Errorf("untracked file detail = %q, want new", rows[1].detail)
	}
}

// TestCenteredWindow checks the automatic window logic: it is the whole list when
// it fits, and otherwise slides to keep the current row visible without leaving
// the list.
func TestCenteredWindow(t *testing.T) {
	cases := []struct {
		name                 string
		total, current, caps int
		wantStart, wantEnd   int
	}{
		{"fits", 3, 1, 10, 0, 3},
		{"empty", 0, -1, 10, 0, 0},
		{"zero capacity", 5, 0, 0, 0, 0},
		{"head", 95, 0, 10, 0, 10},
		{"middle", 95, 40, 10, 35, 45},
		{"tail", 95, 94, 10, 85, 95},
		{"no current", 95, -1, 10, 0, 10},
	}
	for _, c := range cases {
		start, end := centeredWindow(c.total, c.current, c.caps)
		if start != c.wantStart || end != c.wantEnd {
			t.Errorf("%s: window = [%d,%d), want [%d,%d)", c.name, start, end, c.wantStart, c.wantEnd)
		}
	}
}

// TestThinkingLevelsCoverEveryLevel guards the drawer against drifting from the
// enum it mirrors: every level /think accepts must have exactly one row.
func TestThinkingLevelsCoverEveryLevel(t *testing.T) {
	want := []agentcore.ThinkingLevel{
		agentcore.ThinkingOff, agentcore.ThinkingMinimal, agentcore.ThinkingLow,
		agentcore.ThinkingMedium, agentcore.ThinkingHigh, agentcore.ThinkingXHigh,
		agentcore.ThinkingMax,
	}
	if len(thinkingLevels) != len(want) {
		t.Fatalf("thinkingLevels has %d entries, want %d", len(thinkingLevels), len(want))
	}
	for i, lvl := range want {
		if thinkingLevels[i].level != lvl {
			t.Errorf("thinkingLevels[%d] = %q, want %q", i, thinkingLevels[i].level, lvl)
		}
	}
}

// TestRefreshStatusCounts verifies the bar's skill and MCP counts are derived from
// the session sources rather than staying at their construction values.
func TestRefreshStatusCounts(t *testing.T) {
	m := NewModel(Options{Model: "openai/gpt-4o", Skills: nil})
	if m.statusBar.skills != 0 {
		t.Fatalf("skills = %d, want 0 before refresh", m.statusBar.skills)
	}
	m.opts.Skills = skillFixture()
	m.refreshStatusCounts()
	if m.statusBar.skills != len(skillFixture()) {
		t.Errorf("skills = %d, want %d", m.statusBar.skills, len(skillFixture()))
	}
	if m.statusBar.mcpTotal != 0 {
		t.Errorf("mcp total = %d, want 0 with no manager", m.statusBar.mcpTotal)
	}
}

// TestStatusBarDrawerTogglesMCP verifies a click on a server row starts the same
// off-loop switch /mcp performs — busy until it lands, so a prompt cannot race the
// live tool set — and leaves the drawer open so the new state shows in place.
func TestStatusBarDrawerTogglesMCP(t *testing.T) {
	mgr := mcp.Connect(context.Background(), []mcp.ServerConfig{{Name: "github", Disabled: true}}, io.Discard)
	t.Cleanup(func() { _ = mgr.Close() })
	m := NewModel(Options{Model: "openai/gpt-4o"})
	m = apply(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m.session = &runSession{mcp: mgr}
	m.refreshStatusCounts()
	m = clickBar(t, m, sectionMCP)

	if want := fmt.Sprintf("%s 0/1", glyphMCP); !strings.Contains(stripANSI(m.View().Content), want) {
		t.Fatalf("bar should show %q while the server is off:\n%s", want, stripANSI(m.View().Content))
	}
	rows := m.statusPanelRows()
	if len(rows) != 1 || rows[0].action != rowToggleMCP || rows[0].current {
		t.Fatalf("mcp rows = %+v, want one inactive toggle row", rows)
	}
	next := apply(t, m, tea.MouseClickMsg{X: 2, Y: drawerTopRow(m) + 1, Button: tea.MouseLeft})
	if !next.running {
		t.Error("the switch should mark the model busy for its duration")
	}
	if !next.statusPanel.open() {
		t.Error("an mcp row click should keep the drawer open")
	}
	if content := stripANSI(next.View().Content); !strings.Contains(content, "Switching MCP server") {
		t.Errorf("the switch should show the spinner label:\n%s", content)
	}
}

// TestStatusBarDrawerClosesOnSubmit verifies typing a prompt dismisses the drawer:
// the answer is what the user asked for, and it streams into a full-height
// transcript.
func TestStatusBarDrawerClosesOnSubmit(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	started := false
	m.startRunFn = func(prompt string) (chan tea.Msg, tea.Cmd) {
		started = true
		return make(chan tea.Msg), nil
	}
	m = clickBar(t, m, sectionModel)
	m.input.SetValue("hello")

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !started {
		t.Fatal("Enter should have started the run")
	}
	if next.(Model).statusPanel.open() {
		t.Error("submitting should close the drawer")
	}
}

// TestStatusBarDrawerCoexistsWithFullscreen verifies the drawer is reserved in the
// full-screen composer too: the shell stays exactly as tall as the terminal, with
// the drawer above the bar and the bar still last.
func TestStatusBarDrawerCoexistsWithFullscreen(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = apply(t, m, tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	if !m.input.Fullscreen() {
		t.Fatal("Ctrl+E should enter the full-screen composer")
	}
	m = clickBar(t, m, sectionThinking)

	rows := strings.Split(stripANSI(m.View().Content), "\n")
	if len(rows) != m.height {
		t.Fatalf("shell painted %d rows, want %d", len(rows), m.height)
	}
	if !strings.Contains(rows[m.height-1], "pigo") {
		t.Errorf("status bar is not the last row: %q", rows[m.height-1])
	}
	if !strings.Contains(rows[drawerTopRow(m)], "thinking level") {
		t.Errorf("drawer header is not directly above the bar: %q", rows[drawerTopRow(m)])
	}
}

// TestStatusBarDrawerScrollsWithKeys verifies the open drawer owns ↑/↓, PgUp/PgDn
// and Home/End for its own window, and that scrolling it does not move the
// composer's history or the transcript.
func TestStatusBarDrawerScrollsWithKeys(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.statusPanel.section = sectionThinking
	m.relayout()

	start, end := m.statusPanelWindow(m.statusPanelRows())
	if start != 0 || end != len(thinkingLevels) {
		t.Fatalf("a list that fits should be drawn whole, got [%d,%d)", start, end)
	}
	// Nothing scrolls when everything is visible: the keys are swallowed, so the
	// composer must not have seen them either.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.statusPanel.manual {
		t.Error("a list that fits must not become manually scrolled")
	}
	if m.histIdx != len(m.history) {
		t.Error("↓ with a drawer open must not walk the prompt history")
	}
}

// TestStatusBarDrawerScrollWindow checks a long list (the model catalog) scrolls by
// key, clamps at both ends and reports the visible slice in its header.
func TestStatusBarDrawerScrollWindow(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.statusPanel.section = sectionModel
	m.relayout()
	rows := m.statusPanelRows()
	caps := m.statusPanelCapacity()
	if len(rows) <= caps {
		t.Skipf("catalog holds %d models, need more than %d to scroll", len(rows), caps)
	}

	// Down one row from the automatic position, then back up past the top.
	auto, _ := m.statusPanelWindow(rows)
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got, _ := m.statusPanelWindow(rows); got != auto+1 {
		t.Errorf("↓ moved the window to %d, want %d", got, auto+1)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	if got, _ := m.statusPanelWindow(rows); got != 0 {
		t.Errorf("Home put the window at %d, want 0", got)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got, _ := m.statusPanelWindow(rows); got != 0 {
		t.Errorf("↑ past the top clamped to %d, want 0", got)
	}

	// End shows the tail; ↓ past it stays there.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	start, end := m.statusPanelWindow(rows)
	if end != len(rows) || end-start != caps {
		t.Errorf("End window = [%d,%d), want the last %d rows", start, end, caps)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got, _ := m.statusPanelWindow(rows); got != start {
		t.Errorf("↓ past the end clamped to %d, want %d", got, start)
	}

	// PgDn moves a screenful.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if got, _ := m.statusPanelWindow(rows); got != caps {
		t.Errorf("PgDn moved the window to %d, want %d", got, caps)
	}

	// The header says which slice is on screen.
	if content := stripANSI(m.View().Content); !strings.Contains(content, fmt.Sprintf("(%d-%d/%d)", caps+1, 2*caps, len(rows))) {
		t.Errorf("header should report the visible slice:\n%s", content)
	}
}

// TestStatusBarDrawerScrollResetsAndClicksFollowWindow verifies a click resolves to
// the row actually painted after scrolling, and that closing the drawer forgets
// the scroll so the next open starts on the value in effect again.
func TestStatusBarDrawerScrollResetsAndClicksFollowWindow(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.statusPanel.section = sectionModel
	m.relayout()
	rows := m.statusPanelRows()
	caps := m.statusPanelCapacity()
	if len(rows) <= caps {
		t.Skipf("catalog holds %d models, need more than %d to scroll", len(rows), caps)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	start, _ := m.statusPanelWindow(rows)
	row, ok := m.statusPanelRowAt(drawerTopRow(m) + 1)
	if !ok {
		t.Fatal("the first drawn row should be clickable")
	}
	if row.value != rows[start].value {
		t.Errorf("click resolved to %q, want the painted row %q", row.value, rows[start].value)
	}

	m.statusPanel.close()
	if m.statusPanel.manual || m.statusPanel.scroll != 0 {
		t.Errorf("closing should forget the scroll, got manual=%v scroll=%d", m.statusPanel.manual, m.statusPanel.scroll)
	}
	if got, _ := m.statusPanelWindow(rows); got != clampStart(statusPanelCurrentIndex(rows)-caps/2, len(rows)-caps) {
		t.Errorf("reopening should center on the current row, window starts at %d", got)
	}
}

// TestStatusBarDrawerWheelScrollsDrawerOnly verifies the wheel over the drawer
// scrolls the drawer, while the wheel over the transcript still scrolls the
// transcript.
func TestStatusBarDrawerWheelScrollsDrawerOnly(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.statusPanel.section = sectionModel
	m.relayout()
	rows := m.statusPanelRows()
	caps := m.statusPanelCapacity()
	if len(rows) <= caps {
		t.Skipf("catalog holds %d models, need more than %d to scroll", len(rows), caps)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})

	before, _ := m.statusPanelWindow(rows)
	m = apply(t, m, tea.MouseWheelMsg{X: 2, Y: drawerTopRow(m) + 1, Button: tea.MouseWheelDown})
	if got, _ := m.statusPanelWindow(rows); got != before+1 {
		t.Errorf("wheel over the drawer moved it to %d, want %d", got, before+1)
	}

	// A wheel event above the drawer belongs to the transcript, so the drawer
	// window stays put.
	if drawerTopRow(m) > 0 {
		after, _ := m.statusPanelWindow(rows)
		m = apply(t, m, tea.MouseWheelMsg{X: 2, Y: 0, Button: tea.MouseWheelDown})
		if got, _ := m.statusPanelWindow(rows); got != after {
			t.Errorf("wheel above the drawer moved it to %d, want it unmoved at %d", got, after)
		}
	}
}

// clampStart mirrors the window's own clamping for the assertions above.
func clampStart(start, maxStart int) int {
	if start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	return start
}

// TestStatusBarDrawerRowsFillWidthExactly pins the drawer's geometry: header and
// every data row are exactly the shell width (no ragged right edge, no overflow),
// the ● marker sits in the gutter only on the row in effect, and an action row
// keeps its ↵ hint even when the text was clipped to fit.
func TestStatusBarDrawerRowsFillWidthExactly(t *testing.T) {
	for _, width := range []int{40, 72, 120} {
		m := statusPanelTestModel(t, "openai/gpt-4o")
		m = apply(t, m, tea.WindowSizeMsg{Width: width, Height: 26})
		m.statusPanel.section = sectionModel
		m.relayout()

		lines := strings.Split(stripANSI(m.View().Content), "\n")
		top := drawerTopRow(m)
		for i := 0; i < m.statusPanelHeight(); i++ {
			row := []rune(lines[top+i])
			if got := ui.Width(lines[top+i]); got != width {
				t.Errorf("width %d: drawer row %d is %d columns, want %d: %q", width, i, got, width, string(row))
			}
		}

		rows := m.statusPanelRows()
		start, _ := m.statusPanelWindow(rows)
		for i, r := range rows[start : start+m.statusPanelDataCount()] {
			drawn := strings.TrimRight(lines[top+1+i], " ")
			if r.action != rowNone && !strings.HasSuffix(drawn, glyphAction) {
				t.Errorf("width %d: action row has no %q hint: %q", width, glyphAction, drawn)
			}
			marked := strings.HasPrefix(strings.TrimPrefix(lines[top+1+i], " "), "●")
			if marked != r.current {
				t.Errorf("width %d: row %d current marker = %v, want %v: %q", width, i, marked, r.current, lines[top+1+i])
			}
		}
	}
}

// TestStatusBarDrawerLabelColumnAligns checks the detail column starts at the same
// column on every row of a drawer, which is what makes the list scannable.
func TestStatusBarDrawerLabelColumnAligns(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.statusPanel.section = sectionThinking
	m.relayout()

	lines := strings.Split(stripANSI(m.View().Content), "\n")
	top := drawerTopRow(m)
	col := -1
	for i, r := range m.statusPanelRows() {
		line := lines[top+1+i]
		byteAt := strings.Index(line, r.detail)
		if byteAt < 0 {
			t.Fatalf("row %q does not carry its detail %q", line, r.detail)
		}
		// The gutter's ● is multi-byte, so column offsets must be measured with
		// ui.Width rather than as byte offsets into the stripped row.
		at := ui.Width(line[:byteAt])
		if col < 0 {
			col = at
			continue
		}
		if at != col {
			t.Errorf("detail starts at column %d on row %q, want %d", at, line, col)
		}
	}
}

// TestStatusBarDrawerHeaderUsesSectionAccent checks the header is tinted with the
// accent of the block that opened the drawer, so an open drawer is visibly that
// block's list rather than anonymous rows.
func TestStatusBarDrawerHeaderUsesSectionAccent(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	if drawerAccent(sectionModel) == drawerAccent(sectionMCP) {
		t.Error("the model and mcp drawers should not share an accent")
	}
	if drawerAccent(sectionModel) != sbModelBg || drawerAccent(sectionFiles) != sbFilesBg {
		t.Error("a drawer's accent should be its bar block's color")
	}
	m.statusPanel.section = sectionThinking
	m.relayout()
	header := strings.Split(stripANSI(m.View().Content), "\n")[drawerTopRow(m)]
	if !strings.Contains(header, "thinking level") {
		t.Errorf("header = %q", header)
	}
}

// TestStatusBarDrawerRowsAtNarrowWidth verifies every drawer on a narrow terminal
// still paints full-width rows and drops text rather than overflowing.
func TestStatusBarDrawerRowsAtNarrowWidth(t *testing.T) {
	for _, sec := range []statusSection{
		sectionModel, sectionThinking, sectionSkills, sectionMCP, sectionFiles, sectionContext,
	} {
		m := statusPanelTestModel(t, "openai/gpt-4o")
		m = apply(t, m, tea.WindowSizeMsg{Width: 24, Height: 20})
		m.statusPanel.section = sec
		m.relayout()

		for _, line := range strings.Split(stripANSI(m.View().Content), "\n") {
			if got := ui.Width(line); got != 24 {
				t.Fatalf("section %v: row is %d columns, want 24: %q", sec, got, line)
			}
		}
		if m.statusPanelHeight() <= 1 {
			t.Errorf("section %v: the drawer should still list its rows at width 24", sec)
		}
	}
}

// TestContextBlockOpensDrawer verifies the context readout — the amber block that
// used to be display-only — is a drawer like the others, and that Esc dismisses it.
func TestContextBlockOpensDrawer(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m = clickBar(t, m, sectionContext)
	if m.statusPanel.section != sectionContext {
		t.Fatalf("clicking the context block should open its drawer, got %v", m.statusPanel.section)
	}
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "context · ") {
		t.Errorf("context drawer header missing:\n%s", content)
	}
	// The block is highlighted while its drawer is up, exactly like the others.
	bar := strings.Split(content, "\n")[m.height-1]
	if !strings.Contains(bar, glyphCtx) {
		t.Errorf("bar lost its context block: %q", bar)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.statusPanel.open() {
		t.Error("Esc should close the context drawer")
	}
}

// TestContextDrawerExplainsTheReadout checks the drawer's rows account for the
// number on the bar: the window it is measured against, the rule that set the
// threshold, and the distance to it. The threshold is the earliest of the reserve
// line and the [checkpoint] ladder (80% by default), so the drawer quotes the
// ladder rather than window-minus-reserve — which is what the loop compares
// against and what a reader would otherwise compute and get wrong.
func TestContextDrawerExplainsTheReadout(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.live.ContextWindow = 128_000
	m.ctxMeter.reset(128_000)
	m.ctxMeter.observeTurn(agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Usage:     &agentcore.Usage{InputTokens: 40_000, OutputTokens: 1_000},
	}, nil)
	m.statusBar.SetContext(m.ctxMeter.view())
	m.statusPanel.section = sectionContext

	rows := rowByLabel(m.contextDrawerRows())
	if got, want := rows["used"].detail, "41,000 tokens · 32% of the window"; got != want {
		t.Errorf("used = %q, want %q", got, want)
	}
	if !rows["used"].current {
		t.Error("the used row should carry the accent: it is the number the bar shows")
	}
	if got, want := rows["window"].detail, "128,000 tokens · the budget auto-compaction measures against"; got != want {
		t.Errorf("window = %q, want %q", got, want)
	}
	if got, want := rows["threshold"].detail, "102,400 tokens · the 80% threshold ladder"; got != want {
		t.Errorf("threshold = %q, want %q", got, want)
	}
	if got, want := rows["remaining"].detail, "61,400 tokens until auto-compaction"; got != want {
		t.Errorf("remaining = %q, want %q", got, want)
	}
	if rows["remaining"].warn {
		t.Error("a context well under the threshold is not a warning")
	}
	if got, want := rows["measured"].detail, "41,000 from provider usage"; got != want {
		t.Errorf("measured = %q, want %q", got, want)
	}
	if got, want := rows["compactions"].detail, "none yet · the history is intact"; got != want {
		t.Errorf("compactions = %q, want %q", got, want)
	}

	// The percentage must agree with the bar's own readout, since both are shown
	// at once (block + drawer).
	if got, want := m.contextPercent(), m.statusBar.contextPct; got != want {
		t.Errorf("drawer percent = %d, bar percent = %d; they must not disagree", got, want)
	}
}

// TestContextDrawerWarnsOverThreshold checks crossing the auto-compaction line is
// reported as a warning with the overshoot in it, instead of silently showing a
// percentage that is still under 100.
func TestContextDrawerWarnsOverThreshold(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.live.ContextWindow = 128_000
	m.ctxMeter.reset(128_000)
	// 120K is past the 80% ladder (102,400) but still under window-minus-reserve,
	// which is the case the old readout silently reported as "room left".
	m.ctxMeter.usageTokens = 120_000
	m.ctxMeter.measured = true
	m.statusPanel.section = sectionContext

	remaining := rowByLabel(m.contextDrawerRows())["remaining"]
	if !remaining.warn {
		t.Error("a context past the threshold should be flagged")
	}
	if got, want := remaining.detail, "over the threshold by 17,600 tokens"; got != want {
		t.Errorf("remaining = %q, want %q", got, want)
	}
	if !strings.Contains(stripANSI(m.View().Content), "over the threshold") {
		t.Error("the warning is not painted in the drawer")
	}
}

// TestContextDrawerWithoutWindowIsHonest checks the drawer never describes
// auto-compaction that cannot happen: with no context window in the live config
// (which is what buildConfig hands the loop) the loop refuses to compact at all,
// so the drawer says so and omits the threshold figures.
func TestContextDrawerWithoutWindowIsHonest(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.live.ContextWindow = 0
	m.ctxMeter.reset(0)
	m.ctxMeter.usageTokens = 5_000
	m.ctxMeter.measured = true
	m.statusPanel.section = sectionContext

	rows := rowByLabel(m.contextDrawerRows())
	if _, ok := rows["threshold"]; ok {
		t.Error("no window means no threshold; the row should be omitted")
	}
	if got := rows["window"].detail; !strings.Contains(got, "unknown") {
		t.Errorf("window = %q, want it to admit the window is unknown", got)
	}
	if got := rows["remaining"].detail; !strings.Contains(got, "auto-compaction is off") {
		t.Errorf("remaining = %q, want it to say auto-compaction is off", got)
	}

	// A model that reports no window but a meter seeded with the fallback budget is
	// the same trap one step later: the figure exists, the loop still has no window.
	m.ctxMeter.setWindow(cli.DefaultContextWindow)
	rows = rowByLabel(m.contextDrawerRows())
	if got := rows["window"].detail; !strings.Contains(got, "fallback") {
		t.Errorf("window = %q, want it to mark the fallback budget", got)
	}
	if got := rows["remaining"].detail; !strings.Contains(got, "auto-compaction is off") {
		t.Errorf("remaining = %q, want it to say auto-compaction is off", got)
	}
	if _, ok := rows["threshold"]; ok {
		t.Error("threshold figures must not be shown for a loop that cannot compact")
	}
}

// TestContextDrawerReportsCompactions checks the compaction row explains a history
// that has already been folded, including the size it was folded from.
func TestContextDrawerReportsCompactions(t *testing.T) {
	m := statusPanelTestModel(t, "openai/gpt-4o")
	m.ctxMeter.reset(128_000)
	m.ctxMeter.observeCompaction(agentcore.CompactionEvent{
		Reason: "threshold", TokensBefore: 118_000, TokensAfter: 21_000, Summary: "the summary",
	})
	m.statusPanel.section = sectionContext

	rows := rowByLabel(m.contextDrawerRows())
	if got, want := rows["compactions"].detail, "1 · last 118,000 → 21,000 tokens (threshold)"; got != want {
		t.Errorf("compactions = %q, want %q", got, want)
	}
	if got := rows["used"].detail; !strings.Contains(got, "21,000 tokens") {
		t.Errorf("used = %q, want the post-compaction count", got)
	}
}

// rowByLabel indexes drawer rows for assertions that should not depend on order.
func rowByLabel(rows []statusPanelRow) map[string]statusPanelRow {
	out := make(map[string]statusPanelRow, len(rows))
	for _, r := range rows {
		out[r.label] = r
	}
	return out
}
