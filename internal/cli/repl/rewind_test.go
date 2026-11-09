// Tests for the /rewind command wiring: listing restore points and restoring
// files + conversation. The file-snapshot journal itself is tested in
// agenttool; here we exercise runRewind's REPL-level behavior over a replDeps.
package repl

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
)

// /rewind with no argument lists the committed restore points.
func TestREPLRewindListsPoints(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, _ := newTestDeps(t, p)
	deps.snap = agenttool.NewFileSnapshotRecorder()

	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.snap.Record(f)
	deps.snap.Commit("", "add feature X")

	var out bytes.Buffer
	runRewind(&out, &deps, "/rewind")
	got := out.String()
	if !strings.Contains(got, "restore points") || !strings.Contains(got, "add feature X") {
		t.Errorf("listing missing expected content:\n%s", got)
	}
}

// /rewind N restores the file to its baseline and resets the conversation when
// the point's leaf is empty (it was the session's first turn).
func TestREPLRewindRestoresFiles(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, _ := newTestDeps(t, p)
	deps.snap = agenttool.NewFileSnapshotRecorder()
	deps.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}
	deps.persisted = 1

	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.snap.Record(f) // baseline "original"
	if err := os.WriteFile(f, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.snap.Commit("", "edit a.txt")

	var out bytes.Buffer
	runRewind(&out, &deps, "/rewind 1")

	if data, _ := os.ReadFile(f); string(data) != "original" {
		t.Errorf("file not restored: got %q, want original", string(data))
	}
	if len(deps.agentCtx.Messages) != 0 {
		t.Errorf("conversation not reset: %d messages remain", len(deps.agentCtx.Messages))
	}
	if len(deps.snap.Points()) != 0 {
		t.Errorf("journal not truncated after rewind")
	}
	if !strings.Contains(out.String(), "rewound to before point 1") {
		t.Errorf("missing confirmation:\n%s", out.String())
	}
}

// /rewind is unavailable when file tools are disabled (nil recorder).
func TestREPLRewindDisabled(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, _ := newTestDeps(t, p)
	deps.snap = nil

	var out bytes.Buffer
	runRewind(&out, &deps, "/rewind")
	if !strings.Contains(out.String(), "unavailable") {
		t.Errorf("want unavailable message, got:\n%s", out.String())
	}
}

// /rewind tool lists the conversation's tool calls without needing snapshots.
func TestREPLRewindToolLists(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, _ := newTestDeps(t, p)
	deps.snap = nil // no file snapshots: the tool listing does not need them
	deps.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{
			agentcore.NewToolCallContent("c1", "read", json.RawMessage(`{"path":"a.go"}`)),
			agentcore.NewToolCallContent("c2", "bash", json.RawMessage(`{"command":"ls"}`)),
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c1", ToolName: "read",
			Content: agentcore.ContentList{agentcore.NewTextContent("package main")}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c2", ToolName: "bash",
			Content: agentcore.ContentList{agentcore.NewTextContent("boom")}, IsError: true},
	}

	var out bytes.Buffer
	runRewind(&out, &deps, "/rewind tool")
	got := out.String()
	for _, want := range []string{"tool calls", "1. read", "package main", "2. bash", "[error]"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing %q:\n%s", want, got)
		}
	}
}

// /rewind tool N replaces that call's output and branches the conversation.
func TestREPLRewindToolRollsBack(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, _ := newTestDeps(t, p)
	deps.snap = nil
	deps.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{
			agentcore.NewToolCallContent("c1", "read", json.RawMessage(`{"path":"a.go"}`)),
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c1", ToolName: "read",
			Content: agentcore.ContentList{agentcore.NewTextContent("the output that broke the request")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("later turn")}},
	}
	// persisted stays 0: RollbackToolCall flushes the unsaved turn itself, which is
	// what makes the branch point appear on disk.

	var out bytes.Buffer
	runRewind(&out, &deps, "/rewind tool 1")

	if !strings.Contains(out.String(), "rolled back read") {
		t.Fatalf("missing confirmation:\n%s", out.String())
	}
	if got := len(deps.agentCtx.Messages); got != 3 {
		t.Fatalf("context has %d messages, want 3 (the later turn is dropped)", got)
	}
	tr, ok := deps.agentCtx.Messages[2].(agentcore.ToolResultMessage)
	if !ok || !agentcore.IsAbnormalToolResult(tr) {
		t.Fatalf("third message is not the placeholder: %+v", deps.agentCtx.Messages[2])
	}
	if deps.persisted != 3 || deps.curLeaf == "" {
		t.Errorf("cursors not advanced: persisted=%d leaf=%q", deps.persisted, deps.curLeaf)
	}
}

// Invalid or absent selections are reported, not applied.
func TestREPLRewindToolRejectsBadSelection(t *testing.T) {
	p := &replProvider{reply: "hi"}
	deps, _ := newTestDeps(t, p)
	deps.snap = nil

	var out bytes.Buffer
	runRewind(&out, &deps, "/rewind tool 1")
	if !strings.Contains(out.String(), "no tool calls") {
		t.Errorf("want the empty listing, got:\n%s", out.String())
	}

	deps.agentCtx.Messages = agentcore.MessageList{
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{
			agentcore.NewToolCallContent("c1", "read", json.RawMessage(`{}`)),
		}},
	}
	out.Reset()
	runRewind(&out, &deps, "/rewind tool 9")
	if !strings.Contains(out.String(), "invalid selection") {
		t.Errorf("want an invalid-selection message, got:\n%s", out.String())
	}
	if len(deps.agentCtx.Messages) != 1 {
		t.Errorf("a rejected selection modified the conversation")
	}
}
