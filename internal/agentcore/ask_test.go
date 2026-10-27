package agentcore

// Tests for the human-in-the-loop ask seam (ask.go): the context carrier and the
// nil-asker semantics the question tool relies on.

import (
	"context"
	"errors"
	"testing"
)

// TestAskUserRoundTrip verifies the asker installed on a context is returned with
// the exact request a tool passes in, and that its error surfaces unchanged.
func TestAskUserRoundTrip(t *testing.T) {
	sentinel := errors.New("no input")
	var seen AskRequest
	ask := func(_ context.Context, req AskRequest) (string, error) {
		seen = req
		return "", sentinel
	}

	ctx := WithAskUser(context.Background(), ask)
	got, ok := AskUserFromContext(ctx)
	if !ok || got == nil {
		t.Fatal("AskUserFromContext returned no asker after WithAskUser")
	}
	want := AskRequest{Question: "which database?", Options: []string{"postgres", "sqlite"}}
	if _, err := got(ctx, want); !errors.Is(err, sentinel) {
		t.Fatalf("asker error = %v, want sentinel", err)
	}
	if seen.Question != want.Question || len(seen.Options) != 2 || seen.Options[1] != "sqlite" {
		t.Fatalf("asker received %#v, want %#v", seen, want)
	}
}

// TestAskUserFromBareContext verifies a run without an asker (headless, sub-agent)
// reports false, which is how the question tool decides to tell the model it
// cannot ask instead of blocking.
func TestAskUserFromBareContext(t *testing.T) {
	if fn, ok := AskUserFromContext(context.Background()); ok || fn != nil {
		t.Fatalf("AskUserFromContext on bare ctx = (%v, %v), want (nil, false)", fn, ok)
	}
}

// TestWithAskUserNilKeepsContext verifies a driver can install its asker
// unconditionally: a nil asker leaves the context (and any outer seam) alone.
func TestWithAskUserNilKeepsContext(t *testing.T) {
	type otherKey struct{}
	base := context.WithValue(context.Background(), otherKey{}, "kept")
	ctx := WithAskUser(base, nil)
	if got := ctx.Value(otherKey{}); got != "kept" {
		t.Fatalf("outer value lost: %v", got)
	}
	if _, ok := AskUserFromContext(ctx); ok {
		t.Fatal("a nil asker must not register as installed")
	}
}
