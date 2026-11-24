package provider

// Tests for the per-model limit table: the windows and input ceilings here are
// what the compaction planner measures against, so a stale number makes
// pigo either compact too late (a rejected request) or too early. Each case
// cites the vendor figure it pins, and a vendor change should fail here rather
// than pass silently.

import "testing"

// TestBuiltinLimitsForVendorCatalogs pins the ids and windows of the three
// vendors whose catalogs were re-read from the official docs (DeepSeek, Z.AI
// GLM, Moonshot/Kimi). The comments name the published number each case
// encodes.
func TestBuiltinLimitsForVendorCatalogs(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		window   int
		maxInput int
	}{
		// DeepSeek: 1M combined, 384K max output => 616K input.
		{"deepseek", "deepseek-flash", 1_000_000, 616_000},
		{"deepseek", "deepseek-v4-pro", 1_000_000, 616_000},
		// Legacy names are routed by the endpoint to the same models; they must
		// keep the same budget.
		{"deepseek", "deepseek-v4-flash", 1_000_000, 616_000},
		{"deepseek", "deepseek-chat", 1_000_000, 616_000},

		// GLM-5.4 / 5.3 / 5.2 advertise 1M; GLM-5.1 and 4.7 are 200K.
		{"zai", "glm-5.4", 1_000_000, 0},
		{"zai", "glm-5.3", 1_000_000, 0},
		{"zai", "glm-5.3-flash", 1_000_000, 0},
		{"zai", "glm-5.2", 1_000_000, 0},
		{"zai", "glm-5.1", 200_000, 0},
		{"zai", "glm-4.7", 200_000, 0},
		// An aggregator's older GLM keeps the conservative 200K: the 1M rules are
		// provider-scoped so a substring cannot inflate an unknown endpoint.
		{"cerebras", "zai-glm-4.7", 200_000, 0},

		// Kimi K3 / K3.1 are 1,048,576; the K2-derived ids are 262,144. The
		// coding plan's short ids are provider-scoped.
		{"moonshotai", "kimi-k3", 1_048_576, 0},
		{"moonshotai", "kimi-k3.1-pro", 1_048_576, 0},
		{"moonshotai", "kimi-k2.7-code", 262_144, 0},
		{"moonshotai", "kimi-k2.6", 262_144, 0},
		{"kimi-coding", "k3", 1_048_576, 0},
		{"kimi-coding", "k3.1-pro", 1_048_576, 0},
		{"kimi-coding", "kimi-for-coding", 1_048_576, 0},
		{"kimi-coding", "k3-256k", 262_144, 0},
		{"kimi-coding", "kimi-for-coding-highspeed", 262_144, 0},
	}
	for _, tc := range cases {
		limits, source := ModelContextLimits(tc.provider, tc.model)
		if limits.ContextWindow != tc.window {
			t.Errorf("%s/%s: window = %d, want %d", tc.provider, tc.model, limits.ContextWindow, tc.window)
		}
		if got := limits.InputBudget(); got != wantBudget(tc.window, tc.maxInput) {
			t.Errorf("%s/%s: input budget = %d, want %d", tc.provider, tc.model, got, wantBudget(tc.window, tc.maxInput))
		}
		if source != LimitsIdentified && source != LimitsBuiltin {
			t.Errorf("%s/%s: source = %q, want the built-in table or an id rule", tc.provider, tc.model, source)
		}
	}
}

// wantBudget is InputBudget's rule restated for the table: a declared ceiling
// smaller than the window wins.
func wantBudget(window, maxInput int) int {
	if maxInput > 0 && maxInput < window {
		return maxInput
	}
	return window
}

// TestDeepSeekInputCeilingLeavesRoomForMaxOutput pins the arithmetic behind the
// DeepSeek ceiling rather than the literal: the window minus the largest
// completion equals the input ceiling. If either published number changes, this
// fails and points at the constant to update.
func TestDeepSeekInputCeilingLeavesRoomForMaxOutput(t *testing.T) {
	if deepseekMaxInput+deepseekMaxOutput != 1_000_000 {
		t.Fatalf("input %d + output %d != the 1M combined window", deepseekMaxInput, deepseekMaxOutput)
	}
	if deepseekMaxInput != 616_000 {
		t.Errorf("deepseekMaxInput = %d, want 616000 (1M - 384K)", deepseekMaxInput)
	}
}

// TestKimiWindowsDistinct guards the two constants against collapsing into one:
// the K2-derived models really are a quarter of the K3 window, and a single
// value for the whole family would be wrong for one half of it.
func TestKimiWindowsDistinct(t *testing.T) {
	if kimiK3Window != 1_048_576 || kimiK2Window != 262_144 {
		t.Fatalf("windows = %d/%d, want 1048576/262144", kimiK3Window, kimiK2Window)
	}
	if kimiK3Window <= kimiK2Window {
		t.Fatal("the K3 window must be larger than the K2 window")
	}
}

// TestProviderDefaultsRemainConservative verifies each provider's default is not
// larger than any of its catalog entries: a model the table does not name must
// never be assumed to have more room than the smallest known id, because
// overestimating is the failure that gets requests rejected.
func TestProviderDefaultsRemainConservative(t *testing.T) {
	for _, tc := range []struct {
		provider string
		smallest int
	}{
		{"deepseek", 1_000_000},
		{"zai", 200_000},
		{"kimi-coding", 262_144},
		{"moonshotai", 262_144},
	} {
		got := providerLimits[tc.provider].ContextWindow
		if got > tc.smallest {
			t.Errorf("%s default window = %d, larger than its smallest catalog id (%d)", tc.provider, got, tc.smallest)
		}
	}
}
