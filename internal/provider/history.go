// This file implements replay-time repair of tool-call history (#547): the one
// seam that guarantees an outgoing request stays valid no matter what a model
// emitted earlier in the conversation.
//
// A tool call has four parts an upstream validates when the conversation is
// replayed: an id, a name, an object for its arguments, and a matching tool
// result. A model can violate any of them — it streams a truncated argument
// object, a name that is empty or outside `^[a-zA-Z0-9_-]{1,64}$`, or no id at
// all — and the assistant message keeps what it emitted. That is deliberate: the
// loop answers the call with an explanatory error result and the model gets
// another turn to re-issue it, which only works if the transcript stays truthful.
//
// The trouble starts with the *next* request, which replays that message. Every
// provider validates replayed tool calls, so one bad call turns into a 400 for
// the whole conversation — permanently, and again after a resume — which defeats
// the recovery turn: the model never gets to see its own error. Chat Completions
// and Responses used to replay the raw arguments; before this file the same was
// true for ids and names on all three wires.
//
// So the wire is repaired, not the history:
//
//   - ids: empty ones are synthesized, duplicates get a suffix, and the matching
//     tool results are remapped through the same table, so pairing stays exact;
//   - names that cannot be replayed become invalidToolName (the original stays in
//     the assistant message, where the error result already quotes it);
//   - arguments that are not a JSON object become {} (replayToolArguments);
//   - a tool result with no call to pair with is dropped;
//   - a call with no result gets a synthesized error result.
//
// The last two are unreachable from pigo's own loop, which answers every call it
// dispatches (see agenttool.ExecuteToolCalls) and persists a turn only after it
// completes. They exist because the invariant here is about the request, and a
// session file can also be produced by an older build or by an SDK caller.
package provider

import (
	"strconv"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// invalidToolName stands in for a name that cannot be replayed. Both OpenAI and
// Anthropic require tool names to match ^[a-zA-Z0-9_-]{1,64}$, so an empty name
// or one carrying spaces (models do emit "read file") would fail the request.
// A fixed placeholder is used rather than a character-level rewrite because a
// rewrite can land on a real tool name and make a failed call look executed; the
// model still sees the original in the error result it was given.
const invalidToolName = "invalid_tool_call"

// maxToolNameLen is the longest tool name both wires accept.
const maxToolNameLen = 64

// missingToolResultText answers a call the recorded history never answered, so
// the request does not fail the provider's call/result pairing check.
const missingToolResultText = "No result was recorded for this tool call (the turn was interrupted " +
	"before it finished). Re-issue the call if it is still needed."

// pendingCall is one call of an assistant turn awaiting its result.
type pendingCall struct {
	original string // id as recorded in history, matched against the result
	repaired string // id actually sent on the wire
	name     string
}

// historyRepair tracks id assignments for one request so every id it hands out is
// unique within that request.
type historyRepair struct {
	used map[string]struct{}
	seq  int
}

// repairToolCallHistory returns a history in which every tool call and result can
// be replayed through any wire. It never mutates msgs: the live conversation (and
// the session file written from it) keeps what the model actually emitted.
func repairToolCallHistory(msgs agentcore.MessageList) agentcore.MessageList {
	if len(msgs) == 0 {
		return msgs
	}
	r := &historyRepair{used: make(map[string]struct{}, 4)}
	out := make(agentcore.MessageList, 0, len(msgs))
	var pending []pendingCall

	// flush answers calls that the messages after them never answered. It runs
	// before any message that cannot be a result for them, and at the end.
	flush := func() {
		for _, p := range pending {
			out = append(out, agentcore.ToolResultMessage{
				RoleField:  agentcore.RoleToolResult,
				ToolCallID: p.repaired,
				ToolName:   p.name,
				Content:    agentcore.ContentList{agentcore.NewTextContent(missingToolResultText)},
				IsError:    true,
			})
		}
		pending = nil
	}

	for _, m := range msgs {
		switch msg := m.(type) {
		case agentcore.AssistantMessage:
			flush()
			repaired, calls := r.assistant(msg)
			out = append(out, repaired)
			pending = calls
		case agentcore.ToolResultMessage:
			idx := -1
			for i, p := range pending {
				if p.original == msg.ToolCallID {
					idx = i
					break
				}
			}
			if idx < 0 {
				// No call to pair with. A tool message that answers nothing is
				// rejected outright, so it cannot be replayed as-is.
				continue
			}
			msg.ToolCallID = pending[idx].repaired
			pending = append(pending[:idx], pending[idx+1:]...)
			out = append(out, msg)
		default:
			flush()
			out = append(out, m)
		}
	}
	flush()
	return out
}

// assistant repairs one assistant message's tool calls, returning the message and
// the calls its results must pair with.
func (r *historyRepair) assistant(msg agentcore.AssistantMessage) (agentcore.AssistantMessage, []pendingCall) {
	var calls []pendingCall
	var content agentcore.ContentList
	for i, c := range msg.Content {
		call, ok := c.(agentcore.ToolCallContent)
		if !ok {
			continue
		}
		id := r.callID(call.ID)
		name := wireToolName(call.Name)
		calls = append(calls, pendingCall{original: call.ID, repaired: id, name: name})
		if id == call.ID && name == call.Name {
			continue
		}
		if content == nil {
			// Copy once: the caller's ContentList (and the session it was read
			// from) must not change.
			content = make(agentcore.ContentList, len(msg.Content))
			copy(content, msg.Content)
		}
		call.ID, call.Name = id, name
		content[i] = call
	}
	if content != nil {
		msg.Content = content
	}
	return msg, calls
}

// callID returns the id to send for a model-issued call: the sanitized original
// when it is usable and unused, otherwise a synthesized one.
func (r *historyRepair) callID(original string) string {
	base := sanitizeToolCallID(original)
	if base == "" {
		base = r.freshIDPrefix()
	}
	id := base
	// Duplicate ids make it ambiguous which result belongs to which call, so the
	// later ones are renamed (their results follow through the same table).
	for n := 2; ; n++ {
		if _, taken := r.used[id]; !taken {
			break
		}
		id = base + "_" + strconv.Itoa(n)
	}
	r.used[id] = struct{}{}
	return id
}

// freshIDPrefix yields an unused id prefix for a call the model did not identify.
func (r *historyRepair) freshIDPrefix() string {
	for {
		r.seq++
		id := "pigo_call_" + strconv.Itoa(r.seq)
		if _, taken := r.used[id]; !taken {
			return id
		}
	}
}

// wireToolName maps a model-supplied name onto what the wires accept, keeping a
// valid name byte for byte.
func wireToolName(name string) string {
	if name == "" || len(name) > maxToolNameLen {
		return invalidToolName
	}
	for _, c := range name {
		if !isToolNameChar(c) {
			return invalidToolName
		}
	}
	return name
}

// sanitizeToolCallID strips characters a tool-call id may not carry, so an id
// echoed from a model or a gateway cannot break request validation. It returns ""
// when nothing usable is left, in which case the caller synthesizes an id.
func sanitizeToolCallID(id string) string {
	if id == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, c := range id {
		if isToolNameChar(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// isToolNameChar reports whether c may appear in a tool name or tool-call id.
func isToolNameChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		return true
	default:
		return false
	}
}
