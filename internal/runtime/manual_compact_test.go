package runtime

// Tests for the manual compaction shared by /compact in the REPL and the TUI
// (manual_compact.go): it must compact in place exactly like the loop's automatic
// compaction, report the same accounting, persist the checkpoint that lets a
// later /rebuild reload the summary, and leave the caller's list untouched when
// there is nothing to fold or the summarizer fails.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/provider"
)

// manualCompactCfg builds a config whose summarizer is the fixed summaryStream
// fake and whose settings make a handful of fat messages worth compacting while
// keeping the tail verbatim.
func manualCompactCfg(summary provider.StreamFn) RunConfig {
	cfg := newRunCfg(nil)
	cfg.SummaryStream = summary
	cfg.ContextWindow = 2000
	cfg.Compaction = compaction.CompactionSettings{Enabled: true, ReserveTokens: 500, KeepRecentTokens: 100}
	return cfg
}

func TestCompactContext_CompactsAndWritesCheckpoint(t *testing.T) {
	root := t.TempDir()
	cfg := manualCompactCfg(summaryStream("## Goal\nmanual summary"))
	cfg.SessionID = "sess-manual"
	cfg.MemoryRoot = root

	msgs := bigUserMessages(12, 800)
	before := len(msgs)

	res, err := CompactContext(context.Background(), msgs, &cfg, "manual")
	if err != nil {
		t.Fatalf("CompactContext: %v", err)
	}
	if res.NoOp {
		t.Fatal("expected a real compaction, got NoOp")
	}
	// The rebuilt context is a summary plus a verbatim tail, and the caller's list
	// is left as it was (installing it is the front-end's job).
	if len(res.Messages) >= before {
		t.Errorf("compacted context has %d messages, want fewer than %d", len(res.Messages), before)
	}
	if len(msgs) != before {
		t.Errorf("CompactContext must not mutate the caller's list: %d → %d", before, len(msgs))
	}
	if got := res.Messages[0].Role(); got != agentcore.RoleCompaction {
		t.Errorf("first compacted message role = %q, want %q", got, agentcore.RoleCompaction)
	}

	ev := res.Event
	if ev.Reason != "manual" {
		t.Errorf("Reason = %q, want %q (the PreCompact manual trigger)", ev.Reason, "manual")
	}
	if !strings.Contains(ev.Summary, "manual summary") {
		t.Errorf("event summary should carry the summarizer output, got %q", ev.Summary)
	}
	if ev.TokensAfter >= ev.TokensBefore {
		t.Errorf("compaction should shrink the estimate: before=%d after=%d", ev.TokensBefore, ev.TokensAfter)
	}
	if ev.KeptCount != len(res.Messages)-1 {
		t.Errorf("KeptCount = %d, want %d (everything after the summary)", ev.KeptCount, len(res.Messages)-1)
	}
	if ev.SummarizedCount != before-ev.KeptCount {
		t.Errorf("SummarizedCount = %d, want %d", ev.SummarizedCount, before-ev.KeptCount)
	}

	// The summary is persisted as the session checkpoint, which is what makes a
	// later /rebuild or resume reload it instead of re-summarizing.
	cp, ok, err := LoadCheckpoint("sess-manual", root)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if !ok || cp == nil {
		t.Fatal("a manual compaction must persist a checkpoint")
	}
	if cp.Watermark != ev.SummarizedCount {
		t.Errorf("checkpoint watermark = %d, want %d", cp.Watermark, ev.SummarizedCount)
	}
	if !strings.Contains(cp.Summary, "manual summary") {
		t.Errorf("checkpoint summary = %q, want the summarizer output", cp.Summary)
	}
}

func TestCompactContext_NothingToCompactIsNoOp(t *testing.T) {
	cfg := manualCompactCfg(summaryStream("unused"))
	cfg.SessionID = "sess-noop"
	cfg.MemoryRoot = t.TempDir()

	msgs := agentcore.MessageList{textUser("hi")}

	res, err := CompactContext(context.Background(), msgs, &cfg, "manual")
	if err != nil {
		t.Fatalf("CompactContext: %v", err)
	}
	if !res.NoOp {
		t.Fatal("a single short message has nothing to fold; want NoOp")
	}
	if len(res.Messages) != len(msgs) {
		t.Errorf("no-op must return the context unchanged, got %d messages", len(res.Messages))
	}
	if res.Event.TokensAfter != res.Event.TokensBefore {
		t.Errorf("no-op must report unchanged size: before=%d after=%d", res.Event.TokensBefore, res.Event.TokensAfter)
	}
	if res.Event.Summary != "" {
		t.Errorf("no-op must not carry a summary, got %q", res.Event.Summary)
	}
	// Nothing was summarized, so nothing may be written: a checkpoint here would
	// claim the history was folded when it was not.
	if _, ok, _ := LoadCheckpoint("sess-noop", cfg.MemoryRoot); ok {
		t.Error("a no-op compaction must not write a checkpoint")
	}
}

func TestCompactContext_ErrorLeavesContextAlone(t *testing.T) {
	cfg := manualCompactCfg(func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return nil, errors.New("summarizer down")
	})
	cfg.SessionID = "sess-err"
	cfg.MemoryRoot = t.TempDir()

	msgs := bigUserMessages(12, 800)

	res, err := CompactContext(context.Background(), msgs, &cfg, "manual")
	if err == nil {
		t.Fatalf("expected the summarizer failure, got result %+v", res)
	}
	if res != nil {
		t.Errorf("a failed compaction must return no result, got %+v", res)
	}
	if len(msgs) != 12 {
		t.Errorf("a failed compaction must leave the caller's list untouched: %d messages", len(msgs))
	}
	if _, ok, _ := LoadCheckpoint("sess-err", cfg.MemoryRoot); ok {
		t.Error("a failed compaction must not write a checkpoint")
	}
}

func TestCompactContext_ContinuesPriorCheckpoint(t *testing.T) {
	cfg := manualCompactCfg(summaryStream("## Goal\nrolling"))
	cfg.SessionID = "sess-roll"
	cfg.MemoryRoot = t.TempDir()

	msgs := bigUserMessages(12, 800)
	first, err := CompactContext(context.Background(), msgs, &cfg, "manual")
	if err != nil {
		t.Fatalf("first CompactContext: %v", err)
	}

	// Grow the compacted context and compact again: the existing checkpoint must be
	// consolidated (one summary), not treated as ordinary history to re-summarize.
	grown := append(agentcore.MessageList{}, first.Messages...)
	grown = append(grown, bigUserMessages(8, 800)...)

	second, err := CompactContext(context.Background(), grown, &cfg, "manual")
	if err != nil {
		t.Fatalf("second CompactContext: %v", err)
	}
	if second.NoOp {
		t.Fatal("expected the grown context to compact")
	}
	checkpoints := 0
	for _, m := range second.Messages {
		if m.Role() == agentcore.RoleCompaction {
			checkpoints++
		}
	}
	if checkpoints != 1 {
		t.Errorf("compacted context carries %d checkpoints, want exactly 1", checkpoints)
	}
}
