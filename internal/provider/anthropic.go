// This file implements the Anthropic Messages API streaming decoder (US-008).
// It is a stateful Decoder (see transport.go) that translates Anthropic SSE
// event payloads into the provider-agnostic AssistantMessageEvent set.
//
// Anthropic streams a fixed event sequence:
//
//	message_start        → seeds id/model and initial usage (input tokens)
//	content_block_start  → opens a text / thinking / redacted_thinking / tool_use
//	                       block at an index
//	content_block_delta  → text_delta / thinking_delta / signature_delta /
//	                       input_json_delta append to the open block
//	content_block_stop   → closes the block (tool_use JSON is parsed here)
//	message_delta        → carries the final stop_reason and output-token usage
//	message_stop         → terminal; the accumulated message is emitted as done
//	error                → a runtime error payload → terminal error event
//
// Only the parsing of these payloads lives here. Block ordering, signatures,
// usage and stop-reason mapping come from the shared assistantTurn (turn.go),
// so the thinking block behaves identically to the OpenAI Chat and Responses
// decoders.
//
// Per the dual failure model (FR-13) the decoder never panics: malformed
// payloads and Anthropic `error` events are surfaced as a returned error (which
// the transport turns into a terminal StreamErrorEvent) rather than crashing.
package provider

import (
	"encoding/json"
	"fmt"

	"github.com/smallnest/pigo/internal/agentcore"
)

// anthropicBlockRef remembers which accumulated block an Anthropic content-block
// index maps onto, so deltas land in the right block even when the wire
// interleaves blocks (extended thinking emits thinking, text and tool_use in
// one message, and interleaved thinking repeats the pattern per step).
type anthropicBlockRef struct {
	block *turnBlock
	// kind is the Anthropic content_block type as written on the wire.
	kind string
}

// AnthropicDecoder is the stateful SSE decoder for the Anthropic Messages API.
// It implements the transport Decoder interface. It is not safe for concurrent
// use — the transport drives it from a single goroutine.
type AnthropicDecoder struct {
	turn *assistantTurn
	// refs maps a wire content-block index onto the accumulated block. The
	// shared turn keeps production order; this map only routes deltas.
	refs map[int]*anthropicBlockRef
	done bool // message_stop / done already emitted
}

// NewAnthropicDecoder builds a fresh decoder for one streamed response.
func NewAnthropicDecoder() *AnthropicDecoder {
	return NewAnthropicDecoderFor("anthropic")
}

// NewAnthropicDecoderFor builds a decoder whose terminal message is tagged with
// the given provider name (e.g. "bedrock", "minimax"), keeping session metadata
// and error text accurate for Anthropic-compatible gateways.
func NewAnthropicDecoderFor(provider string) *AnthropicDecoder {
	return &AnthropicDecoder{
		turn: newAssistantTurn("anthropic", provider),
		refs: make(map[int]*anthropicBlockRef),
	}
}

// anthropicEvent is the discriminated envelope shared by every Anthropic SSE
// data payload; fields are populated selectively by event type.
type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`

	ContentBlock *struct {
		Type string `json:"type"`
		// text
		Text string `json:"text"`
		// thinking
		Thinking string `json:"thinking"`
		// redacted_thinking carries its opaque payload in data.
		Data string `json:"data"`
		// tool_use
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`

	Delta *struct {
		Type string `json:"type"`
		// text_delta
		Text string `json:"text"`
		// thinking_delta
		Thinking string `json:"thinking"`
		// signature_delta
		Signature string `json:"signature"`
		// input_json_delta
		PartialJSON string `json:"partial_json"`
		// message_delta
		StopReason string `json:"stop_reason"`
	} `json:"delta"`

	Usage *anthropicUsage `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicUsage is the token usage block. cache_creation_input_tokens and
// cache_read_input_tokens are already counted in input_tokens by the API, so
// they are not added again here; only the wire names are accepted.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Decode turns one Anthropic SSE data payload into zero or more StreamEvents.
func (d *AnthropicDecoder) Decode(payload []byte) ([]StreamEvent, error) {
	// Read the discriminator first: a future event type may reuse a field name
	// with a different shape (a string "delta", say), and unmarshalling the full
	// envelope before knowing the type would turn that into a fatal stream error
	// instead of an ignored event.
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return nil, fmt.Errorf("anthropic: parse event: %w", err)
	}
	switch head.Type {
	case "message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop", "ping", "error":
		// Known event: decode the full envelope below.
	default:
		return nil, nil
	}

	var ev anthropicEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("anthropic: parse event: %w", err)
	}

	switch ev.Type {
	case "message_start":
		return d.onMessageStart(ev), nil
	case "content_block_start":
		return d.onBlockStart(ev), nil
	case "content_block_delta":
		return d.onBlockDelta(ev), nil
	case "content_block_stop":
		d.endBlock(ev.Index)
		return nil, nil
	case "message_delta":
		return d.onMessageDelta(ev), nil
	case "message_stop":
		return d.finishDone(), nil
	case "ping":
		return nil, nil
	case "error":
		msg := "anthropic stream error"
		if ev.Error != nil {
			if ev.Error.Type != "" {
				msg = "anthropic " + ev.Error.Type
			}
			if ev.Error.Message != "" {
				msg += ": " + ev.Error.Message
			}
		}
		return nil, fmt.Errorf("%s", msg)
	default:
		// Unknown event types are ignored (forward-compatible): new capability
		// events (server tool use, citations, …) must not break the stream.
		return nil, nil
	}
}

// Finish flushes a terminal done event if the stream ended without an explicit
// message_stop (e.g. a clean EOF mid-stream), so a partial response is still
// delivered rather than lost.
func (d *AnthropicDecoder) Finish() ([]StreamEvent, error) {
	if d.done {
		return nil, nil
	}
	return d.finishDone(), nil
}

func (d *AnthropicDecoder) onMessageStart(ev anthropicEvent) []StreamEvent {
	if ev.Message != nil {
		d.turn.responseID = ev.Message.ID
		d.turn.responseModel = ev.Message.Model
		if ev.Message.Usage != nil {
			d.turn.usage(ev.Message.Usage.InputTokens, ev.Message.Usage.OutputTokens)
		}
	}
	return []StreamEvent{StreamStartEvent{Partial: d.turn.message()}}
}

func (d *AnthropicDecoder) onBlockStart(ev anthropicEvent) []StreamEvent {
	if ev.ContentBlock == nil {
		return nil
	}
	var block *turnBlock
	switch ev.ContentBlock.Type {
	case "text":
		block = d.turn.openBlock(turnBlockText)
		if ev.ContentBlock.Text != "" {
			block.text.WriteString(ev.ContentBlock.Text)
		}
	case "thinking":
		block = d.turn.beginThinking()
		if ev.ContentBlock.Thinking != "" {
			block.thinking.WriteString(ev.ContentBlock.Thinking)
		}
	case "redacted_thinking":
		d.turn.redactedThinking(ev.ContentBlock.Data)
		d.refs[ev.Index] = &anthropicBlockRef{kind: ev.ContentBlock.Type}
		return []StreamEvent{StreamThinkingEvent{Partial: d.turn.message()}}
	case "tool_use":
		block = d.turn.beginTool(ev.ContentBlock.ID, ev.ContentBlock.Name)
	default:
		// Server tool results, citations, images, … are not part of pigo's
		// assistant content model; still record the index so a later delta does
		// not open a stray block.
		d.refs[ev.Index] = &anthropicBlockRef{kind: ev.ContentBlock.Type}
		return nil
	}
	d.refs[ev.Index] = &anthropicBlockRef{block: block, kind: ev.ContentBlock.Type}

	// Announce only blocks that already carry content: an empty thinking or text
	// card in the transcript is noise, and the matching delta will announce it.
	switch block.kind {
	case turnBlockThinking:
		if block.thinking.Len() == 0 {
			return nil
		}
		return []StreamEvent{StreamThinkingEvent{Partial: d.turn.message()}}
	case turnBlockTool:
		return []StreamEvent{StreamToolCallEvent{Partial: d.turn.message()}}
	default:
		if block.text.Len() == 0 {
			return nil
		}
		return []StreamEvent{StreamTextEvent{Partial: d.turn.message()}}
	}
}

func (d *AnthropicDecoder) onBlockDelta(ev anthropicEvent) []StreamEvent {
	if ev.Delta == nil {
		return nil
	}
	ref := d.refs[ev.Index]
	if ref == nil {
		// A delta for an unseen index: open a block of the matching kind so data
		// is never silently dropped by a non-conforming stream.
		ref = d.openImplicitBlock(ev.Delta.Type)
		if ref == nil {
			return nil
		}
		d.refs[ev.Index] = ref
	}
	if ref.block == nil {
		// The index belongs to a block type pigo does not model (server tool use,
		// citations, …). Its deltas must not touch the open block, or they would
		// append text/tool state to an unrelated block.
		return nil
	}
	// Route the delta to the block its index names rather than to whatever was
	// opened last: interleaved thinking interleaves thinking/tool_use blocks, and
	// a delta belonging to an earlier block must not extend a later one.
	d.turn.focus(ref.block)
	switch ev.Delta.Type {
	case "text_delta":
		d.turn.textDelta(ev.Delta.Text)
		return []StreamEvent{StreamTextEvent{Partial: d.turn.message()}}
	case "thinking_delta":
		d.turn.thinkingDelta(ev.Delta.Thinking)
		return []StreamEvent{StreamThinkingEvent{Partial: d.turn.message()}}
	case "signature_delta":
		// The signature belongs to the thinking block it rides on; dropping it
		// would make the next tool-use turn fail the extended-thinking echo.
		d.turn.thinkingSignature(ev.Delta.Signature)
		return []StreamEvent{StreamThinkingEvent{Partial: d.turn.message()}}
	case "input_json_delta":
		d.turn.toolArgs(ev.Delta.PartialJSON)
		return []StreamEvent{StreamToolCallEvent{Partial: d.turn.message()}}
	default:
		return nil
	}
}

// openImplicitBlock opens a block for a delta that arrived before its
// content_block_start, inferring the kind from the delta type. It returns nil for
// a delta type pigo has no block for, so the caller can ignore it instead of
// attaching it to the wrong block.
func (d *AnthropicDecoder) openImplicitBlock(deltaType string) *anthropicBlockRef {
	switch deltaType {
	case "thinking_delta", "signature_delta":
		b := d.turn.beginThinking()
		return &anthropicBlockRef{block: b, kind: string(b.kind)}
	case "text_delta":
		b := d.turn.openBlock(turnBlockText)
		return &anthropicBlockRef{block: b, kind: string(b.kind)}
	case "input_json_delta":
		b := d.turn.beginTool("", "")
		return &anthropicBlockRef{block: b, kind: string(b.kind)}
	default:
		return nil
	}
}

func (d *AnthropicDecoder) onMessageDelta(ev anthropicEvent) []StreamEvent {
	if ev.Delta != nil && ev.Delta.StopReason != "" {
		d.turn.stop(ev.Delta.StopReason)
	}
	if ev.Usage != nil {
		// message_delta reports cumulative output tokens (and sometimes input).
		d.turn.usage(ev.Usage.InputTokens, ev.Usage.OutputTokens)
	}
	// No standalone event kind for usage/stop-reason accumulation; the values
	// surface in the terminal done message.
	return nil
}

// endBlock closes the shared turn's open pointer when the block at index is the
// one currently receiving deltas, so a delta that arrives after its own
// content_block_stop opens a fresh block instead of extending a closed one.
func (d *AnthropicDecoder) endBlock(index int) {
	if ref := d.refs[index]; ref != nil && ref.block != nil && d.turn.open == ref.block {
		d.turn.endBlock()
	}
}

// finishDone builds the terminal assistant message and marks the decoder done.
func (d *AnthropicDecoder) finishDone() []StreamEvent {
	if d.done {
		return nil
	}
	d.done = true
	return []StreamEvent{StreamDoneEvent{Message: d.turn.doneMessage()}}
}

// partial is the current snapshot of the accumulated turn.
func (d *AnthropicDecoder) partial() agentcore.AssistantMessage {
	return d.turn.message()
}
