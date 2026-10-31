// This file implements the OpenAI-compatible streaming decoder (US-009): a
// stateful Decoder (see transport.go) for the OpenAI Chat Completions SSE
// stream, which is also the wire format spoken by most third-party gateways
// (OpenRouter, Groq, together, local servers, …). Selecting the base URL is a
// transport concern (NewRequest builds the *http.Request), so this decoder is
// base-URL agnostic and reused across every OpenAI-compatible provider.
//
// OpenAI streams a sequence of chat.completion.chunk objects:
//
//	{"choices":[{"delta":{"role":"assistant"}}]}                 → first chunk
//	{"choices":[{"delta":{"content":"Hel"}}]}                    → text delta
//	{"choices":[{"delta":{"reasoning_content":"Let me"}}]}       → thinking delta
//	{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1",
//	    "function":{"name":"f","arguments":"{\"a\":"}}}]}]}       → tool-call delta
//	{"choices":[{"finish_reason":"tool_calls"}]}                 → stop reason
//	{"usage":{"prompt_tokens":10,"completion_tokens":5}}         → final usage
//	[DONE]                                                        → transport-level
//
// The chunk is parsed here; accumulation, content ordering and the terminal
// AssistantMessage come from the shared assistantTurn (turn.go), so the thinking
// block behaves identically to the Anthropic and Responses decoders.
//
// Per the dual failure model (FR-13) the decoder never panics: malformed
// payloads surface as a returned error which the transport turns into a
// terminal StreamErrorEvent.
package provider

import (
	"encoding/json"
	"fmt"

	"github.com/smallnest/pigo/internal/agentcore"
)

// OpenAIDecoder is the stateful SSE decoder for the OpenAI Chat Completions API
// and compatible gateways. It implements the transport Decoder interface and is
// not safe for concurrent use — the transport drives it from one goroutine.
type OpenAIDecoder struct {
	turn *assistantTurn
	// toolByIndex maps each streamed tool_calls[].index onto its accumulated
	// block, since Chat Completions can interleave fragments of several calls.
	toolByIndex map[int]*turnBlock
	done        bool
}

// NewOpenAIDecoder builds a fresh decoder for one streamed response.
func NewOpenAIDecoder() *OpenAIDecoder {
	return NewOpenAIDecoderFor("openai")
}

// NewOpenAIDecoderFor builds a decoder whose terminal message is tagged with
// the given concrete provider name (an OpenAI-compatible gateway such as
// "openrouter"), keeping error text and session metadata accurate.
func NewOpenAIDecoderFor(provider string) *OpenAIDecoder {
	return &OpenAIDecoder{
		turn:        newAssistantTurn("openai", provider),
		toolByIndex: make(map[int]*turnBlock),
	}
}

// openaiChunk is the streamed chat.completion.chunk envelope.
type openaiChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// ReasoningContent carries the model's reasoning/thinking stream on the
			// OpenAI wire (DeepSeek-R1, Kimi, and other reasoning models put their
			// chain-of-thought here). Some gateways name it "reasoning" instead, so
			// both are accepted; without this field the thinking stream is silently
			// dropped from the response and from history.
			ReasoningContent string            `json:"reasoning_content"`
			Reasoning        string            `json:"reasoning"`
			ToolCalls        []openaiToolDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Some gateways surface an error object inline on the stream.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type openaiToolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Decode turns one OpenAI SSE data payload into zero or more StreamEvents.
func (d *OpenAIDecoder) Decode(payload []byte) ([]StreamEvent, error) {
	var chunk openaiChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("openai: parse chunk: %w", err)
	}
	if chunk.Error != nil {
		msg := "openai stream error"
		if chunk.Error.Type != "" {
			msg = "openai " + chunk.Error.Type
		}
		if chunk.Error.Message != "" {
			msg += ": " + chunk.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}

	if chunk.ID != "" {
		d.turn.responseID = chunk.ID
	}
	if chunk.Model != "" {
		d.turn.responseModel = chunk.Model
	}
	if chunk.Usage != nil {
		d.turn.usage(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
	}

	var events []StreamEvent
	for _, choice := range chunk.Choices {
		// Reasoning stream (DeepSeek-R1 / Kimi / …): reasoning_content is the
		// common field; a few gateways use "reasoning". Accumulate whichever is set.
		// Use the "leading" variant: some gateways interleave or emit reasoning
		// after the answer even though it logically precedes it, and the reasoning
		// must stay one leading block (unlike Anthropic, whose interleaved
		// thinking order is significant and is preserved verbatim).
		if r := choice.Delta.ReasoningContent; r != "" {
			d.turn.thinkingDeltaLeading(r)
			events = append(events, StreamThinkingEvent{Partial: d.turn.message()})
		} else if r := choice.Delta.Reasoning; r != "" {
			d.turn.thinkingDeltaLeading(r)
			events = append(events, StreamThinkingEvent{Partial: d.turn.message()})
		}
		if choice.Delta.Content != "" {
			d.turn.textDelta(choice.Delta.Content)
			events = append(events, StreamTextEvent{Partial: d.turn.message()})
		}
		for _, tc := range choice.Delta.ToolCalls {
			d.applyToolDelta(tc)
			events = append(events, StreamToolCallEvent{Partial: d.turn.message()})
		}
		if choice.FinishReason != "" {
			d.turn.stop(choice.FinishReason)
		}
	}
	return events, nil
}

// Finish flushes a terminal done event if the stream ended without an explicit
// terminator, so a partial response is still delivered rather than lost.
func (d *OpenAIDecoder) Finish() ([]StreamEvent, error) {
	if d.done {
		return nil, nil
	}
	return d.finishDone(), nil
}

// applyToolDelta merges one tool-call fragment into the accumulated state. The
// delta index identifies the call across fragments; id/name usually arrive once
// in the first fragment and arguments accumulate.
func (d *OpenAIDecoder) applyToolDelta(tc openaiToolDelta) {
	call := d.toolByIndex[tc.Index]
	if call == nil {
		// First fragment for this index: it carries the header and often the
		// opening of the argument JSON as well, so fall through and append.
		call = d.turn.beginTool(tc.ID, tc.Function.Name)
		d.toolByIndex[tc.Index] = call
	}
	if tc.ID != "" {
		call.toolID = tc.ID
	}
	if tc.Function.Name != "" {
		call.toolName = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		call.args.WriteString(tc.Function.Arguments)
	}
}

// finishDone builds the terminal assistant message and marks the decoder done.
func (d *OpenAIDecoder) finishDone() []StreamEvent {
	if d.done {
		return nil
	}
	d.done = true
	return []StreamEvent{StreamDoneEvent{Message: d.turn.doneMessage()}}
}

// partial is the current snapshot; kept for callers that only need the message.
func (d *OpenAIDecoder) partial() agentcore.AssistantMessage {
	return d.turn.message()
}
