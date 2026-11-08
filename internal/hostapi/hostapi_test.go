// This file tests the host protocol's envelope helpers and the AgentEvent →
// SessionEvent mapper. The mapper is the piece most likely to drift when the
// runtime's event set changes, so it is exercised over every event kind the
// server actually forwards.
package hostapi

import (
	"encoding/json"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

func TestEnvelopeShapes(t *testing.T) {
	req := Envelope{ID: "1", Method: MethodPrompt, Params: json.RawMessage(`{"sessionId":"s"}`)}
	if req.IsEvent() || req.IsResponse() {
		t.Fatalf("request misclassified: event=%v response=%v", req.IsEvent(), req.IsResponse())
	}
	if req.Method != MethodPrompt {
		t.Fatalf("method lost: %q", req.Method)
	}
	var p PromptParams
	if err := req.DecodeParams(&p); err != nil {
		t.Fatalf("DecodeParams: %v", err)
	}
	if p.SessionID != "s" {
		t.Fatalf("params decoded as %+v", p)
	}

	resp := Envelope{ID: "1", Result: json.RawMessage(`{"text":"hi"}`)}
	if !resp.IsResponse() || resp.IsEvent() {
		t.Fatal("response misclassified")
	}
	var r PromptResult
	if err := resp.DecodeResult(&r); err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	if r.Text != "hi" {
		t.Fatalf("result decoded as %+v", r)
	}

	ev := Envelope{Event: EventSession, Params: json.RawMessage(`{}`)}
	if !ev.IsEvent() || ev.IsResponse() {
		t.Fatal("event misclassified")
	}

	// An error response has no result but still correlates by id.
	bad := Envelope{ID: "2", Error: Err(CodeBusy, "busy")}
	if !bad.IsResponse() {
		t.Fatal("error response misclassified")
	}
	if got := bad.Error.Error(); got != "busy" {
		t.Fatalf("error text = %q", got)
	}
}

func TestDecodeParamsEmptyLeavesValue(t *testing.T) {
	// session.close takes params, but a no-argument method must not fail just
	// because the client omitted the field.
	var p SessionCloseParams
	if err := (Envelope{Method: MethodServerStatus}).DecodeParams(&p); err != nil {
		t.Fatalf("empty params: %v", err)
	}
	if p.ID != "" {
		t.Fatalf("empty params mutated value: %+v", p)
	}
}

func TestMapperTextDeltaAndTurnEnd(t *testing.T) {
	m := &EventMapper{SessionID: "s1"}
	if ev := m.TextDelta(""); ev != nil {
		t.Fatalf("empty delta should be dropped, got %+v", ev)
	}
	ev := m.TextDelta("hello")
	if ev == nil || ev.Type != SessionEventTextDelta || ev.Text != "hello" || ev.SessionID != "s1" {
		t.Fatalf("text delta = %+v", ev)
	}

	msg := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn}
	msg.Content = agentcore.ContentList{agentcore.NewTextContent("hello")}
	got := m.Handle(agentcore.TurnEndEvent{Message: msg})
	if len(got) != 1 || got[0].Type != SessionEventTurnEnd {
		t.Fatalf("turn end = %+v", got)
	}
	if got[0].Message == nil || agentcore.ContentToText(got[0].Message.Content) != "hello" {
		t.Fatalf("turn end lost the message: %+v", got[0].Message)
	}
}

func TestMapperThinkingIsIncremental(t *testing.T) {
	m := &EventMapper{SessionID: "s"}
	partial := func(text string) agentcore.MessageUpdateEvent {
		msg := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
		msg.Content = agentcore.ContentList{agentcore.NewThinkingContent(text)}
		return agentcore.MessageUpdateEvent{Message: msg}
	}

	first := m.Handle(partial("thin"))
	if len(first) != 1 || first[0].Text != "thin" || first[0].Type != SessionEventThinking {
		t.Fatalf("first thinking = %+v", first)
	}
	second := m.Handle(partial("thinking"))
	if len(second) != 1 || second[0].Text != "king" {
		t.Fatalf("second thinking should carry only the suffix, got %+v", second)
	}
	// A provider that restarts the text must not be silently truncated.
	third := m.Handle(partial("new"))
	if len(third) != 1 || third[0].Text != "new" {
		t.Fatalf("restarted thinking = %+v", third)
	}

	// MessageEnd resets the cursor so the next message starts fresh.
	m.Handle(agentcore.MessageEndEvent{Message: agentcore.AssistantMessage{}})
	fourth := m.Handle(partial("again"))
	if len(fourth) != 1 || fourth[0].Text != "again" {
		t.Fatalf("after reset = %+v", fourth)
	}
}

func TestMapperToolLifecycle(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	start := m.Handle(agentcore.ToolExecutionStartEvent{
		ToolCallID: "c1", ToolName: "bash", Args: map[string]any{"command": "ls"},
	})
	if len(start) != 1 || start[0].Type != SessionEventToolStart || start[0].ToolName != "bash" {
		t.Fatalf("tool start = %+v", start)
	}
	var args map[string]string
	if err := json.Unmarshal(start[0].Arguments, &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if args["command"] != "ls" {
		t.Fatalf("arguments = %v", args)
	}

	upd := m.Handle(agentcore.ToolExecutionUpdateEvent{
		ToolCallID: "c1", ToolName: "bash",
		PartialResult: agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("partial")}},
	})
	if len(upd) != 1 || upd[0].Result != "partial" || upd[0].Type != SessionEventToolUpdate {
		t.Fatalf("tool update = %+v", upd)
	}

	end := m.Handle(agentcore.ToolExecutionEndEvent{
		ToolCallID: "c1", ToolName: "bash",
		Result:  agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("done")}},
		IsError: true,
	})
	if len(end) != 1 || end[0].Result != "done" || !end[0].IsError {
		t.Fatalf("tool end = %+v", end)
	}
}

func TestMapperCompactionAndAgents(t *testing.T) {
	m := &EventMapper{SessionID: "s"}

	if got := m.Handle(agentcore.CompactionStartEvent{Reason: "threshold", TokensBefore: 10}); got != nil {
		t.Fatalf("compaction start should not be forwarded, got %+v", got)
	}
	got := m.Handle(agentcore.CompactionEvent{
		Reason: "threshold", TokensBefore: 10, TokensAfter: 4, SummarizedCount: 6, KeptCount: 2,
	})
	if len(got) != 1 || got[0].Type != SessionEventCompaction || got[0].TokensAfter != 4 || got[0].SummarizedCount != 6 {
		t.Fatalf("compaction = %+v", got)
	}
	failed := m.Handle(agentcore.CompactionEvent{ErrorMessage: "boom"})
	if len(failed) != 1 || failed[0].CompactionError != "boom" {
		t.Fatalf("failed compaction = %+v", failed)
	}

	if got := m.Handle(agentcore.AgentStartEvent{SessionID: "s"}); len(got) != 1 || got[0].Type != SessionEventAgentStart {
		t.Fatalf("agent start = %+v", got)
	}
	if got := m.Handle(agentcore.AgentEndEvent{}); len(got) != 1 || got[0].Type != SessionEventAgentEnd {
		t.Fatalf("agent end = %+v", got)
	}

	prog := m.Handle(agentcore.SubAgentProgressEvent{ToolCallID: "t", Description: "scan", Activity: "Reading"})
	if len(prog) != 1 || prog[0].Activity != "Reading" || prog[0].Description != "scan" {
		t.Fatalf("subagent progress = %+v", prog)
	}
	done := m.Handle(agentcore.SubAgentEndEvent{ToolCallID: "t"})
	if len(done) != 1 || done[0].Type != SessionEventSubAgentDone {
		t.Fatalf("subagent end = %+v", done)
	}

	// Unmapped events (turn start, telemetry, …) must be dropped, not emitted as
	// empty notifications.
	for _, ev := range []agentcore.AgentEvent{
		agentcore.TurnStartEvent{},
		agentcore.TelemetryEvent{},
		agentcore.MessageStartEvent{Message: agentcore.AssistantMessage{}},
	} {
		if got := m.Handle(ev); got != nil {
			t.Fatalf("%T should not be forwarded, got %+v", ev, got)
		}
	}
}

func TestIsThinkingUpdate(t *testing.T) {
	thinking := agentcore.MessageUpdateEvent{AssistantMessageEvent: provider.StreamThinkingEvent{}}
	text := agentcore.MessageUpdateEvent{AssistantMessageEvent: provider.StreamTextEvent{}}
	if !IsThinkingUpdate(thinking) {
		t.Fatal("thinking update not detected")
	}
	if IsThinkingUpdate(text) {
		t.Fatal("text update misdetected as thinking")
	}
	if IsThinkingUpdate(agentcore.TurnStartEvent{}) {
		t.Fatal("non-update misdetected")
	}
}
