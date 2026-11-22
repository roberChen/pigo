// This file tests the /perf renderer: it must show the measured stages, the
// session facts they should be read against, the reading guide, and honor reset.
// The collector is process-wide, so each test resets it first.
package perfcmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/perf"
)

func TestRunPerfRendersStagesAndSession(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	perf.RecordN("tui.reflow", 12*time.Millisecond, 4096)
	perf.Record("stream.turn", 30*time.Millisecond)
	perf.SetGauge("context.tokens", 1000)
	perf.SetGauge("tui.transcript.blocks", 12)
	perf.SetGauge("tui.transcript.bytes", 4096)
	perf.SetGauge("stream.chars_per_sec", 2048)

	var buf bytes.Buffer
	RunPerf(&buf, Input{
		SessionID:     "sess-1",
		Model:         "m1",
		Provider:      "anthropic",
		Messages:      20,
		ContextWindow: 2000,
	})
	out := buf.String()

	for _, want := range []string{
		"performance:", "stages:", "tui.reflow", "stream.turn",
		"session:", "sess-1", "anthropic/m1", "messages:", "20",
		"context:", "1,000 / 2,000 tokens (50%)",
		"transcript:", "12 blocks", "2KB/s",
		"how to read this:", "tui.bridge.send_wait",
		"PIGO_PERF_LOG",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "counters cleared") {
		t.Fatalf("reset message printed without --reset:\n%s", out)
	}
}

func TestRunPerfWithoutSessionStillReportsStages(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	perf.Record("llm.open", time.Millisecond)

	var buf bytes.Buffer
	RunPerf(&buf, Input{})
	out := buf.String()
	if !strings.Contains(out, "llm.open") {
		t.Fatalf("stage missing:\n%s", out)
	}
	if strings.Contains(out, "session:") {
		t.Fatalf("empty session section rendered:\n%s", out)
	}
}

func TestRunPerfEmptyCollector(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	var buf bytes.Buffer
	RunPerf(&buf, Input{ContextWindow: 2000})
	out := buf.String()
	if !strings.Contains(out, "no stages recorded") {
		t.Fatalf("empty report:\n%s", out)
	}
	if !strings.Contains(out, "context window:") {
		t.Fatalf("window not shown before the first turn:\n%s", out)
	}
}

func TestRunPerfResetClearsCounters(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	perf.Record("stage", time.Millisecond)

	var buf bytes.Buffer
	RunPerf(&buf, Input{Reset: true})
	if !strings.Contains(buf.String(), "counters cleared") {
		t.Fatalf("reset message missing:\n%s", buf.String())
	}
	if got := len(perf.Snapshot().Stages); got != 0 {
		t.Fatalf("stages after reset = %d, want 0", got)
	}
}

func TestResetArg(t *testing.T) {
	cases := map[string]bool{
		"/perf":             false,
		"/perf ":            false,
		"/perf reset":       true,
		"  /perf   reset  ": true,
		"/perf resetter":    false,
		"/performance":      false,
	}
	for line, want := range cases {
		if got := ResetArg(line); got != want {
			t.Errorf("ResetArg(%q) = %v, want %v", line, got, want)
		}
	}
}
