// This file implements the question AgentTool: the model asks the human operator
// a question mid-run and gets their answer back as the tool result. It is the one
// tool that needs a human, so it blocks on the run's asker seam
// (agentcore.AskUserFromContext) — installed by the line REPL and the TUI, absent
// in headless runs and sub-agents, where the tool reports that it cannot ask
// instead of hanging. Questions run sequentially: a question inside a parallel
// batch would interleave with other prompts.
package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// questionToolArgs is the decoded argument shape for the question tool.
type questionToolArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

// QuestionTool asks the operator a question via the run's asker seam and returns
// the answer as a tool result the model can act on.
type QuestionTool struct{}

// questionMaxOptions caps the suggested answers a call may offer, keeping the
// prompt a short pick-one list rather than a menu dump.
const questionMaxOptions = 6

// Name implements AgentTool.
func (t *QuestionTool) Name() string { return "question" }

// Description implements AgentTool.
func (t *QuestionTool) Description() string {
	return "Ask the human operator a question and wait for their answer. Use it when a " +
		"decision genuinely needs a person: an ambiguous requirement, a destructive or " +
		"irreversible choice, or missing information only they have. Pass concise options " +
		"when the answer is one of a few choices; the user may still answer something else. " +
		"Do not use it to confirm tool permissions (the tool layer handles that) or for " +
		"questions you can answer yourself from the code or context."
}

// Schema implements AgentTool.
func (t *QuestionTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "The question to ask, one short sentence."},
    "options":  {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional suggested answers (max 6, each a few words), offered as a pick-one list. The user can still answer something else."
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Asking the user must not interleave with
// another tool's prompt, so the whole batch runs sequentially.
func (t *QuestionTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

// Execute asks the operator and returns their answer. Malformed arguments and an
// empty question are rejected before anyone is prompted; a run with no asker
// (headless, sub-agent) and a canceled ask are returned as Go errors, so the
// executor flags them as error tool results the model can retry from and the UI
// shows them as failed rather than answered.
func (t *QuestionTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[questionToolArgs](args, "question")
	if bad != nil {
		return *bad, nil
	}
	question := strings.TrimSpace(a.Question)
	if question == "" {
		return agentcore.AgentToolResult{}, errors.New("question: the question must not be empty")
	}
	options := cleanQuestionOptions(a.Options)

	ask, ok := agentcore.AskUserFromContext(ctx)
	if !ok {
		return agentcore.AgentToolResult{}, errors.New(
			"question: no one can answer here (this run has no interactive input); " +
				"proceed with the most reasonable assumption and state it, or ask in your reply text")
	}
	answer, err := ask(ctx, agentcore.AskRequest{Question: question, Options: options})
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf(
			"question: could not get an answer (%w); proceed with the most reasonable assumption and state it", err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = "(the user gave no answer)"
	}

	// Lead with the answer on the first line: front-ends that collapse a tool
	// result to a single line (the REPL's inline result, the collapsed card) still
	// show what the user decided.
	var b strings.Builder
	b.WriteString("The user answered: ")
	b.WriteString(answer)
	b.WriteString("\nquestion: ")
	b.WriteString(question)
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(b.String())},
		Details: map[string]any{"question": question, "answer": answer},
	}, nil
}

// cleanQuestionOptions trims the suggested answers, drops empty entries and
// duplicates, and caps the list so the prompt stays scannable.
func cleanQuestionOptions(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, opt := range in {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		if _, dup := seen[opt]; dup {
			continue
		}
		seen[opt] = struct{}{}
		out = append(out, opt)
		if len(out) == questionMaxOptions {
			break
		}
	}
	return out
}
