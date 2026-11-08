// This file implements the cancellation contract of the loop: a run the caller
// cancels must end with a terminal assistant message whose stop reason is
// aborted, and it must keep whatever text the consumer already saw. The provider
// transport cannot make that call itself (it reports every abort as a generic
// stream error), so the loop is where the contract is enforced — and where it
// needs a test.
package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// cancelMidStream starts a run against a slow faux provider and cancels it after
// the first streamed text has been observed. It returns the finished messages.
func cancelMidStream(t *testing.T, p *fauxProvider) (agentcore.MessageList, *agentcore.AssistantMessage) {
	t.Helper()
	cfg := newFauxRunCfg(p)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("go")}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := agentLoop(ctx, agentCtx, cfg)

	sawText := false
	for ev := range s.Events() {
		if up, ok := ev.(agentcore.MessageUpdateEvent); ok {
			if text, ok := up.AssistantMessageEvent.(provider.StreamTextEvent); ok {
				if agentcore.ContentToText(text.Partial.Content) != "" {
					sawText = true
					cancel()
				}
			}
		}
	}
	if !sawText {
		t.Fatal("cancelled before any text streamed; the test cannot exercise the partial-turn path")
	}
	msgs, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("run result error: %v", err)
	}
	return msgs, agentcore.LastAssistantOf(msgs)
}

// TestCancelledTurnIsAbortedWithPartialText is the core contract: a user cancel
// is not a provider failure, and the partial answer survives in the transcript.
func TestCancelledTurnIsAbortedWithPartialText(t *testing.T) {
	p := &fauxProvider{turns: []fauxTurn{textTurn("partial answer")}, delay: 30 * time.Millisecond}
	msgs, final := cancelMidStream(t, p)
	if final == nil {
		t.Fatalf("cancelled run produced no assistant message: %+v", msgs)
	}
	if final.StopReason != agentcore.StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted", final.StopReason)
	}
	if got := agentcore.ContentToText(final.Content); got != "partial answer" {
		t.Fatalf("partial text = %q", got)
	}
	// Exactly one assistant message: a second, empty aborted turn would make a
	// resumed transcript end on a blank line the user never saw.
	assistants := 0
	for _, m := range msgs {
		if _, ok := m.(agentcore.AssistantMessage); ok {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant messages = %d, want 1: %+v", assistants, msgs)
	}
}

// TestCancelledBeforeFirstDeltaStillRecordsATurn covers the other ordering: the
// cancel lands before the provider produced anything, so the loop has to append
// the aborted turn itself. Without it the transcript would end on a user message
// and a resume would treat the question as never answered.
func TestCancelledBeforeFirstDeltaStillRecordsATurn(t *testing.T) {
	p := &fauxProvider{turns: []fauxTurn{textTurn("never seen")}, delay: 200 * time.Millisecond}

	cfg := newFauxRunCfg(p)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("go")}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	s := agentLoop(ctx, agentCtx, cfg)
	// Cancel while the provider is still waiting before its first event.
	cancel()
	for range s.Events() {
	}

	msgs := awaitResult(t, s)
	final := agentcore.LastAssistantOf(msgs)
	if final == nil {
		t.Fatalf("cancelled run produced no assistant message: %+v", msgs)
	}
	if final.StopReason != agentcore.StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted", final.StopReason)
	}
	if got := agentcore.ContentToText(final.Content); got != "" {
		t.Fatalf("text before the first delta = %q, want empty", got)
	}
	if role := msgs[len(msgs)-1].Role(); role != agentcore.RoleAssistant {
		t.Fatalf("transcript ends with %q, want the aborted assistant turn", role)
	}
}

// awaitResult reads the stream result. The event channel is closed, so the
// producer has recorded its outcome; the context is detached because the run's
// own context is cancelled by design in these tests.
func awaitResult(t *testing.T, s *LoopEventStream) agentcore.MessageList {
	t.Helper()
	msgs, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("run result error: %v", err)
	}
	return msgs
}
