// This file implements streamAssistantResponse (US-003): it shapes the context
// into a provider request, resolves the API key dynamically, drives the
// provider stream, and back-fills the partial assistant message into the
// context while emitting message_start / message_update / message_end events.
package runtime

import (
	"context"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/compaction"
	"github.com/smallnest/pigo/internal/imageload"
	"github.com/smallnest/pigo/internal/provider"
)

// LoopConfig holds the pluggable behavior of the agent loop. Every hook is
// optional (nil = use the default). The pointer/func-field pattern mirrors pi's
// optional callbacks.
type LoopConfig struct {
	// Model is the model id passed to StreamFn.
	Model string
	// APIKey is the static fallback key when GetAPIKey is nil or returns "".
	APIKey string
	// ThinkingLevel is the reasoning effort for requests.
	ThinkingLevel agentcore.ThinkingLevel
	// Stream produces the provider stream. Required (defaults are wired by
	// callers/tests, e.g. a fake provider).
	Stream provider.StreamFn

	// TransformContext optionally rewrites the message list before conversion
	// (context trimming/injection). Contract: must not error; on failure return
	// a safe fallback. Runs first.
	TransformContext func(ctx context.Context, msgs agentcore.MessageList) agentcore.MessageList
	// ConvertToLlm optionally filters UI-only messages. Defaults to identity.
	// Contract: must not error.
	ConvertToLlm func(msgs agentcore.MessageList) agentcore.MessageList
	// GetAPIKey optionally resolves a fresh key per request (handles short-lived
	// token expiry). Falls back to APIKey when nil or empty.
	GetAPIKey func(ctx context.Context, provider string) string
	// Provider is the provider name passed to GetAPIKey.
	Provider string

	// ContextWindow is the model's total context-token budget, used to decide
	// automatic compaction. When <= 0 the window is unknown and auto-compaction
	// is disabled (ShouldCompact returns false), so the loop behaves exactly as
	// before for callers that do not plumb it through.
	ContextWindow int
	// Compaction holds the thresholds/retention knobs for auto-compaction. Its
	// Enabled flag gates the feature independently of ContextWindow.
	Compaction compaction.CompactionSettings
	// SummaryStream produces the provider stream used to generate compaction
	// summaries. Defaults to Stream when nil.
	SummaryStream provider.StreamFn
	// SummaryModel is the model used for summarization. When zero, a model is
	// synthesized from Model/ContextWindow.
	SummaryModel provider.Model

	// Extra is forwarded to StreamConfig.Extra.
	Extra map[string]any
}

// streamAssistantResponse runs one assistant turn: it builds the request from
// agentCtx, streams the provider response, back-fills the partial into
// agentCtx.Messages, and returns the final assistant message. The sequence
// (transformContext → convertToLlm → resolve key → stream → drain) is kept
// identical to pi. It never returns an error for a request failure — such
// failures arrive as a terminal assistant message with stopReason error/aborted.
func streamAssistantResponse(ctx context.Context, agentCtx *agentcore.AgentContext, cfg LoopConfig, emit agentcore.EmitFunc) (agentcore.AssistantMessage, error) {
	// 1. transformContext (optional, must not error).
	msgs := agentCtx.Messages
	if cfg.TransformContext != nil {
		msgs = cfg.TransformContext(ctx, msgs)
	}
	// 2. convertToLlm (filter UI-only; default identity).
	if cfg.ConvertToLlm != nil {
		msgs = cfg.ConvertToLlm(msgs)
	}
	// 2.5 Drop images the selected model cannot accept. A resumed session may have
	// been recorded against a vision model, and a mid-session /model switch can
	// land on a text-only one; either way the wire would reject the whole request
	// and take the turn down instead of the picture. The session keeps its images
	// — only this LLM-bound copy is flattened, so switching back still sees them.
	//
	// A repair does not arm a run-wide latch: images a later request brings are
	// left alone, and the model decides from the provider's error (see
	// runtime/tool_fallback.go for the repaired cases).
	if !provider.ModelSupportsImages(cfg.Provider, cfg.Model) {
		msgs = agentcore.StripImages(msgs)
	}
	// 2.6 Drop images the wire cannot carry at all (unsupported format, bytes
	// that contradict the declared type, an empty payload). Ingestion already
	// refuses those, but a session recorded before that rule was enforced — and
	// most importantly a *resumed* one — still holds the picture on disk, and
	// every request that re-sends it fails with a 400 that names the image. The
	// session keeps its image; only the LLM-bound copy gets a notice.
	//
	// This is a backstop, not a capability guess: it drops only images that no
	// hosted multimodal endpoint accepts (and the ones whose bytes are not the
	// type they claim). Whether this *model* takes pictures is the check above.
	msgs, _ = imageload.SanitizeWire(msgs)
	// 3. shape the LLM context.
	llm := provider.LlmContext{
		SystemPrompt: agentCtx.SystemPrompt,
		Messages:     msgs,
		Tools:        agentCtx.Tools,
	}
	// 4. resolve API key dynamically, fall back to static.
	key := cfg.APIKey
	if cfg.GetAPIKey != nil {
		if dyn := cfg.GetAPIKey(ctx, cfg.Provider); dyn != "" {
			key = dyn
		}
	}
	// 5. build the provider stream.
	stream, err := cfg.Stream(ctx, cfg.Model, llm, provider.StreamConfig{
		APIKey:        key,
		ThinkingLevel: cfg.ThinkingLevel,
		Extra:         cfg.Extra,
	})
	if err != nil {
		// Early "cannot build stream" failure: synthesize a terminal message so
		// the loop has a uniform assistant message to record.
		return newErrorAssistantMessage(cfg, err), nil
	}

	// 6. drain the stream, back-filling the partial into the context.
	addedPartial := false
	backfill := func(partial agentcore.AssistantMessage) {
		if !addedPartial {
			agentCtx.Messages = append(agentCtx.Messages, partial)
			addedPartial = true
		} else {
			agentCtx.Messages[len(agentCtx.Messages)-1] = partial
		}
	}

	for ev := range stream.Events() {
		switch e := ev.(type) {
		case provider.StreamStartEvent:
			backfill(e.Partial)
			if err := emit(ctx, agentcore.MessageStartEvent{Message: e.Partial}); err != nil {
				return agentcore.AssistantMessage{}, err
			}
		case provider.StreamTextEvent:
			backfill(e.Partial)
			if err := emit(ctx, agentcore.MessageUpdateEvent{Message: e.Partial, AssistantMessageEvent: e}); err != nil {
				return agentcore.AssistantMessage{}, err
			}
		case provider.StreamThinkingEvent:
			backfill(e.Partial)
			if err := emit(ctx, agentcore.MessageUpdateEvent{Message: e.Partial, AssistantMessageEvent: e}); err != nil {
				return agentcore.AssistantMessage{}, err
			}
		case provider.StreamToolCallEvent:
			backfill(e.Partial)
			if err := emit(ctx, agentcore.MessageUpdateEvent{Message: e.Partial, AssistantMessageEvent: e}); err != nil {
				return agentcore.AssistantMessage{}, err
			}
		case provider.StreamDoneEvent:
			finalizeMessage(agentCtx, e.Message, &addedPartial)
			if err := emit(ctx, agentcore.MessageEndEvent{Message: e.Message}); err != nil {
				return agentcore.AssistantMessage{}, err
			}
			return e.Message, nil
		case provider.StreamErrorEvent:
			msg := abortedIfCanceled(ctx, e.Message)
			// The transport builds its failure message from scratch. Keep the text
			// the user already saw (and that would otherwise vanish from the
			// persisted turn) without adopting half-finished tool calls, which would
			// leave an unanswered tool_call in the transcript.
			if addedPartial {
				if prev, ok := agentCtx.Messages[len(agentCtx.Messages)-1].(agentcore.AssistantMessage); ok {
					msg.Content = mergePartialText(msg.Content, prev.Content)
				}
			}
			finalizeMessage(agentCtx, msg, &addedPartial)
			if err := emit(ctx, agentcore.MessageEndEvent{Message: msg}); err != nil {
				return agentcore.AssistantMessage{}, err
			}
			return msg, nil
		}
	}

	// 7. stream ended without done/error: fall back to the stream result. The
	// event channel is closed at this point, which means the producer has already
	// recorded its outcome, so read it without racing the run's (possibly
	// cancelled) context: a plain Result(ctx) picks randomly between the closed
	// result channel and ctx.Done(), and losing that race turns a run that did
	// record messages into a bare "context canceled".
	final, resErr := stream.Result(context.WithoutCancel(ctx))
	if resErr != nil {
		msg := abortedIfCanceled(ctx, newErrorAssistantMessage(cfg, resErr))
		// A cancelled context makes the transport's error event itself
		// undeliverable (Emit drops it), so this is the common cancel path: keep
		// the text the consumer already received instead of reporting an empty turn.
		if addedPartial {
			if prev, ok := agentCtx.Messages[len(agentCtx.Messages)-1].(agentcore.AssistantMessage); ok {
				msg.Content = mergePartialText(msg.Content, prev.Content)
			}
		}
		finalizeMessage(agentCtx, msg, &addedPartial)
		_ = emit(ctx, agentcore.MessageEndEvent{Message: msg})
		return msg, nil
	}
	finalizeMessage(agentCtx, final, &addedPartial)
	if err := emit(ctx, agentcore.MessageEndEvent{Message: final}); err != nil {
		return agentcore.AssistantMessage{}, err
	}
	return final, nil
}

// finalizeMessage replaces the placeholder partial with the final message, or
// appends it if the provider sent done/error without a prior start.
func finalizeMessage(agentCtx *agentcore.AgentContext, final agentcore.AssistantMessage, addedPartial *bool) {
	if *addedPartial {
		agentCtx.Messages[len(agentCtx.Messages)-1] = final
	} else {
		agentCtx.Messages = append(agentCtx.Messages, final)
		*addedPartial = true
	}
}

// abortedIfCanceled rewrites a cancelled request's terminal error into an abort.
// The transport reports a cancelled stream as StopReasonError ("stream aborted")
// because it cannot tell a user cancel from an upstream failure; only the run
// context knows. Without this, every consumer's StopReasonAborted branch (REPL
// Ctrl-C, TUI, headless, the host's cancel reply) is unreachable and a cancel
// looks like a provider failure.
func abortedIfCanceled(ctx context.Context, msg agentcore.AssistantMessage) agentcore.AssistantMessage {
	if msg.StopReason != agentcore.StopReasonError || ctx.Err() == nil {
		return msg
	}
	msg.StopReason = agentcore.StopReasonAborted
	msg.ErrorMessage = "aborted"
	return msg
}

// recordCancelledTurn makes sure a cancelled run leaves a terminal assistant
// message: it finishes the in-flight partial as aborted (keeping its text but
// dropping half-streamed tool calls, which would otherwise sit in the transcript
// without results), or appends an empty aborted turn when nothing was streamed.
func recordCancelledTurn(agentCtx *agentcore.AgentContext, cfg LoopConfig) {
	if n := len(agentCtx.Messages); n > 0 {
		if a, ok := agentCtx.Messages[n-1].(agentcore.AssistantMessage); ok {
			if a.StopReason != "" {
				// Already terminal: the cancel arrived while the terminal message was
				// itself being emitted (the error event was recorded, then its emit
				// failed). Appending another turn would leave an empty ghost message.
				return
			}
			a.StopReason = agentcore.StopReasonAborted
			a.Content = mergePartialText(nil, a.Content)
			agentCtx.Messages[n-1] = a
			return
		}
	}
	agentCtx.Messages = append(agentCtx.Messages, agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Model:      cfg.Model,
		Provider:   cfg.Provider,
		StopReason: agentcore.StopReasonAborted,
	})
}

// mergePartialText appends the text blocks of a streamed partial to a terminal
// message that carries none, so an aborted or failed turn keeps what the user
// already saw. Non-text blocks are dropped on purpose (see the call site).
func mergePartialText(final, partial agentcore.ContentList) agentcore.ContentList {
	if len(final) > 0 {
		return final
	}
	var out agentcore.ContentList
	for _, block := range partial {
		if _, ok := block.(agentcore.TextContent); ok {
			out = append(out, block)
		}
	}
	return out
}

// newErrorAssistantMessage builds a terminal assistant message for an early
// failure that never produced a provider stream.
func newErrorAssistantMessage(cfg LoopConfig, err error) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:    agentcore.RoleAssistant,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		StopReason:   agentcore.StopReasonError,
		ErrorMessage: err.Error(),
	}
}
