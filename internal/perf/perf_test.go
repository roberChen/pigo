// This file tests the perf collector: accumulation, percentile bounds, report
// ordering, the PIGO_PERF run-end output, and the formatting helpers. The global
// counters are process-wide by design, so every test resets them first rather than
// depending on the order the package's tests run in.
package perf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/diag"
)

// resetForTest clears the collector and restores it afterwards, so a test never
// leaks measurements into another one.
func resetForTest(t *testing.T) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
}

func TestRecordAccumulatesAndSortsByTotal(t *testing.T) {
	resetForTest(t)

	RecordN("slow", 10*time.Millisecond, 100)
	RecordN("slow", 30*time.Millisecond, 50)
	Record("fast", time.Millisecond)
	Add("reflows", 3)
	SetGauge("bytes", 2048)

	rep := Snapshot()
	if len(rep.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(rep.Stages))
	}
	if rep.Stages[0].Name != "slow" {
		t.Fatalf("first stage = %q, want slow (highest total)", rep.Stages[0].Name)
	}
	slow := rep.Stages[0]
	if slow.Count != 2 || slow.Total != 40*time.Millisecond || slow.Max != 30*time.Millisecond {
		t.Fatalf("slow = %+v, want count 2 total 40ms max 30ms", slow)
	}
	if slow.Mean != 20*time.Millisecond {
		t.Fatalf("mean = %v, want 20ms", slow.Mean)
	}
	if slow.Bytes != 150 {
		t.Fatalf("bytes = %d, want 150", slow.Bytes)
	}
	if rep.Counters["reflows"] != 3 {
		t.Fatalf("counters = %v, want reflows=3", rep.Counters)
	}
	if rep.Gauges["bytes"] != 2048 {
		t.Fatalf("gauges = %v, want bytes=2048", rep.Gauges)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	resetForTest(t)
	Record("a", time.Millisecond)
	rep := Snapshot()
	Record("a", time.Millisecond)
	if len(rep.Stages) != 1 || rep.Stages[0].Count != 1 {
		t.Fatalf("snapshot mutated by later recording: %+v", rep.Stages)
	}
	if got := Snapshot().Stages[0].Count; got != 2 {
		t.Fatalf("live count = %d, want 2", got)
	}
}

func TestPercentileIsAnUpperBound(t *testing.T) {
	resetForTest(t)
	// 100 observations, one of them 500ms: p95 must reflect the bulk, max the tail.
	for i := 0; i < 99; i++ {
		Record("stage", time.Millisecond)
	}
	Record("stage", 500*time.Millisecond)
	rep := Snapshot()
	st := rep.Stages[0]
	if st.Max != 500*time.Millisecond {
		t.Fatalf("max = %v, want 500ms", st.Max)
	}
	if st.P95 > 2*time.Millisecond {
		t.Fatalf("p95 = %v, want the 1ms bulk (~1-2ms)", st.P95)
	}
	if st.P95 == 0 {
		t.Fatalf("p95 = 0, want a positive upper bound")
	}
}

func TestSpanRecordsOnReturn(t *testing.T) {
	resetForTest(t)
	done := SpanN("span")
	time.Sleep(2 * time.Millisecond)
	done(42)
	rep := Snapshot()
	if len(rep.Stages) != 1 || rep.Stages[0].Count != 1 || rep.Stages[0].Bytes != 42 {
		t.Fatalf("span not recorded: %+v", rep.Stages)
	}
	if rep.Stages[0].Total < 2*time.Millisecond {
		t.Fatalf("span total = %v, want >= 2ms", rep.Stages[0].Total)
	}
}

func TestResetClearsEverything(t *testing.T) {
	resetForTest(t)
	Record("a", time.Millisecond)
	Add("c", 1)
	SetGauge("g", 1)
	since := Snapshot().Since
	time.Sleep(time.Millisecond)
	Reset()
	rep := Snapshot()
	if len(rep.Stages) != 0 || len(rep.Counters) != 0 || len(rep.Gauges) != 0 {
		t.Fatalf("reset left data: %+v %+v %+v", rep.Stages, rep.Counters, rep.Gauges)
	}
	if !rep.Since.After(since) {
		t.Fatalf("since not advanced by reset: %v then %v", since, rep.Since)
	}
}

func TestSlowestOnlyFiresOnADominantStage(t *testing.T) {
	resetForTest(t)
	Record("a", 2*time.Second)
	Record("b", 2*time.Second)
	if slow := Snapshot().slowest(); slow != nil {
		t.Fatalf("slowest = %v, want nil when cost is spread", slow.Name)
	}
	Record("a", 5*time.Second)
	if slow := Snapshot().slowest(); slow == nil || slow.Name != "a" {
		t.Fatalf("slowest = %v, want a", slow)
	}
}

func TestWriteTextContainsEverySection(t *testing.T) {
	resetForTest(t)
	RecordN("tui.reflow", 20*time.Millisecond, 4096)
	Add("compactions", 1)
	SetGauge("tui.transcript.bytes", 4096)
	var buf bytes.Buffer
	Snapshot().WriteText(&buf)
	out := buf.String()
	for _, want := range []string{"stage", "calls", "tui.reflow", "counters:", "compactions", "gauges:", "4KB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

func TestWriteTextOnEmptyReport(t *testing.T) {
	resetForTest(t)
	var buf bytes.Buffer
	Snapshot().WriteText(&buf)
	if !strings.Contains(buf.String(), "no stages recorded") {
		t.Fatalf("empty report = %q", buf.String())
	}
}

func TestHeadlineNamesThroughputAndHotspots(t *testing.T) {
	resetForTest(t)
	RecordN("tui.reflow", 3*time.Second, 100)
	SetGauge("stream.chars_per_sec", 12345)
	head := Snapshot().Headline()
	if !strings.Contains(head, "throughput") || !strings.Contains(head, "tui.reflow") {
		t.Fatalf("headline = %q", head)
	}
}

func TestRunEndWritesJSONAndDiagnostic(t *testing.T) {
	resetForTest(t)
	path := filepath.Join(t.TempDir(), "perf.jsonl")
	t.Setenv("PIGO_PERF", "1")
	t.Setenv("PIGO_PERF_LOG", path)

	var diagBuf bytes.Buffer
	diag.Install(&diagBuf)
	t.Cleanup(func() { diag.Install(nil) })

	RecordN("tui.reflow", 5*time.Millisecond, 128)
	RunEnd("session-1")

	line, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var doc struct {
		Run    string `json:"run"`
		Stages []struct {
			Stage string  `json:"stage"`
			Count int     `json:"count"`
			Total float64 `json:"total_ms"`
			Bytes int     `json:"bytes"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &doc); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if doc.Run != "session-1" || len(doc.Stages) != 1 || doc.Stages[0].Stage != "tui.reflow" {
		t.Fatalf("report = %+v", doc)
	}
	if doc.Stages[0].Bytes != 128 || doc.Stages[0].Total <= 0 {
		t.Fatalf("stage row = %+v", doc.Stages[0])
	}
	if !strings.Contains(diagBuf.String(), "perf: ") || !strings.Contains(diagBuf.String(), "tui.reflow") {
		t.Fatalf("diagnostic = %q", diagBuf.String())
	}

	// A second run appends rather than truncating: the log is one JSON line per run.
	RunEnd("session-2")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log again: %v", err)
	}
	if got := strings.Count(string(data), "\n"); got != 2 {
		t.Fatalf("log lines = %d, want 2:\n%s", got, data)
	}
}

func TestRunEndSilentWithoutPerfButFlagsADominantStage(t *testing.T) {
	resetForTest(t)
	t.Setenv("PIGO_PERF", "")
	t.Setenv("PIGO_PERF_LOG", "")

	var diagBuf bytes.Buffer
	diag.Install(&diagBuf)
	t.Cleanup(func() { diag.Install(nil) })

	// A well-behaved report says nothing at all.
	Record("stream.turn", 10*time.Millisecond)
	RunEnd("quiet")
	if diagBuf.Len() != 0 {
		t.Fatalf("unexpected diagnostic: %q", diagBuf.String())
	}

	// One stage holding most of the time is worth a line even without the flag.
	Record("tui.reflow", 3*time.Second)
	RunEnd("slow")
	if !strings.Contains(diagBuf.String(), "tui.reflow") {
		t.Fatalf("dominant stage not reported: %q", diagBuf.String())
	}
}

func TestEnabledReadsEnvEachCall(t *testing.T) {
	t.Setenv("PIGO_PERF", "")
	if Enabled() {
		t.Fatal("empty PIGO_PERF enabled")
	}
	for _, v := range []string{"1", "true", "yes"} {
		t.Setenv("PIGO_PERF", v)
		if !Enabled() {
			t.Fatalf("PIGO_PERF=%q not enabled", v)
		}
	}
	for _, v := range []string{"0", "false", "FALSE", " "} {
		t.Setenv("PIGO_PERF", v)
		if Enabled() {
			t.Fatalf("PIGO_PERF=%q enabled", v)
		}
	}
}

func TestFormatting(t *testing.T) {
	dur := map[time.Duration]string{
		0:                       "0",
		500 * time.Nanosecond:   "500ns",
		12 * time.Microsecond:   "12µs",
		1500 * time.Microsecond: "1.5ms",
		2500 * time.Millisecond: "2.5s",
		90 * time.Second:        "1m30s",
	}
	for d, want := range dur {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}

	bytesCases := map[int64]string{
		0:                      "0B",
		999:                    "999B",
		1024:                   "1KB",
		3 * 1024 * 1024:        "3MB",
		2 * 1024 * 1024 * 1024: "2GB",
	}
	for n, want := range bytesCases {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}

	if got := FormatGauge("tui.transcript.bytes", 2048); got != "2KB" {
		t.Errorf("byte gauge = %q", got)
	}
	if got := FormatGauge("stream.chars_per_sec", 2048); got != "2KB/s" {
		t.Errorf("rate gauge = %q", got)
	}
	if got := FormatGauge("context.tokens", 12345); got != "12,345" {
		t.Errorf("count gauge = %q", got)
	}
}
