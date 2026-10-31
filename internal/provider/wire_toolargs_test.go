package provider

// This file covers replaying a tool call whose arguments the model emitted as
// malformed JSON (#547).
//
// A model can stream syntactically invalid arguments (truncated, duplicated
// key, stray brace). pigo keeps those bytes in the assistant message verbatim so
// the tool executor can tell the model "arguments are not valid JSON" and the
// model gets a chance to re-issue the call (see runtime/toolcall_recovery_test.go).
// But that message stays in history, and every later request replays it: the
// upstream API validates the replayed arguments and rejects the whole request
// with a 400, so one bad call poisons the session permanently — the recovery
// turn never gets a chance to run.
//
// The fix is at the wire seam: a tool call is replayed with `{}` when its
// arguments are not a JSON object, which is what the Anthropic path already did
// (anthropicToolInput). The in-memory message keeps the original bytes, and the
// session file keeps them too, so nothing is lost for diagnostics.

import (
	"encoding/json"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// malformedToolArgs are argument payloads a model plausibly emits that are not
// usable as a tool_use input / function arguments object.
var malformedToolArgs = []struct {
	name string
	args string
}{
	{"truncated object", `{"path":`},
	{"duplicated key", `{"a":1,"a":{"b":}}`},
	{"trailing comma", `{"a":1,}`},
	{"bare string", `"not an object"`},
	{"array", `[1,2]`},
	{"null", `null`},
	{"not json at all", `path=foo.go`},
	{"empty", ``},
	{"whitespace", "  \n "},
}

// TestOpenAIChatReplaysMalformedToolArgsAsEmptyObject asserts the Chat
// Completions wire never carries arguments that fail upstream JSON validation.
func TestOpenAIChatReplaysMalformedToolArgsAsEmptyObject(t *testing.T) {
	for _, tc := range malformedToolArgs {
		t.Run(tc.name, func(t *testing.T) {
			msg := assistantToolCallMessage("call-1", "read", tc.args)
			entries := encodeOpenAIMessage(msg)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			calls, ok := entries[0]["tool_calls"].([]map[string]any)
			if !ok || len(calls) != 1 {
				t.Fatalf("tool_calls = %#v, want one call", entries[0]["tool_calls"])
			}
			fn := calls[0]["function"].(map[string]any)
			got, ok := fn["arguments"].(string)
			if !ok {
				t.Fatalf("arguments = %#v, want a string", fn["arguments"])
			}
			assertObjectJSON(t, "openai/chat", got)
		})
	}
}

// TestResponsesReplaysMalformedToolArgsAsEmptyObject does the same for the
// Responses wire, whose function_call.arguments is also validated upstream.
func TestResponsesReplaysMalformedToolArgsAsEmptyObject(t *testing.T) {
	for _, tc := range malformedToolArgs {
		t.Run(tc.name, func(t *testing.T) {
			msg := assistantToolCallMessage("call-1", "read", tc.args)
			items := encodeResponsesMessage(msg)
			if len(items) != 1 {
				t.Fatalf("items = %d, want 1 (a tool-call-only turn)", len(items))
			}
			got, ok := items[0]["arguments"].(string)
			if !ok {
				t.Fatalf("arguments = %#v, want a string", items[0]["arguments"])
			}
			assertObjectJSON(t, "openai/resp_api", got)
		})
	}
}

// TestAnthropicReplaysMalformedToolArgsAsEmptyInput does the same for the
// Messages wire, where a non-object input is rejected outright.
func TestAnthropicReplaysMalformedToolArgsAsEmptyInput(t *testing.T) {
	for _, tc := range malformedToolArgs {
		t.Run(tc.name, func(t *testing.T) {
			msg := assistantToolCallMessage("toolu_1", "read", tc.args)
			entry := encodeAnthropicMessage(msg)
			blocks, ok := entry["content"].([]map[string]any)
			if !ok || len(blocks) != 1 {
				t.Fatalf("content = %#v, want one tool_use block", entry["content"])
			}
			if blocks[0]["type"] != "tool_use" {
				t.Fatalf("block type = %v, want tool_use", blocks[0]["type"])
			}
			input, ok := blocks[0]["input"].(map[string]any)
			if !ok {
				t.Fatalf("input = %#v, want a JSON object", blocks[0]["input"])
			}
			if len(input) != 0 {
				t.Errorf("input = %#v, want {} for unusable arguments", input)
			}
		})
	}
}

// TestWellFormedToolArgsSurviveReplay guards the other direction: sanitizing
// must not drop good arguments, including a nested object and a valid but
// unusual scalar-free payload.
func TestWellFormedToolArgsSurviveReplay(t *testing.T) {
	const args = `{"path":"a.go","nested":{"k":[1,2,{"z":null}]}}`

	entries := encodeOpenAIMessage(assistantToolCallMessage("call-1", "read", args))
	got := entries[0]["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["arguments"]
	if got != args {
		t.Errorf("openai/chat arguments = %q, want the original %q", got, args)
	}

	items := encodeResponsesMessage(assistantToolCallMessage("call-1", "read", args))
	if got := items[0]["arguments"]; got != args {
		t.Errorf("openai/resp_api arguments = %q, want the original %q", got, args)
	}

	entry := encodeAnthropicMessage(assistantToolCallMessage("toolu_1", "read", args))
	input := entry["content"].([]map[string]any)[0]["input"].(map[string]any)
	path, _ := input["path"].(string)
	if path != "a.go" {
		t.Errorf("anthropic input.path = %q, want a.go", path)
	}
	if _, ok := input["nested"].(map[string]any); !ok {
		t.Errorf("anthropic input.nested = %#v, want the nested object preserved", input["nested"])
	}
}

// TestMalformedToolArgsStillRecordedVerbatim pins the other half of the
// contract: the wire is sanitized, but the message the loop and the session see
// keeps the model's original bytes so the executor can report the parse error.
func TestMalformedToolArgsStillRecordedVerbatim(t *testing.T) {
	const bad = `{"path":`
	msg := assistantToolCallMessage("call-1", "read", bad)
	tc := msg.ToolCalls()[0]
	if string(tc.Arguments) != bad {
		t.Errorf("recorded arguments = %q, want the original %q", tc.Arguments, bad)
	}
}

// assistantToolCallMessage builds a tool-call-only assistant turn carrying args
// verbatim, bypassing newToolCallContent's helpers so an invalid payload can be
// represented.
func assistantToolCallMessage(id, name, args string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonToolUse,
		Content: agentcore.ContentList{
			agentcore.NewToolCallContent(id, name, json.RawMessage(args)),
		},
	}
}

// assertObjectJSON fails unless s parses as a JSON object, which is what every
// upstream tool-call validator requires of replayed arguments.
func assertObjectJSON(t *testing.T, wire, s string) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		t.Fatalf("%s: replayed arguments %q are not valid JSON: %v", wire, s, err)
	}
	if obj == nil {
		t.Fatalf("%s: replayed arguments %q are not a JSON object", wire, s)
	}
}

// TestReplayOfRecoveredTurnIsAcceptedByEveryWire walks the exact history the
// loop leaves behind after it rejected a malformed call and the model retried:
//
//	user        "start"
//	assistant   tool_call(read, `{"path":`)   ← the bytes the model emitted
//	tool        "Invalid arguments … not valid JSON"
//	assistant   "recovered"
//
// Every subsequent request re-sends that assistant turn. This asserts the fully
// encoded body is valid JSON and that each replayed call carries arguments any
// upstream validator accepts, on all three wires. It is the regression guard for
// the reported symptom: a session that keeps 400-ing after one bad call.
func TestReplayOfRecoveredTurnIsAcceptedByEveryWire(t *testing.T) {
	const bad = `{"path":`
	history := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("start")}},
		assistantToolCallMessage("call-1", "read", bad),
		agentcore.ToolResultMessage{
			ToolCallID: "call-1",
			IsError:    true,
			Content:    agentcore.ContentList{agentcore.NewTextContent("Invalid arguments for tool \"read\": arguments are not valid JSON")},
		},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("recovered")}},
	}

	t.Run("openai/chat", func(t *testing.T) {
		body, err := encodeOpenAIRequest(CompletionRequest{
			Model:   "gpt-test",
			Context: LlmContext{Messages: history},
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		assertBodyValid(t, body)
		var req struct {
			Messages []struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		decodeBodyInto(t, body, &req)
		assertOneObjectArgs(t, "openai/chat", collectOpenAIArgs(req.Messages))
	})

	t.Run("anthropic", func(t *testing.T) {
		body, err := encodeAnthropicRequest(CompletionRequest{
			Model:   "claude-test",
			Context: LlmContext{Messages: history},
		}, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		assertBodyValid(t, body)
		var req struct {
			Messages []struct {
				// A text-only message encodes content as a plain string; an image or
				// mixed message encodes the block array. Decode raw and branch, as a
				// reader of this wire must.
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		decodeBodyInto(t, body, &req)
		var inputs []json.RawMessage
		for _, m := range req.Messages {
			var blocks []struct {
				Type  string          `json:"type"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(m.Content, &blocks); err != nil {
				continue // a plain-string content
			}
			for _, b := range blocks {
				if b.Type == "tool_use" {
					inputs = append(inputs, b.Input)
				}
			}
		}
		if len(inputs) != 1 {
			t.Fatalf("replayed tool_use blocks = %d, want 1", len(inputs))
		}
		assertObjectJSON(t, "anthropic", string(inputs[0]))
	})

	t.Run("openai/resp_api", func(t *testing.T) {
		body, err := encodeResponsesRequest(CompletionRequest{
			Model:   "gpt-test",
			Context: LlmContext{Messages: history},
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		assertBodyValid(t, body)
		var req struct {
			Input []struct {
				Type      string `json:"type"`
				Arguments string `json:"arguments"`
			} `json:"input"`
		}
		decodeBodyInto(t, body, &req)
		var args []string
		for _, it := range req.Input {
			if it.Type == "function_call" {
				args = append(args, it.Arguments)
			}
		}
		assertOneObjectArgs(t, "openai/resp_api", args)
	})
}

// collectOpenAIArgs flattens the replayed tool-call argument strings of a Chat
// Completions body.
func collectOpenAIArgs(msgs []struct {
	ToolCalls []struct {
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}) []string {
	var out []string
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			out = append(out, c.Function.Arguments)
		}
	}
	return out
}

// assertOneObjectArgs requires exactly one replayed call whose arguments are a
// JSON object.
func assertOneObjectArgs(t *testing.T, wire string, args []string) {
	t.Helper()
	if len(args) != 1 {
		t.Fatalf("%s: replayed calls = %d, want 1", wire, len(args))
	}
	assertObjectJSON(t, wire, args[0])
}

// assertBodyValid fails when the encoded request body is not valid JSON at all,
// which a malformed argument payload would otherwise cause if it were inlined.
func assertBodyValid(t *testing.T, body []byte) {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("encoded request body is not valid JSON: %s", body)
	}
}

// decodeBodyInto decodes an encoded request body into out. (thinking_test.go
// already owns a decodeBody helper that returns a generic map.)
func decodeBodyInto(t *testing.T, body []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}
