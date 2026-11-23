// This file implements the model catalog a long-stay run switches between.
//
// The host under test runs with an explicit `--provider openai`, so a switch only
// changes the model id — the provider, credential, tools and conversation all stay
// put. That is exactly the seam worth exercising: after the switch the session must
// be answered by the new model id AND auto-compaction must be planned for the new
// model's window, because a session left on the old plan compacts too late (or too
// early) for the model that now answers it.
//
// The ids are chosen so their windows differ, which is what makes the budget check
// meaningful. Each one is spelled to match a family rule in pigo's built-in limit
// table (see internal/provider/limits.go), and the numbers below mirror it: a
// catalog change there must fail here rather than pass silently.
package main

import (
	"fmt"

	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/hostapi"
)

// longstayModels is the rotation, starting with the model the host boots on.
var longstayModels = []string{
	// The boot model: no family rule matches it, so it gets the provider default.
	"mock-chat",
	// Matches the "claude" family rule.
	"mock-claude-chat",
	// Matches the "gemini" family rule.
	"mock-gemini-chat",
}

// providerNameOpenAI is the provider the host is started with. A model switch is
// supposed to change the model only; if it also changes this, the session is no
// longer the one the harness assembled (tools, memory, hooks) and the check is a
// failure rather than a nice-to-have.
const providerNameOpenAI = "openai"

// baseModel is the model the host is started with and every session is created
// on. It is written out rather than read from the slice because the host's flags
// are built from a constant; a test keeps the two in step.
const baseModel = "mock-chat"

// longstayModelWindow is the combined context window the host is expected to
// resolve for a model in the rotation. It mirrors the built-in limit table:
// openai's provider default is 272K, the "claude" rule is 200K and the "gemini"
// rule is 1M. None of them declares a smaller input ceiling, so the input budget
// equals the window and the ladder fires at 80%.
func longstayModelWindow(model string) (int, bool) {
	switch model {
	case "mock-chat":
		return 272_000, true
	case "mock-claude-chat":
		return 200_000, true
	case "mock-gemini-chat":
		return 1_000_000, true
	}
	return 0, false
}

// nextLongstayModel returns the model to switch to after current. An unknown
// current model (a session this client has not switched yet, or a switch that
// landed on an id the harness does not know) starts the rotation over.
func nextLongstayModel(current string) string {
	for i, model := range longstayModels {
		if model == current {
			return longstayModels[(i+1)%len(longstayModels)]
		}
	}
	return longstayModels[0]
}

// verifySwitchBudget checks the budget the host resolved for a switch against
// what pigo's built-in limit table says the new model should carry.
//
// The default policy is the 80% ladder: the trigger is
// min(80% × input budget, budget − reserve), and reserve is pi's 16384. Rather
// than hardcoding a reserve (a user config may change it), the check asserts the
// relationships that must hold for any sane plan: the window is the one the model
// has, the budget never exceeds it, and the trigger sits strictly inside the
// budget with room left for the reply. A stale plan — the old model's window
// still attached — fails the first assertion, which is the regression this
// scenario exists for.
func verifySwitchBudget(res hostapi.SessionSetModelResult) error {
	want, ok := longstayModelWindow(res.Session.Model)
	if !ok {
		return fmt.Errorf("unknown model %q in the rotation", res.Session.Model)
	}
	if res.ContextWindow != want {
		return fmt.Errorf("context window %d, want %d", res.ContextWindow, want)
	}
	if res.InputBudget <= 0 || res.InputBudget > want {
		return fmt.Errorf("input budget %d is not within (0, %d]", res.InputBudget, want)
	}
	if res.TriggerPoint <= 0 || res.TriggerPoint >= res.InputBudget {
		return fmt.Errorf("trigger point %d is not within (0, input budget %d)", res.TriggerPoint, res.InputBudget)
	}
	switch compaction.TriggerSource(res.TriggerSource) {
	case compaction.TriggerSourceLadder, compaction.TriggerSourceReserve, compaction.TriggerSourceConfigured:
	default:
		return fmt.Errorf("trigger source %q is not a settlement of a live plan", res.TriggerSource)
	}
	return nil
}
