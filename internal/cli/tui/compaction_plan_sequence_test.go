// This file tests the "the compaction plan follows the model" contract over a
// long sequence of switches.
//
// It is deliberately not a unit test of one switch: the bug it guards against
// was a *state* bug. Each switch looked correct in isolation, but the window and
// the plan were left behind, so a session that switched models kept compacting
// against a budget belonging to a model it was no longer talking to — too late
// for a model with a smaller input ceiling, which is how the next request could
// be rejected outright. Driving many turns and many switches through the real
// assembly (prompts.ApplyModelSwitch via the session's slash registry, then
// buildConfig) makes a single lost refresh anywhere in the chain fail loudly.
package tui

import (
	"strconv"
	"testing"

	"github.com/roberChen/pigo/internal/cli"
	"github.com/roberChen/pigo/internal/cli/config"
	"github.com/roberChen/pigo/internal/cli/run"
)

// planModelRotation is the order the test switches through. The budgets must
// genuinely differ for the assertions to mean anything: deepseek-flash
// advertises a 1M window but only a 616K input ceiling (so the ladder has to be
// computed off the ceiling), glm-5.1 is 200K with no ceiling, glm-5.3 is 1M with
// no ceiling, and openrouter/free falls back to the conservative 128K. The guard
// below fails the test rather than letting a collapsed rotation pass silently.
var planModelRotation = []struct{ provider, model string }{
	{"zai", "glm-5.1"},
	{"deepseek", "deepseek-flash"},
	{"zai", "glm-5.3"},
	{"openrouter", "openrouter/free"},
	{"zai", "glm-5.1"},
}

// TestCompactionPlanFollowsModelAcrossManySwitches switches models repeatedly
// and checks, after every switch, that the window, the plan, the reported
// trigger point and the RunConfig the loop consumes all name the current model.
func TestCompactionPlanFollowsModelAcrossManySwitches(t *testing.T) {
	s, _ := newReloadSession(t, run.ReloadSettings{NoSkills: true})

	distinct := make(map[int]bool)
	for _, want := range planModelRotation {
		distinct[run.PlanCompaction(want.provider, want.model).ContextWindow] = true
	}
	if len(distinct) < 3 {
		t.Fatalf("the rotation covers only %d distinct windows; the test would prove nothing", len(distinct))
	}

	for turn := 0; turn < 3; turn++ {
		for step, want := range planModelRotation {
			if _, err := s.slash.ResolveOutcome("/model " + want.model); err != nil {
				t.Fatalf("turn %d step %d: /model %s: %v", turn, step, want.model, err)
			}
			where := "turn " + strconv.Itoa(turn) + " step " + strconv.Itoa(step) + " (" + s.live.Model + ")"
			checkPlanMatchesModel(t, s, where)
		}
	}
}

// checkPlanMatchesModel asserts the whole chain agrees on one model.
func checkPlanMatchesModel(t *testing.T, s *runSession, where string) {
	t.Helper()
	want := run.PlanCompaction(s.live.ProviderName, s.live.Model)

	if s.live.Compaction.Model != s.live.Model || s.live.Compaction.ProviderName != s.live.ProviderName {
		t.Fatalf("%s: plan resolved for (%q, %q), want (%q, %q)",
			where, s.live.Compaction.Model, s.live.Compaction.ProviderName,
			s.live.Model, s.live.ProviderName)
	}
	if s.live.ContextWindow != want.ContextWindow {
		t.Fatalf("%s: context window = %d, want %d", where, s.live.ContextWindow, want.ContextWindow)
	}
	if got := s.live.CompactionDetail(); got.Point != want.TriggerPoint {
		t.Fatalf("%s: trigger = %d, want %d", where, got.Point, want.TriggerPoint)
	}
	// The safety property behind budgeting off the input ceiling rather than the
	// window: the trigger can never sit past what a single request may carry.
	if p := want.TriggerPoint; p > 0 && p > want.InputBudget {
		t.Fatalf("%s: trigger %d exceeds the input budget %d", where, p, want.InputBudget)
	}

	cfg := s.buildConfig()
	if cfg.Model != s.live.Model || cfg.Provider != s.live.ProviderName {
		t.Fatalf("%s: cfg = (%q, %q), want (%q, %q)",
			where, cfg.Model, cfg.Provider, s.live.Model, s.live.ProviderName)
	}
	if cfg.ContextWindow != want.ContextWindow {
		t.Fatalf("%s: cfg.ContextWindow = %d, want %d", where, cfg.ContextWindow, want.ContextWindow)
	}
	if cfg.Compaction != want.Settings {
		t.Fatalf("%s: cfg.Compaction = %+v, want %+v", where, cfg.Compaction, want.Settings)
	}
}

// TestCompactionPlanCarriesThePolicyAcrossSwitches covers the policy half: the
// policy func and its response-capture record belong to the plan, so a switch
// must hand the loop a matching pair. A stale capture record would let the new
// model's policy read the previous model's response metadata.
func TestCompactionPlanCarriesThePolicyAcrossSwitches(t *testing.T) {
	// A plan with no configured policy must install neither half.
	plain := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	plain.RefreshCompaction()
	if plain.Compaction.Trigger != nil || plain.Compaction.ResponseMeta != nil {
		t.Fatal("a plan without a policy must not install a trigger or a capture record")
	}
	if plain.Compaction.PolicyCommand != "" {
		t.Errorf("PolicyCommand = %q, want empty", plain.Compaction.PolicyCommand)
	}

	// With a policy configured, both halves appear, and a switch re-resolves them
	// together for the new model.
	command := `cat >/dev/null; echo '{"decide":false}'`
	config.SetCurrentMemory(config.MemorySettings{CompactionTrigger: command})
	t.Cleanup(func() { config.SetCurrentMemory(config.MemorySettings{}) })

	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	live.RefreshCompaction()
	first := live.Compaction
	if first.Trigger == nil || first.ResponseMeta == nil {
		t.Fatal("a configured policy must install a trigger and a capture record")
	}
	if first.PolicyCommand != command {
		t.Errorf("PolicyCommand = %q, want the configured command", first.PolicyCommand)
	}

	live.Model = "glm-5.1"
	live.ProviderName = "zai"
	live.RefreshCompaction()
	next := live.Compaction
	if next.Model != "glm-5.1" || next.ProviderName != "zai" {
		t.Fatalf("plan resolved for (%q, %q), want (glm-5.1, zai)", next.Model, next.ProviderName)
	}
	if next.Trigger == nil || next.ResponseMeta == nil {
		t.Fatal("the policy was dropped by a refresh")
	}
	if next.ResponseMeta == first.ResponseMeta {
		t.Error("the capture record was shared with the previous plan")
	}
	if next.TriggerPoint == first.TriggerPoint {
		t.Errorf("trigger point did not move with the model (still %d)", first.TriggerPoint)
	}
	if next.PolicyCommand != command {
		t.Errorf("PolicyCommand = %q, want it preserved", next.PolicyCommand)
	}
}
