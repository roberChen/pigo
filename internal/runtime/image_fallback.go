// This file implements the loop's image-degradation policy: the one scenario in
// which a failed turn has its images removed and is retried.
//
// The scenario is deliberately narrow. A vision model that refuses an image is
// only *suspect*; tearing images out of a conversation costs the model context it
// may still be using, so the repair applies only when all of the following hold:
//
//   - the conversation carried no image before this request (so nothing the model
//     already reasoned about is being taken away);
//   - this request added an image through a tool call (view_image, or read on an
//     image file) — the picture the request may have tripped over is new;
//   - the user did not explicitly declare this model vision-capable (a
//     declaration is the user's call to trust; pigo does not override it), and
//   - the run has not already been repaired once.
//
// Everything else — a history that already had images, a user-attached picture, a
// failed request that added no image, a declared-vision model, a cancelled run,
// a run with no images at all — keeps the previous behavior: the error is
// terminal.
//
// The error text is not consulted. A gateway may answer with a bare
// "400 Bad Request", in another language, or in a shape pigo has never seen, and
// a missed classification is expensive: the picture stays in the history and
// every later request fails the same way. The narrow preconditions above are the
// filter instead.
package runtime

import (
	"context"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/diag"
)

// imageFallback is one run's image-degradation policy plus the state it needs:
// the single "already repaired" bit that bounds the retry.
//
// It deliberately carries no run-wide block: a repair removes the picture this
// request added and nothing else. Later view_image/read calls still hand the model
// pictures, and if the endpoint refuses one the model reads the provider's error
// and decides for itself — pigo does not turn one failure into a silent,
// run-long image ban.
type imageFallback struct {
	// declaredVision records that the user's configuration explicitly declares the
	// active model vision-capable; such a run is never repaired.
	declaredVision bool
	// used records that the one allowed repair has happened.
	used bool
}

// newImageFallback builds the policy for a run. declaredVision is the answer to
// provider.ModelImageSupportDeclared for the run's model.
func newImageFallback(declaredVision bool) *imageFallback {
	return &imageFallback{declaredVision: declaredVision}
}

// recover applies the repair when the failed turn matches the degradation
// scenario, and reports whether the turn should be retried.
//
// prevReqLen is the number of messages the *previous* request carried, i.e. the
// boundary between "the conversation as the model last saw it" and "what this
// request added". agentCtx.Messages may have grown (the failed assistant message)
// but the boundary is still valid.
//
// emit is the run's own emitter: the degraded tool results are re-announced with
// IsError set so a front-end that already rendered the call's output — and
// possibly expanded a screenshot preview — learns the call is a failure instead
// of showing a picture the model never saw.
func (f *imageFallback) recover(ctx context.Context, agentCtx *agentcore.AgentContext, prevReqLen int, assistant agentcore.AssistantMessage, emit func(agentcore.AgentEvent) error) bool {
	switch {
	case f.used, f.declaredVision:
		return false
	case ctx.Err() != nil:
		// The user cancelled the run: the retry would fail again, and the image is
		// not the reason.
		return false
	case prevReqLen < 0 || prevReqLen > len(agentCtx.Messages):
		// A compaction or a PrepareNextTurn shrank the context, so the boundary no
		// longer describes the previous request. Do not guess.
		return false
	}
	// The conversation the previous request saw must be image-free, and this
	// request must have added an image through a tool call.
	if agentcore.MessagesHaveImage(agentCtx.Messages[:prevReqLen]) {
		return false
	}
	added := imageToolResults(agentCtx.Messages[prevReqLen:])
	if len(added) == 0 {
		return false
	}

	f.used = true
	repaired := agentcore.StripImagesInHistory(agentCtx.Messages, assistant.ErrorMessage)
	dropTrailingErrorAssistant(agentCtx, assistant)
	for _, tr := range repaired {
		_ = emit(agentcore.ToolExecutionEndEvent{
			ToolCallID: tr.ToolCallID,
			ToolName:   tr.ToolName,
			Result:     agentcore.AgentToolResult{Content: tr.Content, Details: tr.Details},
			IsError:    true,
		})
	}
	diag.Diagf("request failed after %d image-producing tool call(s) (%s); removed the image(s), degraded %d tool result(s) and retrying",
		len(added), assistant.ErrorMessage, len(repaired))
	return true
}

// imageToolResults returns the tool results in msgs that carry an image, in
// order. These are the calls the degradation scenario is about: view_image, or
// read on an image file — the two tools that hand the model a picture.
func imageToolResults(msgs agentcore.MessageList) []agentcore.ToolResultMessage {
	var out []agentcore.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok && agentcore.ContentHasImage(tr.Content) {
			out = append(out, tr)
		}
	}
	return out
}

// dropTrailingErrorAssistant removes the error assistant message a failed stream
// leaves at the end of the context, so the retry does not replay it as an empty
// model turn. The early "cannot build the stream" failure never appends one, so
// the tail is matched defensively rather than assumed.
func dropTrailingErrorAssistant(agentCtx *agentcore.AgentContext, failed agentcore.AssistantMessage) {
	n := len(agentCtx.Messages)
	if n == 0 {
		return
	}
	last, ok := agentCtx.Messages[n-1].(agentcore.AssistantMessage)
	if !ok || last.StopReason != agentcore.StopReasonError || last.ErrorMessage != failed.ErrorMessage {
		return
	}
	agentCtx.Messages = agentCtx.Messages[:n-1]
}
