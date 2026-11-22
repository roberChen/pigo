package runtime

// Tests for the pluggable compaction trigger wired into the loop: a custom
// policy may fire compaction before the fixed threshold, the input-budget guard
// still forces it when a declining policy would send an oversized request, and
// the policy receives the provider's own usage block plus the bounded record of
// the last upstream response. See internal/compaction/trigger.go for the
// contract and loop.maybeAutoCompact for the wiring.

import (
	"context"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/provider"
)

// triggerRunCfg is the shared setup: a main stream that ends the turn, a real
// summary stream, and a window small enough that bigUserMessages(12, 800)
// (~2.5K estimated tokens) is measurable against every trigger point used below.
func triggerRunCfg(t *testing.T, settings compaction.CompactionSettings) RunConfig {
	t.Helper()
	main := scriptedStream([]agentcore.AssistantMessage{
		{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	cfg := newRunCfg(main)
	cfg.SummaryStream = summaryStream("## Goal\ncompacted")
	cfg.Compaction = settings
	return cfg
}

func TestAutoCompactionHonorsCustomPolicy(t *testing.T) {
	// 99% of a 4K window ≈ 3960 tokens: the seeded context (~2.5K) is below it,
	// so the fixed policy alone would not compact. Only the policy can fire here.
	cfg := triggerRunCfg(t, compaction.CompactionSettings{
		Enabled: true, ReserveTokens: 0, TriggerFraction: 0.99, KeepRecentTokens: 100,
	})
	cfg.ContextWindow = 4000
	var sawTrigger bool
	cfg.Trigger = func(_ context.Context, in compaction.TriggerInput) compaction.TriggerDecision {
		sawTrigger = true
		if in.Model != cfg.Model || in.Provider != cfg.Provider {
			t.Errorf("trigger input lost the model identity: model=%q provider=%q", in.Model, in.Provider)
		}
		if in.TriggerPoint != compaction.TriggerPoint(cfg.ContextWindow, cfg.Compaction) {
			t.Errorf("trigger input trigger point = %d, want the fixed policy's %d",
				in.TriggerPoint, compaction.TriggerPoint(cfg.ContextWindow, cfg.Compaction))
		}
		return compaction.TriggerDecision{Decided: true, Compact: true, Reason: "gateway quota header says 3% left"}
	}
	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}

	events := collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))
	if !sawTrigger {
		t.Fatal("the custom policy was never consulted")
	}
	ce := findCompaction(events)
	if ce == nil {
		t.Fatalf("policy compaction expected, got events %+v", events)
	}
	// The event must name the policy and echo its own reason, so the status panel
	// explains a compaction that happened below the configured threshold.
	if want := "policy: gateway quota header says 3% left"; ce.Reason != want {
		t.Errorf("compaction reason = %q, want %q", ce.Reason, want)
	}
	if ce.ErrorMessage != "" {
		t.Fatalf("compaction should have succeeded, got %q", ce.ErrorMessage)
	}
}

func TestAutoCompactionFixedReasonStaysThreshold(t *testing.T) {
	// Same window, but a 50% fraction: the fixed policy fires, no policy set.
	cfg := triggerRunCfg(t, compaction.CompactionSettings{
		Enabled: true, ReserveTokens: 0, TriggerFraction: 0.5, KeepRecentTokens: 100,
	})
	cfg.ContextWindow = 4000
	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}

	events := collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))
	ce := findCompaction(events)
	if ce == nil {
		t.Fatalf("expected a threshold compaction, got events %+v", events)
	}
	if ce.Reason != "threshold" {
		t.Errorf("fixed-policy reason = %q, want %q", ce.Reason, "threshold")
	}
}

func TestAutoCompactionBudgetGuardOverridesDecliningPolicy(t *testing.T) {
	// Combined window 2000 with a 1000-token input ceiling, so the request budget
	// is 1000 while the fixed policy's 50% fraction fires at 500. The seeded
	// context (~2.5K) is past the budget guard (1000 - 10 = 990): a policy that
	// declines must not be allowed to send that request, because the provider
	// would reject it outright.
	cfg := triggerRunCfg(t, compaction.CompactionSettings{
		Enabled: true, MaxInputTokens: 1000, ReserveTokens: 10, TriggerFraction: 0.5, KeepRecentTokens: 100,
	})
	cfg.ContextWindow = 2000
	cfg.Trigger = func(context.Context, compaction.TriggerInput) compaction.TriggerDecision {
		return compaction.TriggerDecision{Decided: true, Compact: false, Reason: "policy prefers a long context"}
	}
	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}

	events := collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))
	ce := findCompaction(events)
	if ce == nil {
		t.Fatalf("budget guard must force compaction, got events %+v", events)
	}
	if ce.Reason != "input budget guard" {
		t.Errorf("compaction reason = %q, want %q", ce.Reason, "input budget guard")
	}
}

func TestCompactionTriggerReceivesUsageAndResponseMeta(t *testing.T) {
	// The turn reports usage, and the transport records a response. Both must
	// reach the policy, and the record must describe THIS request: the stale
	// status/payload seeded before the run has to be gone by the time the policy
	// reads it (the loop resets it before every request).
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonEndTurn,
		Content:    agentcore.ContentList{agentcore.NewTextContent("ok")},
		Usage:      &agentcore.Usage{InputTokens: 1234, OutputTokens: 7},
	}
	cfg := triggerRunCfg(t, compaction.CompactionSettings{
		Enabled: true, ReserveTokens: 0, TriggerFraction: 0.99, KeepRecentTokens: 100,
	})
	cfg.ContextWindow = 4000
	scripted := scriptedStream([]agentcore.AssistantMessage{msg})
	cfg.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		// A real transport fills this record while the response streams.
		if sc.Meta != nil {
			sc.Meta.SetStatus(200)
			sc.Meta.SetHeaders(map[string][]string{"X-RateLimit-Remaining": {"3"}})
			sc.Meta.AddPayload(`{"usage":{"prompt_tokens":1234}}`)
		}
		return scripted(ctx, model, llm, sc)
	}
	cfg.ResponseMeta = agentcore.NewResponseMeta()
	cfg.ResponseMeta.SetStatus(429)
	cfg.ResponseMeta.AddPayload("stale")
	cfg.ResponseMeta.CountBody()

	var gotUsage *agentcore.Usage
	var gotMeta *agentcore.ResponseMetaSnapshot
	cfg.Trigger = func(_ context.Context, in compaction.TriggerInput) compaction.TriggerDecision {
		gotUsage = in.Usage
		gotMeta = in.ResponseMeta.Snapshot()
		return compaction.TriggerDecision{Decided: true, Compact: true, Reason: "usage-aware"}
	}
	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}

	collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))
	if gotUsage == nil || gotUsage.InputTokens != 1234 {
		t.Fatalf("policy usage = %+v, want the turn's 1234 input tokens", gotUsage)
	}
	if gotMeta == nil {
		t.Fatal("policy response meta is nil; capture was not threaded through")
	}
	if gotMeta.Status != 200 {
		t.Errorf("response meta status = %d, want 200 (stale 429 must be reset)", gotMeta.Status)
	}
	if gotMeta.Bodies != 0 || len(gotMeta.Payloads) != 1 || gotMeta.Payloads[0] == "stale" {
		t.Errorf("response meta retained stale data: %+v", gotMeta)
	}
	if vals := gotMeta.Headers["X-RateLimit-Remaining"]; len(vals) != 1 || vals[0] != "3" {
		t.Errorf("response meta headers = %+v, want the gateway quota header", gotMeta.Headers)
	}
}

func TestCompactionTriggerNilPolicyLeavesFixedPolicyAlone(t *testing.T) {
	// With no policy configured, DecideCompact must reproduce the old behavior:
	// no compaction below the trigger point, compaction above it.
	settings := compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}
	if compaction.DecideCompact(context.Background(), compaction.TriggerInput{
		ContextTokens: 100, ContextWindow: 4000, ReserveTokens: 500,
		TriggerPoint: compaction.TriggerPoint(4000, settings),
	}, settings, nil).Compact {
		t.Error("a small context must not trigger with no policy")
	}
	if !compaction.DecideCompact(context.Background(), compaction.TriggerInput{
		ContextTokens: 3800, ContextWindow: 4000, ReserveTokens: 500,
		TriggerPoint: compaction.TriggerPoint(4000, settings),
	}, settings, nil).Compact {
		t.Error("an over-threshold context must trigger with no policy")
	}
}
