package agentcore

// Tests for the human-in-the-loop ask seam (ask.go): the context carrier, the
// batch shape, and the answer helpers the question tool and the front-ends share.

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
	ask := func(_ context.Context, req AskRequest) (AskResponse, error) {
		seen = req
		return AskResponse{}, sentinel
	}

	ctx := WithAskUser(context.Background(), ask)
	got, ok := AskUserFromContext(ctx)
	if !ok || got == nil {
		t.Fatal("AskUserFromContext returned no asker after WithAskUser")
	}
	want := AskRequest{Questions: []AskQuestion{
		{Question: "which database?", Options: []string{"postgres", "sqlite"}},
		{Question: "which features?", Options: []string{"auth", "billing"}, Multi: true},
	}}
	if _, err := got(ctx, want); !errors.Is(err, sentinel) {
		t.Fatalf("asker error = %v, want sentinel", err)
	}
	if len(seen.Questions) != 2 {
		t.Fatalf("asker received %#v, want two questions", seen)
	}
	if q := seen.Questions[0]; q.Question != "which database?" || len(q.Options) != 2 || q.Options[1] != "sqlite" || q.Multi {
		t.Fatalf("first question = %#v, want the single-select question", q)
	}
	if q := seen.Questions[1]; !q.Multi || q.Options[0] != "auth" {
		t.Fatalf("second question = %#v, want the multi-select question", q)
	}
}

// TestAskAnswerDisplay verifies an answer renders consistently for tool results and
// transcripts: options, free text, both, and neither.
func TestAskAnswerDisplay(t *testing.T) {
	cases := []struct {
		name string
		in   AskAnswer
		want string
	}{
		{"options", AskAnswer{Selected: []string{"auth", "billing"}}, "auth, billing"},
		{"text only", AskAnswer{Text: "use postgres"}, "use postgres"},
		{"both", AskAnswer{Selected: []string{"auth"}, Text: "plus SSO"}, "auth (plus SSO)"},
		{"neither", AskAnswer{}, ""},
		{"text is trimmed", AskAnswer{Text: "  spaced  "}, "spaced"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAskAnswerIsEmpty verifies an unanswered question is distinguishable from one
// answered with a blank string, so the tool can say "(no answer)" rather than imply
// consent.
func TestAskAnswerIsEmpty(t *testing.T) {
	if !(AskAnswer{}).IsEmpty() {
		t.Error("an empty answer should report IsEmpty")
	}
	if !(AskAnswer{Text: "   "}).IsEmpty() {
		t.Error("whitespace-only text is not an answer")
	}
	if (AskAnswer{Text: "x"}).IsEmpty() {
		t.Error("free text is an answer")
	}
	if (AskAnswer{Selected: []string{"a"}}).IsEmpty() {
		t.Error("a selected option is an answer")
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
