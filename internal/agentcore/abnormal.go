// This file implements the "abnormal tool call" placeholder: a tool result that
// stands in for a call whose output cannot (or must not) be replayed.
//
// A conversation is only valid while every tool call has a matching result, and
// upstreams reject a request that replays a call whose output broke it (an image
// the endpoint refuses, an oversized or malformed payload, a tool that no longer
// exists). Deleting the call would leave the assistant turn dangling and rewrite
// history behind the user's back; keeping the output repeats the failure. The
// placeholder keeps the pairing intact while telling the model, in plain text,
// that the call is abnormal, what it was asked to do (the arguments verbatim) and
// why its output is unavailable (the provider's error verbatim). The model can
// then decide to re-issue the call or work around it.
//
// The placeholder is used by two paths that share this shape:
//   - the loop's failed-request fallback (runtime/tool_fallback.go), when a
//     request is rejected right after a tool produced output; and
//   - the user's fine-grained history rollback (/rewind tool), when a single
//     call is rewritten so the conversation branches from it.
package agentcore

import "strings"

// AbnormalToolResultMarker is the opening text of every placeholder, so a reader
// (and a test) can recognize one without decoding Details.
const AbnormalToolResultMarker = "[abnormal tool call"

// AbnormalToolResult is the Details payload of an abnormal tool result. It keeps
// what a later reader needs to reconstruct the call: the tool name, the arguments
// exactly as the model sent them, and the reason (the provider's error text
// verbatim when there is one).
//
// Details is `any` on the message, so this value is a struct while in memory and
// a map after a session round-trip; AbnormalDetailsOf understands both.
type AbnormalToolResult struct {
	ToolName      string `json:"toolName,omitempty"`
	Arguments     string `json:"arguments,omitempty"`
	ProviderError string `json:"providerError,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Text renders the model-facing text of the placeholder. It is deliberately
// error- and argument-agnostic in shape: the opening marker says what happened,
// then the arguments and the provider's own words are quoted so the model (and a
// human reading the transcript) sees exactly what was lost.
func (r AbnormalToolResult) Text() string {
	var b strings.Builder
	b.WriteString(AbnormalToolResultMarker)
	if r.ToolName != "" {
		b.WriteString(": ")
		b.WriteString(r.ToolName)
	}
	b.WriteString("] this tool call is abnormal")
	if r.Reason != "" {
		b.WriteString(": ")
		b.WriteString(r.Reason)
	}
	b.WriteString("\narguments: ")
	if strings.TrimSpace(r.Arguments) == "" {
		b.WriteString("(none)")
	} else {
		b.WriteString(r.Arguments)
	}
	if r.ProviderError != "" {
		b.WriteString("\nprovider error: ")
		b.WriteString(r.ProviderError)
	}
	return b.String()
}

// NewAbnormalToolResult builds the placeholder result for the call identified by
// callID/callName. ts is copied from the result it replaces (or now at the call
// site) so the transcript keeps its ordering. The returned message is marked
// IsError and carries the detail in Details, so a later rollback or export can
// explain it without parsing text.
func NewAbnormalToolResult(callID, callName string, detail AbnormalToolResult, ts int64) ToolResultMessage {
	if detail.ToolName == "" {
		detail.ToolName = callName
	}
	return ToolResultMessage{
		RoleField:  RoleToolResult,
		ToolCallID: callID,
		ToolName:   callName,
		Content:    ContentList{NewTextContent(detail.Text())},
		Details:    detail,
		IsError:    true,
		Timestamp:  ts,
	}
}

// AbnormalDetailsOf reports the placeholder detail on tr, whether it was decoded
// from a session file (map) or built in memory (struct).
func AbnormalDetailsOf(tr ToolResultMessage) (AbnormalToolResult, bool) {
	switch d := tr.Details.(type) {
	case AbnormalToolResult:
		return d, true
	case *AbnormalToolResult:
		if d == nil {
			return AbnormalToolResult{}, false
		}
		return *d, true
	case map[string]any:
		out := AbnormalToolResult{}
		out.ToolName, _ = d["toolName"].(string)
		out.Arguments, _ = d["arguments"].(string)
		out.ProviderError, _ = d["providerError"].(string)
		out.Reason, _ = d["reason"].(string)
		if out == (AbnormalToolResult{}) {
			return out, false
		}
		return out, true
	}
	return AbnormalToolResult{}, false
}

// IsAbnormalToolResult reports whether tr is a placeholder (so a rollback or a
// repair does not replace a placeholder with itself).
func IsAbnormalToolResult(tr ToolResultMessage) bool {
	if _, ok := AbnormalDetailsOf(tr); ok {
		return true
	}
	return strings.HasPrefix(ContentToText(tr.Content), AbnormalToolResultMarker)
}

// FindToolCall returns the call with the given id from the assistant messages in
// msgs. It is how a placeholder recovers the arguments the model originally sent,
// which only the assistant turn holds.
func FindToolCall(msgs MessageList, callID string) (ToolCallContent, bool) {
	for _, m := range msgs {
		a, ok := m.(AssistantMessage)
		if !ok {
			continue
		}
		for _, c := range a.ToolCalls() {
			if c.ID == callID {
				return c, true
			}
		}
	}
	return ToolCallContent{}, false
}

// AbnormalizeToolResults rewrites *in place* every tool result whose call id is
// in callIDs into an abnormal placeholder, and returns the rewritten results.
// reason lands in Details.Reason; providerErr is quoted verbatim (empty when the
// caller has none). The remaining text of the original result is kept ahead of
// the placeholder text, so whatever the model can still use (a file descriptor, a
// prose answer) survives.
//
// A result that is already a placeholder is left alone (rewriting it again would
// only nest reasons), and an id that matches no result is ignored, so the caller
// can pass the delta without first filtering it.
func AbnormalizeToolResults(msgs MessageList, callIDs []string, providerErr, reason string) []ToolResultMessage {
	if len(callIDs) == 0 {
		return nil
	}
	want := make(map[string]bool, len(callIDs))
	for _, id := range callIDs {
		want[id] = true
	}
	var out []ToolResultMessage
	for i, m := range msgs {
		tr, ok := m.(ToolResultMessage)
		if !ok || !want[tr.ToolCallID] || IsAbnormalToolResult(tr) {
			continue
		}
		detail := AbnormalToolResult{
			Arguments:     argumentsFor(msgs, tr.ToolCallID),
			ProviderError: providerErr,
			Reason:        reason,
		}
		ph := NewAbnormalToolResult(tr.ToolCallID, tr.ToolName, detail, tr.Timestamp)
		ph.Content = append(append(ContentList(nil), tr.Content...), ph.Content...)
		msgs[i] = ph
		out = append(out, ph)
	}
	return out
}

// argumentsFor returns the verbatim arguments of the call with callID, or "".
func argumentsFor(msgs MessageList, callID string) string {
	call, ok := FindToolCall(msgs, callID)
	if !ok {
		return ""
	}
	return string(call.Arguments)
}
