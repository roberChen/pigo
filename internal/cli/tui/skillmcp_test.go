package tui

// Tests for the /skill multi-select picker and the selected-skills chips, and
// for the /mcp server switch overlay. The skill picker is exercised without a
// live session (Options.Skills is the fallback the model reads), while the MCP
// overlay's apply path is covered by internal/mcp's Enable/Disable tests — here
// we only check the UI contract: opening, rendering, toggling the draft, the
// committed chips, and the one-shot injection into the submitted prompt.

import (
	"context"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/runtime"
)

// skillFixture builds the two-skill catalog the picker tests select from.
func skillFixture() []*runtime.Skill {
	return []*runtime.Skill{
		{Frontmatter: runtime.SkillFrontmatter{Name: "commit", Description: "write a commit message"}, Body: "COMMIT BODY"},
		{Frontmatter: runtime.SkillFrontmatter{Name: "review-it", Description: "review the diff"}, Body: "REVIEW BODY"},
	}
}

// skillTestModel builds a sized model holding the fixture skills in its launch
// options and a stub run seam that records the composed prompt.
func skillTestModel(t *testing.T) (Model, *string) {
	t.Helper()
	m := NewModel(Options{Skills: skillFixture()})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	var ran string
	m.startRunFn = func(prompt string) (chan tea.Msg, tea.Cmd) {
		ran = prompt
		return make(chan tea.Msg, 1), func() tea.Msg { return nil }
	}
	return m, &ran
}

// TestSkillPickerOpensAndSelects verifies bare /skill opens the overlay, Space
// checks the highlighted row, and Enter commits it into the chips above the
// composer while clearing the composer buffer.
func TestSkillPickerOpensAndSelects(t *testing.T) {
	m, _ := skillTestModel(t)

	m.input.SetValue("/skill")
	next, _ := m.submit()
	m = next.(Model)
	if !m.skillPicker.active {
		t.Fatalf("bare /skill should open the picker; transcript:\n%s", stripANSI(m.View().Content))
	}
	if got := m.input.Value(); got != "" {
		t.Errorf("composer should be cleared, got %q", got)
	}
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "select skills") {
		t.Errorf("picker overlay not rendered:\n%s", content)
	}
	if !strings.Contains(content, "commit") || !strings.Contains(content, "review-it") {
		t.Errorf("picker should list both skills:\n%s", content)
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = next.(Model)
	if !m.skillPicker.chosen["commit"] {
		t.Errorf("Space should check the highlighted skill")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.skillPicker.active {
		t.Errorf("Enter should close the picker")
	}
	if !m.skillArgForm.active {
		t.Fatalf("Enter should open the argument form; transcript:\n%s", stripANSI(m.View().Content))
	}
	if len(m.selectedSkills) != 0 {
		t.Fatalf("chips should not commit before the form is applied, got %v", m.selectedSkills)
	}
	// The form always offers at least the optional free-text field, so a second
	// Enter commits the selection without arguments.
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.skillArgForm.active {
		t.Fatal("Enter on the form should apply and close it")
	}
	if len(m.selectedSkills) != 1 || m.selectedSkills[0].Name != "commit" {
		t.Fatalf("selectedSkills = %v, want [commit]", m.selectedSkills)
	}
	if chips := stripANSI(m.View().Content); !strings.Contains(chips, "⚡ skills: commit") {
		t.Errorf("chips strip missing from the shell:\n%s", chips)
	}
}

// TestSkillPickerFilterAndCancel verifies typing filters the rows and Esc
// discards the draft without touching the committed selection.
func TestSkillPickerFilterAndCancel(t *testing.T) {
	m, _ := skillTestModel(t)
	m.setSelectedSkills([]selectedSkill{{Name: "commit"}})

	next, _ := m.openSkillPicker("/skill")
	m = next.(Model)
	if !m.skillPicker.chosen["commit"] {
		t.Fatal("reopening the picker should seed the draft from the chips")
	}
	for _, ch := range "rev" {
		next, _ = m.Update(tea.KeyPressMsg{Text: string(ch)})
		m = next.(Model)
	}
	if n := len(m.skillPicker.filtered); n != 1 {
		t.Fatalf("filtered rows = %d, want 1", n)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if m.skillPicker.active {
		t.Fatal("Esc should close the picker")
	}
	if len(m.selectedSkills) != 1 || m.selectedSkills[0].Name != "commit" {
		t.Fatalf("Esc must not change the committed selection, got %v", m.selectedSkills)
	}
}

// TestSkillChipsComposeIntoNextMessage verifies the committed chips are injected
// into exactly the next submitted prompt (and the transcript still shows what the
// user typed), then cleared so the following turn is plain.
func TestSkillChipsComposeIntoNextMessage(t *testing.T) {
	m, ran := skillTestModel(t)
	m.setSelectedSkills([]selectedSkill{{Name: "commit"}, {Name: "review-it"}})

	m.input.SetValue("ship it")
	next, _ := m.submit()
	m = next.(Model)

	if !strings.Contains(*ran, `<skill name="commit">`) || !strings.Contains(*ran, "COMMIT BODY") {
		t.Errorf("composed prompt missing the first skill:\n%s", *ran)
	}
	if !strings.Contains(*ran, `<skill name="review-it">`) || !strings.Contains(*ran, "REVIEW BODY") {
		t.Errorf("composed prompt missing the second skill:\n%s", *ran)
	}
	if !strings.HasSuffix(*ran, "ship it") {
		t.Errorf("composed prompt should end with the typed text:\n%s", *ran)
	}
	if len(m.selectedSkills) != 0 {
		t.Errorf("selection should be one-shot, still have %v", m.selectedSkills)
	}
	if got := m.skillChipsRows(); got != 0 {
		t.Errorf("chip row should be reclaimed after submit, got %d", got)
	}
}

// TestSkillClearCommand verifies "/skill clear" drops the chips.
func TestSkillClearCommand(t *testing.T) {
	m, _ := skillTestModel(t)
	m.setSelectedSkills([]selectedSkill{{Name: "commit"}})
	next, _ := m.runSlash("/skill clear")
	m = next.(Model)
	if len(m.selectedSkills) != 0 {
		t.Errorf("selectedSkills = %v, want none", m.selectedSkills)
	}
}

// TestSkillChipsReserveLayoutRow verifies the strip takes a shell row while a
// skill is selected, so the transcript shrinks instead of overlapping the input.
func TestSkillChipsReserveLayoutRow(t *testing.T) {
	m, _ := skillTestModel(t)
	before := m.transcript.vp.Height()
	if got := m.skillChipsRows(); got != 0 {
		t.Fatalf("no selection should reserve no row, got %d", got)
	}
	m.setSelectedSkills([]selectedSkill{{Name: "commit"}})
	m.relayout()
	if got := m.skillChipsRows(); got != 1 {
		t.Fatalf("skillChipsRows = %d, want 1", got)
	}
	if got := m.transcript.vp.Height(); got != before-1 {
		t.Errorf("transcript height = %d, want %d after reserving the strip", got, before-1)
	}
}

// TestMCPPickerRendersServerState verifies bare /mcp opens the server overlay,
// renders each server's on/off state, and that Enter just closes it (the switch
// itself happens on Space, applied immediately).
func TestMCPPickerRendersServerState(t *testing.T) {
	m := NewModel(Options{})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.mcpPicker.open([]mcp.ServerStatus{
		{Name: "github", Active: true, Tools: 3},
		{Name: "search", Active: false, Error: "boom"},
	})
	m.relayout()
	content := stripANSI(m.View().Content)
	for _, want := range []string{"mcp servers", "github", "on · 3 tools", "search", "boom"} {
		if !strings.Contains(content, want) {
			t.Errorf("overlay missing %q:\n%s", want, content)
		}
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.mcpPicker.active {
		t.Error("Enter should close the MCP overlay")
	}
}

// TestMCPToggleDoneAppliesResult verifies the completion message clears the
// in-flight marker, prints the status line and rewires the session's tools (a
// session-less model is used, so only the UI bookkeeping is observable).
func TestMCPToggleDoneAppliesResult(t *testing.T) {
	m := NewModel(Options{})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.mcpPicker.open([]mcp.ServerStatus{{Name: "github", Active: false}})
	m.mcpPicker.pending["github"] = true
	m.running = true

	next, _ := m.Update(mcpToggleDoneMsg{name: "github", active: true, message: "MCP server github: on (2 tools)"})
	m = next.(Model)
	if m.mcpPicker.pending["github"] {
		t.Error("pending marker should be cleared")
	}
	if m.running {
		t.Error("the switch should end the busy state")
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "MCP server github: on (2 tools)") {
		t.Errorf("status line missing from the transcript:\n%s", content)
	}
}

// TestMCPCommandWithoutManagerReportsUnavailable verifies /mcp degrades to a
// note (not a panic) when no session/manager is wired, e.g. in tests.
func TestMCPCommandWithoutManagerReportsUnavailable(t *testing.T) {
	m := NewModel(Options{})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	next, _ := m.runSlash("/mcp")
	m = next.(Model)
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "no MCP servers configured") && !strings.Contains(content, "MCP servers unavailable") {
		t.Errorf("expected an explanatory note:\n%s", content)
	}
}

// TestMCPPickerToggleBlocksSubmit verifies a Space switch marks the model busy
// for its duration: a prompt cannot start while the switch mutates the live tool
// set on the UI goroutine, and the completion message ends the busy state.
func TestMCPPickerToggleBlocksSubmit(t *testing.T) {
	mgr := mcp.Connect(context.Background(), nil, io.Discard)
	m := NewModel(Options{})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.session = &runSession{mcp: mgr}
	m.mcpPicker.open([]mcp.ServerStatus{{Name: "github"}})
	m.relayout()

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = next.(Model)
	if !m.running {
		t.Fatal("a picker switch should mark the model busy")
	}
	if !m.mcpPicker.pending["github"] {
		t.Error("the row should show switching…")
	}
	// Enter must not submit a prompt mid-switch.
	m.input.SetValue("hello")
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := next.(Model).input.Value(); got != "hello" {
		t.Errorf("Enter during a switch must not submit, input = %q", got)
	}
	if got := m.View().Content; !strings.Contains(stripANSI(got), "Switching MCP server") {
		t.Errorf("spinner label missing:\n%s", stripANSI(got))
	}
}
