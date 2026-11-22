// This file implements DrainStream (architecture deepening ①): the single place
// that consumes a loop EventStream. The streaming-text delta accounting (track
// how many bytes of the current assistant message have been surfaced, emit only
// the new suffix) and the message_update-vs-turn_end dispatch were previously
// hand-rolled in three places — the REPL, the headless driver, and the
// sub-agent tool. Each consumer now supplies callbacks and shares one drain
// loop, so a bug in the delta arithmetic or the dispatch has exactly one home.
package runtime

import (
	"context"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/perf"
)

// StreamHandler is the set of callbacks DrainStream invokes as it consumes a
// run's events. Every field is optional (nil = ignore that signal). Callbacks
// run on the draining goroutine, in event order.
type StreamHandler struct {
	// OnText receives each new suffix of the streaming assistant text: the bytes
	// produced since the last OnText call for the current turn. The final suffix
	// is flushed at turn end before OnTurnEnd, so a consumer that only implements
	// OnText still sees the complete text.
	OnText func(delta string)
	// OnTurnEnd fires once per completed turn, after the turn's text is fully
	// flushed, carrying the final assistant message and the tool results produced
	// during the turn. Consumers render tool activity here.
	OnTurnEnd func(msg agentcore.AssistantMessage, results []agentcore.ToolResultMessage)
	// OnEvent, when set, receives every raw event before the typed callbacks —
	// used by the stream-json protocol driver, which serialises the whole event.
	OnEvent func(ev agentcore.AgentEvent)
}

// DrainStream consumes stream to completion, invoking h's callbacks, and returns
// the final assistant message (or nil) plus the run's result error. It always
// drains every event even if a callback has side effects that fail, so the
// loop's producer goroutine never blocks on back-pressure (the no-leak
// contract). The final assistant message is taken from the run result when
// available, falling back to the last turn_end message observed on the stream.
func DrainStream(ctx context.Context, stream *LoopEventStream, h StreamHandler) (*agentcore.AssistantMessage, error) {
	// printed tracks how many bytes of the current streaming assistant message
	// have already been surfaced via OnText, so each update emits only the delta.
	printed := 0
	var lastTurn *agentcore.AssistantMessage

	// The streaming measurements below split one turn into the numbers that answer
	// "why does this feel slow": ttft (waiting for the first chunk), gap (stalls
	// between chunks), stream.turn (chars per second the user perceives) and
	// on_text (time the consumer — a channel send into the TUI — held the draining
	// goroutine). A turn's end is taken from message_end, the provider's own
	// completion signal, not from turn_end: the loop emits turn_end after it has
	// executed the turn's tool calls, so measuring there would fold a ten-second
	// bash call into the model's throughput.
	var (
		turnStart  time.Time
		firstAt    time.Time
		lastAt     time.Time
		turnClosed bool
	)

	emitText := func(text string) {
		if len(text) > printed {
			delta := text[printed:]
			if h.OnText != nil {
				start := time.Now()
				h.OnText(delta)
				perf.RecordN("drain.on_text", time.Since(start), len(delta))
			}
			printed = len(text)
		}
	}

	for ev := range stream.Events() {
		if h.OnEvent != nil {
			h.OnEvent(ev)
		}
		switch e := ev.(type) {
		case agentcore.TurnStartEvent:
			// turn_start is emitted before the request is assembled and sent, so
			// ttft is measured from here: it includes everything pigo does before the
			// provider can answer, which is the number a user experiences.
			turnStart, firstAt, lastAt, turnClosed = time.Now(), time.Time{}, time.Time{}, false
		case agentcore.MessageUpdateEvent:
			if a, ok := e.Message.(agentcore.AssistantMessage); ok {
				now := time.Now()
				if firstAt.IsZero() {
					if !turnStart.IsZero() {
						perf.Record("stream.ttft", now.Sub(turnStart))
					}
					firstAt = now
				} else {
					perf.Record("stream.gap", now.Sub(lastAt))
				}
				lastAt = now

				// ContentToText re-walks the whole accumulated message on every
				// update, so this stage grows with the answer length; measuring it
				// is how an O(n²) delta path shows up instead of hiding behind the
				// provider's name.
				start := time.Now()
				text := agentcore.ContentToText(a.Content)
				perf.RecordN("drain.concat", time.Since(start), len(text))
				emitText(text)
			}
		case agentcore.MessageEndEvent:
			// The provider is done with this turn (message_end precedes the loop's
			// turn_end, which also covers tool execution).
			if a, ok := e.Message.(agentcore.AssistantMessage); ok && !turnClosed {
				turnClosed = true
				now := time.Now()
				if !turnStart.IsZero() {
					perf.Record("llm.turn_total", now.Sub(turnStart))
				}
				if !firstAt.IsZero() {
					streamed := now.Sub(firstAt)
					surfaced := len(agentcore.ContentToText(a.Content))
					perf.RecordN("stream.turn", streamed, surfaced)
					if secs := streamed.Seconds(); secs > 0 {
						perf.SetGauge("stream.chars_per_sec", int64(float64(surfaced)/secs))
					}
				}
			}
		case agentcore.TurnEndEvent:
			// Flush any tail the streaming updates did not cover (covers providers
			// that only deliver the complete message at turn end), then reset for
			// the next turn and hand the turn to the consumer.
			now := time.Now()
			start := time.Now()
			text := agentcore.ContentToText(e.Message.Content)
			perf.RecordN("drain.concat", time.Since(start), len(text))
			emitText(text)
			printed = 0
			// A turn whose provider stream never reported message_end (an emit error
			// swallowed the terminal event) still owes the report its total.
			if !turnStart.IsZero() && !turnClosed {
				perf.Record("llm.turn_total", now.Sub(turnStart))
				turnClosed = true
			}
			m := e.Message
			lastTurn = &m
			if h.OnTurnEnd != nil {
				h.OnTurnEnd(e.Message, e.ToolResults)
			}
		}
	}

	// The channel is closed here, so the outcome is already recorded. Use a
	// non-cancelled context: otherwise a cancelled run can lose the messages the
	// loop already produced (the random select with ctx.Done()), and the caller
	// loses the aborted turn's partial text along with its stop reason.
	msgs, resErr := stream.Result(context.WithoutCancel(ctx))
	if resErr != nil {
		return lastTurn, resErr
	}
	if final := agentcore.LastAssistantOf(msgs); final != nil {
		return final, nil
	}
	return lastTurn, nil
}
