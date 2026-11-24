// This file tests the seam between the compaction policy and the per-model
// context sizes: the trigger must fire before the model's input ceiling, and the
// summarization request — whose payload is the very history it is replacing —
// must still fit the ceiling once the context has already run past it.
//
// The second property is the one that used to be missing. A context that has
// overrun the input budget (a resumed session whose model shrank, one oversized
// paste, a tool result larger than the window) built a summarization request of
// the same size, the provider rejected it, and every retry failed identically —
// so the session was permanently over budget and could never recover. The guard
// in boundConversation is what makes compaction a way out instead of a dead end.
package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/provider"
)

// vendorModels are the ids whose published windows the policy has to match. They
// mirror the built-in limit table; limits_test.go pins the table itself, this
// file pins what the compaction policy derives from it.
var vendorModels = []struct{ provider, model string }{
	{"deepseek", "deepseek-flash"},
	{"deepseek", "deepseek-v4-pro"},
	{"zai", "glm-5.4"},
	{"zai", "glm-5.3"},
	{"zai", "glm-5.1"},
	{"kimi-coding", "k3"},
	{"kimi-coding", "k3-256k"},
	{"moonshotai", "kimi-k3.1-pro"},
	{"moonshotai", "kimi-k2.6"},
}

// TestTriggerFiresBeforeTheInputCeiling is the "does the policy match the window"
// check: for every catalog model, auto-compaction lands strictly inside the input
// budget with at least the reserve still free for the summary reply. A trigger at
// or past the ceiling would mean the request that crosses it is already illegal.
func TestTriggerFiresBeforeTheInputCeiling(t *testing.T) {
	for _, m := range vendorModels {
		limits, _ := provider.ModelContextLimits(m.provider, m.model)
		if limits.ContextWindow <= 0 {
			t.Fatalf("%s/%s has no window in the limit table", m.provider, m.model)
		}
		settings := DefaultCompactionSettings
		settings.MaxInputTokens = limits.MaxInputTokens

		budget := InputBudget(limits.ContextWindow, settings)
		point := TriggerPoint(limits.ContextWindow, settings)
		if budget <= 0 || point <= 0 {
			t.Fatalf("%s/%s: budget=%d point=%d, want both positive", m.provider, m.model, budget, point)
		}
		if point >= budget {
			t.Errorf("%s/%s: trigger %d is not inside the input budget %d", m.provider, m.model, point, budget)
		}
		if headroom := budget - point; headroom < settings.ReserveTokens {
			t.Errorf("%s/%s: trigger %d leaves %d tokens, less than the %d reserve",
				m.provider, m.model, point, headroom, settings.ReserveTokens)
		}
	}
}

// TestSummarizationRequestFitsAnOverrunBudget is the exhaustion guard. It builds
// a history far larger than the model can accept, runs the real summary path, and
// asserts the request the provider would receive is inside the input budget while
// still carrying the newest turn.
func TestSummarizationRequestFitsAnOverrunBudget(t *testing.T) {
	const budget = 200_000
	// ~3x the budget in tokens, so a faithful transcript could never be sent.
	msgs := []agentcore.Message{
		bigUser(budget), // oldest: the part that must be dropped
		assistantMsg("old reply", nil, ""),
		bigUser(budget),
		assistantMsg("older reply", nil, ""),
		bigUser(budget),
		userMsg("the newest request: fix the failing test"),
	}

	var captured provider.LlmContext
	stream := fakeStreamFn(assistantText("## Goal\nx"), &captured)
	reserve := DefaultCompactionSettings.ReserveTokens

	out, err := GenerateSummaryOutput(context.Background(), stream,
		provider.Model{ID: "m", ContextWindow: budget}, msgs, reserve, budget, "", nil,
		provider.StreamConfig{})
	if err != nil {
		t.Fatalf("GenerateSummaryOutput: %v", err)
	}
	if out.Summary == "" {
		t.Fatal("expected a summary")
	}

	prompt := textOf(captured.Messages[0].(agentcore.UserMessage).Content)
	used := EstimateTokens(captured.Messages[0]) + EstimateTokens(agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(captured.SystemPrompt)},
	})
	if used > budget {
		t.Fatalf("summarization request is %d tokens, over the %d input budget", used, budget)
	}
	if !strings.Contains(prompt, truncatedWindowMarker) {
		t.Errorf("an over-budget transcript must say it was trimmed:\n%.200s", prompt)
	}
	if !strings.Contains(prompt, "fix the failing test") {
		t.Errorf("the newest turn must survive the trim")
	}
}

// TestBoundConversationKeepsTheTailAndStatesTheLoss covers the trimming helper
// directly: it keeps the newest messages, drops the oldest, and records that it
// did so (a silent trim would let the summary claim the session began mid-task).
func TestBoundConversationKeepsTheTailAndStatesTheLoss(t *testing.T) {
	msgs := []agentcore.Message{
		userMsg("OLDEST " + strings.Repeat("a", 4000)),
		userMsg("MIDDLE " + strings.Repeat("b", 4000)),
		userMsg("NEWEST " + strings.Repeat("c", 4000)),
	}
	got := boundConversation(msgs, 4200) // room for roughly one message plus marker
	if strings.Contains(got, "OLDEST") {
		t.Error("the oldest message should have been dropped")
	}
	if !strings.Contains(got, "NEWEST") {
		t.Error("the newest message must be kept")
	}
	if !strings.Contains(got, truncatedWindowMarker) {
		t.Error("the trim must be stated in the transcript")
	}
	if len(got) > 4200+len(truncatedWindowMarker)+8 {
		t.Errorf("bounded transcript is %d chars, over the ~%d budget", len(got), 4200)
	}

	// No budget means no trim: the previous behavior must be preserved for models
	// whose limits are unknown.
	unbounded := boundConversation(msgs, 0)
	if !strings.Contains(unbounded, "OLDEST") || strings.Contains(unbounded, truncatedWindowMarker) {
		t.Error("an unbounded budget must not trim the transcript")
	}
}

// TestCompactRecoversAnOverrunContext drives the whole path the loop takes: a
// context larger than the window is compacted successfully, and the rebuilt
// context (checkpoint + retained tail) comes back inside the window. Before the
// guard this failed at the summarization request itself, so the session could
// never leave the over-budget state.
func TestCompactRecoversAnOverrunContext(t *testing.T) {
	const window = 20_000
	// Six large turns (~24k tokens) so the history overruns the window while each
	// individual turn still fits it — the shape a long session actually takes. A
	// single message bigger than the window could not be retained verbatim by any
	// cut-point choice, which is a different (and unfixable) case.
	msgs := []agentcore.Message{
		bigUser(4000), assistantMsg("a1", nil, ""),
		bigUser(4000), assistantMsg("a2", nil, ""),
		bigUser(4000), assistantMsg("a3", nil, ""),
		bigUser(4000), assistantMsg("a4", nil, ""),
		bigUser(4000), assistantMsg("a5", nil, ""),
		userMsg("current task"),
	}
	settings := DefaultCompactionSettings
	settings.KeepRecentTokens = 2000

	stream := fakeStreamFn(assistantText("## Goal\nrecovered"), nil)
	res, err := Compact(context.Background(), stream,
		provider.Model{ID: "m", ContextWindow: window}, msgs, settings, -1, nil, "", nil,
		provider.StreamConfig{})
	if err != nil {
		t.Fatalf("Compact on an over-budget context: %v", err)
	}
	if res == nil {
		t.Fatal("Compact returned nil: nothing was folded")
	}
	rebuilt := res.RebuildContext(msgs, 1)
	after := EstimatePostCompactionTokens(rebuilt)
	if after > window {
		t.Fatalf("rebuilt context is %d tokens, still over the %d window", after, window)
	}
	if after >= res.TokensBefore {
		t.Fatalf("compaction did not shrink the context: %d -> %d", res.TokensBefore, after)
	}
}
