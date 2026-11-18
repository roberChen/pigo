package repl

// Tests for the REPL's side of the question tool: the model's questions are printed
// in full on the terminal, the operator answers them from the same shared input as
// the prompt, long option lists are paged, multi-select and free text both work, and
// the collected answers are echoed for confirmation before they are submitted.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/agenttool"
	"github.com/roberChen/pigo/internal/provider"
)

// askProvider replays one scripted turn per call: the first asks the operator a
// question through the question tool, the second is a plain final answer. It keeps
// every request so a test can assert what the model actually received.
type askProvider struct {
	calls    int
	requests []provider.CompletionRequest
	// question is the tool-call arguments for the first turn; when empty the
	// single-question default is used.
	question string
}

func (p *askProvider) Name() string { return "faux" }
func (p *askProvider) Models() []provider.Model {
	return []provider.Model{{Provider: "faux", ID: "faux"}}
}

func (p *askProvider) StreamCompletion(ctx context.Context, req provider.CompletionRequest) (*provider.AssistantMessageEventStream, error) {
	p.calls++
	p.requests = append(p.requests, req)

	var msg agentcore.AssistantMessage
	if p.calls == 1 {
		args := p.question
		if args == "" {
			args = `{"questions":[{"question":"Which branch?","options":["main","release"]}]}`
		}
		msg = agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content: agentcore.ContentList{
				agentcore.NewToolCallContent("call-1", "question", json.RawMessage(args)),
			},
			StopReason: agentcore.StopReasonToolUse,
		}
	} else {
		msg = agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("done")},
			StopReason: agentcore.StopReasonEndTurn,
		}
	}

	s := provider.NewAssistantMessageEventStream(0)
	go func() {
		_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
		if p.calls == 1 {
			_ = s.Emit(ctx, provider.StreamToolCallEvent{Partial: msg})
		} else {
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
		}
		_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		s.Close()
	}()
	return s, nil
}

// runAskREPL drives the REPL with a scripted input and returns its output.
func runAskREPL(t *testing.T, p *askProvider, script string) (string, error) {
	t.Helper()
	deps, _ := newTestDeps(t, p)
	deps.reg = agenttool.NewToolRegistry()
	if err := deps.reg.Register(&agenttool.QuestionTool{}); err != nil {
		t.Fatalf("register question tool: %v", err)
	}
	var out bytes.Buffer
	err := runREPL(strings.NewReader(script), &out, deps)
	return out.String(), err
}

// TestREPLAnswersQuestionTool drives a run whose first turn asks a question: the
// question is printed with its options, the numbered reply selects an option, the
// review is confirmed, and the model's next request carries the answer.
func TestREPLAnswersQuestionTool(t *testing.T) {
	p := &askProvider{}
	got, err := runAskREPL(t, p, "hello\n2\ny\n/exit\n")
	if err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	for _, want := range []string{"? Which branch?", "1) main", "2) release", "answer (number, n/p=page, or text):"} {
		if !strings.Contains(got, want) {
			t.Fatalf("question prompt missing %q:\n%s", want, got)
		}
	}
	// The review echoes what will be submitted before it is.
	for _, want := range []string{"review answers before submitting", "Which branch?", "→ release"} {
		if !strings.Contains(got, want) {
			t.Errorf("review block missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "The user answered: release") {
		t.Errorf("the inline tool result did not show the selected answer:\n%s", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("the run did not continue to the final turn:\n%s", got)
	}

	if p.calls != 2 {
		t.Fatalf("provider called %d times, want 2 (question turn, then final)", p.calls)
	}
	answer := toolResultText(p.requests[1].Context.Messages)
	if !strings.Contains(answer, "release") {
		t.Errorf("the model's second request did not carry the answer; tool results:\n%s", answer)
	}
}

// TestREPLAnswersQuestionWithFreeText verifies a non-numeric reply is passed
// through verbatim instead of being rejected as an unknown option.
func TestREPLAnswersQuestionWithFreeText(t *testing.T) {
	p := &askProvider{}
	if _, err := runAskREPL(t, p, "hello\nfeature/x\ny\n/exit\n"); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider called %d times, want 2", p.calls)
	}
	if answer := toolResultText(p.requests[1].Context.Messages); !strings.Contains(answer, "feature/x") {
		t.Errorf("free-text answer not delivered to the model:\n%s", answer)
	}
}

// TestREPLAnswersMultipleQuestions verifies one call with several questions asks
// each in turn and the review lists all of them together.
func TestREPLAnswersMultipleQuestions(t *testing.T) {
	p := &askProvider{question: `{"questions":[
		{"question":"Which database?","options":["postgres","sqlite"]},
		{"question":"Which features?","options":["auth","billing","reports"],"multi_select":true},
		{"question":"Anything else?"}
	]}`}
	// postgres, then 1 and 3 toggled for the multi question, Enter to finish it, free
	// text for the third, then confirm.
	got, err := runAskREPL(t, p, "hello\n1\n1,3\n\nuse a cache\ny\n/exit\n")
	if err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	for _, want := range []string{"? question 1/3", "? question 2/3", "? question 3/3", "(select all that apply)"} {
		if !strings.Contains(got, want) {
			t.Errorf("batched prompt missing %q:\n%s", want, got)
		}
	}
	for _, want := range []string{"→ postgres", "→ auth, reports", "→ use a cache"} {
		if !strings.Contains(got, want) {
			t.Errorf("review missing %q:\n%s", want, got)
		}
	}
	answer := toolResultText(p.requests[1].Context.Messages)
	for _, want := range []string{"question 1: Which database?", "answer 1: postgres", "answer 2: auth, reports", "answer 3: use a cache"} {
		if !strings.Contains(answer, want) {
			t.Errorf("tool result missing %q:\n%s", want, answer)
		}
	}
}

// TestREPLPagesLongOptionList verifies a menu longer than one page is paged instead
// of dumped: the first page shows its bound, "n" moves on, and a number from the
// second page is accepted.
func TestREPLPagesLongOptionList(t *testing.T) {
	opts := make([]string, 0, 15)
	for _, o := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o"} {
		opts = append(opts, `"`+o+`"`)
	}
	p := &askProvider{question: `{"questions":[{"question":"Pick one","options":[` + strings.Join(opts, ",") + `]}]}`}
	// Numbers are global, so page 2 is reached by typing the number it prints.
	got, err := runAskREPL(t, p, "hello\nn\n13\ny\n/exit\n")
	if err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	for _, want := range []string{"(options 1-10 of 15)", "(options 11-15 of 15)"} {
		if !strings.Contains(got, want) {
			t.Errorf("paging marker missing %q:\n%s", want, got)
		}
	}
	// Option 13 is "m"; a page-relative "3" would have silently picked "c".
	answer := toolResultText(p.requests[1].Context.Messages)
	if !strings.Contains(answer, "The user answered: m") {
		t.Errorf("the second page's option was not selected:\n%s", answer)
	}
}

// TestREPLPrintsLongQuestionInFull verifies a multi-paragraph question is not
// clipped: every line reaches the operator.
func TestREPLPrintsLongQuestionInFull(t *testing.T) {
	long := "first paragraph about the migration\\nsecond paragraph with details\\nthird line here"
	p := &askProvider{question: `{"questions":[{"question":"` + long + `"}]}`}
	got, err := runAskREPL(t, p, "hello\nok\ny\n/exit\n")
	if err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	for _, want := range []string{"first paragraph about the migration", "second paragraph with details", "third line here"} {
		if !strings.Contains(got, want) {
			t.Errorf("long question was clipped: missing %q:\n%s", want, got)
		}
	}
}

// TestREPLEditsAnAnswerAtReview verifies the review is a real checkpoint: a number
// reopens that question, and the corrected answer is what gets submitted.
func TestREPLEditsAnAnswerAtReview(t *testing.T) {
	p := &askProvider{}
	// Answer "main", then at the review choose 1 to edit and answer "release".
	got, err := runAskREPL(t, p, "hello\n1\n1\n2\ny\n/exit\n")
	if err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if !strings.Contains(got, "→ main") {
		t.Errorf("the first review should show the initial answer:\n%s", got)
	}
	answer := toolResultText(p.requests[1].Context.Messages)
	if !strings.Contains(answer, "release") || strings.Contains(answer, "answer: main") {
		t.Errorf("the edited answer was not submitted:\n%s", answer)
	}
}

// TestREPLDeclinesAtReview verifies "n" aborts the batch with a decline error the
// model can act on, rather than submitting answers the operator rejected.
func TestREPLDeclinesAtReview(t *testing.T) {
	p := &askProvider{}
	got, err := runAskREPL(t, p, "hello\n2\nn\n/exit\n")
	if err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if !strings.Contains(got, "declined") {
		t.Errorf("the tool result should report the decline:\n%s", got)
	}
	answer := toolResultText(p.requests[1].Context.Messages)
	if strings.Contains(answer, "The user answered") {
		t.Errorf("a declined batch must not report an answer:\n%s", answer)
	}
}

// toolResultText joins the text of every tool result message in msgs.
func toolResultText(msgs agentcore.MessageList) string {
	var b strings.Builder
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			b.WriteString(agentcore.ContentToText(tr.Content))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestAskUserREPLClosedInput verifies the asker reports EOF so the question tool
// can give the model an actionable result instead of hanging on a dead terminal.
func TestAskUserREPLClosedInput(t *testing.T) {
	deps, _ := newTestDeps(t, &askProvider{})
	deps.in = bufio.NewReader(strings.NewReader(""))
	var out bytes.Buffer
	req := agentcore.AskRequest{Questions: []agentcore.AskQuestion{{Question: "anyone?"}}}
	if _, err := askUserREPL(&out, deps, req); err == nil {
		t.Fatal("asker should report a closed input as an error")
	}
}

// TestParseOptionSelection covers the answer syntaxes a user can type for one
// question, including the ones that must NOT be treated as a selection.
func TestParseOptionSelection(t *testing.T) {
	cases := []struct {
		line  string
		total int
		want  []int
		ok    bool
	}{
		{"1", 5, []int{1}, true},
		{"1,3", 5, []int{1, 3}, true},
		{"1 3", 5, []int{1, 3}, true},
		{"2-4", 5, []int{2, 3, 4}, true},
		{"6", 5, nil, false},            // out of range
		{"use postgres", 5, nil, false}, // free text
		{"3x", 5, nil, false},
		{"", 5, nil, false},
	}
	for _, tc := range cases {
		got, ok := parseOptionSelection(tc.line, tc.total)
		if ok != tc.ok {
			t.Errorf("parseOptionSelection(%q) ok = %v, want %v", tc.line, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseOptionSelection(%q) = %v, want %v", tc.line, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("parseOptionSelection(%q) = %v, want %v", tc.line, got, tc.want)
				break
			}
		}
	}
}

// TestOptionPageBounds verifies paging never shows an empty or out-of-range page,
// including for the last partial page.
func TestOptionPageBounds(t *testing.T) {
	cases := []struct{ page, total, start, end int }{
		{0, 5, 0, 5},
		{0, 15, 0, 10},
		{1, 15, 10, 15},
		{5, 15, 10, 15}, // clamped to the last page
		{0, 0, 0, 0},
	}
	for _, tc := range cases {
		start, end := optionPageBounds(tc.page, tc.total)
		if start != tc.start || end != tc.end {
			t.Errorf("optionPageBounds(%d,%d) = (%d,%d), want (%d,%d)", tc.page, tc.total, start, end, tc.start, tc.end)
		}
	}
}
