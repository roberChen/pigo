// This file implements the loop's failed-request fallback: the scenarios in which
// a turn that the provider rejected is repaired and retried instead of aborting
// the whole run.
//
// A request fails right after a tool produced output in two distinct ways, and
// both are repaired — but differently, and at most once per run:
//
//   - the output itself is a payload the endpoint will not carry. The provider's
//     error names an image (image_error.go) and the request carried one, or the
//     error says nothing usable but the conversation was image-free before this
//     request and a tool call added the picture now. Every image is removed from
//     the conversation (tool results become failed calls quoting the provider
//     error, user turns keep their prompt and lose the picture) and the turn is
//     retried. This reaches a user-attached image, a second screenshot, a resumed
//     session whose history holds it, and a model the user declared
//     vision-capable — a payload the endpoint refuses cannot be sent no matter
//     what the config says.
//   - the output is a *suspect* for a failure of unknown cause: the request added
//     tool results (the delta since the previous request), the provider answered
//     with an error that does not point at the credentials or the rate limit, and
//     replaying the same output would very likely fail the same way. Each of those
//     tool results becomes an abnormal placeholder (agentcore/abnormal.go) holding
//     the call's original arguments and the provider's error verbatim, and the
//     turn is retried once. The placeholder keeps the call/result pairing intact,
//     so the conversation stays valid and the model can re-issue or work around
//     the call instead of the session dying.
//
// Everything else keeps the previous behavior: a failed request that added no tool
// results, an authentication/quota/rate-limit failure (retrying cannot help), a
// cancelled run, a history whose boundary no longer describes the last request
// (compaction), and a second failure after the repair. Those stay terminal.
//
// Why the repair is not run-wide: removing output costs the model context it may
// still be using, and one rejection does not prove the endpoint cannot take the
// payload. A repair therefore touches only what the failed request added (or, when
// the provider named an image, every image in the request) and nothing else: a
// later call still runs, and if the endpoint refuses that one the model sees the
// provider's error and decides.
package runtime

import (
	"context"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/diag"
)

// toolFallback is one run's failed-request policy plus the state it needs: the
// single "already repaired" bit that bounds the retry.
type toolFallback struct {
	// declaredVision records that the user's configuration explicitly declares the
	// active model vision-capable. It disables the *structural* image trigger (a
	// declaration is the user's call to trust), but not the explicit one: an
	// upstream that names the image has contradicted the declaration for this
	// payload, and not the general one, which is not about images at all.
	declaredVision bool
	// used records that the one allowed repair has happened.
	used bool
}

// newToolFallback builds the policy for a run. declaredVision is the answer to
// provider.ModelImageSupportDeclared for the run's model.
func newToolFallback(declaredVision bool) *toolFallback {
	return &toolFallback{declaredVision: declaredVision}
}

// recover applies the repair when the failed turn matches one of the scenarios,
// and reports whether the turn should be retried.
//
// prevReqLen is the number of messages the *previous* request carried, i.e. the
// boundary between "the conversation as the model last saw it" and "what this
// request added". agentCtx.Messages may have grown (the failed assistant message)
// but the boundary is still valid.
//
// emit is the run's own emitter: the degraded tool results are re-announced with
// IsError set so a front-end that already rendered the call's output — and
// possibly expanded a screenshot preview — learns the call is a failure instead
// of showing a result the model never saw.
func (f *toolFallback) recover(ctx context.Context, agentCtx *agentcore.AgentContext, prevReqLen int, assistant agentcore.AssistantMessage, emit func(agentcore.AgentEvent) error) bool {
	switch {
	case f.used:
		return false
	case ctx.Err() != nil:
		// The user cancelled the run: the retry would fail again, and the tool
		// output is not the reason.
		return false
	case prevReqLen < 0 || prevReqLen > len(agentCtx.Messages):
		// A compaction or a PrepareNextTurn shrank the context, so the boundary no
		// longer describes the previous request. Do not guess.
		return false
	}

	trigger := f.trigger(agentCtx, prevReqLen, assistant.ErrorMessage)
	if trigger == repairNone {
		return false
	}
	f.used = true
	var repaired []agentcore.ToolResultMessage
	if trigger == repairExplicit || trigger == repairStructural {
		repaired = agentcore.StripImagesInHistory(agentCtx.Messages, assistant.ErrorMessage)
	} else {
		repaired = agentcore.AbnormalizeToolResults(agentCtx.Messages,
			toolResultIDs(agentCtx.Messages[prevReqLen:]), assistant.ErrorMessage, abnormalReason)
	}
	dropTrailingErrorAssistant(agentCtx, assistant)
	f.announce(repaired, emit)
	diag.Diagf("request failed (%s); triggered %s repair on %d tool result(s) and retrying",
		assistant.ErrorMessage, trigger, len(repaired))
	return true
}

// abnormalReason explains, in the placeholder, why the output was replaced.
const abnormalReason = "the request was rejected while this call's output was in the conversation, so the output was replaced and the turn was retried without it"

// repairTrigger says why (or whether) a failed turn is repairable.
type repairTrigger int

const (
	repairNone repairTrigger = iota
	repairExplicit
	repairStructural
	repairToolOutput
)

func (t repairTrigger) String() string {
	switch t {
	case repairExplicit:
		return "image-explicit"
	case repairStructural:
		return "image-structural"
	case repairToolOutput:
		return "tool-output"
	}
	return "none"
}

// trigger decides which trigger, if any, applies. It is split out of recover so
// the decision reads as one list of conditions and is unit-testable without an
// emitter.
func (f *toolFallback) trigger(agentCtx *agentcore.AgentContext, prevReqLen int, errText string) repairTrigger {
	before := agentCtx.Messages[:prevReqLen]
	addedTools := toolResultsIn(agentCtx.Messages[prevReqLen:])
	addedImages := imageToolResults(agentCtx.Messages[prevReqLen:])

	// Explicit: the provider blamed the image, and the request carried at least
	// one (there is nothing to remove otherwise). It beats the declaration and the
	// "history already had images" rule — the request cannot succeed as it stands.
	if imageRejectionError(errText) && agentcore.MessagesHaveImage(agentCtx.Messages) {
		return repairExplicit
	}
	// Structural: the conversation was image-free before this request, and this
	// request added one through a tool call — the only suspect there is. A model
	// the user declared vision-capable is left alone here; the declaration is
	// their call to trust.
	if !f.declaredVision && !agentcore.MessagesHaveImage(before) && len(addedImages) > 0 {
		return repairStructural
	}
	// General: the request added tool results, carries no image at all, and the
	// error is not one that retrying cannot fix. The output is the only thing this
	// request introduced, so it is what gets degraded; the conversation keeps
	// everything before it.
	//
	// An image-carrying request is deliberately excluded: the two image triggers
	// above are the ones that may touch a picture, and when they decline (a
	// declared-vision model, an image that predates this request) replacing the
	// tool output would not remove the offending image anyway, so the retry could
	// only fail again.
	if len(addedTools) > 0 && !agentcore.MessagesHaveImage(agentCtx.Messages) && !unrepairableFailure(errText) {
		return repairToolOutput
	}
	return repairNone
}

// unrepairableFailure reports whether the provider error is one that degrading a
// tool result cannot fix: a credential, quota or rate-limit rejection answers the
// same way on every retry, so the error stays terminal (the user must act).
// Context-length and opaque failures are not in this list: an oversized tool
// result is a plausible cause, and there is no better evidence to go on.
func unrepairableFailure(text string) bool {
	t := strings.ToLower(text)
	for _, m := range []string{
		"401", "403", "unauthorized", "unauthorised", "authentication",
		"invalid api key", "invalid_api_key", "incorrect api key", "api key not valid",
		"429", "rate limit", "rate_limit", "too many requests", "quota",
		"insufficient_quota", "billing",
	} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// announce re-emits the degraded tool results so the UI flips the cards it
// already rendered.
func (f *toolFallback) announce(repaired []agentcore.ToolResultMessage, emit func(agentcore.AgentEvent) error) {
	for _, tr := range repaired {
		_ = emit(agentcore.ToolExecutionEndEvent{
			ToolCallID: tr.ToolCallID,
			ToolName:   tr.ToolName,
			Result:     agentcore.AgentToolResult{Content: tr.Content, Details: tr.Details},
			IsError:    true,
		})
	}
}

// toolResultsIn returns the tool result messages in msgs, in order.
func toolResultsIn(msgs agentcore.MessageList) []agentcore.ToolResultMessage {
	var out []agentcore.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			out = append(out, tr)
		}
	}
	return out
}

// toolResultIDs returns the call ids of the tool results in msgs.
func toolResultIDs(msgs agentcore.MessageList) []string {
	refs := toolResultsIn(msgs)
	ids := make([]string, 0, len(refs))
	for _, tr := range refs {
		ids = append(ids, tr.ToolCallID)
	}
	return ids
}

// imageToolResults returns the tool results in msgs that carry an image, in
// order. These are the calls the structural trigger is about: view_image, or
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
