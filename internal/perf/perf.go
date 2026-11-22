// Package perf records how long pigo's hot-path stages take and how much data
// they move, so a throughput problem ("it streams slowly once the history is
// large") can be attributed to a stage instead of guessed at.
//
// The stages are the ones a streaming turn actually crosses:
//
//	provider → stream.ttft / stream.gap / stream.turn
//	         → drain.concat / drain.on_text
//	         → tui.update / tui.reflow (render_all + viewport_set) / tui.view
//
// plus the per-turn work that scales with history (context.estimate,
// llm.build_request, session.append). Recording is always on: every call is a
// map lookup plus a few counter updates, while the stages being measured cost
// milliseconds. That matters more than the nanoseconds it adds — a debug flag
// that has to be set before the slow run means the data is missing exactly when
// someone needs it. Reports are produced only on demand (the /perf command) or
// when PIGO_PERF is set (see RunEnd).
//
// tui.render_all (the re-layout) and viewport_set (the hand-off to the window)
// are both expected to be small and flat now that the transcript splices its rows
// incrementally and the window copies none of them; they stay measured because
// "flat" is a claim that has to be checkable on a user's machine, not assumed.
//
// Nothing here changes behavior: no stage is skipped, no decision reads a
// measurement.
package perf

import (
	"encoding/json"
	"io"
	"math/bits"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/roberChen/pigo/internal/diag"
)

// bucketCount covers durations from 1ns to 2^31ns (~2.1s) by powers of two;
// anything longer clamps into the last bucket. That is enough resolution to tell
// a 200µs event from a 20ms one, which is the range a frame budget cares about.
const bucketCount = 32

// stat is one stage's accumulator. count/total/max/bytes are what reports show;
// buckets backs the percentile estimate (see percentile).
type stat struct {
	count   int64
	totalNs int64
	maxNs   int64
	bytes   int64
	buckets [bucketCount]int64
}

var (
	mu       sync.Mutex
	stages   = make(map[string]*stat)
	counters = make(map[string]int64)
	gauges   = make(map[string]int64)
	since    = time.Now()
)

// Span starts timing a stage. The returned func records the elapsed time when
// called, so the idiomatic use is
//
//	defer perf.Span("tui.reflow")()
func Span(name string) func() {
	start := time.Now()
	return func() { Record(name, time.Since(start)) }
}

// SpanN is Span with a byte count attributed to the same call.
func SpanN(name string) func(int) {
	start := time.Now()
	return func(n int) { RecordN(name, time.Since(start), n) }
}

// Record adds one observation to a stage.
func Record(name string, d time.Duration) { RecordN(name, d, 0) }

// RecordN adds one observation to a stage, attributing n bytes to it. Byte
// totals are what make a per-call cost legible ("3.1GB reflowed"), separate from
// the call count.
func RecordN(name string, d time.Duration, n int) {
	ns := d.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	b := bucketOf(uint64(ns))

	mu.Lock()
	s := stages[name]
	if s == nil {
		s = &stat{}
		stages[name] = s
	}
	s.count++
	s.totalNs += ns
	if ns > s.maxNs {
		s.maxNs = ns
	}
	s.bytes += int64(n)
	s.buckets[b]++
	mu.Unlock()
}

// Add increments a plain counter (something that is counted but not timed, e.g.
// reflows triggered by a resize rather than by a delta).
func Add(name string, n int64) {
	mu.Lock()
	counters[name] += n
	mu.Unlock()
}

// SetGauge records a point-in-time value (a queue length, a byte size, the
// current context-token count) that a report shows alongside the timings.
func SetGauge(name string, v int64) {
	mu.Lock()
	gauges[name] = v
	mu.Unlock()
}

// Gauge reads a gauge back. It exists for tests and for callers that need to
// carry a value forward without keeping their own copy.
func Gauge(name string) int64 {
	mu.Lock()
	defer mu.Unlock()
	return gauges[name]
}

// bucketOf maps a nanosecond duration onto its power-of-two bucket.
func bucketOf(ns uint64) int {
	b := bits.Len64(ns)
	if b >= bucketCount {
		b = bucketCount - 1
	}
	return b
}

// percentile estimates the p-th percentile from the bucket histogram: the
// smallest power-of-two upper bound whose cumulative count covers p. It is an
// upper bound, not an exact quantile — precise enough to separate "every reflow
// costs ~10ms" from "1% cost 500ms", and it costs no per-observation allocation.
func (s *stat) percentile(p float64) time.Duration {
	if s.count == 0 {
		return 0
	}
	want := int64(float64(s.count) * p)
	if want < 1 {
		want = 1
	}
	var seen int64
	for i, c := range s.buckets {
		seen += c
		if seen >= want {
			if i == 0 {
				return 0
			}
			return time.Duration(uint64(1) << uint(i))
		}
	}
	return time.Duration(s.maxNs)
}

// Stage is one entry of a Report.
type Stage struct {
	Name  string
	Count int64
	Total time.Duration
	Mean  time.Duration
	P95   time.Duration
	Max   time.Duration
	Bytes int64
}

// Report is an immutable snapshot of everything recorded so far.
type Report struct {
	// Since is when this process first had something to measure against (its
	// start, effectively); Captured is when the snapshot was taken.
	Since    time.Time
	Captured time.Time
	// Stages are ordered by total time descending, so the top row is the
	// hotspot.
	Stages   []Stage
	Counters map[string]int64
	Gauges   map[string]int64
}

// Snapshot copies the current state into a Report. Copying under the lock keeps
// a report renderable while other goroutines keep recording.
func Snapshot() Report {
	mu.Lock()
	defer mu.Unlock()

	rep := Report{
		Since:    since,
		Captured: time.Now(),
		Stages:   make([]Stage, 0, len(stages)),
		Counters: make(map[string]int64, len(counters)),
		Gauges:   make(map[string]int64, len(gauges)),
	}
	for name, s := range stages {
		st := Stage{
			Name:  name,
			Count: s.count,
			Total: time.Duration(s.totalNs),
			Max:   time.Duration(s.maxNs),
			P95:   s.percentile(0.95),
			Bytes: s.bytes,
		}
		if s.count > 0 {
			st.Mean = time.Duration(s.totalNs / s.count)
		}
		rep.Stages = append(rep.Stages, st)
	}
	sort.Slice(rep.Stages, func(i, j int) bool {
		if rep.Stages[i].Total != rep.Stages[j].Total {
			return rep.Stages[i].Total > rep.Stages[j].Total
		}
		if rep.Stages[i].Count != rep.Stages[j].Count {
			return rep.Stages[i].Count > rep.Stages[j].Count
		}
		return rep.Stages[i].Name < rep.Stages[j].Name
	})
	for name, n := range counters {
		rep.Counters[name] = n
	}
	for name, v := range gauges {
		rep.Gauges[name] = v
	}
	return rep
}

// Reset clears every measurement and restarts the "since" clock. /perf reset
// uses it so a measurement can be scoped to one reproduction step.
func Reset() {
	mu.Lock()
	stages = make(map[string]*stat)
	counters = make(map[string]int64)
	gauges = make(map[string]int64)
	since = time.Now()
	mu.Unlock()
}

// Enabled reports whether PIGO_PERF asks for per-run reports. The value is read
// on every call because a pigorc may export it while the config layers load.
func Enabled() bool {
	v := strings.TrimSpace(os.Getenv("PIGO_PERF"))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// RunEnd is called by the loop when a run finishes. With PIGO_PERF set it emits
// a one-line summary through diag (so a TUI debugging session sees it in the
// transcript and PIGO_DEBUG=1 headless runs get it on stderr) and, when
// PIGO_PERF_LOG names a file, appends a JSON snapshot to it. Without PIGO_PERF
// it still watches for a pathological stage and reports that alone — a run that
// spent most of its wall clock re-rendering is worth one diagnostic line even if
// nobody remembered to set the flag.
func RunEnd(runID string) {
	rep := Snapshot()
	slow := rep.slowest()
	switch {
	case Enabled():
		if path := strings.TrimSpace(os.Getenv("PIGO_PERF_LOG")); path != "" {
			if err := appendJSON(path, runID, rep); err != nil {
				diag.Diagf("perf: could not append report to %s: %v", path, err)
			}
		}
		diag.Diagf("perf: %s", rep.Headline())
	case slow != nil:
		diag.Diagf("perf: %s (%s of measured time); set PIGO_PERF=1 for a full report, /perf to inspect", slow.Name, percentOfTotal(*slow, rep.total()))
	}
}

// slowest returns the stage that dominates the measured time, or nil when there
// is nothing suspicious to say. The threshold is deliberately crude: it fires
// only when one stage owns most of the time, which is the shape of a UI
// bottleneck; a well-behaved run has its cost spread across the pipeline.
func (r Report) slowest() *Stage {
	total := r.total()
	if total <= 0 {
		return nil
	}
	for i := range r.Stages {
		st := r.Stages[i]
		// Require both a dominant share and enough absolute time to matter, so a
		// single slow keystroke in a ten-second session does not raise a flag.
		if st.Total > total/2 && st.Total > time.Second {
			return &st
		}
	}
	return nil
}

// total sums every stage's time. It intentionally double counts nested stages
// (tui.reflow contains tui.render_all): the share is a rough attribution, not an
// accounting identity.
func (r Report) total() time.Duration {
	var d time.Duration
	for _, st := range r.Stages {
		d += st.Total
	}
	return d
}

// appendJSON writes one JSON line per run so a benchmark or soak can diff
// reports without parsing the human format.
func appendJSON(path, runID string, rep Report) error {
	doc := map[string]any{
		"run":      runID,
		"captured": rep.Captured.UTC().Format(time.RFC3339Nano),
		"since":    rep.Since.UTC().Format(time.RFC3339Nano),
	}
	rows := make([]map[string]any, 0, len(rep.Stages))
	for _, st := range rep.Stages {
		rows = append(rows, map[string]any{
			"stage":    st.Name,
			"count":    st.Count,
			"total_ms": float64(st.Total) / float64(time.Millisecond),
			"mean_ms":  float64(st.Mean) / float64(time.Millisecond),
			"p95_ms":   float64(st.P95) / float64(time.Millisecond),
			"max_ms":   float64(st.Max) / float64(time.Millisecond),
			"bytes":    st.Bytes,
		})
	}
	doc["stages"] = rows
	line, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Headline renders the report as one line: the three heaviest stages plus the
// measured stream throughput when there is one. It is what PIGO_PERF emits at
// run end, and what a soak gate can grep for.
func (r Report) Headline() string {
	var b strings.Builder
	if secs := r.Gauges["stream.chars_per_sec"]; secs > 0 {
		b.WriteString("throughput ")
		b.WriteString(FormatRate(secs))
		b.WriteString(" · ")
	}
	top := r.Stages
	if len(top) > 3 {
		top = top[:3]
	}
	if len(top) == 0 {
		b.WriteString("no stages recorded")
	} else {
		total := r.total()
		parts := make([]string, 0, len(top))
		for _, st := range top {
			parts = append(parts, st.Name+" "+FormatDuration(st.Total)+" ("+percentOfTotal(st, total)+")")
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	return b.String()
}

// WriteText renders the report body: the stage table, the hotspot line, the
// counters and the gauges. It is plain text (no escape codes) so it can be
// written to a file or pasted into an issue; the caller owns the header (when it
// was captured, over how long) and any decoration.
func (r Report) WriteText(w io.Writer) {
	if len(r.Stages) == 0 {
		writef(w, "  no stages recorded yet — interact with pigo, then run /perf again\n")
		return
	}

	writef(w, "  %-24s %8s %10s %10s %10s %10s %10s\n",
		"stage", "calls", "total", "mean", "p95", "max", "bytes")
	for _, st := range r.Stages {
		writef(w, "  %-24s %8d %10s %10s %10s %10s %10s\n",
			st.Name, st.Count,
			FormatDuration(st.Total), FormatDuration(st.Mean),
			FormatDuration(st.P95), FormatDuration(st.Max),
			FormatBytes(st.Bytes))
	}

	if slow := r.slowest(); slow != nil {
		writef(w, "\n  hotspot: %s holds %s of measured time\n", slow.Name, percentOfTotal(*slow, r.total()))
	}

	if len(r.Counters) > 0 {
		writef(w, "\n  counters:\n")
		for _, name := range sortedKeys(r.Counters) {
			writef(w, "    %-24s %d\n", name, r.Counters[name])
		}
	}
	if len(r.Gauges) > 0 {
		writef(w, "\n  gauges:\n")
		for _, name := range sortedKeys(r.Gauges) {
			writef(w, "    %-24s %s\n", name, FormatGauge(name, r.Gauges[name]))
		}
	}
}

// WhatToLookFor is the reading guide shown after a report. It turns a wall of
// numbers into the two questions worth asking: is the provider slow, or is pigo
// itself the bottleneck (and if so, which stage).
const WhatToLookFor = `how to read this:
  stream.ttft / stream.gap / stream.turn — the provider. High ttft = slow to start
    answering; high gap = stalls between chunks; low stream.turn throughput with
    low gaps means the endpoint itself is slow.
  tui.reflow (+ render_all, viewport_set) — laying the transcript out. It runs
    on every delta, so its mean multiplied by the delta rate is the frame budget
    it steals. The per-delta path re-wraps only the streaming block's new text and
    re-splices only the rows from that block on, so it should stay flat as
    transcript.bytes grows; a mean that tracks transcript.bytes means the layout was
    recomputed from scratch (a width change, a scrollbar column appearing, or a
    cache-invalidating reload), which is the case worth investigating.
  tui.bridge.send_wait — time the run goroutine spent blocked handing deltas to
    the UI. Large totals mean the UI cannot keep up with the stream.
  drain.on_text — time the consumer took per delta; without send_wait it is the
    consumer's own rendering/writing cost.
  context.estimate / llm.build_request / session.append — per-turn work that
    scales with the message count, incurred once per turn rather than per delta.
`

func percentOfTotal(st Stage, total time.Duration) string {
	if total <= 0 {
		return "0%"
	}
	return formatPct(float64(st.Total) / float64(total) * 100)
}

// FormatDuration renders a duration compactly for the report columns: ns for
// tiny spans, then µs, ms, s, and m for long ones.
func FormatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d < time.Microsecond:
		return intString(d.Nanoseconds()) + "ns"
	case d < time.Millisecond:
		return floatString(float64(d)/float64(time.Microsecond)) + "µs"
	case d < time.Second:
		return floatString(float64(d)/float64(time.Millisecond)) + "ms"
	case d < time.Minute:
		return floatString(float64(d)/float64(time.Second)) + "s"
	default:
		m := int(d / time.Minute)
		s := (d - time.Duration(m)*time.Minute).Seconds()
		return intString(int64(m)) + "m" + floatString(s) + "s"
	}
}

// FormatBytes renders a byte count with a binary unit, so "3.1GB reflowed" is
// readable at a glance.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return intString(n) + "B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return floatString(float64(n)/float64(div)) + string("KMGTPE"[exp]) + "B"
}

// FormatRate renders a bytes-per-second figure.
func FormatRate(bytesPerSec int64) string {
	return FormatBytes(bytesPerSec) + "/s"
}

// FormatGauge renders a gauge with a unit inferred from its name: byte-sized
// gauges get binary units, token/message counters plain grouping, rates a /s
// suffix.
func FormatGauge(name string, v int64) string {
	switch {
	case strings.HasSuffix(name, ".bytes"), strings.HasSuffix(name, "_bytes"):
		return FormatBytes(v)
	case strings.HasSuffix(name, "_per_sec"):
		return FormatRate(v)
	default:
		return intString(v)
	}
}
