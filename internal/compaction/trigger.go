// This file implements the pluggable compaction trigger: the contract a
// user-defined policy is called with, and the decision merge that keeps a
// custom policy from ever leaving the context larger than the model can accept.
//
// The fixed policy (ShouldCompact) is a single comparison against a window. That
// is not enough for two real cases:
//
//   - a model whose advertised window is "max input + max output" (DeepSeek and
//     others), where 80% of the combined window already exceeds the input
//     ceiling, so the trigger must be computed against the input budget;
//   - a non-standard gateway that reports its own accounting — a different usage
//     field, a remaining-quota header, a per-request budget in the response
//     body — which only the user's policy can interpret.
//
// The second case is why TriggerInput carries the bounded record of what the
// upstream actually returned (status, headers, raw SSE payloads), not just the
// usage block pigo happens to understand. A policy written in a pigorc can read
// any field it likes out of that record.
package compaction

import (
	"context"

	"github.com/roberChen/pigo/internal/agentcore"
)

// TriggerInput is everything a trigger policy may consult when deciding whether
// the context must be compacted before the next request.
//
// The numeric fields are pre-computed by the caller so a policy does not have to
// re-derive them and risk disagreeing with the loop: ContextTokens is the same
// estimate the meter shows, InputBudget is the model's real input ceiling, and
// TriggerPoint is what the fixed policy would have compared against.
type TriggerInput struct {
	// ContextTokens is the estimated size of the context that the next request
	// would carry.
	ContextTokens int
	// ContextWindow is the model's advertised combined budget (input + output).
	ContextWindow int
	// InputBudget is the token budget a single request may actually carry: the
	// model's input ceiling when one is known, else ContextWindow (see
	// InputBudget).
	InputBudget int
	// ReserveTokens is headroom kept for the summary request.
	ReserveTokens int
	// TriggerPoint is where the fixed policy would fire. 0 means the fixed
	// policy is off (disabled or unknown window).
	TriggerPoint int
	// Messages is how many messages the context currently holds.
	Messages int
	// Model and Provider identify what is being measured, for logging and for a
	// policy that varies by model.
	Model    string
	Provider string

	// Usage is the most recent provider-reported usage block, when the last turn
	// carried one. nil when the provider reported nothing.
	Usage *agentcore.Usage
	// ResponseMeta is the bounded record of the last upstream response: status,
	// sanitized headers and the tail of its raw payloads. nil when unavailable
	// (a resumed run, or a provider that does not capture it).
	ResponseMeta *agentcore.ResponseMeta
}

// TriggerDecision is what a policy returns. Decided=false means "no opinion":
// the caller falls back to the fixed policy, so a policy can handle only the
// models it recognizes and stay silent for the rest. Compact is ignored unless
// Decided is true. Reason is surfaced in the compaction event and the debug
// log, so a surprising decision can be explained.
type TriggerDecision struct {
	Decided bool
	Compact bool
	Reason  string
}

// TriggerFunc decides whether to compact. It is called on the loop's goroutine
// before the next turn; a policy must honor ctx and return promptly. An error
// is not part of the contract: a policy that cannot decide returns
// Decided=false, and the fixed policy takes over.
type TriggerFunc func(ctx context.Context, in TriggerInput) TriggerDecision

// TriggerReason explains a compaction decision in a form fit for the event and
// the log.
type TriggerReason string

const (
	// TriggerFixed means the fixed policy decided (threshold/reserve/ladder).
	TriggerFixed TriggerReason = "fixed threshold"
	// TriggerPolicy means the user's policy decided.
	TriggerPolicy TriggerReason = "custom policy"
	// TriggerPolicyError means the policy failed to run and the fixed policy was
	// used; the context is compacted if the fixed policy says so.
	TriggerPolicyError TriggerReason = "custom policy failed"
	// TriggerBudgetGuard means the policy declined but the context had reached
	// the model's real input ceiling, so compaction was forced.
	TriggerBudgetGuard TriggerReason = "input budget guard"
)

// TriggerOutcome is the decision plus the explanation a front-end or log shows.
// Detail is the policy's own reason when it decided, and empty otherwise.
type TriggerOutcome struct {
	Compact bool
	Reason  TriggerReason
	Detail  string
}

// DecideCompact merges the fixed policy and an optional custom policy into one
// answer, plus the reason it was reached.
//
// Precedence and safety:
//
//  1. No custom policy → the fixed policy decides.
//  2. A custom policy that returns Decided=false → the fixed policy decides.
//  3. A custom policy that decides → its answer is honored, EXCEPT when it says
//     "do not compact" while the context has already reached the input budget.
//     Sending that request would be rejected by the provider, which is a hard
//     failure, not a stylistic preference; compaction is forced and the reason
//     says so. (An underestimate only compacts early, which is recoverable.)
func DecideCompact(ctx context.Context, in TriggerInput, settings CompactionSettings, policy TriggerFunc) TriggerOutcome {
	fixed := TriggerPoint(in.ContextWindow, settings)
	atBudget := in.InputBudget > 0 && in.ContextTokens > in.InputBudget-settings.ReserveTokens

	if policy != nil {
		dec := policy(ctx, in)
		if dec.Decided {
			if !dec.Compact && atBudget {
				return TriggerOutcome{Compact: true, Reason: TriggerBudgetGuard, Detail: dec.Reason}
			}
			if dec.Compact {
				return TriggerOutcome{Compact: true, Reason: TriggerPolicy, Detail: dec.Reason}
			}
			return TriggerOutcome{Compact: false, Reason: TriggerPolicy, Detail: dec.Reason}
		}
	}
	if fixed > 0 && in.ContextTokens > fixed {
		return TriggerOutcome{Compact: true, Reason: TriggerFixed}
	}
	return TriggerOutcome{Compact: false, Reason: TriggerFixed}
}
