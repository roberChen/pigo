// This file implements the retry policy of automatic compaction: once the
// context has been declared over budget, compaction cannot simply give up.
//
// The old behaviour was to emit a CompactionEvent carrying ErrorMessage and let
// the run continue on the unchanged context. That reads as "non-fatal", but it
// is not a state the run can stay in: the context is still over the threshold,
// so the very next turn crosses it again, emits the same failure, and the model
// is sent a request that is too large for the rest of the session. A single
// transient summarizer failure (a 429, a dropped connection, one malformed
// response) therefore degraded every later turn of the run.
//
// Compaction is now sticky until it succeeds: each failed attempt is reported so
// the failure stays observable, then retried after a backoff. The attempt is
// bound to the run's context, so interrupting a run is still an escape hatch —
// cancellation ends the retry loop instead of spinning inside it (see
// CompactionEvent.Retrying, which is how a front-end tells a retried attempt
// from a final failure).
package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/compaction"
)

// Compaction retry backoff. The first retry is immediate-ish so a one-off
// failure costs nothing, then the delay doubles up to the cap: a summarizer that
// is down (bad key, provider outage) should not be hammered with a request every
// few milliseconds, but the run should also recover quickly when the failure was
// transient.
const (
	compactionRetryBase = 250 * time.Millisecond
	compactionRetryMax  = 8 * time.Second
)

// compactionRetryDelay returns how long to wait before attempt n+1, where n is
// the attempt that just failed (1-based). Exponential with a cap, so a sustained
// outage settles at one attempt every compactionRetryMax rather than a hot loop.
func compactionRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := compactionRetryBase
	for i := 1; i < attempt && d < compactionRetryMax; i++ {
		d *= 2
	}
	if d > compactionRetryMax {
		d = compactionRetryMax
	}
	return d
}

// waitCompactionRetry sleeps for d, returning false when ctx ended first. The
// timer is the only thing the retry loop waits on, so an interrupt does not have
// to wait out a backoff before the run can stop.
func waitCompactionRetry(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// compactionAttempt is what one attempt of the retry loop produced: the applied
// result, or the error to report.
type compactionAttempt struct {
	res *compaction.CompactionResult
	err error
	// noop is true when the cut point left no prefix to summarize. It is a
	// success in the sense that retrying cannot change it: the context is not
	// over budget for a reason compaction can fix.
	noop bool
}

// attemptCompaction runs one compaction attempt under cfg.
//
// A panic inside the summarizer (a provider adapter dereferencing a missing
// field, say) is caught and turned into a retryable error. Compaction is now
// mandatory rather than best-effort, so a panic here would otherwise take down
// the whole run and lose the session, when the only thing that failed was one
// attempt at summarizing.
func attemptCompaction(ctx context.Context, msgs agentcore.MessageList, cfg *RunConfig) (att compactionAttempt) {
	defer func() {
		if r := recover(); r != nil {
			att = compactionAttempt{err: panicError(r)}
		}
	}()
	res, err := runCompaction(ctx, msgs, cfg)
	if err != nil {
		return compactionAttempt{err: err}
	}
	if res == nil {
		return compactionAttempt{noop: true}
	}
	return compactionAttempt{res: res}
}

// panicError turns a recovered panic value into an error a retry can report. The
// value is formatted rather than type-asserted because a panic can carry
// anything, including nil.
func panicError(r any) error {
	return fmt.Errorf("summarization panicked: %v", r)
}
