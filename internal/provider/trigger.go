// This file implements the per-model auto-compaction trigger declaration: where
// compaction fires for one model, as stored by the registry.
//
// The trigger is declared next to the model ([[models]] compact_at, or
// `model add --compact-at`) because the right point differs by model: a model
// that publishes an input ceiling well below its combined window has to compact
// earlier than one whose window is fully usable. Two forms are accepted — an
// absolute token count, or a fraction of the input budget — and a fraction is
// deliberately kept unresolved here because the budget comes from the model's
// limits, which may themselves be a declaration or the built-in table.
package provider

import "strings"

// ModelTrigger is a declared compaction trigger: an absolute token count when
// Fraction is 0, otherwise a fraction of the model's input budget.
type ModelTrigger struct {
	// Tokens is the absolute trigger point in context tokens.
	Tokens int
	// Fraction is the trigger as a proportion of the input budget, in (0,1].
	Fraction float64
}

// IsZero reports whether anything was declared, so "no declaration" stays
// distinguishable from a declaration that resolves to nothing.
func (t ModelTrigger) IsZero() bool { return t.Tokens <= 0 && t.Fraction <= 0 }

// Resolve returns the trigger point in tokens for an input budget. It returns 0
// when nothing was declared or the budget is unknown, which the loop reads as
// "no per-model cap".
func (t ModelTrigger) Resolve(inputBudget int) int {
	switch {
	case t.Fraction > 0:
		if inputBudget <= 0 {
			return 0
		}
		return int(float64(inputBudget)*t.Fraction + 0.5)
	case t.Tokens > 0:
		return t.Tokens
	}
	return 0
}

// modelTriggers records per-model trigger declarations made by a user, keyed by
// provider name and then model id. It is the companion of modelLimits: limits
// say what the model accepts, the trigger says where to stop before that.
var modelTriggers = map[string]map[string]ModelTrigger{}

// ModelTriggerFor returns the trigger a user declared for one model, or the zero
// value when nothing was declared.
//
// Like declaredModelLimits it accepts either spelling of a routed id, because
// "mycorp/foo" and "foo" name the same model.
func ModelTriggerFor(providerName, model string) ModelTrigger {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	model = strings.TrimSpace(model)
	if providerName == "" || model == "" {
		return ModelTrigger{}
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	decls := modelTriggers[providerName]
	if len(decls) == 0 {
		return ModelTrigger{}
	}
	if v, ok := decls[model]; ok {
		return v
	}
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		if v, ok := decls[model[i+1:]]; ok {
			return v
		}
	}
	return ModelTrigger{}
}
