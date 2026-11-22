// This file implements the one-shot manual compaction behind /compact (REPL and
// TUI). Auto-compaction lives in the loop (loop.go's maybeAutoCompact) and only
// runs while a turn is settling; /compact has to work when no run is in flight,
// so it needs the same accounting without the loop around it.
//
// Both front-ends used to have their own version of this: the REPL called
// compaction.Compact directly, and the TUI had no /compact at all (the slash
// registry entry was a stub, so the command silently did nothing). Rather than
// let a third copy appear, the flow — summarize, persist a checkpoint, rebuild
// the context, report the accounting — lives here once, and the front-ends only
// decide when to run it and how to show the result.
//
// It is deliberately side-effect free with respect to the caller's context: the
// rebuilt MessageList is returned for the caller to install, and no goroutine or
// loop state is touched. The one write it performs is the session checkpoint,
// which is what makes a manual /compact survivable by /rebuild and by resume.
package runtime

import (
	"context"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/compaction"
)

// ManualCompaction is the outcome of one manual compaction.
type ManualCompaction struct {
	// Messages is the rebuilt context: a checkpoint summary followed by the
	// retained recent tail. When NoOp is true it is the original list, unchanged.
	Messages agentcore.MessageList
	// Event reports the compaction in the shape the loop emits, so a front-end
	// presents a manual /compact exactly like an automatic one (card, hook
	// notification, context meter). Its Reason is the caller's request reason —
	// "manual" for /compact, which is what the PreCompact hook maps to the manual
	// trigger. On a no-op it carries the unchanged before/after size and an empty
	// Summary, so consumers can tell nothing was folded.
	Event agentcore.CompactionEvent
	// NoOp is true when there was nothing to summarize (an empty history, or a cut
	// point that left no prefix). The context is then unchanged.
	NoOp bool
}

// CompactContext performs one manual compaction of msgs under cfg, the way the
// loop's auto-compaction does: it asks the summarizer for a checkpoint, keeps the
// most recent messages verbatim (settings.KeepRecentTokens), and rebuilds the list
// as summary + tail. A prior checkpoint in msgs is consolidated rather than
// restarted, so compacting repeatedly keeps one rolling summary and its
// user-instruction ledger instead of drifting away from the original requirements.
//
// The checkpoint is persisted (best-effort, diag on failure) when cfg carries a
// SessionID and MemoryRoot, so a later /rebuild can reload this summary rather
// than re-summarizing. Summarization uses cfg.SummaryStream/SummaryModel when set,
// falling back to the primary Stream/Model, and resolves the API key through
// cfg.GetAPIKey exactly as a turn does.
//
// reason labels the resulting event: "manual" for a user-requested compaction,
// which the PreCompact hook reports as the manual trigger.
//
// The caller must guarantee no run is in flight: during a run the pump goroutine
// owns the message list, so compacting it from elsewhere is a data race.
func CompactContext(ctx context.Context, msgs agentcore.MessageList, cfg *RunConfig, reason string) (*ManualCompaction, error) {
	before := compaction.EstimateContextTokens(msgs).Tokens
	// One attempt, through the same wrapper auto-compaction uses, so a panic in a
	// summarizer adapter becomes a retryable error here too instead of taking the
	// front-end process down with it. A manual /compact has no loop to retry
	// behind, but the caller does: the TUI keeps the session in its compacting
	// state and offers another attempt, and the REPL reports the failure to a user
	// who can re-run the command.
	att := attemptCompaction(ctx, msgs, cfg)
	if att.err != nil {
		return nil, att.err
	}
	res := att.res
	if res == nil {
		// Nothing to summarize: the cut point left no prefix. Report it as a no-op
		// so the front-end can say so instead of claiming a compaction happened.
		return &ManualCompaction{
			Messages: msgs,
			NoOp:     true,
			Event: agentcore.CompactionEvent{
				Reason:       reason,
				TokensBefore: before,
				TokensAfter:  before,
				KeptCount:    len(msgs),
			},
		}, nil
	}
	// Persist the checkpoint before rewriting the context: a crash after applying
	// the rebuilt list would otherwise leave the session on disk with the old,
	// unfolded history and no record of the summary that replaced it.
	writeCompactionCheckpoint(ctx, msgs, res, cfg)

	rebuilt := res.RebuildContext(msgs, nowMillis())
	kept := len(rebuilt) - 1
	return &ManualCompaction{
		Messages: rebuilt,
		Event: agentcore.CompactionEvent{
			Reason:          reason,
			TokensBefore:    before,
			TokensAfter:     compaction.EstimatePostCompactionTokens(rebuilt),
			SummarizedCount: len(msgs) - kept,
			KeptCount:       kept,
			Summary:         res.Text(),
		},
	}, nil
}
