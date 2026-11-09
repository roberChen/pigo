// This file implements the fine-grained history rollback behind the front-ends'
// /rewind tool command: listing the tool calls a conversation contains, and
// rewriting the session so one call's output is replaced by an abnormal
// placeholder (agentcore/abnormal.go) with everything after it dropped.
//
// Why a placeholder instead of deleting the call: a conversation is only valid
// while every tool call has a matching result, and the assistant turn that asked
// for the call stays in history — removing only the result would leave it
// dangling and make every later request invalid. The placeholder keeps the
// pairing, records the call's original arguments and why the output is gone, and
// tells the model it may re-issue the call.
//
// Why a branch instead of a rewrite: the session is an append-only entry tree
// (session package), so the abandoned branch stays on disk and /tree can return
// to it. The rollback appends the pruned assistant turn and the placeholder under
// the assistant turn's parent, then points the session's active leaf at the new
// tip. It follows the same tail-alignment rule PersistTurn uses: the in-memory
// context is the tail of the on-disk path, because a compaction checkpoint is
// attached at the last summarized entry rather than at the file's head.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// ToolCallRef describes one assistant tool call paired with its result, as the
// /rewind tool listing presents it and as RollbackToolCall addresses it.
//
// AssistantIndex is the position in the conversation where the call was issued;
// the rollback keeps everything up to (and including) the pruned form of that
// message. ResultIndex is -1 when the call never got a result (a cancelled or
// aborted turn), which is still a valid rollback point.
type ToolCallRef struct {
	CallID         string
	Name           string
	Arguments      json.RawMessage
	AssistantIndex int
	CallIndex      int
	ResultIndex    int
	ResultSummary  string
	IsError        bool
	// Abnormal marks a result that is already an abnormal placeholder, so the
	// listing can say so instead of presenting it as real output.
	Abnormal bool
}

// ToolCalls walks msgs and returns every assistant tool call in conversation
// order, paired with its result by call id. Calls without a result are included
// (ResultIndex -1): they are exactly the ones a user most wants to roll back.
func ToolCalls(msgs agentcore.MessageList) []ToolCallRef {
	results := make(map[string]agentcore.ToolResultMessage, len(msgs))
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			results[tr.ToolCallID] = tr
		}
	}
	var out []ToolCallRef
	for i, m := range msgs {
		a, ok := m.(agentcore.AssistantMessage)
		if !ok {
			continue
		}
		for j, call := range a.ToolCalls() {
			ref := ToolCallRef{
				CallID:         call.ID,
				Name:           call.Name,
				Arguments:      call.Arguments,
				AssistantIndex: i,
				CallIndex:      j,
				ResultIndex:    -1,
			}
			if tr, ok := results[call.ID]; ok {
				ref.ResultIndex = messageIndex(msgs, tr)
				ref.ResultSummary = summarizeToolResult(tr)
				ref.IsError = tr.IsError
				ref.Abnormal = agentcore.IsAbnormalToolResult(tr)
			}
			out = append(out, ref)
		}
	}
	return out
}

// messageIndex returns the position of the tool result with tr's call id in msgs,
// or -1. Two results for one call id cannot occur in a valid conversation, so the
// first match is the right one.
func messageIndex(msgs agentcore.MessageList, tr agentcore.ToolResultMessage) int {
	for i, m := range msgs {
		if other, ok := m.(agentcore.ToolResultMessage); ok && other.ToolCallID == tr.ToolCallID {
			return i
		}
	}
	return -1
}

// summarizeToolResult reduces a result to one line for the listing: the first
// non-empty line of its text, clipped. A placeholder is summarized by its own
// marker so the listing does not present rolled-back output as real.
func summarizeToolResult(tr agentcore.ToolResultMessage) string {
	text := strings.TrimSpace(agentcore.ContentToText(tr.Content))
	if agentcore.IsAbnormalToolResult(tr) {
		if d, ok := agentcore.AbnormalDetailsOf(tr); ok && d.Reason != "" {
			return "abnormal: " + d.Reason
		}
		return "abnormal placeholder"
	}
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return clipRunes(line, 80)
		}
	}
	if text == "" {
		return "(no output)"
	}
	return text
}

// clipRunes truncates s to at most max runes, ending with an ellipsis when it had
// to cut. It counts runes, not bytes, so a multi-byte line is not split mid-glyph.
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// RollbackToolCall rewrites the session so ref's call result becomes an abnormal
// placeholder and everything after the call is dropped. The abandoned turns stay
// in the session file as a sibling branch (session tree), reachable with /tree.
//
// It persists any unsaved turn first, so the branch is written against an
// up-to-date tree, and reports failure (without modifying the conversation) when
// the on-disk tree cannot be aligned with the in-memory context.
func RollbackToolCall(out io.Writer, h Host, ref ToolCallRef) bool {
	msgs := h.AgentCtx().Messages
	if ref.AssistantIndex < 0 || ref.AssistantIndex >= len(msgs) {
		fmt.Fprintln(out, "pigo: that tool call is no longer in the conversation — run /rewind tool to list calls")
		return false
	}
	assistant, ok := msgs[ref.AssistantIndex].(agentcore.AssistantMessage)
	if !ok {
		fmt.Fprintln(out, "pigo: that conversation node is not an assistant turn — run /rewind tool to list calls")
		return false
	}

	// Flush the live turn so the entry we branch from is already on disk. After
	// this, the context is still the tail of the session path.
	PersistTurn(out, h)
	msgs = h.AgentCtx().Messages

	pruned, call, ok := pruneAfterCall(assistant, ref.CallID)
	if !ok {
		fmt.Fprintf(out, "pigo: tool call %s is not in message %d\n", ref.CallID, ref.AssistantIndex)
		return false
	}

	_, entries, err := h.Store().LoadEntries(h.Header().ID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "pigo: cannot read session tree: %v\n", err)
		return false
	}
	aligned := alignTail(session.PathToLeaf(entries, h.CurLeaf()), msgs)
	if aligned == nil {
		fmt.Fprintln(out, "pigo: the in-memory conversation does not match the saved session tree; nothing was rolled back")
		return false
	}
	parent := aligned[ref.AssistantIndex].ParentID

	header := h.Header()
	header.UpdatedAt = time.Now().UTC()
	placeholder := agentcore.NewAbnormalToolResult(call.ID, call.Name, agentcore.AbnormalToolResult{
		Arguments: string(call.Arguments),
		Reason:    "the user rolled back this tool call, so its output is not available",
	}, time.Now().UnixMilli())
	leaf, err := h.Store().AppendBranch(header, parent, agentcore.MessageList{pruned, placeholder})
	if err != nil {
		fmt.Fprintf(out, "pigo: rollback save failed: %v\n", err)
		return false
	}

	newCtx := make(agentcore.MessageList, 0, ref.AssistantIndex+2)
	newCtx = append(newCtx, msgs[:ref.AssistantIndex]...)
	newCtx = append(newCtx, pruned, placeholder)
	h.AgentCtx().Messages = newCtx
	h.SetCurLeaf(leaf)
	h.SetPersisted(len(newCtx))
	return true
}

// pruneAfterCall returns assistant with its tool calls cut off after callID (the
// target call stays; later calls never ran in the rolled-back world), plus the
// target call itself. ok is false when callID is not one of the message's calls,
// which keeps a stale listing from rewriting the wrong turn.
func pruneAfterCall(assistant agentcore.AssistantMessage, callID string) (agentcore.AssistantMessage, agentcore.ToolCallContent, bool) {
	calls := assistant.ToolCalls()
	keep := -1
	for i, c := range calls {
		if c.ID == callID {
			keep = i
			break
		}
	}
	if keep < 0 {
		return assistant, agentcore.ToolCallContent{}, false
	}
	kept := make(map[string]bool, keep+1)
	for _, c := range calls[:keep+1] {
		kept[c.ID] = true
	}
	// Keep every non-call block (text, thinking) and the calls up to the target, in
	// their original order.
	out := make(agentcore.ContentList, 0, len(assistant.Content))
	for _, c := range assistant.Content {
		tc, isCall := c.(agentcore.ToolCallContent)
		if isCall && !kept[tc.ID] {
			continue
		}
		out = append(out, c)
	}
	assistant.Content = out
	return assistant, calls[keep], true
}

// alignTail returns the entries that correspond position by position to msgs, or
// nil when the path is shorter than the context or does not end with it. The
// context is expected to be the tail of the active path (see the file comment):
// a compaction checkpoint is attached at the last summarized entry, so the path
// starts with the summarized history the context no longer carries.
func alignTail(path []session.Entry, msgs agentcore.MessageList) []session.Entry {
	if len(path) < len(msgs) {
		return nil
	}
	tail := path[len(path)-len(msgs):]
	for i, e := range tail {
		if e.Message.Role() != msgs[i].Role() {
			return nil
		}
	}
	return tail
}
