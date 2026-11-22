// This file holds the REPL-integration tests for /perf: the command is
// intercepted before slash resolution, never reaches the model, prints the stage
// table, and "/perf reset" clears the counters. The renderer itself is tested in
// internal/cli/perfcmd.
package repl

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/perf"
)

func TestRunPerfViaREPL(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	perf.RecordN("tui.reflow", 7*time.Millisecond, 2048)

	p := &replProvider{reply: "should not be called"}
	deps, _ := newTestDeps(t, p)

	var out bytes.Buffer
	if err := runREPL(strings.NewReader("/perf\n/exit\n"), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if p.calls != 0 {
		t.Errorf("model calls = %d, want 0 for /perf", p.calls)
	}
	got := out.String()
	for _, want := range []string{"performance:", "stages:", "tui.reflow", "how to read this:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("REPL /perf output missing %q:\n%s", want, got)
		}
	}
}

func TestRunPerfResetViaREPL(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	perf.Record("stream.turn", 3*time.Millisecond)

	p := &replProvider{reply: ""}
	deps, _ := newTestDeps(t, p)

	var out bytes.Buffer
	if err := runREPL(strings.NewReader("/perf reset\n/exit\n"), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if !strings.Contains(out.String(), "counters cleared") {
		t.Fatalf("reset confirmation missing:\n%s", out.String())
	}
	if got := len(perf.Snapshot().Stages); got != 0 {
		t.Fatalf("stages after /perf reset = %d, want 0", got)
	}
}

func TestPerfNameIsNotIntercepted(t *testing.T) {
	// "/performance" must not run /perf: like every unknown slash command it
	// resolves against the registry, fails, and never reaches the model (the
	// REPL never forwards an unrecognized command as a prompt).
	p := &replProvider{reply: "model called"}
	deps, _ := newTestDeps(t, p)

	var out bytes.Buffer
	if err := runREPL(strings.NewReader("/performance\n/exit\n"), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if strings.Contains(out.String(), "performance:") {
		t.Fatalf("/performance ran the perf report:\n%s", out.String())
	}
}
