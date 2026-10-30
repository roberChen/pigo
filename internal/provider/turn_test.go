// This file implements the cross-protocol conformance suite for the shared
// assistant turn accumulator (#546). Each wire protocol is driven through its
// real decoder with a stream that means the same thing, and every protocol must
// produce the same AssistantMessage: the same block order, the same thinking
// text, the same tool calls, the same usage and the same stop reason.
//
// This is the regression net the refactor exists for. Before the accumulator,
// each decoder had its own ordering/dedup/stop-reason logic, so a capability
// added on one wire (a reasoning block, a signature, a usage field) could work
// there and silently do nothing on another. A test that only exercises one
// protocol cannot catch that; this one compares them.
package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// streamCase is one protocol's encoding of the shared scenario below: think,
// answer, then call a tool with streamed arguments.
type streamCase struct {
	name string
	body string
}

// parityScenarioSSE holds the same turn rendered on each wire:
//
//	thinking "let me check " → text "checking" → tool get_weather{"city":"SF"}
//	usage in=11 out=7, stop reason = tool_use.
var parityScenarioSSE = []streamCase{
	{
		name: "openai/chat",
		body: `data: {"id":"c1","model":"m1","choices":[{"delta":{"role":"assistant","reasoning_content":"let me check "}}]}

data: {"id":"c1","model":"m1","choices":[{"delta":{"content":"checking"}}]}

data: {"id":"c1","model":"m1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}

data: {"id":"c1","model":"m1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}

data: {"id":"c1","model":"m1","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}

data: [DONE]

`,
	},
	{
		name: "anthropic",
		body: `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m1","usage":{"input_tokens":11,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me check "}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"checking"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`,
	},
	{
		name: "openai/resp_api",
		body: `data: {"type":"response.created","response":{"id":"resp_1","model":"m1"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_text.delta","output_index":0,"item_id":"rs_1","delta":"let me check "}

data: {"type":"response.reasoning_text.done","output_index":0,"item_id":"rs_1","text":"let me check "}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant"}}

data: {"type":"response.output_text.delta","output_index":1,"item_id":"msg_1","delta":"checking"}

data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather"}}

data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"city\":"}

data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"\"SF\"}"}

data: {"type":"response.completed","response":{"id":"resp_1","model":"m1","usage":{"input_tokens":11,"output_tokens":7},"output":[{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"let me check "}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}]}}

`,
	},
}

// decoderFor returns the decoder a driver of that protocol uses in production,
// so the parity test exercises the real wiring rather than a test-only path.
func decoderFor(name string) Decoder {
	switch name {
	case "anthropic":
		return NewAnthropicDecoder()
	case "openai/resp_api":
		return NewResponsesDecoder("openai")
	default:
		return NewOpenAIDecoder()
	}
}

// TestAssistantTurnParityAcrossProtocols asserts that one logical turn renders
// identically on every wire: thinking before text before the tool call, one
// call with the streamed arguments joined, shared usage, and tool_use as the
// stop reason.
func TestAssistantTurnParityAcrossProtocols(t *testing.T) {
	var reference agentcore.AssistantMessage
	for i, tc := range parityScenarioSSE {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, msg := feedSSE(t, decoderFor(tc.name), tc.body)

			assertParityMessage(t, msg)

			if i == 0 {
				reference = msg
				return
			}
			// Block kinds and their order must match the first protocol exactly.
			if got, want := contentKinds(msg.Content), contentKinds(reference.Content); !equalStrings(got, want) {
				t.Errorf("content order = %v, want %v (as %s)", got, want, parityScenarioSSE[0].name)
			}
			if got, want := agentcore.ThinkingText(msg.Content), agentcore.ThinkingText(reference.Content); got != want {
				t.Errorf("thinking = %q, want %q", got, want)
			}
			if got, want := agentcore.ContentToText(msg.Content), agentcore.ContentToText(reference.Content); got != want {
				t.Errorf("text = %q, want %q", got, want)
			}
			if got, want := msg.ToolCalls(), reference.ToolCalls(); len(got) != len(want) || len(got) != 1 {
				t.Fatalf("tool calls = %d, want %d", len(got), len(want))
			}
			if got, want := msg.ToolCalls()[0], reference.ToolCalls()[0]; got.ID != want.ID ||
				got.Name != want.Name || string(got.Arguments) != string(want.Arguments) {
				t.Errorf("tool call = %+v, want %+v", got, want)
			}
			if msg.Usage == nil || reference.Usage == nil ||
				*msg.Usage != *reference.Usage {
				t.Errorf("usage = %+v, want %+v", msg.Usage, reference.Usage)
			}
		})
	}
}

// assertParityMessage checks the protocol-independent expectations of the shared
// scenario, independent of which wire produced it.
func assertParityMessage(t *testing.T, msg agentcore.AssistantMessage) {
	t.Helper()
	wantKinds := []string{"thinking", "text", "tool_call"}
	if got := contentKinds(msg.Content); !equalStrings(got, wantKinds) {
		t.Fatalf("content kinds = %v, want %v", got, wantKinds)
	}
	if got := agentcore.ThinkingText(msg.Content); got != "let me check " {
		t.Errorf("thinking = %q, want %q", got, "let me check ")
	}
	if got := agentcore.ContentToText(msg.Content); got != "checking" {
		t.Errorf("text = %q, want %q", got, "checking")
	}
	calls := msg.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_weather" {
		t.Errorf("tool call = id:%q name:%q, want call_1/get_weather", calls[0].ID, calls[0].Name)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("tool arguments %q not an object: %v", calls[0].Arguments, err)
	}
	if args["city"] != "SF" {
		t.Errorf("tool args = %v, want city=SF", args)
	}
	if msg.StopReason != agentcore.StopReasonToolUse {
		t.Errorf("stop reason = %q, want tool_use", msg.StopReason)
	}
	if msg.Usage == nil || msg.Usage.InputTokens != 11 || msg.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want {11 7}", msg.Usage)
	}
	if msg.API == "" || msg.Provider == "" {
		t.Errorf("message must be tagged with api/provider, got %q/%q", msg.API, msg.Provider)
	}
}

// TestThinkingBeforeTextRegardlessOfArrivalOrder covers a gateway that emits
// reasoning *after* answer text. Grouping channels naively would append a second
// thinking block after the text; the accumulator must extend the existing
// reasoning block so the model's thinking stays one block and stays first.
func TestThinkingBeforeTextRegardlessOfArrivalOrder(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"delta":{"reasoning_content":"late reasoning"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	_, msg := feedSSE(t, NewOpenAIDecoder(), body)

	if got := contentKinds(msg.Content); !equalStrings(got, []string{"thinking", "text"}) {
		t.Fatalf("content kinds = %v, want [thinking text]", got)
	}
	if got := agentcore.ThinkingText(msg.Content); got != "late reasoning" {
		t.Errorf("thinking = %q, want %q", got, "late reasoning")
	}
	if got := agentcore.ContentToText(msg.Content); got != "answer" {
		t.Errorf("text = %q, want %q", got, "answer")
	}
}

// TestOpenAIReasoningFieldAlias covers gateways that name the reasoning stream
// "reasoning" instead of DeepSeek's "reasoning_content". Dropping it would lose
// the thinking block entirely for those providers.
func TestOpenAIReasoningFieldAlias(t *testing.T) {
	body := `data: {"choices":[{"delta":{"reasoning":"thinking via alias"}}]}

data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}

data: [DONE]

`
	_, msg := feedSSE(t, NewOpenAIDecoder(), body)
	if got := agentcore.ThinkingText(msg.Content); got != "thinking via alias" {
		t.Errorf("thinking = %q, want %q", got, "thinking via alias")
	}
}

// TestAnthropicInterleavedThinkingPreservesOrder covers extended/interleaved
// thinking, where the model alternates thinking and tool_use within one turn. The
// blocks must keep production order and must not merge, because the next request
// has to echo each thinking block adjacent to the tool_use that follows it.
func TestAnthropicInterleavedThinkingPreservesOrder(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_i","model":"m","usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_a","name":"lookup"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"a\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"step two"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"sig2"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_b","name":"lookup"}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"b\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	_, msg := feedSSE(t, NewAnthropicDecoder(), body)

	want := []string{"thinking", "tool_call", "thinking", "tool_call"}
	if got := contentKinds(msg.Content); !equalStrings(got, want) {
		t.Fatalf("content kinds = %v, want %v", got, want)
	}
	// Each thinking block must keep its own text and its own signature; a merged
	// block would carry only one signature and the next request would be rejected.
	sigs := thinkingSignatures(msg.Content)
	if !equalStrings(sigs, []string{"sig1", "sig2"}) {
		t.Errorf("thinking signatures = %v, want [sig1 sig2]", sigs)
	}
	if got, want := thinkingTexts(msg.Content), []string{"step one", "step two"}; !equalStrings(got, want) {
		t.Errorf("per-block thinking = %v, want %v (blocks must not merge)", got, want)
	}
	calls := msg.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "call_a" || calls[1].ID != "call_b" {
		t.Errorf("tool calls = %+v, want call_a then call_b", calls)
	}
}

// TestAnthropicRedactedThinkingBlock covers redacted_thinking: the opaque payload
// must survive as a thinking block (flagged redacted) so the next request can
// echo it back; dropping it makes extended thinking unusable after a redaction.
func TestAnthropicRedactedThinkingBlock(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_r","model":"m","usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque=="}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	_, msg := feedSSE(t, NewAnthropicDecoder(), body)

	var redacted *agentcore.ThinkingContent
	for _, c := range msg.Content {
		if tc, ok := c.(agentcore.ThinkingContent); ok && tc.Redacted {
			redacted = &tc
		}
	}
	if redacted == nil {
		t.Fatalf("no redacted thinking block in %v", contentKinds(msg.Content))
	}
	if redacted.ThinkingSignature != "opaque==" {
		t.Errorf("redacted payload = %q, want %q", redacted.ThinkingSignature, "opaque==")
	}
	if got := agentcore.ContentToText(msg.Content); got != "answer" {
		t.Errorf("text = %q, want %q", got, "answer")
	}
}

// TestStopReasonInferenceWithoutFinishReason covers a wire that ends a turn
// without reporting a finish reason even though it issued a tool call. Without
// inferring tool_use the loop would stop and the call would be dropped.
func TestStopReasonInferenceWithoutFinishReason(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"noop","arguments":"{}"}}]}}]}

data: [DONE]

`
	_, msg := feedSSE(t, NewOpenAIDecoder(), body)
	if msg.StopReason != agentcore.StopReasonToolUse {
		t.Errorf("stop reason = %q, want tool_use inferred from the tool call", msg.StopReason)
	}
}

// TestEmptyThinkingBlockIsDropped covers a decoder that opens a reasoning block
// which never receives text (a summary-only or aborted reasoning item). An empty
// thinking block would render as a blank card and be replayed on the next turn.
func TestEmptyThinkingBlockIsDropped(t *testing.T) {
	body := `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[]}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"hi"}

data: {"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}}

`
	_, msg := feedSSE(t, NewResponsesDecoder("openai"), body)
	if got := contentKinds(msg.Content); !equalStrings(got, []string{"text"}) {
		t.Errorf("content kinds = %v, want [text] (empty thinking must be dropped)", got)
	}
}

// TestResponsesPermissionlessToolCallWithoutDeltas covers a Responses
// implementation that reports a finished function_call only as a done item, with
// no argument deltas. The call must still surface and stop on tool_use.
func TestResponsesToolCallOnlyOnDoneItem(t *testing.T) {
	body := `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"read_file","arguments":"{\"path\":\"a.go\"}"}}

data: {"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":2,"output_tokens":3},"output":[{"type":"function_call","id":"fc_1","call_id":"call_9","name":"read_file","arguments":"{\"path\":\"a.go\"}"}]}}

`
	_, msg := feedSSE(t, NewResponsesDecoder("openai"), body)
	calls := msg.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "call_9" || calls[0].Name != "read_file" || string(calls[0].Arguments) != `{"path":"a.go"}` {
		t.Errorf("tool call = %+v, want call_9/read_file with raw args", calls[0])
	}
	if msg.StopReason != agentcore.StopReasonToolUse {
		t.Errorf("stop reason = %q, want tool_use", msg.StopReason)
	}
}

// TestDecodersIgnoreUnknownEvents covers forward compatibility: an event type a
// newer protocol revision adds must not fail the stream, unlike a pinned SDK
// whose event union would reject it.
func TestDecodersIgnoreUnknownEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		dec  Decoder
	}{
		{"openai/chat", NewOpenAIDecoder()},
		{"anthropic", NewAnthropicDecoder()},
		{"openai/resp_api", NewResponsesDecoder("openai")},
	} {
		if _, err := tc.dec.Decode([]byte(`{"type":"some.brand_new.event","delta":"x"}`)); err != nil {
			t.Errorf("%s: unknown event type must be ignored, got %v", tc.name, err)
		}
	}
}

// TestMalformedPayloadNeverPanics is the FR-13 guarantee at the decoder level:
// every decoder returns an error rather than panicking on garbage.
func TestMalformedPayloadNeverPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		dec  Decoder
	}{
		{"openai/chat", NewOpenAIDecoder()},
		{"anthropic", NewAnthropicDecoder()},
		{"openai/resp_api", NewResponsesDecoder("openai")},
	} {
		// Truncated/invalid JSON must always surface as an error, never a panic.
		for _, payload := range []string{`{not json`, `[]`, `{"choices":`} {
			if _, err := tc.dec.Decode([]byte(payload)); err == nil {
				t.Errorf("%s: payload %q should return an error", tc.name, payload)
			}
		}
		// A well-formed payload carrying only fields this build does not know is
		// ignored, not rejected — that is the forward-compatibility contract.
		if _, err := tc.dec.Decode([]byte(`{"brand_new_field":{"a":1}}`)); err != nil {
			t.Errorf("%s: an unknown field must be ignored, got %v", tc.name, err)
		}
	}
}

// TestDecodeThroughTransportMatchesDirectDecode drives every protocol through
// the real transport pump over HTTP and asserts the resulting message equals the
// one the decoder produces when fed the same bytes directly. It catches a driver
// that forgets to wire its decoder, or wires the wrong provider tag.
func TestDecodeThroughTransportMatchesDirectDecode(t *testing.T) {
	for _, tc := range parityScenarioSSE {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			direct, want := feedSSE(t, decoderFor(tc.name), tc.body)

			srv := sseServer(t, tc.body)
			defer srv.Close()
			stream, err := StreamRequest(context.Background(), TransportConfig{
				NewRequest: newReqFn(srv.URL),
				Decoder:    decoderFor(tc.name),
			})
			if err != nil {
				t.Fatalf("StreamRequest: %v", err)
			}
			var kinds []string
			for ev := range stream.Events() {
				kinds = append(kinds, ev.EventKind())
			}
			got, err := stream.Result(context.Background())
			if err != nil {
				t.Fatalf("stream result: %v", err)
			}

			if len(kinds) == 0 || kinds[len(kinds)-1] != StreamEventDone {
				t.Errorf("stream must end with a done event, got %v", kinds)
			}
			if got.StopReason != want.StopReason {
				t.Errorf("stop reason = %q, want %q", got.StopReason, want.StopReason)
			}
			if agentcore.ThinkingText(got.Content) != agentcore.ThinkingText(want.Content) {
				t.Errorf("thinking = %q, want %q", agentcore.ThinkingText(got.Content), agentcore.ThinkingText(want.Content))
			}
			if agentcore.ContentToText(got.Content) != agentcore.ContentToText(want.Content) {
				t.Errorf("text = %q, want %q", agentcore.ContentToText(got.Content), agentcore.ContentToText(want.Content))
			}
			if len(got.ToolCalls()) != len(want.ToolCalls()) {
				t.Errorf("tool calls = %d, want %d", len(got.ToolCalls()), len(want.ToolCalls()))
			}
			if len(direct) == 0 {
				t.Error("direct decode produced no events")
			}
		})
	}
}

// TestEveryThinkingStreamEmitsThinkingEvents asserts a thinking partial reaches
// the front-end on all three wires, not just in the final message: without it the
// TUI has nothing to render while the model reasons.
func TestEveryThinkingStreamEmitsThinkingEvents(t *testing.T) {
	for _, tc := range parityScenarioSSE {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			events, _ := feedSSE(t, decoderFor(tc.name), tc.body)
			var sawThinking bool
			for _, ev := range events {
				if te, ok := ev.(StreamThinkingEvent); ok {
					sawThinking = true
					if agentcore.ThinkingText(te.Partial.Content) == "" {
						t.Error("a thinking partial must carry the accumulated reasoning")
					}
				}
			}
			if !sawThinking {
				t.Errorf("no StreamThinkingEvent emitted; kinds = %v", eventKinds(events))
			}
		})
	}
}

// contentKinds names each block of a content list in order.
func contentKinds(list agentcore.ContentList) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		switch c.(type) {
		case agentcore.ThinkingContent:
			out = append(out, "thinking")
		case agentcore.TextContent:
			out = append(out, "text")
		case agentcore.ToolCallContent:
			out = append(out, "tool_call")
		case agentcore.ImageContent:
			out = append(out, "image")
		default:
			out = append(out, "unknown")
		}
	}
	return out
}

// thinkingTexts returns the text of each thinking block, in order.
func thinkingTexts(list agentcore.ContentList) []string {
	var out []string
	for _, c := range list {
		if tc, ok := c.(agentcore.ThinkingContent); ok {
			out = append(out, tc.Thinking)
		}
	}
	return out
}

// thinkingSignatures returns the signature of each thinking block, in order.
func thinkingSignatures(list agentcore.ContentList) []string {
	var out []string
	for _, c := range list {
		if tc, ok := c.(agentcore.ThinkingContent); ok {
			out = append(out, tc.ThinkingSignature)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAnthropicDeltaForUnmodeledBlockDoesNotLeak covers a delta whose
// content-block index belongs to a block type pigo does not model (server tool
// use, citations, images, …). Such a delta must be ignored. The failure mode is
// subtle: appending it to whatever block is open prefixes the model's answer on
// the next turn with unrelated server-tool text, and the Anthropic API then
// rejects the echoed history.
func TestAnthropicDeltaForUnmodeledBlockDoesNotLeak(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_u","model":"m","usage":{"input_tokens":2,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"LEAKED"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	_, msg := feedSSE(t, NewAnthropicDecoder(), body)

	if got := agentcore.ContentToText(msg.Content); got != "answer" {
		t.Errorf("text = %q, want %q (the unmodeled block's delta must not leak into the answer)", got, "answer")
	}
	if got := contentKinds(msg.Content); !equalStrings(got, []string{"text"}) {
		t.Errorf("content kinds = %v, want [text] (no stray block for an unmodeled type)", got)
	}
}
