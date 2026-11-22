// This file implements the shared compaction resolver: the one place that turns a
// (provider, model) pair plus the layered config into the inputs the loop's
// auto-compaction needs — the context window, the input budget, the trigger
// ladder, the reserve, and the optional user policy.
//
// It exists because "where does compaction fire" was previously assembled by each
// driver separately, and each of them got a different answer: the REPL/TUI built
// their own LoopConfig with DefaultCompactionSettings, sub-agents used a fixed
// 128K window, and headless went through run.NewConfig with no window at all.
// A per-model window (or input ceiling) could therefore take effect in one driver
// and be ignored in the next. Resolving once, here, is what makes the answer the
// same everywhere — including after a /model switch, which used to leave the old
// model's window in place.
package run

import (
	"os"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/config"
	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/diag"
	"github.com/roberChen/pigo/internal/provider"
	"github.com/roberChen/pigo/internal/runtime"
)

// CompactionPlan is the resolved auto-compaction configuration for one model: what
// the loop measures against, when it fires, and who is allowed to decide.
//
// It is a value, so a driver can resolve it once at assembly and re-resolve it
// after a model switch without keeping a reference to any mutable config.
type CompactionPlan struct {
	// ProviderName and Model identify what the plan was resolved for, so a caller
	// can tell a stale plan from a current one.
	ProviderName string
	Model        string

	// ContextWindow is the model's advertised combined budget (input + output).
	ContextWindow int
	// Limits is the full limit pair, including the input-only ceiling when the
	// provider publishes one smaller than the window.
	Limits provider.ModelLimits
	// LimitsSource says where the numbers came from (declared, built-in table,
	// model-id family, conservative fallback), so the configuration report can
	// explain a number instead of presenting a guess as fact.
	LimitsSource provider.LimitsSource

	// Settings is the compaction policy the loop runs (enabled, reserve, keep
	// recent, input ceiling, trigger caps).
	Settings compaction.CompactionSettings
	// Trigger is the user's policy from [compaction] trigger, adapted to the
	// loop's seam. nil means "no policy": the fixed threshold alone decides.
	Trigger compaction.TriggerFunc
	// ResponseMeta captures the bounded record of each upstream response when a
	// policy is configured, so the policy can read gateway-specific accounting.
	// nil when there is no policy to read it (capture costs nothing then).
	ResponseMeta *agentcore.ResponseMeta

	// PolicyCommand is the raw configured command, empty when none is set. It is
	// kept so a front-end can show that a policy is in force, and so the
	// configuration report does not have to re-read the config layers.
	PolicyCommand string
	// TriggerInfo is where the fixed policy fires and which cap put it there. It
	// is the same number the loop compares against, so a front-end renders it
	// rather than recomputing window−reserve (which is wrong whenever the input
	// ceiling or the ladder is the tighter rule).
	TriggerInfo compaction.TriggerDetail
	// TriggerPoint is where the fixed policy will fire, in tokens. 0 means
	// automatic compaction is off for this model.
	TriggerPoint int
	// InputBudget is the budget a single request may carry (the input ceiling when
	// known, else the window). It is the number TriggerPoint is measured against.
	InputBudget int
}

// Enabled reports whether automatic compaction can fire for this plan.
func (p CompactionPlan) Enabled() bool { return p.TriggerPoint > 0 }

// PlanCompaction resolves the compaction plan for a model. It reads the process's
// published configuration (config.CurrentMemory) and the provider registry, so it
// is safe to call from any driver — and cheap enough to re-call after a model
// switch.
//
// Resolution order, all of it the smallest/earliest value winning:
//
//  1. the window and input ceiling: a [[models]] declaration, else pigo's built-in
//     table, else a model-id family match, else the conservative fallback;
//  2. the trigger ladder: the highest [checkpoint] thresholds entry (80% by
//     default) of the INPUT budget, not the combined window — a model that
//     advertises "max input + max output = 1M" rejects the request long before
//     the window is full;
//  3. the reserve: [checkpoint] reserved (tokens or a percentage), else pi's
//     default;
//  4. the absolute caps: [compaction] max_context and the per-model compact_at.
func PlanCompaction(providerName, model string) CompactionPlan {
	cwd, err := os.Getwd()
	if err != nil {
		// A policy command in an unresolvable cwd still runs; it just runs in the
		// process's default directory. Losing it entirely would be worse: the
		// policy may be the only thing that knows the gateway's real budget.
		diag.Diagf("compaction: cannot resolve the working directory for the policy command: %v", err)
		cwd = ""
	}
	return planCompactionIn(cwd, providerName, model)
}

// planCompactionIn is PlanCompaction with an explicit directory, which is where a
// configured policy command runs. It is the seam a reload/host caller uses when the
// session directory is not the process's own.
func planCompactionIn(cwd, providerName, model string) CompactionPlan {
	limits, source := provider.ModelContextLimits(providerName, model)
	window := limits.ContextWindow
	if window <= 0 {
		// A declaration may state only the input ceiling. The window is then at
		// least that ceiling, and never smaller than the conservative default.
		window = provider.DefaultContextWindow
		if limits.MaxInputTokens > window {
			window = limits.MaxInputTokens
		}
	}
	limits.ContextWindow = window

	settings := compaction.DefaultCompactionSettings
	settings.MaxInputTokens = limits.MaxInputTokens

	mem := config.CurrentMemory()
	budget := compaction.InputBudget(window, settings)

	// The reserve: an explicit [checkpoint] reserved wins over pi's default. A
	// percentage form is a share of the input budget, which is the budget the
	// trigger is measured against.
	if r := mem.CheckpointReserved; r.Set {
		switch {
		case r.IsInt && r.Int >= 0:
			settings.ReserveTokens = r.Int
		case !r.IsInt:
			if f, ok := config.ParseThresholdFraction(r.Str); ok && budget > 0 {
				settings.ReserveTokens = int(float64(budget)*f + 0.5)
			}
		}
	}

	// The ladder ceiling: the highest configured threshold is where the loop
	// compacts. A ladder entry below the reserve-based line fires earlier, which is
	// the point of configuring one.
	if ladder := mem.CheckpointThresholds; len(ladder) > 0 {
		top := ladder[0]
		for _, f := range ladder[1:] {
			if f > top {
				top = f
			}
		}
		settings.TriggerFraction = top
	}

	// The absolute caps: the global [compaction] max_context and the per-model
	// compact_at. Both resolve against the input budget; the smaller one wins.
	caps := make([]int, 0, 2)
	if mem.MaxContext.IsSet() {
		if cap := mem.MaxContext.Resolve(budget); cap > 0 {
			caps = append(caps, cap)
		}
	}
	if mt := provider.ModelTriggerFor(providerName, model); !mt.IsZero() {
		if cap := mt.Resolve(budget); cap > 0 {
			caps = append(caps, cap)
		}
	}
	for _, cap := range caps {
		if settings.TriggerTokens == 0 || cap < settings.TriggerTokens {
			settings.TriggerTokens = cap
		}
	}

	plan := CompactionPlan{
		ProviderName:  providerName,
		Model:         model,
		ContextWindow: window,
		Limits:        limits,
		LimitsSource:  source,
		Settings:      settings,
		InputBudget:   budget,
		TriggerInfo:   compaction.ExplainTrigger(window, settings),
		TriggerPoint:  compaction.TriggerPoint(window, settings),
	}
	// A configured policy gets a capture record; without one there is nothing to
	// read the metadata, so capture stays off and the transport pays nothing.
	if command := mem.CompactionTrigger; command != "" {
		plan.PolicyCommand = command
		cmd := compaction.NewCommandTrigger(command, cwd)
		cmd.Warn = diag.Diagf
		plan.Trigger = cmd.Func()
		plan.ResponseMeta = agentcore.NewResponseMeta()
	}
	return plan
}

// AttachCompaction installs a plan onto a run config: the window, the settings,
// the policy and its response record. It is the seam every hand-built RunConfig
// (the REPL/TUI turn configs, /btw, /goal, sub-agent children) goes through, so a
// driver cannot accidentally run with a different compaction policy than the one
// the assembly resolved.
func AttachCompaction(cfg *runtime.RunConfig, plan CompactionPlan) {
	if cfg == nil {
		return
	}
	cfg.ContextWindow = plan.ContextWindow
	cfg.Compaction = plan.Settings
	cfg.Trigger = plan.Trigger
	cfg.ResponseMeta = plan.ResponseMeta
}
