package runtime

// This file covers recovery from a model turn whose tool-call arguments are not
// valid JSON (a truncated or corrupted call, which models do emit). The loop must
// not run the tool on garbage and must not abort the run: the call is answered
// with an error tool result explaining the parse failure, and the model gets
// another turn to re-issue it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// TestLoopRecoversFromMalformedToolArguments drives the real loop with a turn
// whose tool call carries unparseable arguments followed by a normal text turn.
// It asserts the tool never executes, the call is answered with an explanatory
// error result, and the run reaches the model's next turn instead of dying.
func TestLoopRecoversFromMalformedToolArguments(t *testing.T) {
	ran := false
	tool := execTool{
		name:   "echo",
		schema: `{"type":"object","required":["msg"],"properties":{"msg":{"type":"string"}}}`,
		run: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			ran = true
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("echo")}}, nil
		},
	}
	p := &fauxProvider{
		name:   "faux",
		models: []provider.Model{{Provider: "faux", ID: "faux"}},
		turns: []fauxTurn{
			toolCallTurn("call-1", "echo", `{"msg":`), // truncated JSON
			textTurn("recovered"),
		},
	}
	cfg := newFauxRunCfg(p, tool)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("start")}},
	}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	if ran {
		t.Error("the tool must not execute when its arguments do not parse")
	}
	if got := countKind(kinds, agentcore.EventToolExecutionStart); got != 0 {
		t.Errorf("expected no tool_execution_start for a rejected call, got %d", got)
	}
	if got := countKind(kinds, agentcore.EventToolExecutionEnd); got != 1 {
		t.Errorf("expected the rejected call to be finalized once, got %d", got)
	}

	var result agentcore.ToolResultMessage
	found := false
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			result, found = tr, true
		}
	}
	if !found {
		t.Fatalf("no tool result message in %+v", msgs)
	}
	if result.ToolCallID != "call-1" || !result.IsError {
		t.Errorf("result = %+v, want an error result for call-1", result)
	}
	text := agentcore.ContentToText(result.Content)
	for _, want := range []string{"not valid JSON", "re-issue the call with a single valid JSON object"} {
		if !strings.Contains(text, want) {
			t.Errorf("error result missing %q:\n%s", want, text)
		}
	}

	// The loop asked the model again and finished with its retry turn.
	if got := p.callCount(); got != 2 {
		t.Fatalf("provider called %d times, want 2 (malformed turn, then retry)", got)
	}
	last := msgs[len(msgs)-1]
	if a, ok := last.(agentcore.AssistantMessage); !ok || agentcore.ContentToText(a.Content) != "recovered" {
		t.Errorf("last message = %T %+v, want the retry's assistant text", last, last)
	}
}
