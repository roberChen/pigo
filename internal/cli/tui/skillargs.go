// This file implements the TUI's /skill argument form: the second stage of the
// multi-select picker. Every selected skill contributes one field per template
// slot its body declares ($1, $@, ${1:-default}), prefilled from the args the
// chip already carries or from the declared default; skills that declare no
// slot still get one free-text field, whose value is appended to the body just
// like "/skill <name> <text>". Committing stores the values on the chips
// (selectedSkill.Args); composeWithSkills then expands each body with them.
package tui

import (
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/cli/prompts"
	"github.com/smallnest/pigo/internal/runtime"
)

// maxSkillArgRows caps how many form rows are visible at once; a longer form
// (several skills, or a skill with many slots) scrolls around the cursor.
const maxSkillArgRows = 8

// skillArgField is one editable slot: the skill it belongs to, the slot the
// body declared, and the value being typed. Caret is a rune offset into Value.
type skillArgField struct {
	Skill string
	Param runtime.SkillParam
	// FreeText marks the synthetic slot of a skill whose body declares no
	// placeholders: whatever is typed is appended to the body.
	FreeText bool
	Value    string
	Caret    int
}

// skillArgForm is the modal argument editor shown between the skill picker and
// the chips. It owns the keyboard while active: printable text edits the focused
// field, Tab/arrows move, Enter commits, Esc returns to the picker.
type skillArgForm struct {
	theme  Theme
	active bool
	names  []string
	fields []skillArgField
	cursor int
}

// newSkillArgForm builds an inactive form bound to the theme used for its rows.
func newSkillArgForm(theme Theme) skillArgForm { return skillArgForm{theme: theme} }

// open builds the fields for the committed selection, in picker order. Each
// skill's previous args (a reopened picker, or "/skill <name> <args>") prefill
// the slots so adjusting an existing chip never loses what was typed.
func (f *skillArgForm) open(skills []*runtime.Skill, sels []selectedSkill) {
	f.names = f.names[:0]
	f.fields = f.fields[:0]
	f.cursor = 0
	for _, sel := range sels {
		s := prompts.FindSkill(skills, sel.Name)
		if s == nil {
			continue
		}
		f.names = append(f.names, s.Frontmatter.Name)
		params := runtime.SkillParams(s.Body)
		free := len(params) == 0
		if free {
			// No placeholders: one optional free-text field whose tokens the
			// engine appends after the body.
			params = []runtime.SkillParam{{Rest: true}}
		}
		values := fillParamValues(params, sel.Args)
		for i, p := range params {
			f.fields = append(f.fields, skillArgField{
				Skill:    s.Frontmatter.Name,
				Param:    p,
				FreeText: free,
				Value:    values[i],
				Caret:    len([]rune(values[i])),
			})
		}
	}
	f.active = len(f.fields) > 0
}

// close hides the form and drops its draft.
func (f *skillArgForm) close() {
	f.active = false
	f.names = nil
	f.fields = nil
	f.cursor = 0
}

// selection turns the current field values back into committed chips.
func (f skillArgForm) selection() []selectedSkill {
	out := make([]selectedSkill, 0, len(f.names))
	for _, name := range f.names {
		var params []runtime.SkillParam
		var values []string
		for _, fld := range f.fields {
			if fld.Skill != name {
				continue
			}
			params = append(params, fld.Param)
			values = append(values, fld.Value)
		}
		out = append(out, selectedSkill{Name: name, Args: runtime.SkillParamArgs(params, values)})
	}
	return out
}

// fillParamValues maps previously carried tokens onto params: positional slots
// take their token by index, the rest slot takes everything after the highest
// positional slot. Empty values fall back to a declared default.
func fillParamValues(params []runtime.SkillParam, tokens []string) []string {
	vals := make([]string, len(params))
	maxIdx := 0
	for _, p := range params {
		if !p.Rest && p.Index > maxIdx {
			maxIdx = p.Index
		}
	}
	for i, p := range params {
		switch {
		case p.Rest && maxIdx < len(tokens):
			vals[i] = strings.Join(tokens[maxIdx:], " ")
		case !p.Rest && p.Index <= len(tokens):
			vals[i] = tokens[p.Index-1]
		}
		if vals[i] == "" {
			vals[i] = p.Default
		}
	}
	return vals
}

// move shifts the focus by delta fields, stopping at the ends.
func (f *skillArgForm) move(delta int) {
	if len(f.fields) == 0 {
		return
	}
	f.cursor += delta
	if f.cursor < 0 {
		f.cursor = 0
	}
	if f.cursor >= len(f.fields) {
		f.cursor = len(f.fields) - 1
	}
}

// field returns the focused field (nil when the form has none).
func (f *skillArgForm) field() *skillArgField {
	if !f.active || f.cursor < 0 || f.cursor >= len(f.fields) {
		return nil
	}
	return &f.fields[f.cursor]
}

// insert types s at the caret of the focused field.
func (f *skillArgForm) insert(s string) {
	fld := f.field()
	if fld == nil || s == "" {
		return
	}
	runes := []rune(fld.Value)
	if fld.Caret > len(runes) {
		fld.Caret = len(runes)
	}
	next := make([]rune, 0, len(runes)+len([]rune(s)))
	next = append(next, runes[:fld.Caret]...)
	next = append(next, []rune(s)...)
	next = append(next, runes[fld.Caret:]...)
	fld.Value = string(next)
	fld.Caret += len([]rune(s))
}

// backspace deletes the rune before the caret.
func (f *skillArgForm) backspace() {
	fld := f.field()
	if fld == nil {
		return
	}
	runes := []rune(fld.Value)
	if fld.Caret > len(runes) {
		fld.Caret = len(runes)
	}
	if fld.Caret == 0 {
		return
	}
	f.deleteAt(fld.Caret - 1)
}

// deleteForward deletes the rune under the caret.
func (f *skillArgForm) deleteForward() {
	if fld := f.field(); fld != nil {
		f.deleteAt(fld.Caret)
	}
}

// deleteAt removes the rune at i and shifts the caret left when it sat after it.
func (f *skillArgForm) deleteAt(i int) {
	fld := f.field()
	if fld == nil {
		return
	}
	runes := []rune(fld.Value)
	if i < 0 || i >= len(runes) {
		return
	}
	fld.Value = string(append(runes[:i], runes[i+1:]...))
	if fld.Caret > i {
		fld.Caret--
	}
}

// left/right/home/end move the caret inside the focused field.
func (f *skillArgForm) left() {
	if fld := f.field(); fld != nil && fld.Caret > 0 {
		fld.Caret--
	}
}

func (f *skillArgForm) right() {
	if fld := f.field(); fld != nil && fld.Caret < len([]rune(fld.Value)) {
		fld.Caret++
	}
}

func (f *skillArgForm) home() {
	if fld := f.field(); fld != nil {
		fld.Caret = 0
	}
}

func (f *skillArgForm) end() {
	if fld := f.field(); fld != nil {
		fld.Caret = len([]rune(fld.Value))
	}
}

// rows reports how many terminal rows the form occupies.
func (f skillArgForm) rows() int {
	if !f.active {
		return 0
	}
	n := len(f.fields)
	if n > maxSkillArgRows {
		n = maxSkillArgRows
	}
	if n == 0 {
		n = 1
	}
	return 1 + n
}

// view renders the form: a title line, then one row per visible field with the
// focused row marked by "›" and a caret inside its value.
func (f skillArgForm) view(width int) string {
	if !f.active {
		return ""
	}
	rowWidth := width - 2
	if rowWidth < 1 {
		rowWidth = width
	}
	var b strings.Builder
	b.WriteString(TruncateToWidth(f.theme.Accent.Render("skill arguments  (Tab/↑/↓ move · Enter apply · Esc back)"), width))
	b.WriteByte('\n')
	start, end := pickerWindow(f.cursor, len(f.fields), maxSkillArgRows)
	for i := start; i < end; i++ {
		fld := f.fields[i]
		slot := fld.Param.Syntax()
		if fld.FreeText {
			slot = "args (optional)"
		}
		label := fmt.Sprintf("%s · %s", fld.Skill, slot)
		line := TruncateToWidth(label+"  ["+caretValue(fld)+"]", rowWidth)
		if i > start {
			b.WriteByte('\n')
		}
		if i == f.cursor {
			b.WriteString(f.theme.Accent.Render("› " + line))
		} else {
			b.WriteString(f.theme.System.Render("  " + line))
		}
	}
	return b.String()
}

// caretValue renders a field value with the text caret ("▏") inserted at Caret.
func caretValue(fld skillArgField) string {
	runes := []rune(fld.Value)
	at := fld.Caret
	if at < 0 {
		at = 0
	}
	if at > len(runes) {
		at = len(runes)
	}
	return string(runes[:at]) + "▏" + string(runes[at:])
}
