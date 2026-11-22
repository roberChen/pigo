package repl

// Tests for /compact in the REPL. The flow itself is runtime.CompactContext
// (shared with the TUI); what is REPL-specific is that the shared context is
// replaced in place and the report goes to stdout.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
)

// fatHistory fills a history past KeepRecentTokens (20k) with fat user messages,
// so there is a prefix worth summarizing and a tail worth keeping.
func fatHistory(n, chars int) agentcore.MessageList {
	body := strings.Repeat("x", chars)
	msgs := make(agentcore.MessageList, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(body)},
		})
	}
	return msgs
}

// TestManualCompactReplacesContext verifies /compact summarizes through the live
// provider, swaps the context for checkpoint + retained tail, and prints the
// before/after accounting.
func TestManualCompactReplacesContext(t *testing.T) {
	p := &replProvider{reply: "## Goal\nfolded by the summarizer"}
	deps, _ := newTestDeps(t, p)
	deps.agentCtx.Messages = fatHistory(40, 4000)

	var out bytes.Buffer
	runManualCompact(&out, deps)

	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (the summarization)", p.calls)
	}
	msgs := deps.agentCtx.Messages
	if len(msgs) == 0 || msgs[0].Role() != agentcore.RoleCompaction {
		t.Fatalf("context should start at the checkpoint, got %d messages (first role %q)", len(msgs), msgs[0].Role())
	}
	if len(msgs) >= 40 {
		t.Errorf("context should shrink, still %d messages", len(msgs))
	}
	cm, ok := msgs[0].(agentcore.CompactionMessage)
	if !ok {
		t.Fatalf("context[0] = %T, want CompactionMessage", msgs[0])
	}
	if !strings.Contains(cm.Summary, "folded by the summarizer") {
		t.Errorf("checkpoint summary = %q, want the summarizer output", cm.Summary)
	}
	got := out.String()
	if !strings.Contains(got, "Compacting conversation…") {
		t.Errorf("output should announce the compaction:\n%s", got)
	}
	if !strings.Contains(got, "compacted:") {
		t.Errorf("output should carry the accounting:\n%s", got)
	}
}

// TestManualCompactNothingToDo verifies a short history reports a no-op and leaves
// the context alone instead of inventing a checkpoint.
func TestManualCompactNothingToDo(t *testing.T) {
	p := &replProvider{reply: "unused"}
	deps, _ := newTestDeps(t, p)
	deps.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
	}

	var out bytes.Buffer
	runManualCompact(&out, deps)

	if p.calls != 0 {
		t.Errorf("provider calls = %d, want 0 (nothing to summarize)", p.calls)
	}
	if len(deps.agentCtx.Messages) != 1 || deps.agentCtx.Messages[0].Role() != agentcore.RoleUser {
		t.Errorf("a no-op must leave the context alone, got %+v", deps.agentCtx.Messages)
	}
	if !strings.Contains(out.String(), "nothing to compact") {
		t.Errorf("output should say nothing was compacted:\n%s", out.String())
	}
}
