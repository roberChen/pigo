package agentcore

import "context"

// This file defines the human-in-the-loop seam: how a tool asks the operator a
// question during a run. The seam travels on the run context, exactly like the
// progress emitter (progress_ctx.go), so the tool layer stays independent of any
// front-end. Each driver installs its own asker: the line REPL reads a line from
// its shared input, the TUI round-trips the question through its message loop,
// and a headless run (or a sub-agent) installs none, which the question tool
// reports back to the model instead of blocking forever.

// AskRequest is one question put to the operator by the question tool.
type AskRequest struct {
	// Question is the text to show. The model writes it; it should be one short,
	// specific question.
	Question string
	// Options are optional suggested answers, in display order. A front-end may
	// offer them as a menu, and the user may still answer something else.
	Options []string
}

// AskUserFunc asks the operator req and returns their answer (never empty on a
// nil error; the front-end substitutes a placeholder). A non-nil error means the
// question could not be answered — ctx was canceled, input reached EOF, or no
// interactive terminal is attached — and the question tool turns it into an
// error tool result.
type AskUserFunc func(ctx context.Context, req AskRequest) (string, error)

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
