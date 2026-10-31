package provider

// This file covers replay-time repair of abnormal tool calls (#547): a model
// that streams a call without an id, with two calls sharing an id, with an empty
// or invalid tool name, or with a result that pairs with nothing.
//
// The invariant under test is about the request, not about one wire: whatever the
// history holds, every encoded request must (a) be valid JSON, (b) carry only
// ids/names the provider accepts, (c) keep ids unique within the request, and
// (d) pair every call with exactly one result and every result with a call.
// Without that, the loop's recovery turn ("arguments are not valid JSON" and
// friends) is never reached, because the provider rejects the request first.

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// replayedCall is one tool call as it appears on an encoded wire.
type replayedCall struct {
	id   string
	name string
	args string
}

// replayedResult is one tool result as it appears on an encoded wire, identified
// by the call id it answers.
type replayedResult struct {
	callID string
}

// wireCase encodes a history for one provider protocol and extracts its replayed
// tool calls and results.
type wireCase struct {
	name    string
	encode  func(agentcore.MessageList) ([]byte, error)
	extract func(t *testing.T, body []byte) (calls []replayedCall, results []replayedResult)
}

// wireCases covers every protocol pigo can speak.
var wireCases = []wireCase{
	{"openai/chat", encodeHistoryAsOpenAI, extractOpenAIToolCalls},
	{"anthropic", encodeHistoryAsAnthropic, extractAnthropicToolCalls},
	{"openai/resp_api", encodeHistoryAsResponses, extractResponsesToolCalls},
}

func encodeHistoryAsOpenAI(msgs agentcore.MessageList) ([]byte, error) {
	return encodeOpenAIRequest(CompletionRequest{Model: "gpt-test", Context: LlmContext{Messages: msgs}})
}

func encodeHistoryAsAnthropic(msgs agentcore.MessageList) ([]byte, error) {
	return encodeAnthropicRequest(CompletionRequest{Model: "claude-test", Context: LlmContext{Messages: msgs}}, nil)
}

func encodeHistoryAsResponses(msgs agentcore.MessageList) ([]byte, error) {
	return encodeResponsesRequest(CompletionRequest{Model: "gpt-test", Context: LlmContext{Messages: msgs}})
}

func extractOpenAIToolCalls(t *testing.T, body []byte) ([]replayedCall, []replayedResult) {
	t.Helper()
	var req struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	decodeBodyInto(t, body, &req)
	var calls []replayedCall
	var results []replayedResult
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			calls = append(calls, replayedCall{id: c.ID, name: c.Function.Name, args: c.Function.Arguments})
		}
		if m.Role == "tool" {
			results = append(results, replayedResult{callID: m.ToolCallID})
		}
	}
	return calls, results
}

func extractAnthropicToolCalls(t *testing.T, body []byte) ([]replayedCall, []replayedResult) {
	t.Helper()
	var req struct {
		Messages []struct {
			// A text-only message encodes content as a plain string.
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	decodeBodyInto(t, body, &req)
	var calls []replayedCall
	var results []replayedResult
	for _, m := range req.Messages {
		var blocks []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			// tool_result
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue // plain-string content
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				calls = append(calls, replayedCall{id: b.ID, name: b.Name, args: string(b.Input)})
			case "tool_result":
				results = append(results, replayedResult{callID: b.ToolUseID})
			}
		}
	}
	return calls, results
}

func extractResponsesToolCalls(t *testing.T, body []byte) ([]replayedCall, []replayedResult) {
	t.Helper()
	var req struct {
		Input []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"input"`
	}
	decodeBodyInto(t, body, &req)
	var calls []replayedCall
	var results []replayedResult
	for _, it := range req.Input {
		switch it.Type {
		case "function_call":
			calls = append(calls, replayedCall{id: it.CallID, name: it.Name, args: it.Arguments})
		case "function_call_output":
			results = append(results, replayedResult{callID: it.CallID})
		}
	}
	return calls, results
}

// TestAbnormalToolCallsStayReplayable drives histories containing each abnormal
// call through every wire and asserts the request invariants hold afterwards.
func TestAbnormalToolCallsStayReplayable(t *testing.T) {
	cases := []struct {
		name string
		msgs agentcore.MessageList
		// wantCalls / wantResults are the counts expected after repair; they
		// differ from the input for the orphan-result and unanswered-call cases.
		wantCalls   int
		wantResults int
	}{
		{
			name: "call without an id",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("", "read", `{"path":"a.go"}`)),
				toolResultFor("", "ok"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "two calls sharing an id",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", "read", `{"path":"a.go"}`), call("call_1", "read", `{"path":"b.go"}`)),
				toolResultFor("call_1", "a"), toolResultFor("call_1", "b"),
			},
			wantCalls: 2, wantResults: 2,
		},
		{
			name: "empty tool name",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", "", `{}`)),
				toolResultFor("call_1", "unknown tool"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "tool name with a space",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", "read file", `{}`)),
				toolResultFor("call_1", "unknown tool"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "tool name that is too long",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", strings.Repeat("a", maxToolNameLen+1), `{}`)),
				toolResultFor("call_1", "unknown tool"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "tool name with non-ASCII characters",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", "读取文件", `{}`)),
				toolResultFor("call_1", "unknown tool"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "call id with invalid characters",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call:1", "read", `{}`)),
				toolResultFor("call:1", "ok"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "call without id or name",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("", "", ``)),
				toolResultFor("", "unknown tool"),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "result that pairs with no call",
			msgs: agentcore.MessageList{
				userText("start"),
				toolResultFor("ghost", "stale output"),
				assistantText("continuing"),
			},
			wantCalls: 0, wantResults: 0,
		},
		{
			name: "call that no result answers",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", "read", `{"path":"a.go"}`)),
			},
			wantCalls: 1, wantResults: 1,
		},
		{
			name: "unanswered call followed by the next turn",
			msgs: agentcore.MessageList{
				userText("start"),
				assistantCalls(call("call_1", "read", `{}`)),
				userText("stop"),
			},
			wantCalls: 1, wantResults: 1,
		},
	}

	for _, wire := range wireCases {
		for _, tc := range cases {
			t.Run(wire.name+"/"+tc.name, func(t *testing.T) {
				body, err := wire.encode(tc.msgs)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				assertBodyValid(t, body)
				calls, results := wire.extract(t, body)
				if len(calls) != tc.wantCalls {
					t.Fatalf("replayed calls = %d, want %d", len(calls), tc.wantCalls)
				}
				if len(results) != tc.wantResults {
					t.Fatalf("replayed results = %d, want %d", len(results), tc.wantResults)
				}
				assertReplayable(t, callIDs(calls), calls, results)
			})
		}
	}
}

// TestReplayRepairLeavesvalidHistoryAlone guards the common path: a well-formed
// history must reach the wire byte for byte, with no ids or names invented. A
// repair pass that rewrites good data would corrupt what the model is reasoning
// about (and would churn session diffs).
func TestReplayRepairLeavesValidHistoryAlone(t *testing.T) {
	msgs := agentcore.MessageList{
		userText("start"),
		assistantCalls(
			call("call_abc", "read", `{"path":"a.go"}`),
			call("call_def", "grep", `{"pattern":"needle","nested":{"k":[1,2]}}`),
		),
		toolResultFor("call_abc", "file body"),
		toolResultFor("call_def", "matches"),
		assistantText("done"),
	}
	wantIDs := []string{"call_abc", "call_def"}

	for _, wire := range wireCases {
		t.Run(wire.name, func(t *testing.T) {
			body, err := wire.encode(msgs)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			calls, results := wire.extract(t, body)
			if got := callIDs(calls); !equalStrings(got, wantIDs) {
				t.Fatalf("ids = %v, want %v", got, wantIDs)
			}
			if calls[0].name != "read" || calls[1].name != "grep" {
				t.Errorf("names = %q, %q, want read, grep", calls[0].name, calls[1].name)
			}
			// Compared semantically, not byte for byte: the Anthropic wire
			// re-encodes input as a JSON object, which reorders keys. Repair must
			// preserve every value.
			assertSameJSON(t, calls[1].args, `{"pattern":"needle","nested":{"k":[1,2]}}`)
			if got := resultIDs(results); !equalStrings(got, wantIDs) {
				t.Errorf("result ids = %v, want %v", got, wantIDs)
			}
		})
	}
}

// TestRepairDoesNotMutateHistory pins the separation between the wire and the
// record: repair must not touch the live conversation (or the session file
// written from it), so the transcript keeps what the model actually emitted.
func TestRepairDoesNotMutateHistory(t *testing.T) {
	msgs := agentcore.MessageList{
		userText("start"),
		assistantCalls(call("", "read file", `{"path":`)),
		toolResultFor("", "broken"),
	}
	repaired := repairToolCallHistory(msgs)

	original := msgs[1].(agentcore.AssistantMessage).ToolCalls()[0]
	if original.ID != "" || original.Name != "read file" || string(original.Arguments) != `{"path":` {
		t.Errorf("history was mutated: %+v", original)
	}
	if got := msgs[2].(agentcore.ToolResultMessage).ToolCallID; got != "" {
		t.Errorf("tool result was mutated: id = %q, want empty", got)
	}
	if len(repaired) != len(msgs) {
		t.Fatalf("repaired length = %d, want %d", len(repaired), len(msgs))
	}
}

// assertSameJSON fails unless got and want carry the same JSON value.
func assertSameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("replayed arguments are not JSON: %s (%v)", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("expected arguments are not JSON: %s (%v)", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("arguments = %s, want the values of %s", got, want)
	}
}

var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// assertReplayable applies the request invariants shared by every wire.
func assertReplayable(t *testing.T, ids []string, calls []replayedCall, results []replayedResult) {
	t.Helper()
	seen := make(map[string]struct{}, len(calls))
	for _, c := range calls {
		if c.id == "" {
			t.Errorf("call %q has an empty id", c.name)
			continue
		}
		if !toolNamePattern.MatchString(c.id) {
			t.Errorf("call id %q is not a replayable id", c.id)
		}
		if _, dup := seen[c.id]; dup {
			t.Errorf("duplicate call id %q in one request", c.id)
		}
		seen[c.id] = struct{}{}
		if !toolNamePattern.MatchString(c.name) {
			t.Errorf("call name %q would be rejected by the provider", c.name)
		}
		assertObjectJSON(t, c.name, c.args)
	}
	// Every result answers exactly one call, and every call is answered.
	answered := make(map[string]int, len(results))
	for _, r := range results {
		if _, ok := seen[r.callID]; !ok {
			t.Errorf("result for %q has no call in the request", r.callID)
		}
		answered[r.callID]++
	}
	for id := range seen {
		if answered[id] != 1 {
			t.Errorf("call %q answered %d times, want 1", id, answered[id])
		}
	}
}

// callIDs returns the replayed ids in order.
func callIDs(calls []replayedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.id)
	}
	return out
}

// resultIDs returns the ids answered by the results, in order.
func resultIDs(results []replayedResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.callID)
	}
	return out
}

// userText builds a plain user turn.
func userText(s string) agentcore.UserMessage {
	return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(s)}}
}

// assistantText builds a plain assistant turn.
func assistantText(s string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonEndTurn,
		Content:    agentcore.ContentList{agentcore.NewTextContent(s)},
	}
}

// assistantCalls builds a tool-call-only assistant turn.
func assistantCalls(calls ...agentcore.ToolCallContent) agentcore.AssistantMessage {
	content := make(agentcore.ContentList, 0, len(calls))
	for _, c := range calls {
		content = append(content, c)
	}
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonToolUse,
		Content:    content,
	}
}

// call builds a ToolCallContent with arguments taken verbatim.
func call(id, name, args string) agentcore.ToolCallContent {
	return agentcore.NewToolCallContent(id, name, json.RawMessage(args))
}

// toolResultFor builds a successful tool result answering callID.
func toolResultFor(callID, text string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{
		ToolCallID: callID,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}
