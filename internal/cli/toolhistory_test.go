// Tests for the shared /rewind tool history core: pairing calls with results,
// pruning an assistant turn down to the rolled-back call, aligning the in-memory
// context with the on-disk session path, and the full rollback write.

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// toolTurn builds an assistant message that issues the given calls.
func toolTurn(calls ...agentcore.ToolCallContent) agentcore.AssistantMessage {
	content := make(agentcore.ContentList, len(calls))
	for i, c := range calls {
		content[i] = c
	}
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: content}
}

func call(id, name, args string) agentcore.ToolCallContent {
	return agentcore.NewToolCallContent(id, name, json.RawMessage(args))
}

func result(id, name, text string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: id, ToolName: name,
		Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}

// TestToolCallsPairsResults covers the listing: calls are reported in order with
// their result position, a call without a result is listed with ResultIndex -1,
// and a placeholder is flagged instead of being presented as real output.
func TestToolCallsPairsResults(t *testing.T) {
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		toolTurn(call("c1", "read", `{"path":"a.go"}`), call("c2", "bash", `{"command":"ls"}`)),
		result("c1", "read", "package main\n\nfunc main() {}"),
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c2", ToolName: "bash",
			Content: agentcore.ContentList{agentcore.NewTextContent(agentcore.AbnormalToolResult{
				Arguments: `{"command":"ls"}`, Reason: "rolled back",
			}.Text())}, Details: agentcore.AbnormalToolResult{Reason: "rolled back", Arguments: `{"command":"ls"}`}, IsError: true},
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		toolTurn(call("c3", "grep", `{}`)),
	}

	refs := ToolCalls(msgs)
	if len(refs) != 3 {
		t.Fatalf("ToolCalls = %d refs, want 3: %+v", len(refs), refs)
	}
	if r := refs[0]; r.CallID != "c1" || r.Name != "read" || r.AssistantIndex != 1 || r.CallIndex != 0 || r.ResultIndex != 2 || r.IsError || r.Abnormal {
		t.Errorf("c1 = %+v", r)
	}
	if r := refs[0]; r.ResultSummary != "package main" {
		t.Errorf("c1 summary = %q, want its first non-empty line", r.ResultSummary)
	}
	if r := refs[1]; r.CallID != "c2" || !r.Abnormal || !r.IsError || !strings.Contains(r.ResultSummary, "rolled back") {
		t.Errorf("c2 = %+v, want it flagged abnormal", r)
	}
	if r := refs[2]; r.CallID != "c3" || r.ResultIndex != -1 || r.AssistantIndex != 5 {
		t.Errorf("c3 = %+v, want an unanswered call at index 5", r)
	}
}

// TestSummarizeToolResultClip keeps a long or empty result scannable.
func TestSummarizeToolResultClip(t *testing.T) {
	long := result("c1", "read", strings.Repeat("x", 200))
	if got := summarizeToolResult(long); len([]rune(got)) > 80 || !strings.HasSuffix(got, "…") {
		t.Errorf("long summary = %q (len %d runes), want it clipped to 80 runes", got, len([]rune(got)))
	}
	if got := summarizeToolResult(result("c1", "read", "\n\n  \n")); got != "(no output)" {
		t.Errorf("empty summary = %q", got)
	}
}

// TestPruneAfterCall keeps the target call (and the text before it) and drops the
// calls that came after: in the rolled-back world they never ran.
func TestPruneAfterCall(t *testing.T) {
	turn := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse,
		Content: agentcore.ContentList{
			agentcore.NewTextContent("reading both files"),
			call("c1", "read", `{"path":"a"}`),
			call("c2", "read", `{"path":"b"}`),
		}}

	pruned, target, ok := pruneAfterCall(turn, "c1")
	if !ok || target.ID != "c1" {
		t.Fatalf("pruneAfterCall = %+v, %+v, %v", pruned, target, ok)
	}
	if got := len(pruned.ToolCalls()); got != 1 {
		t.Fatalf("pruned keeps %d calls, want 1: %+v", got, pruned.Content)
	}
	if text := agentcore.ContentToText(pruned.Content); !strings.Contains(text, "reading both files") {
		t.Errorf("pruning dropped the assistant's text: %+v", pruned.Content)
	}

	if _, _, ok := pruneAfterCall(turn, "nope"); ok {
		t.Error("an unknown call id was pruned")
	}
}

// TestAlignTail accepts a path that ends with the context, rejects a shorter path
// and a path whose tail does not match.
func TestAlignTail(t *testing.T) {
	user := agentcore.UserMessage{RoleField: agentcore.RoleUser}
	assistant := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
	path := []session.Entry{{ID: "1", Message: user}, {ID: "2", Message: assistant}}

	if got := alignTail(path, agentcore.MessageList{user, assistant}); len(got) != 2 || got[0].ID != "1" {
		t.Fatalf("alignTail(full) = %+v", got)
	}
	if got := alignTail(path, agentcore.MessageList{assistant}); len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("alignTail(tail) = %+v", got)
	}
	if alignTail(path, agentcore.MessageList{user, assistant, user}) != nil {
		t.Error("a context longer than the path was aligned")
	}
	if alignTail(path, agentcore.MessageList{assistant, user}) != nil {
		t.Error("a role-mismatched context was aligned")
	}
}

// testHost satisfies Host via embedding: only the members RollbackToolCall uses
// are overridden, so an unexpected call panics instead of silently passing.
type testHost struct {
	Host
	store     *session.Store
	header    session.SessionHeader
	ctx       *agentcore.AgentContext
	curLeaf   string
	persisted int
}

func (h *testHost) Store() *session.Store             { return h.store }
func (h *testHost) Header() session.SessionHeader     { return h.header }
func (h *testHost) AgentCtx() *agentcore.AgentContext { return h.ctx }
func (h *testHost) CurLeaf() string                   { return h.curLeaf }
func (h *testHost) SetCurLeaf(id string)              { h.curLeaf = id }
func (h *testHost) Persisted() int                    { return h.persisted }
func (h *testHost) SetPersisted(n int)                { h.persisted = n }

// newToolHistoryHost persists the given conversation and returns a host whose
// in-memory context and cursors match the on-disk tree.
func newToolHistoryHost(t *testing.T, msgs agentcore.MessageList) *testHost {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	header := session.SessionHeader{ID: session.NewID(time.Now()), CreatedAt: time.Now().UTC()}
	h := &testHost{store: store, header: header, ctx: &agentcore.AgentContext{Messages: msgs}}
	leaf, err := store.AppendBranch(header, "", msgs)
	if err != nil {
		t.Fatal(err)
	}
	h.curLeaf, h.persisted = leaf, len(msgs)
	return h
}

// TestRollbackToolCallBranches covers the happy path: the target call's result
// becomes an abnormal placeholder holding the arguments and the reason, everything
// after the call is dropped from the live context, the abandoned branch stays on
// disk, and a reload of the session reads back exactly the new branch.
func TestRollbackToolCallBranches(t *testing.T) {
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		toolTurn(call("c1", "read", `{"path":"a.go"}`)),
		result("c1", "read", "huge output the endpoint rejected"),
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("later turn")}},
	}
	h := newToolHistoryHost(t, msgs)
	refs := ToolCalls(msgs)
	if len(refs) != 1 {
		t.Fatalf("setup: %d calls, want 1", len(refs))
	}

	var out bytes.Buffer
	if !RollbackToolCall(&out, h, refs[0]) {
		t.Fatalf("rollback failed: %s", out.String())
	}

	if got := len(h.ctx.Messages); got != 3 {
		t.Fatalf("context has %d messages, want 3 (user, pruned call, placeholder): %+v", got, h.ctx.Messages)
	}
	if h.persisted != 3 || h.curLeaf == "" {
		t.Errorf("cursors not advanced: persisted=%d leaf=%q", h.persisted, h.curLeaf)
	}
	tr, ok := h.ctx.Messages[2].(agentcore.ToolResultMessage)
	if !ok || !agentcore.IsAbnormalToolResult(tr) {
		t.Fatalf("third message is not the placeholder: %+v", h.ctx.Messages[2])
	}
	detail, _ := agentcore.AbnormalDetailsOf(tr)
	if detail.Arguments != `{"path":"a.go"}` || !strings.Contains(detail.Reason, "rolled back") {
		t.Errorf("placeholder detail = %+v", detail)
	}

	// The file keeps both branches; the active one is the rolled-back path.
	_, entries, err := h.store.LoadEntries(h.header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2+5 {
		t.Errorf("session holds %d entries, want 7 (2 new + 5 abandoned)", len(entries))
	}
	_, active, err := h.store.LoadPath(h.header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 3 {
		t.Fatalf("active path has %d entries, want 3", len(active))
	}
	if _, ok := active[2].Message.(agentcore.ToolResultMessage); !ok {
		t.Errorf("active path tip = %T, want the placeholder", active[2].Message)
	}
}

// TestRollbackToolCallRejectsStaleRef: a ref whose message is gone (or whose call
// id is not in that turn) changes nothing and reports failure.
func TestRollbackToolCallRejectsStaleRef(t *testing.T) {
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		toolTurn(call("c1", "read", `{}`)),
		result("c1", "read", "out"),
	}

	t.Run("message index", func(t *testing.T) {
		h := newToolHistoryHost(t, msgs)
		var out bytes.Buffer
		if RollbackToolCall(&out, h, ToolCallRef{CallID: "c1", AssistantIndex: 99}) {
			t.Fatal("rollback accepted an out-of-range index")
		}
		if !strings.Contains(out.String(), "/rewind tool") {
			t.Errorf("message does not point at the listing: %q", out.String())
		}
		if h.persisted != len(msgs) {
			t.Errorf("cursors moved on a rejected rollback")
		}
	})

	t.Run("call id", func(t *testing.T) {
		h := newToolHistoryHost(t, msgs)
		var out bytes.Buffer
		if RollbackToolCall(&out, h, ToolCallRef{CallID: "ghost", AssistantIndex: 1}) {
			t.Fatal("rollback accepted a call id that is not in the turn")
		}
		if len(h.ctx.Messages) != len(msgs) {
			t.Errorf("context changed on a rejected rollback")
		}
	})
}

// TestRollbackToolCallRefusesUnalignedTree: when the in-memory context is not the
// tail of the saved path, the rollback must not guess where to branch.
func TestRollbackToolCallRefusesUnalignedTree(t *testing.T) {
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		toolTurn(call("c1", "read", `{}`)),
		result("c1", "read", "out"),
	}
	h := newToolHistoryHost(t, msgs)
	// The context is a different window of the conversation than the saved path's
	// tail (e.g. a session swapped under a stale host): same nodes, wrong order.
	h.ctx.Messages = agentcore.MessageList{msgs[2], msgs[1]}
	h.persisted = len(h.ctx.Messages)

	var out bytes.Buffer
	if RollbackToolCall(&out, h, ToolCallRef{CallID: "c1", AssistantIndex: 1}) {
		t.Fatal("rollback branched from an unaligned context")
	}
	if !strings.Contains(out.String(), "nothing was rolled back") {
		t.Errorf("missing refusal message: %q", out.String())
	}
}
