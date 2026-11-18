package agenttool

// Tests for the question tool: the one tool that needs a human. It must return the
// operator's answers as a plain tool result — for one question or a batch — and,
// when nobody can answer (headless run, canceled run), an actionable error result
// instead of blocking.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
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

// answering installs an asker that records the request and replies with answers.
func answering(asked *agentcore.AskRequest, answers ...agentcore.AskAnswer) context.Context {
	return agentcore.WithAskUser(context.Background(), func(_ context.Context, req agentcore.AskRequest) (agentcore.AskResponse, error) {
		*asked = req
		return agentcore.AskResponse{Answers: answers}, nil
	})
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
	if schema.Type != "object" || schema.Properties["questions"] == nil {
		t.Errorf("schema = %s, want an object with a questions array", tool.Schema())
	}
	if len(schema.Required) != 1 || schema.Required[0] != "questions" {
		t.Errorf("required = %v, want [questions]", schema.Required)
	}
	// The item shape must advertise multi-select and options; the model needs both
	// to ask a "select all that apply" question.
	items := string(schema.Properties["questions"])
	for _, want := range []string{"multi_select", "options", "question"} {
		if !strings.Contains(items, want) {
			t.Errorf("questions item schema missing %q:\n%s", want, items)
		}
	}
	// A question in a parallel batch would interleave with other prompts.
	if got := tool.ExecutionMode(); got != agentcore.ToolExecutionSequential {
		t.Errorf("ExecutionMode() = %q, want sequential", got)
	}
}

// TestQuestionToolReturnsAnswer verifies the single-question happy path: the tool
// asks through the run's asker and hands the answer back as a normal tool result.
func TestQuestionToolReturnsAnswer(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := answering(&asked, agentcore.AskAnswer{Selected: []string{"postgres"}})

	res := runQuestion(t, ctx, `{"questions":[{"question":"Which database?","options":["postgres","sqlite"]}]}`)
	if res.Content == nil {
		t.Fatal("result has no content")
	}
	text := agentcore.ContentToText(res.Content)
	for _, want := range []string{"Which database?", "postgres"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
	if len(asked.Questions) != 1 || asked.Questions[0].Question != "Which database?" ||
		len(asked.Questions[0].Options) != 2 || asked.Questions[0].Options[0] != "postgres" {
		t.Errorf("asker received %#v, want the question and options", asked)
	}
	if details, ok := res.Details.(map[string]any); !ok || details["answer"] != "postgres" {
		t.Errorf("Details = %#v, want the question/answer pair", res.Details)
	}
}

// TestQuestionToolBatch verifies several questions are asked in one call and every
// answer comes back aligned with its question — the reason batching exists.
func TestQuestionToolBatch(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := answering(&asked,
		agentcore.AskAnswer{Selected: []string{"postgres"}},
		agentcore.AskAnswer{Selected: []string{"auth", "billing"}},
		agentcore.AskAnswer{Text: "keep it simple"},
	)
	res := runQuestion(t, ctx, `{"questions":[
		{"question":"Which database?","options":["postgres","sqlite"]},
		{"question":"Which features?","options":["auth","billing","reports"],"multi_select":true},
		{"question":"Anything else?"}
	]}`)

	if len(asked.Questions) != 3 {
		t.Fatalf("asker received %d questions, want 3", len(asked.Questions))
	}
	if !asked.Questions[1].Multi {
		t.Error("multi_select was not propagated to the asker")
	}
	if asked.Questions[2].Multi {
		t.Error("the last question is single-select")
	}
	text := agentcore.ContentToText(res.Content)
	for _, want := range []string{"Which database?", "postgres", "Which features?", "auth, billing", "Anything else?", "keep it simple"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
	// The pairs are numbered so the model can match each answer to its question.
	for _, want := range []string{"question 1:", "answer 1:", "question 3:", "answer 3:"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing labelled pair %q:\n%s", want, text)
		}
	}
	details, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("Details = %#v, want a map", res.Details)
	}
	answers, _ := details["answers"].([]string)
	if len(answers) != 3 || answers[1] != "auth, billing" {
		t.Errorf("Details answers = %#v, want one display string per question", details["answers"])
	}
}

// TestQuestionToolNoAnswerMarked verifies an unanswered question is reported as
// such, so the model cannot read silence as consent.
func TestQuestionToolNoAnswerMarked(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := answering(&asked, agentcore.AskAnswer{})
	res := runQuestion(t, ctx, `{"questions":[{"question":"Optional?"}]}`)
	if text := agentcore.ContentToText(res.Content); !strings.Contains(text, "(no answer)") {
		t.Errorf("result should mark the unanswered question:\n%s", text)
	}
}

// TestQuestionToolDedupesSelection verifies a front-end that toggles options can
// hand back repeats without leaking them into the tool result.
func TestQuestionToolDedupesSelection(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := answering(&asked, agentcore.AskAnswer{Selected: []string{"auth", "auth", " ", "billing"}})
	res := runQuestion(t, ctx, `{"questions":[{"question":"Which?","options":["auth","billing"],"multi_select":true}]}`)
	if text := agentcore.ContentToText(res.Content); !strings.Contains(text, "auth, billing") || strings.Contains(text, "auth, auth") {
		t.Errorf("selection was not deduped:\n%s", text)
	}
}

// TestQuestionToolAnswerCountMismatch verifies a front-end that answers the wrong
// number of questions is a loud internal error: silently shifting answers onto the
// wrong questions would be worse than failing.
func TestQuestionToolAnswerCountMismatch(t *testing.T) {
	var asked agentcore.AskRequest
	ctx := answering(&asked, agentcore.AskAnswer{Selected: []string{"a"}})
	_, err := (&QuestionTool{}).Execute(ctx, "call-1", json.RawMessage(`{"questions":[{"question":"one"},{"question":"two"}]}`), nil)
	if err == nil || !strings.Contains(err.Error(), "got 1 answers for 2 questions") {
		t.Fatalf("err = %v, want an answer-count mismatch error", err)
	}
}

// TestQuestionToolNoAsker verifies a run with no interactive input (headless,
// sub-agent) gets an explanatory error result telling the model to proceed with an
// assumption, rather than hanging or failing the run.
func TestQuestionToolNoAsker(t *testing.T) {
	_, err := (&QuestionTool{}).Execute(context.Background(), "call-1", json.RawMessage(`{"questions":[{"question":"Which database?"}]}`), nil)
	if err == nil {
		t.Fatal("a run with no asker should report an error, not a result")
	}
	if msg := err.Error(); !strings.Contains(msg, "no one can answer here") || !strings.Contains(msg, "assumption") {
		t.Errorf("error should explain the tool cannot ask: %v", err)
	}
}

// TestQuestionToolCanceledAsker verifies a canceled run (Esc / Ctrl+C while the
// panel is up) becomes an error result the model can react to.
func TestQuestionToolCanceledAsker(t *testing.T) {
	ctx := agentcore.WithAskUser(context.Background(), func(ctx context.Context, _ agentcore.AskRequest) (agentcore.AskResponse, error) {
		return agentcore.AskResponse{}, errors.New("input closed")
	})
	_, err := (&QuestionTool{}).Execute(ctx, "call-1", json.RawMessage(`{"questions":[{"question":"Which database?"}]}`), nil)
	if err == nil {
		t.Fatal("a canceled asker should report an error")
	}
	if !strings.Contains(err.Error(), "could not get an answer") || !strings.Contains(err.Error(), "input closed") {
		t.Errorf("error should report the failed answer: %v", err)
	}
}

// TestQuestionToolRejectsBadQuestions verifies empty, over-long and too-many
// questions are rejected before anyone is prompted.
func TestQuestionToolRejectsBadQuestions(t *testing.T) {
	asked := false
	ctx := agentcore.WithAskUser(context.Background(), func(context.Context, agentcore.AskRequest) (agentcore.AskResponse, error) {
		asked = true
		return agentcore.AskResponse{}, nil
	})
	cases := []struct {
		name string
		args string
		want string
	}{
		{"no questions", `{"questions":[]}`, "at least one question"},
		{"empty question", `{"questions":[{"question":"   "}]}`, "must not be empty"},
		{"empty second question", `{"questions":[{"question":"ok"},{"question":" "}]}`, "question 2 is empty"},
		{"too long", `{"questions":[{"question":"` + strings.Repeat("x", questionMaxQuestionRunes+1) + `"}]}`, "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asked = false
			_, err := (&QuestionTool{}).Execute(ctx, "call-1", json.RawMessage(tc.args), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if asked {
				t.Error("a rejected call must not reach the operator")
			}
		})
	}
}

// TestQuestionToolCapsOptions verifies suggested answers are trimmed, deduped and
// capped. The cap bounds a runaway call; paging lives in the front-ends.
func TestQuestionToolCapsOptions(t *testing.T) {
	var got []string
	ctx := agentcore.WithAskUser(context.Background(), func(_ context.Context, req agentcore.AskRequest) (agentcore.AskResponse, error) {
		got = req.Questions[0].Options
		return agentcore.AskResponse{Answers: []agentcore.AskAnswer{{Selected: []string{"a"}}}}, nil
	})
	var opts []string
	for _, o := range []string{" a ", "", "a", "  ", "b", "c", "d", "e", "f"} {
		opts = append(opts, `"`+o+`"`)
	}
	for i := 0; i < questionMaxOptions; i++ {
		opts = append(opts, `"x`+strings.Repeat("y", i)+`"`)
	}
	runQuestion(t, ctx, `{"questions":[{"question":"pick","options":[`+strings.Join(opts, ",")+`]}]}`)
	want := []string{"a", "b", "c", "d", "e", "f"}
	if len(got) != questionMaxOptions {
		t.Fatalf("options len = %d, want the cap %d", len(got), questionMaxOptions)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("options[%d] = %q, want %q (all %#v)", i, got[i], want[i], got)
		}
	}
}
