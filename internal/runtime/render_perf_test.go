// This file tests the streaming measurements DrainStream records: ttft, the gap
// between chunks, per-turn throughput, and the time the consumer took. The
// collector is process-wide, so each test resets it and checks the stages the
// drain is responsible for rather than an exact total.
package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/perf"
)

// TestDrainStreamRecordsStreamingStages drives one scripted turn and asserts the
// pipeline stages are present, counted once per turn where they should be, and
// that the throughput gauge carries the surfaced byte count.
func TestDrainStreamRecordsStreamingStages(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)

	ctx := context.Background()
	stream := emitStream(func(s *LoopEventStream) {
		_ = s.Emit(ctx, agentcore.TurnStartEvent{})
		_ = s.Emit(ctx, agentcore.MessageUpdateEvent{Message: assistantWith("Hel")})
		_ = s.Emit(ctx, agentcore.MessageUpdateEvent{Message: assistantWith("Hello")})
		_ = s.Emit(ctx, agentcore.MessageEndEvent{Message: assistantWith("Hello")})
		_ = s.Emit(ctx, agentcore.TurnEndEvent{Message: assistantWith("Hello")})
		s.SetResult([]agentcore.AgentMessage{assistantWith("Hello")})
	})

	if _, err := DrainStream(ctx, stream, StreamHandler{OnText: func(string) {}}); err != nil {
		t.Fatalf("DrainStream: %v", err)
	}

	stages := map[string]perf.Stage{}
	for _, st := range perf.Snapshot().Stages {
		stages[st.Name] = st
	}
	for _, name := range []string{"stream.ttft", "stream.gap", "stream.turn", "llm.turn_total", "drain.concat", "drain.on_text"} {
		if _, ok := stages[name]; !ok {
			t.Fatalf("stage %q not recorded; recorded: %v", name, keysOf(stages))
		}
	}
	if got := stages["stream.ttft"].Count; got != 1 {
		t.Errorf("stream.ttft count = %d, want 1 per turn", got)
	}
	if got := stages["stream.gap"].Count; got != 1 {
		t.Errorf("stream.gap count = %d, want 1 (one gap between two chunks)", got)
	}
	if got := stages["stream.turn"].Bytes; got != int64(len("Hello")) {
		t.Errorf("stream.turn bytes = %d, want %d", got, len("Hello"))
	}
	if got := stages["drain.on_text"].Bytes; got != int64(len("Hello")) {
		t.Errorf("drain.on_text bytes = %d, want %d (deltas, not re-emitted text)", got, len("Hello"))
	}
	if rate := perf.Gauge("stream.chars_per_sec"); rate <= 0 {
		t.Errorf("stream.chars_per_sec gauge = %d, want > 0", rate)
	}
}

// TestDrainStreamTurnEndOnlyProviderStillRecordsTurn covers the provider that
// streams no deltas at all: the turn is still attributed (no ttft, no throughput
// samples — there is no first chunk to measure from) instead of vanishing from
// the report.
func TestDrainStreamTurnEndOnlyProviderStillRecordsTurn(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)

	ctx := context.Background()
	stream := emitStream(func(s *LoopEventStream) {
		_ = s.Emit(ctx, agentcore.TurnStartEvent{})
		time.Sleep(2 * time.Millisecond)
		_ = s.Emit(ctx, agentcore.MessageEndEvent{Message: assistantWith("all at once")})
		_ = s.Emit(ctx, agentcore.TurnEndEvent{Message: assistantWith("all at once")})
		s.SetResult([]agentcore.AgentMessage{assistantWith("all at once")})
	})

	if _, err := DrainStream(ctx, stream, StreamHandler{OnText: func(string) {}}); err != nil {
		t.Fatalf("DrainStream: %v", err)
	}
	stages := map[string]perf.Stage{}
	for _, st := range perf.Snapshot().Stages {
		stages[st.Name] = st
	}
	if _, ok := stages["stream.ttft"]; ok {
		t.Fatalf("stream.ttft recorded for a turn with no update events")
	}
	if got := stages["llm.turn_total"].Count; got != 1 {
		t.Fatalf("llm.turn_total count = %d, want 1", got)
	}
	if got := stages["drain.on_text"].Bytes; got != int64(len("all at once")) {
		t.Fatalf("drain.on_text bytes = %d, want %d", got, len("all at once"))
	}
}

// keysOf lists a stage map's names for a failure message.
func keysOf(m map[string]perf.Stage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
