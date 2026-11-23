// Tests for LiveConfig's compaction plan ownership: the window and the plan
// describe one model, so they are refreshed together and a plan resolved for a
// different model is never mistaken for the current one.
package cli

import (
	"testing"

	"github.com/roberChen/pigo/internal/cli/run"
	"github.com/roberChen/pigo/internal/compaction"
)

// TestRefreshCompactionFollowsTheModel pins the invariant the loop depends on:
// after RefreshCompaction, ContextWindow, Compaction and CompactionDetail all
// describe the current (ProviderName, Model) pair.
func TestRefreshCompactionFollowsTheModel(t *testing.T) {
	live := &LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	live.RefreshCompaction()
	first := live.ContextWindow
	firstPoint := live.CompactionDetail().Point

	live.Model = "glm-4.7"
	live.ProviderName = "zai"
	live.RefreshCompaction()

	want := run.PlanCompaction(live.ProviderName, live.Model)
	if live.ContextWindow != want.ContextWindow {
		t.Errorf("context window = %d, want %d", live.ContextWindow, want.ContextWindow)
	}
	if live.Compaction.Model != "glm-4.7" || live.Compaction.ProviderName != "zai" {
		t.Errorf("plan resolved for (%q, %q), want (glm-4.7, zai)",
			live.Compaction.Model, live.Compaction.ProviderName)
	}
	if got := live.CompactionDetail(); got.Point != want.TriggerPoint {
		t.Errorf("trigger = %d, want %d", got.Point, want.TriggerPoint)
	}
	if live.ContextWindow == first || live.CompactionDetail().Point == firstPoint {
		t.Errorf("nothing changed (%d → %d, trigger %d → %d); the fixture proves nothing",
			first, live.ContextWindow, firstPoint, live.CompactionDetail().Point)
	}
}

// TestStalePlanIsNotUsedForTheThreshold verifies the fallback: if a caller
// mutates Model without refreshing (older code, a test fixture), the reported
// settings are the built-in defaults rather than another model's plan. The
// fallback is deliberately conservative — a stale plan would carry the wrong
// input ceiling — and is what makes forgetting a refresh visible instead of
// silently applying the previous model's numbers.
func TestStalePlanIsNotUsedForTheThreshold(t *testing.T) {
	live := &LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	live.RefreshCompaction()
	stale := live.Compaction

	live.Model = "glm-4.7"
	if got := live.CompactionSettings(); got != compaction.DefaultCompactionSettings {
		t.Errorf("settings = %+v, want the built-in defaults for an unresolved model", got)
	}
	// The window is only re-derived by a refresh, so it still names the old
	// model — which is precisely why RefreshCompaction pairs the two.
	if live.ContextWindow != stale.ContextWindow {
		t.Errorf("ContextWindow = %d, want the un-refreshed %d", live.ContextWindow, stale.ContextWindow)
	}
	live.RefreshCompaction()
	if live.Compaction.Model != live.Model {
		t.Errorf("after refresh the plan is for %q, want %q", live.Compaction.Model, live.Model)
	}
}

func TestRefreshCompactionOnNilIsSafe(t *testing.T) {
	var live *LiveConfig
	live.RefreshCompaction() // must not panic
}

func TestNilLiveConfigReportsDefaults(t *testing.T) {
	var live *LiveConfig
	if live.CompactionSettings() != compaction.DefaultCompactionSettings {
		t.Error("nil config should report the built-in defaults")
	}
	if live.CompactionDetail().Source != compaction.TriggerSourceOff {
		t.Error("nil config should report auto-compaction off")
	}
}
