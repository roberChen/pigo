// This file implements the protocol-agnostic assistant turn accumulator
// (#546): the one place every wire decoder records what a model has produced so
// far. Decoders differ only in how they parse bytes into channel updates;
// block ordering, empty-block suppression, usage, stop-reason mapping and the
// terminal AssistantMessage are decided here once, so a capability such as a
// thinking block cannot work on one protocol and silently regress on another.
//
// The contract a decoder follows:
//
//	turn := newAssistantTurn("openai", d.name)
//	turn.thinkingDelta("partial reasoning") // opens/extends the open block
//	turn.textDelta("hello")
//	call := turn.beginTool(id, name)         // opens a tool block
//	turn.toolArgs(`{"city":`)                // extends the open tool block
//	turn.stop("tool_calls")                  // protocol-specific raw reason
//	msg := turn.message()                    // the terminal AssistantMessage
//
// Blocks are kept in the order the model produced them (thinking, text and
// tool calls alike) rather than grouped by kind. That matters for Anthropic
// extended thinking, where the prior thinking block and its signature must be
// echoed back adjacent to the tool_use that follows, and where interleaved
// thinking produces thinking/text/tool_use/thinking/tool_use sequences. Grouping
// channels would silently reorder that history and get the follow-up request
// rejected. For the common single-reasoning-block stream the arrival order is
// exactly thinking → text → tool calls, which is what the TUI renders.
package provider

import (
	"encoding/json"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// Block kinds carried by a turnBlock.
const (
	turnBlockThinking = "thinking"
	turnBlockText     = "text"
	turnBlockTool     = "tool"
)

// turnBlock is one content block under construction. Only the fields for its
// kind are used; the rest stay zero.
type turnBlock struct {
	kind string

	// thinking
	thinking  strings.Builder
	signature string
	redacted  bool

	// text
	text strings.Builder

	// tool
	toolID   string
	toolName string
	args     strings.Builder
}

// assistantTurn is the shared accumulator every decoder writes into. It is not
// safe for concurrent use — a decoder is driven from one goroutine.
type assistantTurn struct {
	// api and provider tag the produced message; they are the only
	// protocol-identifying facts a caller sees.
	api      string
	provider string

	blocks []*turnBlock
	// open is the block currently receiving deltas; nil between blocks or when
	// the next delta should open a new one.
	open *turnBlock

	responseID    string
	responseModel string

	inputTokens  int
	outputTokens int

	// stopReason is a pigo StopReason once the decoder has mapped one.
	stopReason string

	// sawToolCall records a tool call regardless of protocol, so stop-reason
	// inference (tool_use) does not depend on each decoder remembering to set it.
	sawToolCall bool
}

// newAssistantTurn starts an empty accumulator tagged with the wire API
// ("openai", "anthropic", "openai/resp_api", …) and the concrete provider name.
func newAssistantTurn(api, provider string) *assistantTurn {
	return &assistantTurn{api: api, provider: provider}
}

// openBlock returns the open block when it has the wanted kind, else opens a new
// one. This is what makes streaming decoders idempotent: repeating a delta on
// the same channel extends the block instead of starting a second one.
func (t *assistantTurn) openBlock(kind string) *turnBlock {
	if t.open != nil && t.open.kind == kind {
		return t.open
	}
	b := &turnBlock{kind: kind}
	t.blocks = append(t.blocks, b)
	t.open = b
	return b
}

// beginThinking opens a fresh reasoning block even if one is already open. A
// protocol that signals explicit block boundaries (Anthropic content_block_start)
// uses it so two reasoning blocks never merge.
func (t *assistantTurn) beginThinking() *turnBlock {
	b := &turnBlock{kind: turnBlockThinking}
	t.blocks = append(t.blocks, b)
	t.open = b
	return b
}

// thinkingDelta appends reasoning text to the open reasoning block.
func (t *assistantTurn) thinkingDelta(s string) {
	if s == "" {
		return
	}
	t.openBlock(turnBlockThinking).thinking.WriteString(s)
}

// thinkingDeltaLeading appends reasoning that logically precedes the answer even
// when the wire delivers it late (after the answer text has started). Chat
// Completions has no notion of reasoning-block signatures, so a stream that
// emits reasoning out of order must still render as one leading thinking block
// rather than a second one after the answer. Anthropic must NOT use this: its
// interleaved thinking deliberately alternates thinking and tool_use, and the
// block order has to be preserved verbatim for the echo requirement.
func (t *assistantTurn) thinkingDeltaLeading(s string) {
	if s == "" {
		return
	}
	// Extend the first reasoning block when one exists, so a late fragment
	// joins the model's reasoning instead of starting a second block.
	for _, b := range t.blocks {
		if b.kind == turnBlockThinking {
			b.thinking.WriteString(s)
			t.open = b
			return
		}
	}
	b := &turnBlock{kind: turnBlockThinking}
	b.thinking.WriteString(s)
	t.blocks = append([]*turnBlock{b}, t.blocks...)
	t.open = b
}

// thinkingSignature attaches a signature to the open reasoning block — the
// Anthropic extended-thinking echo requirement.
func (t *assistantTurn) thinkingSignature(sig string) {
	if sig == "" {
		return
	}
	t.openBlock(turnBlockThinking).signature += sig
}

// redactedThinking records an opaque reasoning block (Anthropic
// redacted_thinking) whose payload rides in the signature field.
func (t *assistantTurn) redactedThinking(data string) {
	b := t.beginThinking()
	b.redacted = true
	b.signature = data
	t.endBlock()
}

// textDelta appends assistant answer text to the open text block.
func (t *assistantTurn) textDelta(s string) {
	if s == "" {
		return
	}
	t.openBlock(turnBlockText).text.WriteString(s)
}

// beginTool opens a new tool-call block and returns it. id/name may be filled in
// later by the wire (Chat Completions sends them in the first fragment only).
func (t *assistantTurn) beginTool(id, name string) *turnBlock {
	b := &turnBlock{kind: turnBlockTool, toolID: id, toolName: name}
	t.blocks = append(t.blocks, b)
	t.open = b
	t.sawToolCall = true
	return b
}

// toolArgs appends argument fragments to the open tool call, opening a bare one
// when the wire sent arguments before the call header.
func (t *assistantTurn) toolArgs(frag string) *turnBlock {
	call := t.openBlock(turnBlockTool)
	call.args.WriteString(frag)
	return call
}

// focus makes b the block that receives subsequent deltas, so a decoder that
// knows which block an update belongs to (Anthropic routes by content-block
// index) does not depend on arrival order or on a stray endBlock having closed
// the block it wants to extend.
func (t *assistantTurn) focus(b *turnBlock) {
	if b != nil {
		t.open = b
	}
}

// endBlock closes the open block; the next delta opens a new one. It is a no-op
// when no block is open.
func (t *assistantTurn) endBlock() { t.open = nil }

// usage records token usage. Zero values are ignored so a wire that omits them
// leaves the message without a Usage block rather than reporting 0/0.
func (t *assistantTurn) usage(in, out int) {
	if in != 0 {
		t.inputTokens = in
	}
	if out != 0 {
		t.outputTokens = out
	}
}

// stop records a raw protocol stop reason via the shared mapper.
func (t *assistantTurn) stop(raw string) {
	if raw == "" {
		return
	}
	t.stopReason = mapStopReason(t.api, raw)
}

// setStopReason records an already-mapped pigo StopReason.
func (t *assistantTurn) setStopReason(reason string) {
	if reason != "" {
		t.stopReason = reason
	}
}

// message materializes the accumulated turn in production order. Empty thinking
// and text blocks are dropped so a protocol that opens a block and streams
// nothing into it does not add stray content. StopReason defaults to end_turn
// and is upgraded to tool_use whenever a tool call was seen, so a wire that
// forgets to report the finish reason still drives the loop correctly.
func (t *assistantTurn) message() agentcore.AssistantMessage {
	msg := agentcore.AssistantMessage{
		RoleField:     agentcore.RoleAssistant,
		API:           t.api,
		Provider:      t.provider,
		StopReason:    t.stopReason,
		ResponseID:    t.responseID,
		ResponseModel: t.responseModel,
	}
	if t.inputTokens != 0 || t.outputTokens != 0 {
		msg.Usage = &agentcore.Usage{InputTokens: t.inputTokens, OutputTokens: t.outputTokens}
	}

	for _, b := range t.blocks {
		switch b.kind {
		case turnBlockThinking:
			if b.redacted {
				msg.Content = append(msg.Content, agentcore.ThinkingContent{
					Type:              agentcore.ContentTypeThinking,
					ThinkingSignature: b.signature,
					Redacted:          true,
				})
				continue
			}
			if b.thinking.Len() == 0 {
				continue
			}
			tc := agentcore.NewThinkingContent(b.thinking.String())
			tc.ThinkingSignature = b.signature
			msg.Content = append(msg.Content, tc)
		case turnBlockText:
			if b.text.Len() == 0 {
				continue
			}
			msg.Content = append(msg.Content, agentcore.NewTextContent(b.text.String()))
		case turnBlockTool:
			msg.Content = append(msg.Content, agentcore.NewToolCallContent(
				b.toolID, b.toolName, toolArguments(b.args.String())))
		}
	}

	if msg.StopReason == "" {
		msg.StopReason = agentcore.StopReasonEndTurn
	}
	if t.sawToolCall && msg.StopReason == agentcore.StopReasonEndTurn {
		// A wire that ended without a finish reason but did call a tool must still
		// drive the next turn, otherwise the loop stops and the call is dropped.
		msg.StopReason = agentcore.StopReasonToolUse
	}
	return msg
}

// toolArguments normalizes accumulated argument fragments into a RawMessage.
// Empty arguments become {}; malformed JSON is passed through verbatim so the
// executor can report the parse failure to the model (ToolCallContent keeps the
// original bytes and re-serializes them safely).
func toolArguments(raw string) json.RawMessage {
	args := json.RawMessage(strings.TrimSpace(raw))
	if len(args) == 0 {
		return json.RawMessage("{}")
	}
	return args
}

// mapStopReason maps a wire stop reason to pigo's StopReason set for a given
// API. Unknown reasons default to end_turn (a natural, non-error stop), and a
// gateway that mixes vocabularies across protocols is still understood.
func mapStopReason(api, raw string) string {
	if r, ok := anthropicStopReasons[raw]; ok && api == "anthropic" {
		return r
	}
	if r, ok := openAIStopReasons[raw]; ok && (api == "openai" || api == ProtocolOpenAIResponses) {
		return r
	}
	// Cross-protocol fallback: gateways in the wild relabel finish reasons
	// (a Chat Completions endpoint answering "end_turn"), so accept the union
	// before defaulting.
	if r, ok := openAIStopReasons[raw]; ok {
		return r
	}
	if r, ok := anthropicStopReasons[raw]; ok {
		return r
	}
	return agentcore.StopReasonEndTurn
}

var openAIStopReasons = map[string]string{
	"stop":              agentcore.StopReasonEndTurn,
	"end_turn":          agentcore.StopReasonEndTurn,
	"tool_calls":        agentcore.StopReasonToolUse,
	"function_call":     agentcore.StopReasonToolUse,
	"length":            agentcore.StopReasonLength,
	"max_output_tokens": agentcore.StopReasonLength,
}

var anthropicStopReasons = map[string]string{
	"end_turn":      agentcore.StopReasonEndTurn,
	"stop_sequence": agentcore.StopReasonEndTurn,
	"tool_use":      agentcore.StopReasonToolUse,
	"max_tokens":    agentcore.StopReasonLength,
}
