package runtime

// Tests for sub-agent session persistence: a child run is a complete agent whose
// transcript is saved as its own session, parented to the dispatching
// conversation, and the parent continues that same conversation by passing the
// reported session id. Direct resumption of a child session is a session-layer
// concern (see internal/session); here we prove the tool writes and reopens it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/session"
)

// newSessionedTask builds the generic task tool over a scripted child provider and
// returns it with a context that carries the given parent session id.
func newSessionedTask(t *testing.T, turns ...fauxTurn) (*SubAgentTool, context.Context) {
	t.Helper()
	t.Setenv("PIGO_HOME", t.TempDir())
	child := &fauxProvider{
		name:   "faux-child",
		models: []provider.Model{{Provider: "faux-child", ID: "child"}},
		turns:  turns,
	}
	factory := func() RunConfig {
		return RunConfig{
			LoopConfig: LoopConfig{Model: "child", Stream: provider.StreamFnFromProvider(child)},
			Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
		}
	}
	return NewTaskTool(factory, nil, nil), agentcore.WithSessionID(context.Background(), "parent-session-1")
}

// runSubAgent executes the tool with args, failing on a Go error.
func runSubAgent(t *testing.T, tool *SubAgentTool, ctx context.Context, args string) agentcore.AgentToolResult {
	t.Helper()
	res, err := tool.Execute(ctx, "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return res
}

// TestSubAgentSessionPersistedAndContinued verifies a child run is saved as its
// own session (parented to the caller, marked as a sub-agent) and that passing the
// reported id continues it instead of starting from a fresh context.
func TestSubAgentSessionPersistedAndContinued(t *testing.T) {
	tool, ctx := newSessionedTask(t, textTurn("first report"), textTurn("second report"))

	res := runSubAgent(t, tool, ctx, `{"prompt":"do the first half"}`)
	text := agentcore.ContentToText(res.Content)
	details, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("result details = %#v, want a map with the session id", res.Details)
	}
	sessionID, _ := details["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("result details missing sessionId")
	}
	if !strings.Contains(text, "[sub-agent session: "+sessionID+"]") {
		t.Errorf("result text should report the session id for continuation:\n%s", text)
	}

	store, err := session.OpenDefaultStore()
	if err != nil {
		t.Fatalf("OpenDefaultStore: %v", err)
	}
	header, entries, err := store.LoadEntries(sessionID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if header.SubAgent != "task" {
		t.Errorf("header.SubAgent = %q, want task", header.SubAgent)
	}
	if header.ParentSession != "parent-session-1" {
		t.Errorf("header.ParentSession = %q, want the calling conversation", header.ParentSession)
	}
	if len(entries) != 2 {
		t.Fatalf("child session has %d entries, want user+assistant", len(entries))
	}

	// Continue the same conversation: the child keeps its transcript and appends.
	res = runSubAgent(t, tool, ctx, `{"prompt":"do the second half","session_id":"`+sessionID+`"}`)
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "second report") {
		t.Errorf("continuation result = %q, want the second report", got)
	}
	_, entries, err = store.LoadEntries(sessionID)
	if err != nil {
		t.Fatalf("LoadEntries after continue: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("child session has %d entries after continuation, want 4", len(entries))
	}
}

// TestSubAgentSessionContinueValidation verifies continuation refuses a session
// that is not a sub-agent session, belongs to another conversation, or belongs to
// another agent: a child must not be able to read an unrelated conversation.
func TestSubAgentSessionContinueValidation(t *testing.T) {
	tool, ctx := newSessionedTask(t, textTurn("r1"), textTurn("r2"), textTurn("r3"))

	res := runSubAgent(t, tool, ctx, `{"prompt":"work"}`)
	details := res.Details.(map[string]any)
	sessionID := details["sessionId"].(string)

	// Wrong parent conversation.
	otherParent := agentcore.WithSessionID(context.Background(), "parent-session-2")
	if _, err := tool.Execute(otherParent, "c", json.RawMessage(`{"prompt":"x","session_id":"`+sessionID+`"}`), nil); err == nil {
		t.Error("continuing a session from another conversation must fail")
	} else if !strings.Contains(err.Error(), "another conversation") {
		t.Errorf("error = %v, want a wrong-conversation error", err)
	}

	// Wrong agent name.
	child := &fauxProvider{name: "faux", models: []provider.Model{{Provider: "faux", ID: "m"}}, turns: []fauxTurn{textTurn("x")}}
	otherAgent := NewSubAgentTool(SubAgentSpec{
		Name:         "researcher",
		SystemPrompt: "research",
		NewRunConfig: func() RunConfig {
			return RunConfig{
				LoopConfig: LoopConfig{Model: "m", Stream: provider.StreamFnFromProvider(child)},
				Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
			}
		},
	})
	if _, err := otherAgent.Execute(ctx, "c", json.RawMessage(`{"prompt":"x","session_id":"`+sessionID+`"}`), nil); err == nil {
		t.Error("continuing another agent's session must fail")
	} else if !strings.Contains(err.Error(), `belongs to sub-agent "task"`) {
		t.Errorf("error = %v, want an agent-mismatch error", err)
	}

	// A top-level session is not continuable by an agent.
	store, err := session.OpenDefaultStore()
	if err != nil {
		t.Fatalf("OpenDefaultStore: %v", err)
	}
	top := session.SessionHeader{ID: "20260101-000000-000001"}
	if err := store.Save(top, agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := tool.Execute(ctx, "c", json.RawMessage(`{"prompt":"x","session_id":"20260101-000000-000001"}`), nil); err == nil {
		t.Error("continuing a top-level session must fail")
	} else if !strings.Contains(err.Error(), "not a sub-agent session") {
		t.Errorf("error = %v, want a not-a-subagent-session error", err)
	}
}

// TestSubAgentWithoutParentSessionIsNotPersisted verifies a run with no backing
// session (SDK / no persistence) still works and reports no session id.
func TestSubAgentWithoutParentSessionIsNotPersisted(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	child := &fauxProvider{name: "faux", models: []provider.Model{{Provider: "faux", ID: "m"}}, turns: []fauxTurn{textTurn("report")}}
	tool := NewTaskTool(func() RunConfig {
		return RunConfig{
			LoopConfig: LoopConfig{Model: "m", Stream: provider.StreamFnFromProvider(child)},
			Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
		}
	}, nil, nil)

	res := runSubAgent(t, tool, context.Background(), `{"prompt":"work"}`)
	if got := agentcore.ContentToText(res.Content); got != "report" {
		t.Errorf("result = %q, want the child report", got)
	}
	if res.Details != nil {
		t.Errorf("details = %#v, want none without a parent session", res.Details)
	}
	store, err := session.OpenDefaultStore()
	if err != nil {
		t.Fatalf("OpenDefaultStore: %v", err)
	}
	headers, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(headers) != 0 {
		t.Errorf("session store has %d sessions, want none", len(headers))
	}
}

// TestOpenSubAgentSessionContinuesActiveBranch verifies a continued child resumes
// the branch it actually descends from. A child that was rewound holds abandoned
// sibling turns in its file; passing the whole file to the model would replay
// turns the conversation no longer continues from.
func TestOpenSubAgentSessionContinuesActiveBranch(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	store, err := session.OpenDefaultStore()
	if err != nil {
		t.Fatalf("OpenDefaultStore: %v", err)
	}
	header := session.SessionHeader{ID: "20260101-000000-000042", ParentSession: "parent-session-1", SubAgent: "task"}
	if _, err := store.AppendBranch(header, "", agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("start")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("abandoned answer")}},
	}); err != nil {
		t.Fatalf("AppendBranch: %v", err)
	}
	_, entries, err := store.LoadEntries(header.ID)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	// A replacement branch from the root: the original assistant turn becomes an
	// abandoned sibling, exactly like a /tree leaf switch.
	leaf, err := store.AppendBranch(header, entries[0].ID, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("corrective prompt")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("kept answer")}},
	})
	if err != nil {
		t.Fatalf("AppendBranch alt: %v", err)
	}

	child, msgs, err := openSubAgentSession("parent-session-1", header.ID, "task", "child-model", "faux", "sys")
	if err != nil {
		t.Fatalf("openSubAgentSession: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("continued child sees %d messages, want 3 (root + replacement branch)", len(msgs))
	}
	for _, m := range msgs {
		switch msg := m.(type) {
		case agentcore.UserMessage:
			if strings.Contains(agentcore.ContentToText(msg.Content), "abandoned answer") {
				t.Fatalf("continued child replayed an abandoned branch: %+v", msgs)
			}
		case agentcore.AssistantMessage:
			if strings.Contains(agentcore.ContentToText(msg.Content), "abandoned answer") {
				t.Fatalf("continued child replayed an abandoned branch: %+v", msgs)
			}
		}
	}
	if child.curLeaf != leaf {
		t.Errorf("curLeaf = %q, want the active tip %q", child.curLeaf, leaf)
	}
}
