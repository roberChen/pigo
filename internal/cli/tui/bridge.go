package tui

import (
	"context"
	"encoding/json"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/perf"
	"github.com/roberChen/pigo/internal/provider"
	"github.com/roberChen/pigo/internal/runtime"
)

// This file bridges the agent run seam (runtime.StartRun + runtime.DrainStream)
// to Bubble Tea (US-004, SPEC 5.1 bridge / 3.2). The agent loop runs on its own
// goroutine and emits AgentEvents; a Bubble Tea program consumes tea.Msg values
// one at a time from its Update loop. The bridge is a pump: a goroutine drains
// the run and converts every event into the matching tea.Msg (see msgs.go),
// sending it into a buffered channel; a tea.Cmd (waitForEvent) receives one msg
// per Update tick. The channel is the only synchronization point, so the
// producer never touches the model and the model never touches the run — all
// state transitions happen on the tea goroutine.
//
// Back-pressure is intentional: the channel blocks the draining goroutine when
// the buffer is full, so no event is ever dropped (the tea loop always catches
// up). Node #388 wires startRun into Model.Init/Update; this file only provides
// the reusable, unit-testable primitives.

// eventChanCap is the buffer size of the bridge channel. A modest buffer lets a
// burst of tool events queue without blocking the run's goroutine on every send,
// while still bounding memory (blocking, never dropping, past the cap).
const eventChanCap = 64

// newEventChan allocates the buffered channel the bridge pumps run events
// through.
func newEventChan() chan tea.Msg {
	return make(chan tea.Msg, eventChanCap)
}

// newStreamHandler builds the runtime.StreamHandler that converts each run event
// into a tea.Msg and sends it into ch. Sends block when ch is full, applying
// back-pressure to the draining goroutine so no event is lost. It is factored
// out of pump so the callback→msg conversion can be unit-tested without a real
// provider run (see bridge_test.go).
func newStreamHandler(ch chan tea.Msg, extra func(agentcore.AgentEvent)) runtime.StreamHandler {
	return runtime.StreamHandler{
		OnText: func(delta string) {
			sendEvent(ch, textDeltaMsg{delta: delta})
		},
		OnTurnEnd: func(msg agentcore.AssistantMessage, results []agentcore.ToolResultMessage) {
			sendEvent(ch, turnEndMsg{msg: msg, results: results})
		},
		OnEvent: func(ev agentcore.AgentEvent) {
			// Deliver observer events (plugin notifier, SessionEnd/PreCompact hook)
			// first, then translate into TUI messages.
			if extra != nil {
				extra(ev)
			}
			switch e := ev.(type) {
			case agentcore.MessageUpdateEvent:
				// Only reasoning updates are interesting here: text already streams
				// through OnText, and tool-call deltas are shown by the tool cards.
				if _, ok := e.AssistantMessageEvent.(provider.StreamThinkingEvent); ok {
					if partial, ok := e.Message.(agentcore.AssistantMessage); ok {
						if text := agentcore.ThinkingText(partial.Content); text != "" {
							sendEvent(ch, thinkingDeltaMsg{text: text})
						}
					}
				}
			case agentcore.ToolExecutionStartEvent:
				input := argsToMap(e.Args)
				sendEvent(ch, toolStartMsg{id: e.ToolCallID, name: e.ToolName, input: input, rawArgs: rawToolArgs(e.Args, input)})
			case agentcore.ToolExecutionUpdateEvent:
				sendEvent(ch, toolUpdateMsg{id: e.ToolCallID, partial: agentcore.ContentToText(e.PartialResult.Content)})
			case agentcore.ToolExecutionEndEvent:
				sendEvent(ch, toolEndMsg{id: e.ToolCallID, ok: !e.IsError, result: agentcore.ContentToText(e.Result.Content), details: e.Result.Details})
			case agentcore.SubAgentProgressEvent:
				sendEvent(ch, subagentProgressMsg{id: e.ToolCallID, desc: e.Description, activity: e.Activity, tokens: e.Tokens})
			case agentcore.SubAgentEndEvent:
				sendEvent(ch, subagentDoneMsg{id: e.ToolCallID})
			case agentcore.TelemetryEvent:
				sendEvent(ch, telemetryMsg{ev: e})
			case agentcore.CompactionStartEvent:
				sendEvent(ch, compactionStartMsg{attempt: e.Attempt})
			case agentcore.CompactionEvent:
				sendEvent(ch, compactionMsg{ev: e})
			}
		},
	}
}

// argsToMap coerces a tool call's untyped Args into a map[string]any. The event
// layer carries Args as an untyped any: the tool executor emits it as a
// json.RawMessage (the raw decoded JSON arguments), but a caller may also hand
// an already-decoded map. Both are supported here so the tool card can show the
// call's arguments; anything that is not a JSON object yields nil.
func argsToMap(args any) map[string]any {
	switch v := args.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		return unmarshalArgsMap(v)
	case []byte:
		return unmarshalArgsMap(v)
	case string:
		return unmarshalArgsMap([]byte(v))
	}
	return nil
}

// unmarshalArgsMap parses JSON object bytes into a map, returning nil for empty
// input or anything that is not a JSON object.
func unmarshalArgsMap(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// pump runs the agent loop to completion on the calling goroutine, converting
// every event to a tea.Msg on ch, and finally sends a runEndMsg carrying the
// run's result error. It is meant to be launched as a goroutine by startRun.
func pump(ctx context.Context, ch chan tea.Msg, agentCtx *agentcore.AgentContext, cfg runtime.RunConfig, onEvent func(agentcore.AgentEvent)) {
	stream := runtime.StartRun(ctx, agentCtx, cfg)
	_, err := runtime.DrainStream(ctx, stream, newStreamHandler(ch, onEvent))
	// One perf report per user-visible run (never per sub-agent): PIGO_PERF makes
	// this a diagnostic line in the transcript, PIGO_PERF_LOG appends JSON.
	perf.RunEnd(cfg.SessionID)
	ch <- runEndMsg{err: err}
}

// sendEvent hands one bridge message to the UI goroutine. The send blocks while
// the UI is behind (the channel is bounded and nothing may be dropped), so the
// time it takes is the run goroutine's back-pressure wait — the direct measure of
// "the model is done with this chunk but the TUI has not drawn the last ones".
// It is recorded on every send, which is cheap next to the send itself.
func sendEvent(ch chan tea.Msg, msg tea.Msg) {
	start := time.Now()
	ch <- msg
	perf.Record("tui.bridge.send_wait", time.Since(start))
}

// waitForEvent returns a tea.Cmd that blocks until the next bridge msg arrives.
// The Update loop re-issues it after handling each msg (except runEndMsg) to
// keep pulling events one at a time, so ordering is preserved and the tea
// goroutine never spins.
func waitForEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

// startRun launches the run pump on a new goroutine and returns the channel it
// feeds together with the first waitForEvent Cmd. The caller (node #388's model)
// stores the channel and, on every subsequent event, issues waitForEvent(ch)
// again to pull the next msg. Returning the channel keeps the bridge
// self-contained: the model owns the handle and decides when to stop pulling
// (after runEndMsg).
func startRun(ctx context.Context, agentCtx *agentcore.AgentContext, cfg runtime.RunConfig, onEvent func(agentcore.AgentEvent)) (chan tea.Msg, tea.Cmd) {
	ch := newEventChan()
	// Install the run's asker: the question tool delivers its question on this
	// same channel and blocks until the UI answers, so the model can pause
	// mid-run without a second I/O path (see ask.go).
	go pump(agentcore.WithAskUser(ctx, askViaChannel(ch)), ch, agentCtx, cfg, onEvent)
	return ch, waitForEvent(ch)
}
