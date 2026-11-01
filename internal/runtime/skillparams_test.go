package runtime

// Tests for skill argument-slot discovery: the /skill form derives its fields
// from the body's placeholders and converts the filled values back into the
// token list ExpandTemplate consumes.

import (
	"reflect"
	"testing"
)

func TestSkillParamsNoPlaceholders(t *testing.T) {
	if got := SkillParams("Just do the thing."); got != nil {
		t.Errorf("SkillParams = %v, want nil", got)
	}
}

func TestSkillParamsPositional(t *testing.T) {
	got := SkillParams("Compare $1 with $2 and $1 again.")
	want := []SkillParam{{Index: 1}, {Index: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkillParams = %#v, want %#v", got, want)
	}
}

func TestSkillParamsRestForms(t *testing.T) {
	for _, body := range []string{
		"do $@",
		"do $ARGUMENTS",
		"do ${@}",
		"do ${ARGUMENTS}",
		"do ${@:2}",
		"do ${@:2:3}",
	} {
		got := SkillParams(body)
		if len(got) != 1 || !got[0].Rest {
			t.Errorf("SkillParams(%q) = %#v, want one rest slot", body, got)
		}
	}
}

func TestSkillParamsDefaults(t *testing.T) {
	got := SkillParams("branch ${1:-main} against ${@:-HEAD}")
	want := []SkillParam{{Index: 1, Default: "main"}, {Rest: true, Default: "HEAD"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkillParams = %#v, want %#v", got, want)
	}
}

func TestSkillParamsKeepsHolesAndOrder(t *testing.T) {
	got := SkillParams("$3 then $1")
	want := []SkillParam{{Index: 1}, {Index: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkillParams = %#v, want %#v", got, want)
	}
}

func TestSkillParamsRepeatedDefaultKeepsFirst(t *testing.T) {
	got := SkillParams("${1:-a} ${1:-b}")
	if len(got) != 1 || got[0].Default != "a" {
		t.Errorf("SkillParams = %#v, want slot 1 defaulting to a", got)
	}
}

func TestSkillParamSyntax(t *testing.T) {
	if got := (SkillParam{Index: 2}).Syntax(); got != "$2" {
		t.Errorf("Syntax() = %q, want $2", got)
	}
	if got := (SkillParam{Rest: true}).Syntax(); got != "$@" {
		t.Errorf("Syntax() = %q, want $@", got)
	}
}

func TestSkillParamArgs(t *testing.T) {
	params := []SkillParam{{Index: 1}, {Index: 2}, {Rest: true}}
	got := SkillParamArgs(params, []string{"main", "origin", "release v1 'two words'"})
	want := []string{"main", "origin", "release", "v1", "two words"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkillParamArgs = %#v, want %#v", got, want)
	}
}

func TestSkillParamArgsPadsHoles(t *testing.T) {
	params := []SkillParam{{Index: 1}, {Index: 3}}
	got := SkillParamArgs(params, []string{"a", "c"})
	want := []string{"a", "", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SkillParamArgs = %#v, want %#v", got, want)
	}
}

func TestSkillParamArgsMalformedQuote(t *testing.T) {
	params := []SkillParam{{Rest: true}}
	got := SkillParamArgs(params, []string{"it's"})
	if !reflect.DeepEqual(got, []string{"it's"}) {
		t.Errorf("SkillParamArgs = %#v, want the literal token", got)
	}
}

func TestSkillExpandArgs(t *testing.T) {
	s := &Skill{Body: "Review $1 against ${2:-main}."}
	if got := s.ExpandArgs([]string{"feature", ""}); got != "Review feature against main." {
		t.Errorf("ExpandArgs = %q", got)
	}
}

func TestSkillParamArgsDropsTrailingEmptySlots(t *testing.T) {
	params := []SkillParam{{Index: 1}, {Index: 2}}
	if got := SkillParamArgs(params, []string{"", ""}); got != nil {
		t.Errorf("SkillParamArgs = %#v, want nil when every slot is empty", got)
	}
	if got := SkillParamArgs(params, []string{"", "b"}); !reflect.DeepEqual(got, []string{"", "b"}) {
		t.Errorf("SkillParamArgs = %#v, want the hole kept", got)
	}
}
