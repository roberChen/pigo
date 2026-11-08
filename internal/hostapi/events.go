// This file maps the runtime's AgentEvent stream onto the host protocol's
// SessionEvent notifications. The mapping is deliberately one-way and
// lossless-enough: a client can render a complete transcript from
// (text_delta | turn_end) plus the tool lifecycle, and can ignore everything it
// does not understand (the server never requires an ack).
package hostapi

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// EventMapper converts one session's AgentEvent stream into SessionEvents. It
// is stateful only for thinking deltas: the runtime reports a growing partial
// message, so the mapper remembers what it already sent and emits the suffix
// (the same contract runtime.StreamHandler.OnText has for text).
//
// A zero Mapper is usable. It is not safe for concurrent use; the server drives
// one mapper from the goroutine that drains the run stream.
type EventMapper struct {
	SessionID string

	prevThinking string
}

// Handle returns the SessionEvents that correspond to ev. An event that has no
// wire representation (turn_start, message_start, telemetry, …) returns nil.
func (m *EventMapper) Handle(ev agentcore.AgentEvent) []SessionEvent {
	switch e := ev.(type) {
	case agentcore.AgentStartEvent:
		return []SessionEvent{{SessionID: m.SessionID, Type: SessionEventAgentStart}}
	case agentcore.AgentEndEvent:
		return []SessionEvent{{SessionID: m.SessionID, Type: SessionEventAgentEnd}}
	case agentcore.MessageUpdateEvent:
		msg, ok := e.Message.(agentcore.AssistantMessage)
		if !ok {
			return nil
		}
		return m.thinkingDelta(msg.Content)
	case agentcore.MessageEndEvent:
		// A turn's thinking stream ends with the message; reset the cursor so the
		// next message starts from zero rather than being truncated by a stale
		// length. (A duplicated full-text update is harmless; a missing one is not.)
		m.prevThinking = ""
		return nil
	case agentcore.TurnEndEvent:
		msg := e.Message
		return []SessionEvent{{SessionID: m.SessionID, Type: SessionEventTurnEnd, Message: &msg}}
	case agentcore.ToolExecutionStartEvent:
		args, err := json.Marshal(e.Args)
		if err != nil {
			args = []byte("null")
		}
		return []SessionEvent{{
			SessionID:  m.SessionID,
			Type:       SessionEventToolStart,
			ToolCallID: e.ToolCallID,
			ToolName:   e.ToolName,
			Arguments:  bytes.Clone(args),
		}}
	case agentcore.ToolExecutionUpdateEvent:
		return []SessionEvent{{
			SessionID:  m.SessionID,
			Type:       SessionEventToolUpdate,
			ToolCallID: e.ToolCallID,
			ToolName:   e.ToolName,
			Result:     agentcore.ContentToText(e.PartialResult.Content),
		}}
	case agentcore.ToolExecutionEndEvent:
		return []SessionEvent{{
			SessionID:  m.SessionID,
			Type:       SessionEventToolEnd,
			ToolCallID: e.ToolCallID,
			ToolName:   e.ToolName,
			Result:     agentcore.ContentToText(e.Result.Content),
			IsError:    e.IsError,
		}}
	case agentcore.CompactionStartEvent:
		// The start event carries only a reason and a token estimate, which has no
		// field on SessionEvent; the completion below is what a UI renders.
		return nil
	case agentcore.CompactionEvent:
		return []SessionEvent{{
			SessionID:       m.SessionID,
			Type:            SessionEventCompaction,
			TokensBefore:    e.TokensBefore,
			TokensAfter:     e.TokensAfter,
			SummarizedCount: e.SummarizedCount,
			KeptCount:       e.KeptCount,
			CompactionError: e.ErrorMessage,
		}}
	case agentcore.SubAgentProgressEvent:
		return []SessionEvent{{
			SessionID:   m.SessionID,
			Type:        SessionEventSubAgent,
			ToolCallID:  e.ToolCallID,
			Description: e.Description,
			Activity:    e.Activity,
		}}
	case agentcore.SubAgentEndEvent:
		return []SessionEvent{{
			SessionID:  m.SessionID,
			Type:       SessionEventSubAgentDone,
			ToolCallID: e.ToolCallID,
		}}
	}
	return nil
}

// TextDelta turns one runtime text delta into a wire event. Text arrives through
// StreamHandler.OnText rather than the event stream because the runtime already
// computes the delta there (re-emitting a provider's full text would make the
// client duplicate it).
func (m *EventMapper) TextDelta(text string) *SessionEvent {
	if text == "" {
		return nil
	}
	return &SessionEvent{SessionID: m.SessionID, Type: SessionEventTextDelta, Text: text}
}

// thinkingDelta emits only the newly appended thinking text of a partial
// message. A provider that replaces streamed thinking instead of appending it
// makes the mapper fall back to the whole current text, which is a superset and
// therefore still readable; the authoritative full text is on turn_end.
func (m *EventMapper) thinkingDelta(content agentcore.ContentList) []SessionEvent {
	current := agentcore.ThinkingText(content)
	if current == "" {
		return nil
	}
	delta := current
	if strings.HasPrefix(current, m.prevThinking) {
		delta = current[len(m.prevThinking):]
	}
	m.prevThinking = current
	if delta == "" {
		return nil
	}
	return []SessionEvent{{SessionID: m.SessionID, Type: SessionEventThinking, Text: delta}}
}

// IsThinkingUpdate reports whether ev is a provider thinking delta, used by the
// mapper tests and by callers that want to suppress thinking notifications.
func IsThinkingUpdate(ev agentcore.AgentEvent) bool {
	mu, ok := ev.(agentcore.MessageUpdateEvent)
	if !ok {
		return false
	}
	_, ok = mu.AssistantMessageEvent.(provider.StreamThinkingEvent)
	return ok
}
