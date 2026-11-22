package runtime

// Tests for auto-compaction wiring into the loop (US-004, #120): when context
// usage exceeds the usable window after a turn settles, runLoop compacts in
// place and emits a CompactionEvent. A failed attempt is reported (with the
// original context preserved) and then retried in place until one succeeds —
// compaction is sticky, because leaving an over-budget context alone just makes
// the next turn cross the threshold again. Cancelling the run ends the retries.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/provider"
)

// bigUserMessages returns n user messages each carrying `chars` characters, used
// to inflate estimated context tokens past a small window.
func bigUserMessages(n, chars int) agentcore.MessageList {
	body := strings.Repeat("x", chars)
	msgs := make(agentcore.MessageList, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(body)},
		})
	}
	return msgs
}

// findCompaction returns the first CompactionEvent emitted, or nil.
func findCompaction(events []agentcore.AgentEvent) *agentcore.CompactionEvent {
	for _, ev := range events {
		if c, ok := ev.(agentcore.CompactionEvent); ok {
			return &c
		}
	}
	return nil
}

// collectEvents drains a stream returning the concrete events (not just kinds).
func collectEvents(t *testing.T, s *LoopEventStream) []agentcore.AgentEvent {
	t.Helper()
	var out []agentcore.AgentEvent
	for ev := range s.Events() {
		out = append(out, ev)
	}
	if _, err := s.Result(context.Background()); err != nil {
		t.Fatalf("stream result: %v", err)
	}
	return out
}

// summaryStream yields a fixed summary text as an end_turn assistant message,
// standing in for the summarization model.
func summaryStream(text string) provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			StopReason: agentcore.StopReasonEndTurn,
			Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		}
		s := provider.NewAssistantMessageEventStream(0)
		go func() { _ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg}); s.Close() }()
		return s, nil
	}
}

func TestAutoCompactionFiresOnThreshold(t *testing.T) {
	// Main stream just ends the turn with text; the summary stream is separate so
	// the summarization does not consume main-stream scripted turns.
	main := scriptedStream([]agentcore.AssistantMessage{
		{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	cfg := newRunCfg(main)
	cfg.SummaryStream = summaryStream("## Goal\ncompacted")
	// Small window + reserve so a handful of fat messages exceed the threshold.
	cfg.ContextWindow = 2000
	cfg.Compaction = compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}

	// Seed a long history so EstimateContextTokens > window-reserve.
	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}

	events := collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))
	ce := findCompaction(events)
	if ce == nil {
		t.Fatalf("expected a CompactionEvent, got events %+v", events)
	}
	// A CompactionStartEvent must precede the CompactionEvent so a front-end can
	// show an in-progress indicator while summarization is in flight.
	var startedBefore bool
	for _, ev := range events {
		if _, ok := ev.(agentcore.CompactionStartEvent); ok {
			startedBefore = true
		}
		if _, ok := ev.(agentcore.CompactionEvent); ok {
			if !startedBefore {
				t.Errorf("CompactionStartEvent must be emitted before CompactionEvent")
			}
			break
		}
	}
	if !startedBefore {
		t.Errorf("expected a CompactionStartEvent, got events %+v", events)
	}
	if ce.ErrorMessage != "" {
		t.Fatalf("compaction should have succeeded, got error %q", ce.ErrorMessage)
	}
	if ce.TokensAfter >= ce.TokensBefore {
		t.Errorf("compaction should reduce tokens: before=%d after=%d", ce.TokensBefore, ce.TokensAfter)
	}
	if ce.SummarizedCount <= 0 {
		t.Errorf("expected some messages summarized, got %d", ce.SummarizedCount)
	}
	// The event must carry the checkpoint body, not only the accounting: it is what
	// every front-end prints (REPL), card-renders (TUI) and exports (stream-json).
	if !strings.Contains(ce.Summary, "compacted") {
		t.Errorf("CompactionEvent.Summary should carry the checkpoint text, got %q", ce.Summary)
	}
	// The context must now begin with a compaction checkpoint.
	if len(agentCtx.Messages) == 0 || agentCtx.Messages[0].Role() != agentcore.RoleCompaction {
		t.Errorf("context should start with a compaction checkpoint, got %+v", agentCtx.Messages)
	}
	// ...and the event's summary must be exactly what that checkpoint tells the
	// model, so a reader of the report is not looking at a different document.
	if head, ok := agentCtx.Messages[0].(agentcore.CompactionMessage); ok && head.Text() != ce.Summary {
		t.Errorf("event summary differs from the checkpoint in the context:\nevent=%q\ncheckpoint=%q", ce.Summary, head.Text())
	}
}

func TestAutoCompactionDisabledWhenWindowUnknown(t *testing.T) {
	main := scriptedStream([]agentcore.AssistantMessage{
		{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	cfg := newRunCfg(main)
	cfg.SummaryStream = summaryStream("unused")
	cfg.ContextWindow = 0 // unknown → disabled
	cfg.Compaction = compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}

	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}
	before := len(agentCtx.Messages)

	events := collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))
	if ce := findCompaction(events); ce != nil {
		t.Fatalf("no compaction expected when window unknown, got %+v", ce)
	}
	// Context grows by one assistant reply only (no checkpoint replacement).
	if len(agentCtx.Messages) != before+1 {
		t.Errorf("context should be untouched by compaction, got %d messages", len(agentCtx.Messages))
	}
}

func TestCompactionEventEnvelope(t *testing.T) {
	env := eventEnvelope(agentcore.CompactionEvent{
		Reason:          "threshold",
		TokensBefore:    1000,
		TokensAfter:     400,
		SummarizedCount: 8,
		KeptCount:       3,
		Summary:         "## Goal\ncompacted",
	})
	if env["type"] != agentcore.EventCompaction {
		t.Errorf("type = %v, want %q", env["type"], agentcore.EventCompaction)
	}
	if env["tokensBefore"] != 1000 || env["tokensAfter"] != 400 {
		t.Errorf("token fields wrong: %+v", env)
	}
	if env["summarizedCount"] != 8 || env["keptCount"] != 3 {
		t.Errorf("count fields wrong: %+v", env)
	}
	// The checkpoint body is what the model now carries; a stream-json consumer
	// must be able to read it, not just the token counts.
	if env["summary"] != "## Goal\ncompacted" {
		t.Errorf("summary = %v, want the checkpoint text", env["summary"])
	}
	if _, hasErr := env["error"]; hasErr {
		t.Errorf("no error key expected on success: %+v", env)
	}
	// Failure envelope carries the error and no summary (nothing was written).
	failEnv := eventEnvelope(agentcore.CompactionEvent{Reason: "threshold", ErrorMessage: "boom"})
	if failEnv["error"] != "boom" {
		t.Errorf("error key expected on failure, got %+v", failEnv)
	}
	if _, hasSummary := failEnv["summary"]; hasSummary {
		t.Errorf("no summary key expected on failure: %+v", failEnv)
	}
	// A plain failed attempt is final: no retrying key, so a stream consumer that
	// predates retries still reads it as "the compaction failed".
	if _, hasRetry := failEnv["retrying"]; hasRetry {
		t.Errorf("a final failure must not carry retrying: %+v", failEnv)
	}
	if _, hasAttempt := failEnv["attempt"]; hasAttempt {
		t.Errorf("attempt is omitted when the producer did not count: %+v", failEnv)
	}
}

// TestCompactionRetryEnvelope pins the wire contract of an intermediate attempt:
// a retried failure is still reported, but with retrying=true and its attempt
// number so a machine consumer (stream-json, a CI log pipeline) can tell it apart
// from the final word instead of filing every transient failure as fatal.
func TestCompactionRetryEnvelope(t *testing.T) {
	env := eventEnvelope(agentcore.CompactionEvent{
		Reason:       "threshold",
		TokensBefore: 1000,
		TokensAfter:  1000,
		ErrorMessage: "summarizer down",
		Attempt:      2,
		Retrying:     true,
	})
	if env["error"] != "summarizer down" {
		t.Errorf("the failure must still be reported: %+v", env)
	}
	if env["attempt"] != 2 {
		t.Errorf("attempt = %v, want 2", env["attempt"])
	}
	if env["retrying"] != true {
		t.Errorf("retrying = %v, want true", env["retrying"])
	}

	start := eventEnvelope(agentcore.CompactionStartEvent{Reason: "threshold", TokensBefore: 1000, Attempt: 2})
	if start["attempt"] != 2 {
		t.Errorf("start attempt = %v, want 2", start["attempt"])
	}
}

// TestAutoCompactionRetriesUntilItSucceeds pins the sticky-compaction rule: once
// the context is over budget, a failed attempt is reported and retried in place
// until one succeeds. Two distinct things are asserted — that every failed
// attempt is still reported (so the failure stays observable), and that the
// retried attempt is what installs the checkpoint. A transient summarizer
// failure must not leave the run on an over-budget context.
func TestAutoCompactionRetriesUntilItSucceeds(t *testing.T) {
	main := scriptedStream([]agentcore.AssistantMessage{
		{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	cfg := newRunCfg(main)
	// Fail the first two attempts, then behave: the retry loop must reach attempt
	// three on its own.
	attempts := 0
	summary := summaryStream("## Goal\nrecovered")
	cfg.SummaryStream = func(ctx context.Context, model string, llm provider.LlmContext, c provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("summarizer down")
		}
		return summary(ctx, model, llm, c)
	}
	cfg.ContextWindow = 2000
	cfg.Compaction = compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}

	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}

	events := collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))

	var failed, final []agentcore.CompactionEvent
	for _, ev := range events {
		ce, ok := ev.(agentcore.CompactionEvent)
		if !ok {
			continue
		}
		if ce.ErrorMessage == "" {
			final = append(final, ce)
			continue
		}
		failed = append(failed, ce)
	}
	if len(failed) != 2 {
		t.Fatalf("expected both failed attempts to be reported, got %d: %+v", len(failed), failed)
	}
	for i, ce := range failed {
		if !ce.Retrying {
			t.Errorf("failed attempt %d must carry Retrying=true (another attempt follows)", i+1)
		}
		if ce.Attempt != i+1 {
			t.Errorf("failed attempt %d carries Attempt=%d", i+1, ce.Attempt)
		}
		// A failed attempt installs nothing, so the reported size is the unchanged one.
		if ce.TokensAfter != ce.TokensBefore {
			t.Errorf("on failure tokens must be unchanged: before=%d after=%d", ce.TokensBefore, ce.TokensAfter)
		}
	}
	if len(final) != 1 {
		t.Fatalf("expected exactly one successful compaction, got %d: %+v", len(final), final)
	}
	if !strings.Contains(final[0].Summary, "recovered") {
		t.Errorf("the installed checkpoint should be the successful attempt's, got %q", final[0].Summary)
	}
	if final[0].Attempt != 3 {
		t.Errorf("success on Attempt=%d, want 3", final[0].Attempt)
	}
	// And it really installed: the context now starts at the checkpoint.
	if len(agentCtx.Messages) == 0 || agentCtx.Messages[0].Role() != agentcore.RoleCompaction {
		t.Errorf("a successful retry must insert the checkpoint, got %+v", agentCtx.Messages)
	}
	// The run still ends normally, after the compaction resolved.
	if events[len(events)-1].EventType() != agentcore.EventAgentEnd {
		t.Errorf("run must end with agent_end, got %q", events[len(events)-1].EventType())
	}
}

// TestAutoCompactionRetryStopsOnCancel verifies the escape hatch: retrying is
// bound to the run's context, so cancelling the run ends the retry loop instead
// of spinning forever. The last failure must carry Retrying=false, because that
// is the signal a front-end uses to drop its "compacting" state — without it an
// interrupted run would leave the UI stuck on an indicator that never resolves.
func TestAutoCompactionRetryStopsOnCancel(t *testing.T) {
	main := scriptedStream([]agentcore.AssistantMessage{
		{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	cfg := newRunCfg(main)
	// A summarizer that always fails: only the cancellation can end the loop.
	cfg.SummaryStream = func(ctx context.Context, model string, llm provider.LlmContext, c provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return nil, errors.New("summarizer down")
	}
	cfg.ContextWindow = 2000
	cfg.Compaction = compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}

	agentCtx := &agentcore.AgentContext{Messages: bigUserMessages(12, 800)}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the first failure, which happens on the first turn end.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan []agentcore.AgentEvent, 1)
	go func() { done <- collectEvents(t, agentLoop(ctx, agentCtx, cfg)) }()

	var events []agentcore.AgentEvent
	select {
	case events = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the run must end the compaction retry loop")
	}

	var last agentcore.CompactionEvent
	for _, ev := range events {
		if ce, ok := ev.(agentcore.CompactionEvent); ok {
			last = ce
		}
	}
	if last.ErrorMessage == "" {
		t.Fatalf("expected a reported compaction failure, got %+v", last)
	}
	if last.Retrying {
		t.Error("the final attempt after cancellation must carry Retrying=false")
	}
	// Nothing was installed, so the context is untouched.
	if len(agentCtx.Messages) > 0 && agentCtx.Messages[0].Role() == agentcore.RoleCompaction {
		t.Error("a cancelled compaction must not insert a checkpoint")
	}
}

// TestCompactionRetryDelayIsCapped guards the backoff shape: it doubles so a
// transient failure retries promptly, and stops growing so a sustained outage
// does not become a hot request loop.
func TestCompactionRetryDelayIsCapped(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		d := compactionRetryDelay(attempt)
		if d < prev {
			t.Errorf("attempt %d delay %v went backwards from %v", attempt, d, prev)
		}
		if d > compactionRetryMax {
			t.Errorf("attempt %d delay %v exceeds the cap %v", attempt, d, compactionRetryMax)
		}
		prev = d
	}
	if got := compactionRetryDelay(1); got != compactionRetryBase {
		t.Errorf("first delay = %v, want %v", got, compactionRetryBase)
	}
}

// TestAutoCompactionKeepsUserInstructions verifies the loop's auto-compaction puts
// the ledger into the compacted context: the checkpoint that replaces the prefix
// must carry the user's requirements, not only a model-authored summary. This is
// the end-to-end path a long session actually takes.
func TestAutoCompactionKeepsUserInstructions(t *testing.T) {
	main := scriptedStream([]agentcore.AssistantMessage{
		{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
	})
	cfg := newRunCfg(main)
	cfg.SummaryStream = summaryStream("## Goal\ncompacted\n\n<user-instructions>\n- never write to deploy/\n</user-instructions>")
	cfg.ContextWindow = 2000
	cfg.Compaction = compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}

	msgs := bigUserMessages(12, 800)
	// One short, distinctive request early on: it must survive the collapse
	// verbatim even though its own text is negligible next to the big messages.
	msgs[2] = agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("keep the public API stable")},
	}
	agentCtx := &agentcore.AgentContext{Messages: msgs}

	collectEvents(t, agentLoop(context.Background(), agentCtx, cfg))

	if len(agentCtx.Messages) == 0 || agentCtx.Messages[0].Role() != agentcore.RoleCompaction {
		t.Fatalf("context should start with a compaction checkpoint, got %+v", agentCtx.Messages)
	}
	head := agentCtx.Messages[0].(agentcore.CompactionMessage)
	if head.UserInstructions == nil {
		t.Fatal("checkpoint should carry the user-instruction section")
	}
	if head.UserInstructions.Summary != "- never write to deploy/" {
		t.Errorf("digest = %q", head.UserInstructions.Summary)
	}
	if !strings.Contains(head.Text(), "keep the public API stable") {
		t.Errorf("verbatim request lost from the checkpoint:\n%s", head.Text())
	}
}
