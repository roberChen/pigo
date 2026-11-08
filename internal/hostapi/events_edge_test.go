// This file tests the AgentEvent → SessionEvent mapper's edge cases. The mapper
// is the only place where the runtime's event stream narrows into the wire
// protocol, so what matters is that nothing panics, nothing is dropped silently
// (thinking deltas are the exception, by design), and every emitted notification
// carries the session id and a type the client can switch on.
package hostapi

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

func assistantMsg(content ...agentcore.Content) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList(content),
		StopReason: agentcore.StopReasonEndTurn,
	}
}

func updateOf(content ...agentcore.Content) agentcore.MessageUpdateEvent {
	return agentcore.MessageUpdateEvent{Message: assistantMsg(content...)}
}

// TestMapperHandlesEveryEventVariant walks the whole AgentEvent set: mapped
// events must produce exactly the documented notifications, unmapped ones must
// produce nothing at all (an empty notification would render as a blank line in
// every UI).
func TestMapperHandlesEveryEventVariant(t *testing.T) {
	msg := assistantMsg(agentcore.NewTextContent("hi"))
	cases := []struct {
		name string
		ev   agentcore.AgentEvent
		want []string // SessionEvent.Type values, in order; nil = not forwarded
	}{
		{"agent_start", agentcore.AgentStartEvent{SessionID: "other"}, []string{SessionEventAgentStart}},
		{"agent_end", agentcore.AgentEndEvent{Messages: []agentcore.AgentMessage{msg}}, []string{SessionEventAgentEnd}},
		{"turn_start", agentcore.TurnStartEvent{}, nil},
		{"turn_end", agentcore.TurnEndEvent{Message: msg}, []string{SessionEventTurnEnd}},
		{"message_start", agentcore.MessageStartEvent{Message: msg}, nil},
		{"message_update_thinking", updateOf(agentcore.NewThinkingContent("hm")), []string{SessionEventThinking}},
		{"message_update_text_only", updateOf(agentcore.NewTextContent("hm")), nil},
		{"message_update_empty", updateOf(), nil},
		{"message_end", agentcore.MessageEndEvent{Message: msg}, nil},
		{"tool_start", agentcore.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "bash", Args: map[string]any{"command": "ls"}}, []string{SessionEventToolStart}},
		{"tool_update", agentcore.ToolExecutionUpdateEvent{ToolCallID: "c1", ToolName: "bash"}, []string{SessionEventToolUpdate}},
		{"tool_end", agentcore.ToolExecutionEndEvent{ToolCallID: "c1", ToolName: "bash"}, []string{SessionEventToolEnd}},
		{"compaction_start", agentcore.CompactionStartEvent{Reason: "threshold", TokensBefore: 10}, nil},
		{"compaction", agentcore.CompactionEvent{Reason: "threshold"}, []string{SessionEventCompaction}},
		{"telemetry", agentcore.TelemetryEvent{Turns: 3}, nil},
		{"subagent_progress", agentcore.SubAgentProgressEvent{ToolCallID: "c1"}, []string{SessionEventSubAgent}},
		{"subagent_end", agentcore.SubAgentEndEvent{ToolCallID: "c1"}, []string{SessionEventSubAgentDone}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh mapper per case: the mapper is stateful for thinking.
			m := &EventMapper{SessionID: "s1"}
			got := m.Handle(tc.ev)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("%s forwarded as %+v", tc.name, got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s produced %d events, want %d: %+v", tc.name, len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if got[i].Type != want {
					t.Fatalf("%s[%d].Type = %q, want %q", tc.name, i, got[i].Type, want)
				}
				// The session id is the client's only demultiplexing key.
				if got[i].SessionID != "s1" {
					t.Fatalf("%s[%d].SessionID = %q, want s1", tc.name, i, got[i].SessionID)
				}
			}
		})
	}
}

// TestZeroMapperIsUsable documents the "a zero Mapper is usable" contract: it
// emits events with an empty session id instead of panicking.
func TestZeroMapperIsUsable(t *testing.T) {
	var m EventMapper
	got := m.Handle(agentcore.AgentStartEvent{})
	if len(got) != 1 || got[0].SessionID != "" || got[0].Type != SessionEventAgentStart {
		t.Fatalf("zero mapper = %+v", got)
	}
	// The same mapper keeps working afterwards (state initialised lazily, not
	// half-built).
	if ev := m.TextDelta("x"); ev == nil || ev.Text != "x" {
		t.Fatalf("zero mapper text delta = %+v", ev)
	}
}

// TestMapperSessionIDOverridesEventID: the conversation the mapper was built for
// wins over whatever the event claims, because a sub-agent event carries its own
// id and must still land on the parent's session stream.
func TestMapperSessionIDOverridesEventID(t *testing.T) {
	m := &EventMapper{SessionID: "parent"}
	got := m.Handle(agentcore.AgentStartEvent{SessionID: "child"})
	if len(got) != 1 || got[0].SessionID != "parent" {
		t.Fatalf("session id = %+v", got)
	}
}

func TestMapperTextDeltaEdges(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	if ev := m.TextDelta(""); ev != nil {
		t.Fatalf("empty delta = %+v, want nil", ev)
	}
	// Whitespace is content: dropping it would collapse the user's spacing.
	if ev := m.TextDelta(" "); ev == nil || ev.Text != " " {
		t.Fatalf("space delta = %+v", ev)
	}
	if ev := m.TextDelta("\n\n"); ev == nil || ev.Text != "\n\n" {
		t.Fatalf("newline delta = %+v", ev)
	}
	if ev := m.TextDelta("héllo 世界"); ev == nil || ev.Text != "héllo 世界" {
		t.Fatalf("unicode delta = %+v", ev)
	}
	if ev := m.TextDelta("x"); ev.Type != SessionEventTextDelta || ev.SessionID != "s" {
		t.Fatalf("text delta = %+v", ev)
	}
}

// TestMapperThinkingIsIncrementalAcrossMessages is the thinking stream's core
// contract: only the appended suffix is sent, the cursor resets at message end,
// and a provider that restarts the text re-sends it rather than truncating.
func TestMapperThinkingIsIncrementalAcrossMessages(t *testing.T) {
	m := &EventMapper{SessionID: "s"}
	think := func(text string) []SessionEvent {
		return m.Handle(updateOf(agentcore.NewThinkingContent(text)))
	}

	steps := []struct {
		text string
		want string
		emit bool
	}{
		{"t", "t", true},
		{"th", "h", true},
		{"thi", "i", true},
		{"thi", "", false}, // no growth: nothing new to say
		{"think", "nk", true},
		{"other", "other", true}, // provider replaced the text: full fallback
		{"", "", false},          // content vanished: nothing to emit
	}
	for _, step := range steps {
		got := think(step.text)
		if !step.emit {
			if got != nil {
				t.Fatalf("thinking %q emitted %+v, want nothing", step.text, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Text != step.want || got[0].Type != SessionEventThinking {
			t.Fatalf("thinking %q = %+v, want %q", step.text, got, step.want)
		}
	}

	// MessageEnd resets the cursor even though the event itself is not forwarded.
	if got := m.Handle(agentcore.MessageEndEvent{Message: assistantMsg()}); got != nil {
		t.Fatalf("message_end forwarded %+v", got)
	}
	if got := think("thinking"); len(got) != 1 || got[0].Text != "thinking" {
		t.Fatalf("after reset = %+v, want the full text", got)
	}

	// A second message starts from zero as well.
	m.Handle(agentcore.MessageEndEvent{})
	if got := think("again"); len(got) != 1 || got[0].Text != "again" {
		t.Fatalf("second message = %+v", got)
	}
}

func TestMapperThinkingContentShapes(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	// Text blocks are not thinking: a message with only text must not emit.
	if got := m.Handle(updateOf(agentcore.NewTextContent("answer"))); got != nil {
		t.Fatalf("text-only update forwarded as %+v", got)
	}
	// Mixed content: the thinking text is what is compared, text blocks are
	// ignored for the cursor.
	first := m.Handle(updateOf(agentcore.NewThinkingContent("ab"), agentcore.NewTextContent("answer")))
	if len(first) != 1 || first[0].Text != "ab" {
		t.Fatalf("mixed content = %+v", first)
	}
	second := m.Handle(updateOf(agentcore.NewThinkingContent("abc"), agentcore.NewTextContent("answer")))
	if len(second) != 1 || second[0].Text != "c" {
		t.Fatalf("mixed content delta = %+v", second)
	}
	// Multiple thinking blocks are concatenated before the delta is computed.
	multi := m.Handle(updateOf(agentcore.NewThinkingContent("abc"), agentcore.NewThinkingContent("de")))
	if len(multi) != 1 || multi[0].Text != "de" {
		t.Fatalf("multi-block thinking = %+v", multi)
	}
	// A non-assistant message in a MessageUpdateEvent has no thinking stream.
	if got := m.Handle(agentcore.MessageUpdateEvent{Message: agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewThinkingContent("x")},
	}}); got != nil {
		t.Fatalf("user message update forwarded as %+v", got)
	}
	// Neither does a nil message, or an assistant message delivered by pointer
	// (the runtime always sends a value; a pointer would be a silent drop, which
	// this test records rather than hides).
	if got := m.Handle(agentcore.MessageUpdateEvent{}); got != nil {
		t.Fatalf("nil message update forwarded as %+v", got)
	}
	ptr := assistantMsg(agentcore.NewThinkingContent("x"))
	if got := m.Handle(agentcore.MessageUpdateEvent{Message: &ptr}); got != nil {
		t.Fatalf("pointer message update forwarded as %+v", got)
	}
}

// TestMapperToolArgumentsMarshalFailure covers the fallback that keeps a tool
// call renderable when its arguments cannot be encoded. Sending "null" loses the
// arguments but keeps the transcript and the client's JSON parser intact.
func TestMapperToolArgumentsMarshalFailure(t *testing.T) {
	type inner struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name string
		args any
		want string
	}{
		{"nil_args", nil, `null`},
		{"empty_map", map[string]any{}, `{}`},
		{"map", map[string]any{"command": "ls"}, `{"command":"ls"}`},
		{"struct", inner{Name: "x"}, `{"name":"x"}`},
		{"raw", json.RawMessage(`{"a":1}`), `{"a":1}`},
		{"slice", []string{"a", "b"}, `["a","b"]`},
		{"func_value", map[string]any{"fn": func() {}}, `null`},
		{"chan_value", map[string]any{"ch": make(chan int)}, `null`},
		{"nan", map[string]any{"n": math.NaN()}, `null`},
		{"inf", map[string]any{"n": math.Inf(1)}, `null`},
		{"nested_func", map[string]any{"outer": map[string]any{"fn": func() {}}}, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &EventMapper{SessionID: "s"}
			got := m.Handle(agentcore.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "bash", Args: tc.args})
			if len(got) != 1 || got[0].Type != SessionEventToolStart {
				t.Fatalf("tool start = %+v", got)
			}
			if string(got[0].Arguments) != tc.want {
				t.Fatalf("arguments = %s, want %s", got[0].Arguments, tc.want)
			}
			if !json.Valid(got[0].Arguments) {
				t.Fatalf("arguments are not valid JSON: %s", got[0].Arguments)
			}
			if got[0].ToolCallID != "c1" || got[0].ToolName != "bash" {
				t.Fatalf("tool identity lost: %+v", got[0])
			}
		})
	}
}

// TestMapperToolContentEdges: tool result text is flattened, thinking blocks are
// not part of it, and a nil/empty content list becomes an empty string rather
// than a nil pointer the client would dereference.
func TestMapperToolContentEdges(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	end := m.Handle(agentcore.ToolExecutionEndEvent{ToolCallID: "c1", ToolName: "bash"})
	if len(end) != 1 || end[0].Result != "" || end[0].IsError {
		t.Fatalf("end without content = %+v", end)
	}
	if end[0].ToolCallID != "c1" || end[0].ToolName != "bash" {
		t.Fatalf("tool identity lost: %+v", end[0])
	}

	end = m.Handle(agentcore.ToolExecutionEndEvent{
		ToolCallID: "c1",
		Result: agentcore.AgentToolResult{Content: agentcore.ContentList{
			agentcore.NewTextContent("a"),
			agentcore.NewThinkingContent("hidden"),
			agentcore.NewTextContent("b"),
		}},
	})
	if len(end) != 1 || end[0].Result != "ab" {
		t.Fatalf("multi-block result = %+v", end)
	}

	upd := m.Handle(agentcore.ToolExecutionUpdateEvent{
		ToolCallID:    "c1",
		PartialResult: agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("partial")}},
	})
	if len(upd) != 1 || upd[0].Result != "partial" || upd[0].IsError {
		t.Fatalf("update = %+v", upd)
	}
	// A progress update is never an error, even when the eventual end is.
	upd = m.Handle(agentcore.ToolExecutionUpdateEvent{PartialResult: agentcore.AgentToolResult{}})
	if len(upd) != 1 || upd[0].Result != "" {
		t.Fatalf("empty update = %+v", upd)
	}
}

func TestMapperTurnEndMessageIsACopy(t *testing.T) {
	m := &EventMapper{SessionID: "s"}
	msg := assistantMsg(agentcore.NewTextContent("hello"))
	ev := agentcore.TurnEndEvent{Message: msg}

	got := m.Handle(ev)
	if len(got) != 1 || got[0].Message == nil {
		t.Fatalf("turn end = %+v", got)
	}
	if text := agentcore.ContentToText(got[0].Message.Content); text != "hello" {
		t.Fatalf("message text = %q", text)
	}
	// The mapper takes an address of a copy: mutating the emitted message must
	// not reach back into the runtime's event.
	got[0].Message.Content = agentcore.ContentList{agentcore.NewTextContent("mutated")}
	if agentcore.ContentToText(ev.Message.Content) != "hello" {
		t.Fatal("mapper aliased the event's message")
	}

	// An empty message still yields a non-nil body: clients read .message to
	// decide whether to replace their streamed text.
	empty := m.Handle(agentcore.TurnEndEvent{})
	if len(empty) != 1 || empty[0].Message == nil {
		t.Fatalf("empty turn end = %+v", empty)
	}
	if text := agentcore.ContentToText(empty[0].Message.Content); text != "" {
		t.Fatalf("empty turn end text = %q", text)
	}
}

func TestMapperCompactionFields(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	got := m.Handle(agentcore.CompactionEvent{
		Reason: "overflow", TokensBefore: 1000, TokensAfter: 250, SummarizedCount: 8, KeptCount: 2,
	})
	if len(got) != 1 {
		t.Fatalf("compaction = %+v", got)
	}
	if got[0].TokensBefore != 1000 || got[0].TokensAfter != 250 ||
		got[0].SummarizedCount != 8 || got[0].KeptCount != 2 || got[0].CompactionError != "" {
		t.Fatalf("compaction fields = %+v", got[0])
	}
	// The reason has no wire field: a client that needs it must infer it from the
	// surrounding events, so this test records that it is intentionally dropped.
	if got[0].Text != "" {
		t.Fatalf("compaction carried text: %+v", got[0])
	}

	// A zero compaction event is still forwarded (it is a real, if empty, state
	// change) and must not panic or carry an error message.
	zero := m.Handle(agentcore.CompactionEvent{})
	if len(zero) != 1 || zero[0].CompactionError != "" {
		t.Fatalf("zero compaction = %+v", zero)
	}
	failed := m.Handle(agentcore.CompactionEvent{ErrorMessage: "summarizer failed"})
	if len(failed) != 1 || failed[0].CompactionError != "summarizer failed" {
		t.Fatalf("failed compaction = %+v", failed)
	}
}

func TestMapperSubAgentFields(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	got := m.Handle(agentcore.SubAgentProgressEvent{
		ToolCallID: "t1", Description: "scan repo", Activity: "Reading", Tokens: 99,
	})
	if len(got) != 1 || got[0].Type != SessionEventSubAgent {
		t.Fatalf("subagent progress = %+v", got)
	}
	if got[0].ToolCallID != "t1" || got[0].Description != "scan repo" || got[0].Activity != "Reading" {
		t.Fatalf("subagent fields = %+v", got[0])
	}
	// Tokens has no wire field; the client estimates it, so it must not leak into
	// an unrelated field.
	if got[0].Text != "" || got[0].Result != "" {
		t.Fatalf("subagent progress leaked into text/result: %+v", got[0])
	}

	// A bare progress event is legal (the status line shows whatever it has).
	bare := m.Handle(agentcore.SubAgentProgressEvent{})
	if len(bare) != 1 || bare[0].Type != SessionEventSubAgent {
		t.Fatalf("bare subagent progress = %+v", bare)
	}

	done := m.Handle(agentcore.SubAgentEndEvent{})
	if len(done) != 1 || done[0].Type != SessionEventSubAgentDone || done[0].ToolCallID != "" {
		t.Fatalf("bare subagent end = %+v", done)
	}
}

// TestMapperToolStartArgumentsAreIndependent makes sure the emitted Arguments do
// not alias a buffer another event could rewrite: the server queues these
// notifications and encodes them after the mapper returns.
func TestMapperToolStartArgumentsAreIndependent(t *testing.T) {
	m := &EventMapper{SessionID: "s"}
	args := map[string]any{"command": "ls"}
	first := m.Handle(agentcore.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "bash", Args: args})
	args["command"] = "rm -rf /"
	second := m.Handle(agentcore.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "bash", Args: args})

	if string(first[0].Arguments) != `{"command":"ls"}` {
		t.Fatalf("first arguments changed after the caller mutated its map: %s", first[0].Arguments)
	}
	if string(second[0].Arguments) != `{"command":"rm -rf /"}` {
		t.Fatalf("second arguments = %s", second[0].Arguments)
	}
	if len(first[0].Arguments) == 0 || &first[0].Arguments[0] == &second[0].Arguments[0] {
		t.Fatal("two events share the same argument buffer")
	}
}

func TestIsThinkingUpdateEdges(t *testing.T) {
	cases := []struct {
		name string
		ev   agentcore.AgentEvent
		want bool
	}{
		{"thinking_value", agentcore.MessageUpdateEvent{AssistantMessageEvent: provider.StreamThinkingEvent{}}, true},
		{"thinking_pointer", agentcore.MessageUpdateEvent{AssistantMessageEvent: &provider.StreamThinkingEvent{}}, false},
		{"text", agentcore.MessageUpdateEvent{AssistantMessageEvent: provider.StreamTextEvent{}}, false},
		{"nil_provider_event", agentcore.MessageUpdateEvent{}, false},
		{"other_event", agentcore.AgentStartEvent{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsThinkingUpdate(tc.ev); got != tc.want {
				t.Fatalf("IsThinkingUpdate(%T) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}
