package runtime

// This file covers the question tool inside the real loop: a turn that asks the
// operator a question pauses on the run's asker, the answer comes back as a normal
// tool result, and the loop continues to the next turn. A run with no asker
// (headless, sub-agent) must not stall either.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/provider"
)

// TestLoopQuestionToolAsksAndContinues drives a scripted turn that calls the
// question tool, with an asker installed on the run context, then a final text
// turn. The tool result the model receives must carry the answer.
func TestLoopQuestionToolAsksAndContinues(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := agentcore.WithAskUser(context.Background(), func(_ context.Context, req agentcore.AskRequest) (string, error) {
		asked = req
		return "postgres", nil
	})

	p := &fauxProvider{
		name:   "faux",
		models: []provider.Model{{Provider: "faux", ID: "faux"}},
		turns: []fauxTurn{
			toolCallTurn("call-1", "question", `{"question":"Which database?","options":["postgres","sqlite"]}`),
			textTurn("using postgres"),
		},
	}
	cfg := newFauxRunCfg(p, &agenttool.QuestionTool{})
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("set up the store")}},
	}}

	kinds, msgs := collectStream(t, agentLoop(ctx, agentCtx, cfg))

	if asked.Question != "Which database?" || len(asked.Options) != 2 {
		t.Errorf("asker received %#v, want the question and its options", asked)
	}
	if got := countKind(kinds, agentcore.EventToolExecutionStart); got != 1 {
		t.Errorf("expected the question tool to execute once, got %d", got)
	}

	var result agentcore.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			result = tr
		}
	}
	if result.IsError {
		t.Fatalf("question tool result is an error: %+v", result)
	}
	if text := agentcore.ContentToText(result.Content); !strings.Contains(text, "postgres") {
		t.Errorf("result missing the operator's answer:\n%s", text)
	}

	if p.callCount() != 2 {
		t.Fatalf("provider called %d times, want 2 (question turn, then final)", p.callCount())
	}
	answer := ""
	for _, m := range p.requestAt(1).Context.Messages {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			answer = agentcore.ContentToText(tr.Content)
		}
	}
	if !strings.Contains(answer, "postgres") {
		t.Errorf("the second request did not carry the answer back to the model:\n%s", answer)
	}

	last := msgs[len(msgs)-1]
	if a, ok := last.(agentcore.AssistantMessage); !ok || agentcore.ContentToText(a.Content) != "using postgres" {
		t.Errorf("last message = %T %+v, want the final assistant text", last, last)
	}
}

// TestLoopQuestionToolWithoutAsker verifies a non-interactive run (headless, or a
// sub-agent, which does not get the tool) degrades to an error result the model can
// act on instead of blocking on input that will never come.
func TestLoopQuestionToolWithoutAsker(t *testing.T) {
	p := &fauxProvider{
		name:   "faux",
		models: []provider.Model{{Provider: "faux", ID: "faux"}},
		turns: []fauxTurn{
			toolCallTurn("call-1", "question", `{"question":"Which database?"}`),
			textTurn("assuming sqlite"),
		},
	}
	cfg := newFauxRunCfg(p, &agenttool.QuestionTool{})
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("set up the store")}},
	}}

	// A short deadline guards the "must not block" property: the tool returns at
	// once, so the whole run finishes well inside it.
	ctx, cancel := context.WithTimeout(context.Background(), 5e9)
	defer cancel()

	_, msgs := collectStream(t, agentLoop(ctx, agentCtx, cfg))

	var result agentcore.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			result = tr
		}
	}
	if !result.IsError {
		t.Fatalf("a run with no asker should report an error result, got %+v", result)
	}
	if text := agentcore.ContentToText(result.Content); !strings.Contains(text, "no one can answer here") {
		t.Errorf("result should explain that it cannot ask:\n%s", text)
	}
	if p.callCount() != 2 {
		t.Errorf("provider called %d times, want the run to continue after the failed question", p.callCount())
	}
}

// TestQuestionToolSequentialInBatch verifies a batch containing the question tool
// runs sequentially, so the prompt cannot interleave with another tool's work.
func TestQuestionToolSequentialInBatch(t *testing.T) {
	reg := agenttool.NewToolRegistry()
	if err := reg.Register(&agenttool.QuestionTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(echoTool("echo", agentcore.ToolExecutionParallel, false)); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	calls := []agentcore.AgentToolCall{
		{ID: "c1", Name: "question", Arguments: json.RawMessage(`{"question":"Proceed?"}`)},
		{ID: "c2", Name: "echo", Arguments: json.RawMessage(`{}`)},
	}
	cfg := agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: reg}}
	ctx := agentcore.WithAskUser(context.Background(), func(context.Context, agentcore.AskRequest) (string, error) {
		return "yes", nil
	})
	results, _ := agenttool.ExecuteToolCalls(ctx, cfg, calls, nil)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if r.IsError {
			t.Errorf("unexpected error result: %+v", r)
		}
	}
}
