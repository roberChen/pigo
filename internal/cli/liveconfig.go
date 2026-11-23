// This file defines LiveConfig, the mutable run configuration a control command
// may change mid-session. It was moved verbatim from cmd/pigo (the former
// liveRunConfig) and exported so the run, repl, btw, status and goal
// subpackages can read and mutate it through the Host contract. The run closure
// reads it on every prompt, so a /model switch takes effect on the next turn.
// It carries no lock: it is read and written only on the REPL's single main
// goroutine (slash actions and the run are both invoked synchronously from the
// REPL loop, never concurrently).
package cli

import (
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/run"
	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/provider"
)

// LiveConfig is the mutable run configuration a control command may change
// mid-session.
type LiveConfig struct {
	Model        string
	ProviderName string
	Provider     provider.Provider
	BaseURL      string
	Protocol     string
	// ThinkingLevel is the reasoning-effort level applied to each turn. It is
	// seeded from the resolved config chain and read on every prompt.
	ThinkingLevel agentcore.ThinkingLevel
	// ContextWindow is the model's total context-token budget, used to gate
	// automatic compaction. When 0 the window is unknown and auto-compaction is
	// disabled; the REPL seeds it with a conservative default so long sessions
	// still compact rather than overflow.
	//
	// It is derived from Compaction (which see) rather than set independently: a
	// model switch re-resolves the plan and copies its window here, so the status
	// bar, /memory and the loop cannot disagree about the budget.
	ContextWindow int

	// Compaction is the resolved auto-compaction plan for Model/ProviderName: the
	// window and input ceiling from the provider registry, the trigger ladder and
	// reserve from config.toml, and the optional user policy command. Every driver
	// installs it onto its RunConfig with run.AttachCompaction, so the loop's
	// decision and the figures the UI shows come from the same numbers.
	Compaction run.CompactionPlan

	// FetchedModels is the online model catalog pulled from the live provider's
	// endpoint by an explicit "/models fetch" (issue #566), sorted and
	// deduplicated. It is session-lifetime state: /model prefers it over the
	// heuristic chain for ids it contains (so a fetched id stays on the gateway
	// that serves it) and /models fetch refreshes it. Nil until the first
	// successful fetch; never persisted.
	FetchedModels []string
	FetchedAt     time.Time
}

// DefaultContextWindow is the fallback context-token budget used when a model's
// true window is unknown. It is deliberately large so auto-compaction only fires
// on genuinely long sessions (threshold = window - ReserveTokens), never on
// ordinary short exchanges.
const DefaultContextWindow = 128000

// RefreshCompaction re-resolves the compaction plan for the current Model and
// ProviderName and republishes the window derived from it. It is the only place
// that pairs the two, so they cannot drift apart.
//
// Every path that changes Model (a /model switch or pick, a config reload, a
// session swap) must call this: ContextWindow and Compaction describe one model,
// and leaving them behind means the loop keeps compacting against the previous
// model's budget — too late for a model with a smaller input ceiling, which is
// exactly how a switch used to end in a request the endpoint rejects.
func (l *LiveConfig) RefreshCompaction() {
	if l == nil {
		return
	}
	l.Compaction = run.PlanCompaction(l.ProviderName, l.Model)
	l.ContextWindow = l.Compaction.ContextWindow
}

// CompactionSettings returns the settings the loop runs for the live model: the
// resolved plan's, when the plan was resolved for this very model, else the
// built-in defaults (a hand-built LiveConfig, or one whose plan is stale).
func (l *LiveConfig) CompactionSettings() compaction.CompactionSettings {
	if l == nil {
		return compaction.DefaultCompactionSettings
	}
	if p := l.Compaction; p.Model == l.Model && p.ProviderName == l.ProviderName {
		return p.Settings
	}
	return compaction.DefaultCompactionSettings
}

// CompactionDetail reports where auto-compaction will fire for this model and
// which rule put it there — the numbers /status, /memory and the TUI drawer
// render.
//
// It resolves through the same settings the loop runs and the same window the loop
// compares against, so a front-end cannot show a threshold the loop would never
// use: the model's input budget (not its combined window), the configured ladder,
// the configured reserve and any absolute cap all come from one place. A UI that
// recomputes window−reserve on its own is wrong whenever the reserve, the input
// ceiling or the ladder is the tighter rule — which is the common case.
func (l *LiveConfig) CompactionDetail() compaction.TriggerDetail {
	if l == nil {
		return compaction.TriggerDetail{Source: compaction.TriggerSourceOff}
	}
	return compaction.ExplainTrigger(l.ContextWindow, l.CompactionSettings())
}
