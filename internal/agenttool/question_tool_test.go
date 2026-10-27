package agenttool

// Tests for the question tool: the one tool that needs a human. It must return
// the operator's answer as a plain tool result, and — when nobody can answer
// (headless run, canceled run) — an actionable error result instead of blocking.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// runQuestion executes the question tool with args and the given context.
func runQuestion(t *testing.T, ctx context.Context, args string) agentcore.AgentToolResult {
	t.Helper()
	res, err := (&QuestionTool{}).Execute(ctx, "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v (failures must be tool results)", err)
	}
	return res
}

func TestQuestionToolMetadata(t *testing.T) {
	tool := &QuestionTool{}
	if got := tool.Name(); got != "question" {
		t.Errorf("Name() = %q, want question", got)
	}
	if !strings.Contains(tool.Description(), "Ask the human operator") {
		t.Errorf("Description() should explain what it does, got %q", tool.Description())
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("Schema is not valid JSON: %v", err)
	}
	if schema.Type != "object" || schema.Properties["question"] == nil || schema.Properties["options"] == nil {
		t.Errorf("schema = %s, want an object with question and options", tool.Schema())
	}
	if len(schema.Required) != 1 || schema.Required[0] != "question" {
		t.Errorf("required = %v, want [question]", schema.Required)
	}
	// A question in a parallel batch would interleave with other prompts.
	if got := tool.ExecutionMode(); got != agentcore.ToolExecutionSequential {
		t.Errorf("ExecutionMode() = %q, want sequential", got)
	}
}

// TestQuestionToolReturnsAnswer verifies the happy path: the tool asks through the
// run's asker and hands the answer back to the model as a normal tool result.
func TestQuestionToolReturnsAnswer(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := agentcore.WithAskUser(context.Background(), func(_ context.Context, req agentcore.AskRequest) (string, error) {
		asked = req
		return "postgres", nil
	})

	res := runQuestion(t, ctx, `{"question":"Which database?","options":["postgres","sqlite"]}`)
	if res.Content == nil {
		t.Fatal("result has no content")
	}
	text := agentcore.ContentToText(res.Content)
	for _, want := range []string{"Which database?", "postgres"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
	if asked.Question != "Which database?" || len(asked.Options) != 2 || asked.Options[0] != "postgres" {
		t.Errorf("asker received %#v, want the question and options", asked)
	}
	if details, ok := res.Details.(map[string]any); !ok || details["answer"] != "postgres" {
		t.Errorf("Details = %#v, want the question/answer pair", res.Details)
	}
}

// TestQuestionToolNoAsker verifies a run with no interactive input (headless,
// sub-agent) gets an explanatory error result telling the model to proceed with an
// assumption, rather than hanging or failing the run.
func TestQuestionToolNoAsker(t *testing.T) {
	_, err := (&QuestionTool{}).Execute(context.Background(), "call-1", json.RawMessage(`{"question":"Which database?"}`), nil)
	if err == nil {
		t.Fatal("a run with no asker should report an error, not a result")
	}
	if msg := err.Error(); !strings.Contains(msg, "no one can answer here") || !strings.Contains(msg, "assumption") {
		t.Errorf("error should explain the tool cannot ask: %v", err)
	}
}

// TestQuestionToolCanceledAsker verifies a canceled run (Esc / Ctrl+C while the
// prompt is up) becomes an error result the model can react to.
func TestQuestionToolCanceledAsker(t *testing.T) {
	ctx := agentcore.WithAskUser(context.Background(), func(ctx context.Context, _ agentcore.AskRequest) (string, error) {
		return "", errors.New("input closed")
	})
	_, err := (&QuestionTool{}).Execute(ctx, "call-1", json.RawMessage(`{"question":"Which database?"}`), nil)
	if err == nil {
		t.Fatal("a canceled asker should report an error")
	}
	if !strings.Contains(err.Error(), "could not get an answer") || !strings.Contains(err.Error(), "input closed") {
		t.Errorf("error should report the failed answer: %v", err)
	}
}

// TestQuestionToolEmptyQuestion verifies an empty question is rejected without
// bothering the operator.
func TestQuestionToolEmptyQuestion(t *testing.T) {
	asked := false
	ctx := agentcore.WithAskUser(context.Background(), func(context.Context, agentcore.AskRequest) (string, error) {
		asked = true
		return "", nil
	})
	_, err := (&QuestionTool{}).Execute(ctx, "call-1", json.RawMessage(`{"question":"   "}`), nil)
	if asked {
		t.Error("an empty question must not reach the operator")
	}
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("err = %v, want an empty-question error", err)
	}
}

// TestQuestionToolCleansOptions verifies suggested answers are trimmed, deduped,
// emptied and capped before they reach the front-end's menu.
func TestQuestionToolCleansOptions(t *testing.T) {
	var got []string
	ctx := agentcore.WithAskUser(context.Background(), func(_ context.Context, req agentcore.AskRequest) (string, error) {
		got = req.Options
		return "a", nil
	})
	runQuestion(t, ctx, `{"question":"pick","options":[" a ","","a","  ","b","c","d","e","f","g"]}`)
	want := []string{"a", "b", "c", "d", "e", "f"}
	if len(got) != len(want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("options[%d] = %q, want %q (all %#v)", i, got[i], want[i], got)
		}
	}
}
