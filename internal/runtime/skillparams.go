// This file implements skill argument-slot discovery. A skill body may declare
// template placeholders ($1, $@, ${1:-default}, …); the TUI's /skill form uses
// this to show one field per slot it has to ask for, and SkillParamArgs turns
// the filled values back into the token list ExpandTemplate consumes. Parsing
// mirrors ExpandTemplate's grammar exactly, so the form never offers a slot the
// expansion would ignore.
package runtime

import (
	"strconv"
	"strings"
)

// SkillParam is one argument slot a skill body declares. Index is the 1-based
// positional slot ($1 -> 1) and Rest marks the all-arguments slot ($@,
// $ARGUMENTS, ${@:N}). Default carries the literal from ${N:-default} /
// ${@:-default}; it is empty when the placeholder declares none.
type SkillParam struct {
	Index   int
	Rest    bool
	Default string
}

// Syntax renders the slot's canonical placeholder, e.g. "$1" or "$@".
func (p SkillParam) Syntax() string {
	if p.Rest {
		return "$@"
	}
	return "$" + strconv.Itoa(p.Index)
}

// SkillParams returns the argument slots body declares, ordered: positional
// slots 1..N ascending, then the all-arguments slot when the body joins every
// argument ($@, $ARGUMENTS, ${@:N} or ${@:N:L}). A body without placeholders
// returns nil. When a slot is defaulted more than once, the first non-empty
// default wins.
func SkillParams(body string) []SkillParam {
	defaults := make(map[int]string)
	seen := make(map[int]bool)
	rest, restDefault := false, ""
	for i := 0; i+1 < len(body); i++ {
		if body[i] != '$' {
			continue
		}
		next := body[i+1]
		switch {
		case next == '{':
			end := strings.IndexByte(body[i+2:], '}')
			if end < 0 {
				continue
			}
			name, def, form := splitBraced(body[i+2 : i+2+end])
			i += 2 + end
			switch form {
			case bracedSlice:
				// ${@:N} / ${@:N:L} consume the argument list, so the form asks
				// for the whole list and the engine slices it.
				rest = true
			case bracedDefault:
				if name == "@" || name == "ARGUMENTS" {
					rest = true
					if restDefault == "" {
						restDefault = def
					}
					continue
				}
				if idx, err := strconv.Atoi(name); err == nil && idx >= 1 {
					markPositional(seen, defaults, idx, def)
				}
			default:
				if name == "@" || name == "ARGUMENTS" {
					rest = true
					continue
				}
				if idx, err := strconv.Atoi(name); err == nil && idx >= 1 {
					markPositional(seen, defaults, idx, "")
				}
			}
		case next == '@':
			rest = true
			i++
		case isDigitByte(next):
			j := i + 1
			for j < len(body) && isDigitByte(body[j]) {
				j++
			}
			if idx, _ := strconv.Atoi(body[i+1 : j]); idx >= 1 {
				markPositional(seen, defaults, idx, "")
			}
			i = j - 1
		case strings.HasPrefix(body[i+1:], "ARGUMENTS"):
			rest = true
			i += len("ARGUMENTS")
		}
	}
	maxIdx := 0
	for idx := range seen {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	if maxIdx == 0 && !rest {
		return nil
	}
	out := make([]SkillParam, 0, len(seen)+1)
	for idx := 1; idx <= maxIdx; idx++ {
		if seen[idx] {
			out = append(out, SkillParam{Index: idx, Default: defaults[idx]})
		}
	}
	if rest {
		out = append(out, SkillParam{Rest: true, Default: restDefault})
	}
	return out
}

// bracedForm names the three ${...} shapes ExpandTemplate understands.
type bracedForm int

const (
	bracedPlain   bracedForm = iota // ${1}, ${@}
	bracedDefault                   // ${1:-x}
	bracedSlice                     // ${@:2}, ${@:2:3}
)

// splitBraced splits the inside of ${...} the same way expandBraced dispatches:
// a ":-" wins over a bare ":".
func splitBraced(inner string) (name, def string, form bracedForm) {
	if idx := strings.Index(inner, ":-"); idx >= 0 {
		return inner[:idx], inner[idx+2:], bracedDefault
	}
	if idx := strings.IndexByte(inner, ':'); idx >= 0 {
		return inner[:idx], "", bracedSlice
	}
	return inner, "", bracedPlain
}

// markPositional records that a positional slot is used and, when the slot is
// defaulted, the default to prefill the form with.
func markPositional(seen map[int]bool, defaults map[int]string, idx int, def string) {
	seen[idx] = true
	if defaults[idx] == "" && def != "" {
		defaults[idx] = def
	}
}

// ParamHint renders the slots a skill body declares as "($1 $2 $@)" for
// listings and picker rows, or "" when the body declares none.
func (s *Skill) ParamHint() string {
	params := SkillParams(s.Body)
	if len(params) == 0 {
		return ""
	}
	syntaxes := make([]string, len(params))
	for i, p := range params {
		syntaxes[i] = p.Syntax()
	}
	return "(" + strings.Join(syntaxes, " ") + ")"
}

// SkillParamArgs converts form values (one per SkillParam, same order) into the
// token list ExpandTemplate consumes. Positional values fill their slot and holes
// are padded with empty strings so later slots keep their index; the rest value
// is shell-tokenized, falling back to one literal token when it cannot be parsed
// (matching SlashCommand.Expand's treatment of a malformed argument string).
func SkillParamArgs(params []SkillParam, values []string) []string {
	var positional, rest []string
	for i, p := range params {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		if p.Rest {
			tokens, err := SplitArgs(v)
			if err != nil {
				if strings.TrimSpace(v) != "" {
					rest = []string{v}
				}
				continue
			}
			rest = tokens
			continue
		}
		for len(positional) < p.Index-1 {
			positional = append(positional, "")
		}
		if len(positional) < p.Index {
			positional = append(positional, v)
			continue
		}
		positional[p.Index-1] = v
	}
	// Drop trailing empty slots: a missing positional already expands to "", so
	// keeping the padding would only make the chip claim empty arguments.
	all := append(positional, rest...)
	for len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if len(all) == 0 {
		return nil
	}
	return all
}
