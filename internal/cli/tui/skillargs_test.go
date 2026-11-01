package tui

// Tests for the /skill argument form: the fields it derives from a skill body's
// template placeholders, the prefill rules, the keyboard contract (Tab/arrows
// move, Enter applies, Esc backs out) and the way the committed values expand
// the skill into the next message.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/runtime"
)

// skillParamFixture builds a catalog with one positional/defaulted skill and one
// skill without placeholders, so the form's field derivation and its free-text
// fallback are both exercised.
func skillParamFixture() []*runtime.Skill {
	return []*runtime.Skill{
		{Frontmatter: runtime.SkillFrontmatter{Name: "review", Description: "review a change"}, Body: "Review $1 against ${2:-main}."},
		{Frontmatter: runtime.SkillFrontmatter{Name: "note", Description: "write a note"}, Body: "NOTE BODY"},
	}
}

// paramTestModel builds a sized model over skillParamFixture with a run seam
// that records the composed prompt.
func paramTestModel(t *testing.T) (Model, *string) {
	t.Helper()
	m := NewModel(Options{Skills: skillParamFixture()})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	var ran string
	m.startRunFn = func(prompt string) (chan tea.Msg, tea.Cmd) {
		ran = prompt
		return make(chan tea.Msg, 1), func() tea.Msg { return nil }
	}
	return m, &ran
}

// TestSkillFormDerivesFieldsFromPlaceholders verifies one field per declared slot
// ($1 and ${2:-main}), that the default prefills the second field, and that the
// picker row advertises the slots before they are selected.
func TestSkillFormDerivesFieldsFromPlaceholders(t *testing.T) {
	m, _ := paramTestModel(t)

	next, _ := m.openSkillPicker("/skill")
	m = next.(Model)
	if content := stripANSI(m.View().Content); !strings.Contains(content, "($1 $2)") {
		t.Errorf("picker row should announce the declared slots:\n%s", content)
	}

	next, _ = m.openSkillArgs([]selectedSkill{{Name: "review"}})
	m = next.(Model)
	if !m.skillArgForm.active {
		t.Fatal("the form should open for a skill with placeholders")
	}
	if len(m.skillArgForm.fields) != 2 {
		t.Fatalf("fields = %d, want 2", len(m.skillArgForm.fields))
	}
	if got := m.skillArgForm.fields[0]; got.Value != "" || got.Param.Syntax() != "$1" {
		t.Errorf("first field = %#v, want an empty $1", got)
	}
	if got := m.skillArgForm.fields[1]; got.Value != "main" || got.Param.Syntax() != "$2" {
		t.Errorf("second field = %#v, want $2 prefilled with main", got)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "skill arguments") {
		t.Errorf("form overlay not rendered:\n%s", content)
	}
}

// TestSkillFormTypingAndApply verifies typed values land on the chips and expand
// the skill body with its argument tokens when the message is submitted.
func TestSkillFormTypingAndApply(t *testing.T) {
	m, ran := paramTestModel(t)
	next, _ := m.openSkillArgs([]selectedSkill{{Name: "review"}})
	m = next.(Model)

	m = typeText(t, m, "fix")
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = next.(Model)
	if m.skillArgForm.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after Tab", m.skillArgForm.cursor)
	}
	// The default is prefilled at the caret; clear it before typing.
	for range "main" {
		next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m = next.(Model)
	}
	m = typeText(t, m, "dev")

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.skillArgForm.active {
		t.Fatal("Enter should apply and close the form")
	}
	if len(m.selectedSkills) != 1 || m.selectedSkills[0].Name != "review" {
		t.Fatalf("selectedSkills = %v", m.selectedSkills)
	}
	if got := m.selectedSkills[0].Args; len(got) != 2 || got[0] != "fix" || got[1] != "dev" {
		t.Fatalf("Args = %v, want [fix dev]", got)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "review(fix dev)") {
		t.Errorf("chips should show the arguments:\n%s", content)
	}

	m.input.SetValue("please")
	next, _ = m.submit()
	m = next.(Model)
	if !strings.Contains(*ran, "Review fix against dev.") {
		t.Errorf("composed prompt should carry the substituted body:\n%s", *ran)
	}
	if !strings.Contains(*ran, `args="fix dev"`) {
		t.Errorf("composed prompt should record the args attribute:\n%s", *ran)
	}
	if !strings.HasSuffix(*ran, "please") {
		t.Errorf("composed prompt should end with the typed text:\n%s", *ran)
	}
}

// TestSkillNamedArgsSelectsInsteadOfRunning verifies "/skill <name> <args>" no
// longer starts a turn: it opens the form with the args prefilled, and nothing
// runs until the user applies the form and submits a message.
func TestSkillNamedArgsSelectsInsteadOfRunning(t *testing.T) {
	m, ran := paramTestModel(t)
	next, _ := m.runSlash("/skill review fix dev")
	m = next.(Model)

	if *ran != "" {
		t.Fatalf("a named /skill must not run immediately, ran %q", *ran)
	}
	if !m.skillArgForm.active {
		t.Fatal("a named /skill should open the argument form")
	}
	if len(m.selectedSkills) != 0 {
		t.Fatalf("chips should stay untouched until the form is applied, got %v", m.selectedSkills)
	}
	if got := m.skillArgForm.fields[0].Value; got != "fix" {
		t.Errorf("$1 = %q, want fix", got)
	}
	if got := m.skillArgForm.fields[1].Value; got != "dev" {
		t.Errorf("$2 = %q, want dev", got)
	}
}

// TestSkillFormEscReturnsToPicker verifies Esc backs out to the picker with the
// checked skill and the typed value still present.
func TestSkillFormEscReturnsToPicker(t *testing.T) {
	m, _ := paramTestModel(t)
	next, _ := m.openSkillArgs([]selectedSkill{{Name: "review"}})
	m = next.(Model)
	m = typeText(t, m, "fix")

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if m.skillArgForm.active {
		t.Fatal("Esc should close the form")
	}
	if !m.skillPicker.active || !m.skillPicker.chosen["review"] {
		t.Fatalf("Esc should return to the picker with review checked")
	}
	// The typed value survives, and so does the default the form prefilled.
	if got := m.skillPicker.args["review"]; len(got) != 2 || got[0] != "fix" || got[1] != "main" {
		t.Errorf("typed args should survive the back navigation, got %v", got)
	}
}

// TestSkillFormPrefillsCarriedArgs verifies re-entering the picker from an
// existing chip keeps both the selection and its argument values.
func TestSkillFormPrefillsCarriedArgs(t *testing.T) {
	m, _ := paramTestModel(t)
	m.setSelectedSkills([]selectedSkill{{Name: "review", Args: []string{"fix", "dev"}}})

	next, _ := m.openSkillPicker("/skill")
	m = next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if !m.skillArgForm.active {
		t.Fatal("Enter in the picker should open the form")
	}
	if got := m.skillArgForm.fields[0].Value; got != "fix" {
		t.Errorf("$1 = %q, want the carried fix", got)
	}
	if got := m.skillArgForm.fields[1].Value; got != "dev" {
		t.Errorf("$2 = %q, want the carried dev", got)
	}
}

// TestSkillFormFreeTextForParameterlessSkill verifies a skill without
// placeholders still gets one optional field whose tokens are appended to the
// body, matching the textual "/skill <name> <text>" behavior.
func TestSkillFormFreeTextForParameterlessSkill(t *testing.T) {
	m, ran := paramTestModel(t)
	next, _ := m.openSkillArgs([]selectedSkill{{Name: "note"}})
	m = next.(Model)
	if len(m.skillArgForm.fields) != 1 || !m.skillArgForm.fields[0].Param.Rest {
		t.Fatalf("fields = %#v, want one optional rest field", m.skillArgForm.fields)
	}
	m = typeText(t, m, "remember this")
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)

	m.input.SetValue("go on")
	next, _ = m.submit()
	m = next.(Model)
	if !strings.Contains(*ran, "NOTE BODY\n\nremember this") {
		t.Errorf("free-text args should be appended to the body:\n%s", *ran)
	}
}

// TestSkillFormCaretEditing verifies left/backspace edit inside a prefilled
// value instead of appending to it.
func TestSkillFormCaretEditing(t *testing.T) {
	m, _ := paramTestModel(t)
	next, _ := m.openSkillArgs([]selectedSkill{{Name: "review"}})
	m = next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = next.(Model) // focus $2, prefilled "main"

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m = next.(Model)
	m = typeText(t, m, "pre-")
	if got := m.skillArgForm.fields[1].Value; got != "pre-main" {
		t.Errorf("value = %q, want pre-main", got)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	m = next.(Model)
	if got := m.skillArgForm.fields[1].Value; got != "pre-min" {
		t.Errorf("value = %q, want pre-min after Delete", got)
	}
}
