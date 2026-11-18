package agentcore

import (
	"context"
	"strings"
)

// This file implements the human-in-the-loop seam: how a tool asks the operator
// questions during a run. The seam travels on the run context, exactly like the
// progress emitter (progress_ctx.go), so the tool layer stays independent of any
// front-end. Each driver installs its own asker: the line REPL reads from its
// shared input, the TUI round-trips the questions through its message loop, and a
// headless run (or a sub-agent) installs none, which the question tool reports
// back to the model instead of blocking forever.
//
// A request is a *batch*: the model may ask several things at once (related
// decisions are usually cheaper to answer together than in successive rounds) and
// gets every answer back in one tool result. The front-end owns presentation —
// paging through questions and options, and reviewing the whole batch before it is
// submitted — so the seam carries no layout or key-binding knowledge.

// AskQuestion is one question in a batch.
type AskQuestion struct {
	// Question is the text to show. The model writes it; it may be arbitrarily
	// long (a context block plus the actual question), and front-ends must make
	// all of it reachable rather than clipping it.
	Question string
	// Options are suggested answers, in display order. A front-end offers them as
	// a menu, and the operator may always answer in free text instead.
	Options []string
	// Multi allows choosing several options at once. When false the front-end
	// picks at most one, so a yes/no style question needs a single keystroke.
	Multi bool
}

// AskRequest is one batch of questions put to the operator by the question tool.
// Questions are answered together: the operator sees every answer before the batch
// is submitted.
type AskRequest struct {
	Questions []AskQuestion
}

// AskAnswer is the operator's answer to one question.
type AskAnswer struct {
	// Selected holds the chosen options in the order they were offered. It may be
	// empty, and may hold several entries when the question allowed multi-select.
	Selected []string
	// Text is a free-form answer typed outside the options. It may be set on its
	// own (the operator rejected every option) or in addition to Selected.
	Text string
}

// IsEmpty reports whether the operator left the question unanswered.
func (a AskAnswer) IsEmpty() bool {
	return len(a.Selected) == 0 && strings.TrimSpace(a.Text) == ""
}

// Display renders the answer on one line: the selected options joined by ", ",
// plus the free-form text in parentheses when the operator typed something in
// addition to (or instead of) the options. Tool results and transcripts use this
// so an answer reads the same everywhere.
func (a AskAnswer) Display() string {
	selected := strings.Join(a.Selected, ", ")
	text := strings.TrimSpace(a.Text)
	switch {
	case text == "":
		return selected
	case selected == "":
		return text
	default:
		return selected + " (" + text + ")"
	}
}

// AskResponse is the batch's answers, aligned with AskRequest.Questions: the
// answer at index i belongs to the question at index i.
type AskResponse struct {
	Answers []AskAnswer
}

// AskUserFunc asks the operator req and returns their answers (one per question,
// in order). A non-nil error means the batch could not be answered — ctx was
// canceled, input reached EOF, the operator declined the review, or no interactive
// terminal is attached — and the question tool turns it into an error tool result.
type AskUserFunc func(ctx context.Context, req AskRequest) (AskResponse, error)

// askUserKey is the unexported context key under which the run's AskUserFunc is
// stored.
type askUserKey struct{}

// WithAskUser returns a child context carrying fn as the run's asker. A nil fn
// leaves ctx unchanged, so a driver can install its asker unconditionally.
func WithAskUser(ctx context.Context, fn AskUserFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, askUserKey{}, fn)
}

// AskUserFromContext returns the run's asker and whether one is installed. The
// question tool must treat false as "the user cannot be asked here" rather than
// blocking.
func AskUserFromContext(ctx context.Context) (AskUserFunc, bool) {
	fn, ok := ctx.Value(askUserKey{}).(AskUserFunc)
	return fn, ok && fn != nil
}
