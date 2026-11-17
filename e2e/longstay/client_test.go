package main

import (
	"testing"

	"github.com/roberChen/pigo/internal/hostapi"
)

func TestHandleOutcomeAcceptsCompactedTail(t *testing.T) {
	r := &runner{cfg: Config{Scenario: ScenarioNormal}, mock: &mockLLM{failedTokens: make(map[string]bool)}}
	s := &clientState{runner: r, attempt: 1, running: true}
	s.trace = &turnTrace{token: "compacted-token", summary: true}
	s.handleOutcome(turnOutcome{
		attempt: 1,
		token:   "compacted-token",
		result:  hostapi.PromptResult{Text: "working on compacted-token (stage 2)"},
	})
	if got := r.metrics.turnsCompacted.Load(); got != 1 {
		t.Fatalf("turnsCompacted = %d, want 1", got)
	}
	if got := r.metrics.protocolErrors.Load(); got != 0 {
		t.Fatalf("protocolErrors = %d, want 0", got)
	}
	if s.running {
		t.Fatal("compacted outcome left the client running")
	}
}
