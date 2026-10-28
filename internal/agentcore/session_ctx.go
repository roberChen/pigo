package agentcore

import "context"

// This file carries the run's session identity into tool executions. Tools that
// create or continue a session of their own (the sub-agent tools) need to know
// which conversation spawned them, so the child session can be parented to it;
// the loop injects it next to the progress emitter and the asker, keeping the
// AgentTool interface free of run-level plumbing.

// sessionIDKey is the unexported context key under which the run's session id is
// stored.
type sessionIDKey struct{}

// WithSessionID returns a child context carrying id as the run's session id. An
// empty id leaves the context unchanged (a run without a backing session, e.g.
// the SDK without persistence).
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// SessionIDFromContext returns the run's session id, or "" when the run has no
// backing session.
func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}
