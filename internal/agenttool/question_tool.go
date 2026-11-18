// This file implements the question AgentTool: the model asks the human operator
// one or more questions mid-run and gets their answers back as the tool result. It
// is the one tool that needs a human, so it blocks on the run's asker seam
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

	"github.com/roberChen/pigo/internal/agentcore"
)

// questionToolQuestion is one question in the decoded argument shape.
type questionToolQuestion struct {
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	MultiSelect bool     `json:"multi_select"`
}

// questionToolArgs is the decoded argument shape for the question tool.
type questionToolArgs struct {
	Questions []questionToolQuestion `json:"questions"`
}

// QuestionTool asks the operator one or more questions via the run's asker seam
// and returns the answers as a tool result the model can act on.
type QuestionTool struct{}

// Caps on a single call. Batching exists so related decisions are answered in one
// round, not so a call can turn into an interrogation; options are paged rather
// than clipped, so the cap is generous well past what a terminal shows at once.
const (
	questionMaxQuestions     = 10
	questionMaxOptions       = 20
	questionMaxQuestionRunes = 8000
)

// Name implements AgentTool.
func (t *QuestionTool) Name() string { return "question" }

// Description implements AgentTool.
func (t *QuestionTool) Description() string {
	return "Ask the human operator one or more questions and wait for the answers. Use it " +
		"when a decision genuinely needs a person: an ambiguous requirement, a destructive or " +
		"irreversible choice, or missing information only they have. Ask related decisions in " +
		"one call (up to 10 questions) rather than several rounds; each question may carry " +
		"concise options, and set multi_select when several answers may apply. The operator " +
		"can always answer in free text instead of picking an option, and sees all answers " +
		"before submitting them. Do not use it to confirm tool permissions (the tool layer " +
		"handles that) or for questions you can answer yourself from the code or context."
}

// Schema implements AgentTool.
func (t *QuestionTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "minItems": 1,
      "maxItems": 10,
      "description": "The questions to ask, in the order they should be answered. Related decisions in one call beat several rounds. Each question's text may be long: the operator sees all of it.",
      "items": {
        "type": "object",
        "properties": {
          "question": {"type": "string", "description": "The question text. May span several lines or include context the operator needs."},
          "options": {
            "type": "array",
            "items": {"type": "string"},
            "maxItems": 20,
            "description": "Suggested answers, offered as a menu the operator can page through. The operator can still answer in free text."
          },
          "multi_select": {
            "type": "boolean",
            "description": "Allow choosing several options. Use it for 'select all that apply'; leave it false for pick-one questions."
          }
        },
        "required": ["question"],
        "additionalProperties": false
      }
    }
  },
  "required": ["questions"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Asking the user must not interleave with
// another tool's prompt, so the whole batch runs sequentially.
func (t *QuestionTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

// Execute asks the operator and returns the answers. Malformed arguments, an empty
// question and an over-long batch are rejected before anyone is prompted; a run
// with no asker (headless, sub-agent) and a canceled ask are returned as Go errors,
// so the executor flags them as error tool results the model can retry from and the
// UI shows them as failed rather than answered.
func (t *QuestionTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[questionToolArgs](args, "question")
	if bad != nil {
		return *bad, nil
	}
	questions, err := a.normalizedQuestions()
	if err != nil {
		return agentcore.AgentToolResult{}, err
	}

	ask, ok := agentcore.AskUserFromContext(ctx)
	if !ok {
		return agentcore.AgentToolResult{}, errors.New(
			"question: no one can answer here (this run has no interactive input); " +
				"proceed with the most reasonable assumption and state it, or ask in your reply text")
	}
	req := agentcore.AskRequest{Questions: questions}
	resp, err := ask(ctx, req)
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf(
			"question: could not get an answer (%w); proceed with the most reasonable assumption and state it", err)
	}
	// The front-end must answer every question: a short response would silently
	// shift the answers onto the wrong questions, which is worse than failing.
	if len(resp.Answers) != len(questions) {
		return agentcore.AgentToolResult{}, fmt.Errorf(
			"question: internal error: got %d answers for %d questions", len(resp.Answers), len(questions))
	}
	for i := range resp.Answers {
		resp.Answers[i].Selected = dedupeStrings(resp.Answers[i].Selected)
		resp.Answers[i].Text = strings.TrimSpace(resp.Answers[i].Text)
	}

	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(formatQuestionAnswers(questions, resp.Answers))},
		Details: questionResultDetails(questions, resp.Answers),
	}, nil
}

// normalizedQuestions validates the decoded arguments and returns the batch to
// ask. A call must contain at least one question; a single question with no text
// is a model error the model can fix by calling again.
func (a questionToolArgs) normalizedQuestions() ([]agentcore.AskQuestion, error) {
	raw := a.Questions
	if len(raw) == 0 {
		return nil, errors.New("question: at least one question is required (pass them in \"questions\")")
	}
	if len(raw) > questionMaxQuestions {
		return nil, fmt.Errorf("question: too many questions (%d); ask at most %d per call", len(raw), questionMaxQuestions)
	}
	out := make([]agentcore.AskQuestion, 0, len(raw))
	for i, q := range raw {
		text := strings.TrimSpace(q.Question)
		if text == "" {
			if len(raw) == 1 {
				return nil, errors.New("question: the question must not be empty")
			}
			return nil, fmt.Errorf("question: question %d is empty", i+1)
		}
		if n := len([]rune(text)); n > questionMaxQuestionRunes {
			return nil, fmt.Errorf("question: question %d is too long (%d characters, max %d)", i+1, n, questionMaxQuestionRunes)
		}
		out = append(out, agentcore.AskQuestion{
			Question: text,
			Options:  cleanQuestionOptions(q.Options),
			Multi:    q.MultiSelect,
		})
	}
	return out, nil
}

// formatQuestionAnswers renders the batch for the model. A single question keeps
// the original two-line shape ("The user answered: ..."); a batch numbers the
// pairs so the model can match each answer to the question it belongs to.
//
// The first line always carries the answer itself: front-ends that collapse a
// tool result to one line (the REPL's inline result, the collapsed card) still show
// what the user decided.
func formatQuestionAnswers(questions []agentcore.AskQuestion, answers []agentcore.AskAnswer) string {
	var b strings.Builder
	if len(questions) == 1 {
		b.WriteString("The user answered: ")
		b.WriteString(answerDisplay(answers[0]))
		b.WriteString("\nquestion: ")
		b.WriteString(questions[0].Question)
		return b.String()
	}
	fmt.Fprintf(&b, "The user answered your %d questions:", len(questions))
	for i, q := range questions {
		fmt.Fprintf(&b, "\n\nquestion %d: %s", i+1, q.Question)
		if q.Multi {
			b.WriteString("\n(select all that apply)")
		}
		fmt.Fprintf(&b, "\nanswer %d: %s", i+1, answerDisplay(answers[i]))
	}
	return b.String()
}

// answerDisplay renders one answer, marking an unanswered question explicitly so
// the model does not mistake silence for consent.
func answerDisplay(a agentcore.AskAnswer) string {
	if a.IsEmpty() {
		return "(no answer)"
	}
	return a.Display()
}

// questionResultDetails exposes the batch in the tool result details so a
// front-end can render it without re-parsing the text.
func questionResultDetails(questions []agentcore.AskQuestion, answers []agentcore.AskAnswer) map[string]any {
	texts := make([]string, len(questions))
	displays := make([]string, len(answers))
	for i, q := range questions {
		texts[i] = q.Question
	}
	for i, a := range answers {
		displays[i] = answerDisplay(a)
	}
	details := map[string]any{"questions": texts, "answers": displays}
	if len(questions) == 1 {
		details["question"] = texts[0]
		details["answer"] = displays[0]
	}
	return details
}

// cleanQuestionOptions trims the suggested answers, drops empty entries and
// duplicates, and caps the list. Paging belongs to the front-end, so the cap only
// bounds a runaway call rather than the user's view.
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

// dedupeStrings drops repeats and blank entries from a front-end's selection,
// preserving order. A front-end that toggles options can hand back duplicates.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
