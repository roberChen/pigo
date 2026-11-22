// This file tests the TUI's performance instrumentation: a streamed delta must
// record the reflow it causes (with the size of the content it pushed into the
// viewport) and the bridge's send must record how long it waited for the UI to
// accept an event. It asserts on the stage names and shapes, not on absolute
// timings, which vary with the machine.
package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/perf"
)

// TestReflowRecordsRenderStages feeds one delta through Update and asserts the
// reflow path published its stages and the transcript gauges.
func TestReflowRecordsRenderStages(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)

	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 60, Height: 20})
	m = apply(t, m, textDeltaMsg{delta: "streaming text"})
	// View is driven by the bubbletea runtime, not by Update; call it once so the
	// frame-time stage is present.
	_ = m.View()

	stages := map[string]perf.Stage{}
	for _, st := range perf.Snapshot().Stages {
		stages[st.Name] = st
	}
	for _, name := range []string{"tui.update", "tui.view", "tui.reflow", "tui.layout", "tui.render_all", "tui.viewport_set"} {
		if _, ok := stages[name]; !ok {
			t.Fatalf("stage %q not recorded; recorded: %v", name, stageNames(stages))
		}
	}
	// viewport_set carries the rendered content size: the row that makes a
	// growing transcript visible in the report.
	if got := stages["tui.viewport_set"].Bytes; got <= 0 {
		t.Fatalf("tui.viewport_set bytes = %d, want the rendered transcript size", got)
	}
	if got := perf.Gauge("tui.transcript.blocks"); got != 1 {
		t.Fatalf("transcript blocks gauge = %d, want 1", got)
	}
	if got := perf.Gauge("tui.transcript.bytes"); got <= 0 {
		t.Fatalf("transcript bytes gauge = %d, want > 0", got)
	}
}

// TestBridgeSendRecordsWait proves every event handed to the UI is measured, and
// that the measure is the blocking send (so a full channel with no consumer
// would show up as a large wait).
func TestBridgeSendRecordsWait(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)

	ch := make(chan tea.Msg, 1)
	h := newStreamHandler(ch, nil)
	h.OnText("hi")
	<-ch

	rep := perf.Snapshot()
	found := false
	for _, st := range rep.Stages {
		if st.Name == "tui.bridge.send_wait" {
			found = true
			if st.Count != 1 {
				t.Fatalf("send_wait count = %d, want 1", st.Count)
			}
		}
	}
	if !found {
		t.Fatalf("tui.bridge.send_wait not recorded (stages: %d)", len(rep.Stages))
	}
}

// stageNames maps a stage map to its names for failure messages.
func stageNames(m map[string]perf.Stage) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}

// TestSlashPerfRendersReport verifies /perf is intercepted by the TUI and folds
// the stage table into the transcript as a system block, without starting a run.
func TestSlashPerfRendersReport(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)
	perf.Record("stream.turn", 5*time.Millisecond)

	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	next, cmd := m.runSlash("/perf")
	if cmd != nil {
		t.Fatalf("runSlash(/perf) returned a command, want nil")
	}
	m = next.(Model)
	var last string
	for _, blk := range m.transcript.blocks {
		last = blk.text
	}
	for _, want := range []string{"performance:", "stages:", "stream.turn", "how to read this:"} {
		if !strings.Contains(last, want) {
			t.Fatalf("transcript block missing %q:\n%s", want, last)
		}
	}
}
