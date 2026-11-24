// This file tests the pluggable compaction trigger: where the fixed policy puts
// the threshold (and which cap put it there), and how a custom policy's answer
// merges with it. The merge is the safety-critical part: a policy may fire early,
// but it must never be able to leave an over-budget context uncompacted, because
// the provider would then reject the next request outright.
package compaction

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireShell skips a test that shells out when no POSIX shell is available, so
// the command-trigger contract does not turn into a platform failure.
func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func TestInputBudgetPrefersTheInputCeiling(t *testing.T) {
	tests := []struct {
		name     string
		window   int
		maxInput int
		want     int
	}{
		{"no ceiling", 1_000_000, 0, 1_000_000},
		{"smaller ceiling wins", 1_000_000, 512_000, 512_000},
		{"ceiling above the window is ignored", 200_000, 400_000, 200_000},
		{"unknown window", 0, 512_000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := CompactionSettings{Enabled: true, MaxInputTokens: tt.maxInput}
			if got := InputBudget(tt.window, s); got != tt.want {
				t.Fatalf("InputBudget = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestExplainTriggerNamesTheDecidingCap pins the attribution a UI renders. The
// drawer quotes "the 80% ladder" instead of a bare number, so the source must be
// the cap that actually decided — not merely one that was configured.
func TestExplainTriggerNamesTheDecidingCap(t *testing.T) {
	// window=1M but the model only accepts 512K of input: the ladder must be a
	// share of the input budget, never of the combined window.
	ceiling := CompactionSettings{
		Enabled:         true,
		ReserveTokens:   16384,
		MaxInputTokens:  512_000,
		TriggerFraction: 0.80,
	}
	d := ExplainTrigger(1_000_000, ceiling)
	if d.Budget != 512_000 {
		t.Errorf("budget = %d, want the 512000 input ceiling", d.Budget)
	}
	if d.Source != TriggerSourceLadder {
		t.Errorf("source = %q, want ladder", d.Source)
	}
	if d.Point != 409_600 {
		t.Errorf("point = %d, want 80%% of the input budget (409600)", d.Point)
	}

	// A reserve tighter than the ladder decides instead.
	tightReserve := CompactionSettings{Enabled: true, ReserveTokens: 100_000, TriggerFraction: 0.80}
	if d := ExplainTrigger(200_000, tightReserve); d.Source != TriggerSourceReserve || d.Point != 100_000 {
		t.Errorf("tight reserve: source=%q point=%d, want reserve/100000", d.Source, d.Point)
	}

	// An absolute cap below both wins.
	capped := CompactionSettings{Enabled: true, ReserveTokens: 16384, TriggerFraction: 0.80, TriggerTokens: 50_000}
	if d := ExplainTrigger(200_000, capped); d.Source != TriggerSourceConfigured || d.Point != 50_000 {
		t.Errorf("configured cap: source=%q point=%d, want configured/50000", d.Source, d.Point)
	}

	// An absolute cap above both loses: the smaller value always wins, so a
	// compact_at can only fire earlier than the safe line, never later.
	loose := CompactionSettings{Enabled: true, ReserveTokens: 16384, TriggerFraction: 0.80, TriggerTokens: 190_000}
	if d := ExplainTrigger(200_000, loose); d.Source != TriggerSourceLadder || d.Point != 160_000 {
		t.Errorf("loose cap: source=%q point=%d, want ladder/160000", d.Source, d.Point)
	}
}

func TestExplainTriggerOffCases(t *testing.T) {
	disabled := CompactionSettings{Enabled: false, ReserveTokens: 16384, TriggerFraction: 0.80}
	if d := ExplainTrigger(200_000, disabled); d.Source != TriggerSourceOff || d.Point != 0 {
		t.Errorf("disabled: source=%q point=%d, want off/0", d.Source, d.Point)
	}
	if d := ExplainTrigger(0, DefaultCompactionSettings); d.Source != TriggerSourceOff || d.Point != 0 {
		t.Errorf("unknown window: source=%q point=%d, want off/0", d.Source, d.Point)
	}
	// A reserve larger than the budget has no meaningful trigger point; reporting
	// one would let a UI promise space that does not exist.
	huge := CompactionSettings{Enabled: true, ReserveTokens: 500_000, TriggerFraction: 0.80}
	if d := ExplainTrigger(100_000, huge); d.Source != TriggerSourceOff || d.Point != 0 {
		t.Errorf("reserve over budget: source=%q point=%d, want off/0", d.Source, d.Point)
	}
}

func TestDecideCompactMergesPolicyAndFixed(t *testing.T) {
	s := CompactionSettings{Enabled: true, ReserveTokens: 16384, TriggerFraction: 0.80}
	// 200K budget -> ladder at 160K.
	base := TriggerInput{ContextWindow: 200_000, InputBudget: 200_000}

	tests := []struct {
		name      string
		tokens    int
		policy    TriggerFunc
		want      bool
		wantCause TriggerReason
	}{
		{
			name:      "no policy uses the fixed threshold",
			tokens:    170_000,
			want:      true,
			wantCause: TriggerFixed,
		},
		{
			name:      "no policy under the threshold",
			tokens:    100_000,
			want:      false,
			wantCause: TriggerFixed,
		},
		{
			name:   "a policy may fire early",
			tokens: 10_000,
			policy: func(context.Context, TriggerInput) TriggerDecision {
				return TriggerDecision{Decided: true, Compact: true, Reason: "quota header"}
			},
			want:      true,
			wantCause: TriggerPolicy,
		},
		{
			name:   "an undecided policy leaves the fixed threshold alone",
			tokens: 170_000,
			policy: func(context.Context, TriggerInput) TriggerDecision {
				return TriggerDecision{Decided: false}
			},
			want:      true,
			wantCause: TriggerFixed,
		},
		{
			name:   "a declining policy is honored below the budget",
			tokens: 100_000,
			policy: func(context.Context, TriggerInput) TriggerDecision {
				return TriggerDecision{Decided: true, Compact: false}
			},
			want:      false,
			wantCause: TriggerPolicy,
		},
		{
			// The safety rule: declining must not let a request over the input
			// ceiling through, because the provider would reject it.
			name:   "a declining policy cannot override the input budget",
			tokens: 195_000,
			policy: func(context.Context, TriggerInput) TriggerDecision {
				return TriggerDecision{Decided: true, Compact: false, Reason: "looks fine"}
			},
			want:      true,
			wantCause: TriggerBudgetGuard,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.ContextTokens = tt.tokens
			got := DecideCompact(context.Background(), in, s, tt.policy)
			if got.Compact != tt.want {
				t.Errorf("compact = %v, want %v", got.Compact, tt.want)
			}
			if got.Reason != tt.wantCause {
				t.Errorf("reason = %q, want %q", got.Reason, tt.wantCause)
			}
		})
	}
}

// TestDecideCompactPassesThePolicyItsInput guards the contract a policy is
// written against: the numbers are pre-computed by the caller, so the policy can
// trust them instead of re-deriving (and disagreeing with) the loop.
func TestDecideCompactPassesThePolicyItsInput(t *testing.T) {
	s := CompactionSettings{Enabled: true, ReserveTokens: 1000, TriggerFraction: 0.80}
	in := TriggerInput{
		ContextTokens: 42_000,
		ContextWindow: 200_000,
		InputBudget:   100_000,
		TriggerPoint:  80_000,
		Model:         "m",
		Provider:      "p",
	}
	var seen TriggerInput
	policy := func(_ context.Context, got TriggerInput) TriggerDecision {
		seen = got
		return TriggerDecision{Decided: true, Compact: true}
	}
	DecideCompact(context.Background(), in, s, policy)
	if seen != in {
		t.Errorf("policy input = %+v, want %+v", seen, in)
	}
}

// TestCommandTriggerShellsOut exercises the real command path, because that is
// the form users configure: one JSON document in on stdin, one decision out on
// stdout, with diagnostics around it tolerated.
func TestCommandTriggerShellsOut(t *testing.T) {
	requireShell(t)
	trig := &CommandTrigger{
		// Echo the input back as diagnostics, and print a decision after it.
		Command: `in=$(cat); echo "saw: $in" >&2; printf '{"compact":true,"reason":"from %s"}\n' "$(echo "$in" | sed -n 's/.*"model":"\([^"]*\)".*/\1/p')"`,
		Shell:   "sh",
		Timeout: 5 * time.Second,
	}
	in := TriggerInput{ContextTokens: 150_000, InputBudget: 200_000, Model: "gpt-x", Provider: "openai"}
	got := DecideCompact(context.Background(), in, DefaultCompactionSettings, trig.Func())
	if !got.Compact || got.Reason != TriggerPolicy {
		t.Fatalf("outcome = %+v, want a policy decision to compact", got)
	}
	if !strings.Contains(got.Detail, "gpt-x") {
		t.Errorf("detail = %q, want the model the policy echoed", got.Detail)
	}
}

func TestCommandTriggerNoOpinionFallsBack(t *testing.T) {
	requireShell(t)
	trig := &CommandTrigger{Command: `cat >/dev/null; echo '{"decide":false}'`, Shell: "sh", Timeout: 5 * time.Second}
	s := CompactionSettings{Enabled: true, ReserveTokens: 1000, TriggerFraction: 0.80}
	// 170K is past the 160K ladder, so the fixed policy has a firm answer.
	in := TriggerInput{ContextTokens: 170_000, ContextWindow: 200_000, InputBudget: 200_000}
	got := DecideCompact(context.Background(), in, s, trig.Func())
	if !got.Compact || got.Reason != TriggerFixed {
		t.Fatalf("outcome = %+v, want the fixed threshold to decide", got)
	}
}

// TestCommandTriggerFailureIsFailOpen pins the rule that a broken policy can
// never stop an over-budget context from compacting: no decision, a crash, and a
// timeout all mean "no opinion".
func TestCommandTriggerFailureIsFailOpen(t *testing.T) {
	requireShell(t)
	s := CompactionSettings{Enabled: true, ReserveTokens: 1000, TriggerFraction: 0.80}
	over := TriggerInput{ContextTokens: 170_000, ContextWindow: 200_000, InputBudget: 200_000}

	cases := []struct {
		name    string
		command string
		timeout time.Duration
	}{
		{"non-zero exit", `cat >/dev/null; echo boom >&2; exit 3`, 5 * time.Second},
		{"unparseable output", `cat >/dev/null; echo "not json"`, 5 * time.Second},
		{"timeout", `sleep 5`, 50 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warned []string
			trig := &CommandTrigger{
				Command: tc.command,
				Shell:   "sh",
				Timeout: tc.timeout,
				Warn:    func(f string, a ...any) { warned = append(warned, f) },
			}
			got := DecideCompact(context.Background(), over, s, trig.Func())
			if !got.Compact || got.Reason != TriggerFixed {
				t.Fatalf("outcome = %+v, want the fixed threshold to decide", got)
			}
			if len(warned) == 0 {
				t.Error("a failed policy must be reported to the diagnostic sink")
			}
		})
	}
}

func TestParseCommandDecision(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantOK     bool
		wantDecide bool
		wantSpan   bool
	}{
		{"plain compact", `{"compact":true,"reason":"x"}`, true, true, true},
		{"compact false omits decide", `{"compact":false}`, true, true, false},
		{"explicit no opinion", `{"decide":false,"compact":true}`, true, false, true},
		{"leading noise tolerated", "log line\n{\"compact\":true}", true, true, true},
		{"brace inside a reason", `{"compact":true,"reason":"a {b} c"}`, true, true, true},
		{"no object", `true`, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCommandDecision([]byte(tt.out))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Decided != tt.wantDecide {
				t.Errorf("decided = %v, want %v", got.Decided, tt.wantDecide)
			}
			if tt.wantDecide && got.Decided && got.Compact != tt.wantSpan {
				t.Errorf("compact = %v, want %v", got.Compact, tt.wantSpan)
			}
		})
	}
}

// TestNewCommandTriggerEmptyIsNil keeps "not configured" and "configured"
// distinguishable: the loop relies on nil to skip the policy entirely, and a
// non-nil trigger that never decides would add a subprocess per turn.
func TestNewCommandTriggerEmptyIsNil(t *testing.T) {
	if trig := NewCommandTrigger("   ", ""); trig != nil {
		t.Fatalf("empty command = %+v, want nil", trig)
	}
	var nilTrigger *CommandTrigger
	if nilTrigger.Func() != nil {
		t.Fatal("a nil trigger must adapt to a nil TriggerFunc")
	}
}

// TestBoundedBufferDropsTheOverflow keeps a runaway policy from growing the
// process's memory through its own output.
func TestBoundedBufferDropsTheOverflow(t *testing.T) {
	var b boundedBuffer
	b.limit = 8
	n, err := b.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write = %d,%v, want 16,nil (all input consumed)", n, err)
	}
	if got := b.String(); got != "01234567" {
		t.Errorf("buffer = %q, want the 8-byte prefix", got)
	}
	if !b.over {
		t.Error("overflow must be recorded")
	}
}
