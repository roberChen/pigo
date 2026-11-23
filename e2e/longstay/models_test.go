package main

import (
	"testing"

	"github.com/roberChen/pigo/internal/cli/run"
	"github.com/roberChen/pigo/internal/hostapi"
	"github.com/roberChen/pigo/internal/provider"
)

// TestModelRotationIsClosedAndDistinct keeps the catalog honest: the rotation must
// return to its start, every step must be a different id, and the boot model must
// be the first one (the host's flags are built from baseModel).
func TestModelRotationIsClosedAndDistinct(t *testing.T) {
	if len(longstayModels) < 2 {
		t.Fatalf("rotation has %d models, want at least 2", len(longstayModels))
	}
	if baseModel != longstayModels[0] {
		t.Fatalf("baseModel = %q, rotation starts at %q", baseModel, longstayModels[0])
	}
	seen := map[string]bool{}
	model := baseModel
	for i := 0; i < len(longstayModels); i++ {
		if seen[model] {
			t.Fatalf("rotation revisits %q after %d steps", model, i)
		}
		seen[model] = true
		model = nextLongstayModel(model)
	}
	if model != baseModel {
		t.Fatalf("rotation ended at %q, want %q", model, baseModel)
	}
	// An unknown id starts the rotation over rather than inventing a target.
	if got := nextLongstayModel("not-a-longstay-model"); got != baseModel {
		t.Fatalf("nextLongstayModel(unknown) = %q, want %q", got, baseModel)
	}
}

// TestModelWindowsMatchBuiltinTable is the guard that makes the scenario a real
// regression test: if pigo's limit table changes, the harness's expectations must
// change with it instead of quietly asserting the old window.
func TestModelWindowsMatchBuiltinTable(t *testing.T) {
	for _, model := range longstayModels {
		want, ok := longstayModelWindow(model)
		if !ok {
			t.Fatalf("model %q has no expected window", model)
		}
		limits, _ := provider.ModelContextLimits(providerNameOpenAI, model)
		if limits.ContextWindow != want {
			t.Fatalf("%s: table says %d, harness expects %d", model, limits.ContextWindow, want)
		}
		if limits.MaxInputTokens != 0 {
			t.Fatalf("%s: table declares an input ceiling of %d; the harness assumes budget == window", model, limits.MaxInputTokens)
		}
	}
}

// TestVerifySwitchBudgetMatchesResolvedPlan runs the check against the plan the
// host would actually resolve, so the assertion cannot drift from PlanCompactionIn.
func TestVerifySwitchBudgetMatchesResolvedPlan(t *testing.T) {
	for _, model := range longstayModels {
		plan := run.PlanCompactionIn(t.TempDir(), providerNameOpenAI, model)
		res := hostapi.SessionSetModelResult{
			Session:       hostapi.SessionInfo{Model: model, Provider: providerNameOpenAI},
			ContextWindow: plan.ContextWindow,
			InputBudget:   plan.InputBudget,
			TriggerPoint:  plan.TriggerPoint,
			TriggerSource: string(plan.TriggerInfo.Source),
		}
		if err := verifySwitchBudget(res); err != nil {
			t.Fatalf("%s: rejected the host's own plan: %v", model, err)
		}
	}
}

// TestVerifySwitchBudgetRejectsStalePlan is the regression itself: a switch that
// keeps the previous model's window must fail, which is what makes the check worth
// running on every switch instead of once at startup.
func TestVerifySwitchBudgetRejectsStalePlan(t *testing.T) {
	// Pretend the session switched to the 1M model but kept the 200K plan.
	plan := run.PlanCompactionIn(t.TempDir(), providerNameOpenAI, "mock-claude-chat")
	res := hostapi.SessionSetModelResult{
		Session:       hostapi.SessionInfo{Model: "mock-gemini-chat", Provider: providerNameOpenAI},
		ContextWindow: plan.ContextWindow,
		InputBudget:   plan.InputBudget,
		TriggerPoint:  plan.TriggerPoint,
		TriggerSource: string(plan.TriggerInfo.Source),
	}
	if err := verifySwitchBudget(res); err == nil {
		t.Fatal("a plan left over from the previous model was accepted")
	}
}

// TestVerifySwitchBudgetRejectsIncoherentNumbers covers the shapes a broken or
// mis-wired host could return without naming the wrong model.
func TestVerifySwitchBudgetRejectsIncoherentNumbers(t *testing.T) {
	const model = "mock-claude-chat"
	window, _ := longstayModelWindow(model)
	base := hostapi.SessionSetModelResult{
		Session:       hostapi.SessionInfo{Model: model, Provider: providerNameOpenAI},
		ContextWindow: window,
		InputBudget:   window,
		TriggerPoint:  window * 8 / 10,
		TriggerSource: "ladder",
	}
	if err := verifySwitchBudget(base); err != nil {
		t.Fatalf("the baseline was rejected: %v", err)
	}
	cases := map[string]func(*hostapi.SessionSetModelResult){
		"unknown model":      func(r *hostapi.SessionSetModelResult) { r.Session.Model = "mystery" },
		"zero window":        func(r *hostapi.SessionSetModelResult) { r.ContextWindow = 0 },
		"budget over window": func(r *hostapi.SessionSetModelResult) { r.InputBudget = window + 1 },
		"zero budget":        func(r *hostapi.SessionSetModelResult) { r.InputBudget = 0 },
		"trigger at budget":  func(r *hostapi.SessionSetModelResult) { r.TriggerPoint = r.InputBudget },
		"trigger above":      func(r *hostapi.SessionSetModelResult) { r.TriggerPoint = r.InputBudget + 1 },
		"trigger zero":       func(r *hostapi.SessionSetModelResult) { r.TriggerPoint = 0 },
		"off source":         func(r *hostapi.SessionSetModelResult) { r.TriggerSource = "off" },
		"unknown source":     func(r *hostapi.SessionSetModelResult) { r.TriggerSource = "vibes" },
		"empty source":       func(r *hostapi.SessionSetModelResult) { r.TriggerSource = "" },
	}
	for name, mutate := range cases {
		candidate := base
		mutate(&candidate)
		if err := verifySwitchBudget(candidate); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}
