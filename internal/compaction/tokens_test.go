package compaction

import (
	"encoding/json"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
)

func userMsg(text string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

func assistantMsg(text string, usage *agentcore.Usage, stop string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		Usage:      usage,
		StopReason: stop,
	}
}

func TestEstimateTokensText(t *testing.T) {
	// 8 chars / 4 = 2 tokens.
	if got := EstimateTokens(userMsg("abcdefgh")); got != 2 {
		t.Fatalf("text estimate: got %d, want 2", got)
	}
	// 9 chars -> ceil(9/4) = 3.
	if got := EstimateTokens(userMsg("abcdefghi")); got != 3 {
		t.Fatalf("ceil estimate: got %d, want 3", got)
	}
	// empty -> 0.
	if got := EstimateTokens(userMsg("")); got != 0 {
		t.Fatalf("empty estimate: got %d, want 0", got)
	}
}

func TestEstimateTokensImage(t *testing.T) {
	m := agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewImageContent("data", "image/png")},
	}
	// estimatedImageChars (4800) / 4 = 1200.
	if got := EstimateTokens(m); got != 1200 {
		t.Fatalf("image estimate: got %d, want 1200", got)
	}
}

func TestEstimateTokensAssistantToolCall(t *testing.T) {
	args := json.RawMessage(`{"path":"a.go"}`) // 15 chars
	m := agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content: agentcore.ContentList{
			agentcore.NewToolCallContent("id1", "read", args), // name "read" = 4 chars
		},
	}
	// (4 + 15) / 4 = ceil(19/4) = 5.
	if got := EstimateTokens(m); got != 5 {
		t.Fatalf("toolcall estimate: got %d, want 5", got)
	}
}

func TestEstimateTokensUnknownRoleZero(t *testing.T) {
	if got := EstimateTokens(agentcore.ToolResultMessage{}); got != 0 {
		t.Fatalf("empty tool result: got %d, want 0", got)
	}
}

func TestEstimateContextTokensNoUsageFallsBackToEstimate(t *testing.T) {
	msgs := []agentcore.Message{
		userMsg("abcdefgh"),                   // 2
		assistantMsg("abcd", nil, "end_turn"), // 1
	}
	est := EstimateContextTokens(msgs)
	if est.LastUsageIndex != -1 {
		t.Fatalf("LastUsageIndex: got %d, want -1", est.LastUsageIndex)
	}
	if est.Tokens != 3 {
		t.Fatalf("Tokens: got %d, want 3", est.Tokens)
	}
	if est.TrailingTokens != 3 || est.UsageTokens != 0 {
		t.Fatalf("trailing/usage: got %d/%d, want 3/0", est.TrailingTokens, est.UsageTokens)
	}
}

func TestEstimateContextTokensPrefersUsage(t *testing.T) {
	usage := &agentcore.Usage{InputTokens: 1000, OutputTokens: 200}
	msgs := []agentcore.Message{
		userMsg("first"),
		assistantMsg("reply", usage, "end_turn"),
		userMsg("abcdefgh"), // trailing: 2 tokens
	}
	est := EstimateContextTokens(msgs)
	if est.LastUsageIndex != 1 {
		t.Fatalf("LastUsageIndex: got %d, want 1", est.LastUsageIndex)
	}
	if est.UsageTokens != 1200 {
		t.Fatalf("UsageTokens: got %d, want 1200", est.UsageTokens)
	}
	if est.TrailingTokens != 2 {
		t.Fatalf("TrailingTokens: got %d, want 2", est.TrailingTokens)
	}
	if est.Tokens != 1202 {
		t.Fatalf("Tokens: got %d, want 1202", est.Tokens)
	}
}

func TestEstimateContextTokensSkipsAbortedAndErrorUsage(t *testing.T) {
	good := &agentcore.Usage{InputTokens: 500, OutputTokens: 0}
	bad := &agentcore.Usage{InputTokens: 9999, OutputTokens: 0}
	msgs := []agentcore.Message{
		assistantMsg("ok", good, "end_turn"),
		assistantMsg("aborted", bad, agentcore.StopReasonAborted),
		assistantMsg("errored", bad, agentcore.StopReasonError),
	}
	est := EstimateContextTokens(msgs)
	if est.LastUsageIndex != 0 {
		t.Fatalf("LastUsageIndex: got %d, want 0 (should skip aborted/error)", est.LastUsageIndex)
	}
	if est.UsageTokens != 500 {
		t.Fatalf("UsageTokens: got %d, want 500", est.UsageTokens)
	}
}

func TestEstimateContextTokensSkipsZeroUsage(t *testing.T) {
	zero := &agentcore.Usage{InputTokens: 0, OutputTokens: 0}
	msgs := []agentcore.Message{
		userMsg("abcdefgh"), // 2
		assistantMsg("reply", zero, "end_turn"),
	}
	est := EstimateContextTokens(msgs)
	// zero usage is ignored, so falls back to full estimation.
	if est.LastUsageIndex != -1 {
		t.Fatalf("LastUsageIndex: got %d, want -1", est.LastUsageIndex)
	}
}

// TestEstimatePostCompactionTokensIgnoresStaleUsage pins the fix for a /compact
// that reported "90,502 → 90,500 tokens". The retained tail was produced before
// the cut, so its usage block measures a request that still carried the summarized
// prefix; preferring it (as EstimateContextTokens does) reports the pre-compaction
// size back and makes a real compaction look like a no-op.
func TestEstimatePostCompactionTokensIgnoresStaleUsage(t *testing.T) {
	stale := &agentcore.Usage{InputTokens: 90_000, OutputTokens: 500}
	rebuilt := []agentcore.Message{
		agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "## Goal\nshort checkpoint"},
		userMsg("next ask"),
		assistantMsg("tail answer", stale, "end_turn"),
	}

	if got := EstimateContextTokens(rebuilt).Tokens; got != 90_500 {
		t.Fatalf("precondition: the stale block should dominate the naive reading, got %d", got)
	}
	got := EstimatePostCompactionTokens(rebuilt)
	if got >= 90_500 {
		t.Errorf("post-compaction estimate re-read the pre-compaction size: %d", got)
	}
	// The number must be the sum of what the rebuilt list actually holds, since
	// that is exactly what the next request carries.
	want := EstimateTokens(rebuilt[0]) + EstimateTokens(rebuilt[1]) + EstimateTokens(rebuilt[2])
	if got != want {
		t.Errorf("EstimatePostCompactionTokens = %d, want the per-message sum %d", got, want)
	}
}

// TestEstimatePostCompactionTokensKeepsCheckpointCost verifies the measurement
// counts the checkpoint's user-instruction section, which is re-sent on every
// later request — the section is a permanent cost, not a one-off.
func TestEstimatePostCompactionTokensKeepsCheckpointCost(t *testing.T) {
	plain := agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "text"}
	withInstructions := plain
	withInstructions.UserInstructions = &agentcore.UserInstructionBlock{
		Summary:  "never touch deploy/",
		Verbatim: []string{"do not touch deploy/"},
	}
	base := EstimatePostCompactionTokens([]agentcore.Message{plain})
	rich := EstimatePostCompactionTokens([]agentcore.Message{withInstructions})
	if rich <= base {
		t.Errorf("instruction section must add to the estimate: %d vs %d", rich, base)
	}
}

func TestShouldCompactThresholdBoundaries(t *testing.T) {
	s := CompactionSettings{Enabled: true, ReserveTokens: 16384}
	window := 200000
	usable := window - s.ReserveTokens // 183616

	tests := []struct {
		name          string
		contextTokens int
		want          bool
	}{
		{"far below", 1000, false},
		{"equal to usable", usable, false}, // strictly greater required
		{"one over usable", usable + 1, true},
		{"far over", window * 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldCompact(tt.contextTokens, window, s); got != tt.want {
				t.Fatalf("ShouldCompact(%d): got %v, want %v", tt.contextTokens, got, tt.want)
			}
		})
	}
}

func TestShouldCompactDisabled(t *testing.T) {
	s := CompactionSettings{Enabled: false, ReserveTokens: 16384}
	if ShouldCompact(1_000_000, 200000, s) {
		t.Fatal("disabled settings must never compact")
	}
}

func TestShouldCompactUnknownWindow(t *testing.T) {
	s := CompactionSettings{Enabled: true, ReserveTokens: 16384}
	if ShouldCompact(1_000_000, 0, s) {
		t.Fatal("unknown (0) context window must never compact")
	}
}

func TestDefaultCompactionSettings(t *testing.T) {
	if DefaultCompactionSettings.ReserveTokens != 16384 {
		t.Fatalf("ReserveTokens: got %d, want 16384", DefaultCompactionSettings.ReserveTokens)
	}
	if DefaultCompactionSettings.KeepRecentTokens != 20000 {
		t.Fatalf("KeepRecentTokens: got %d, want 20000", DefaultCompactionSettings.KeepRecentTokens)
	}
	if !DefaultCompactionSettings.Enabled {
		t.Fatal("default settings should be enabled")
	}
}
