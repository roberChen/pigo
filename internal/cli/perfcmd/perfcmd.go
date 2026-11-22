// This file implements the /perf slash command: a report of how long each stage
// of the streaming and rendering pipeline took, so a throughput problem ("output
// slows down as the conversation grows") can be attributed to the provider, the
// TUI renderer, or pigo's per-turn bookkeeping instead of guessed at.
//
// The measurements come from internal/perf, which every hot path records into
// unconditionally; this command only formats them, adds the session context
// (message count and window, the variables that make a stage's cost grow), and
// explains how to read the numbers. Both front-ends render it through this one
// function, like /status and /memory, so the REPL and the TUI stay identical.
package perfcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/roberChen/pigo/internal/cli/ui"
	"github.com/roberChen/pigo/internal/perf"
)

// Input is the live session state the report correlates the timings with. Every
// field is optional: /perf still works before a session exists (it then shows the
// process-wide measurements alone, which is what a startup-latency investigation
// wants anyway).
type Input struct {
	SessionID     string
	Model         string
	Provider      string
	Messages      int
	ContextWindow int
	// Reset clears the counters after the report is printed, so the next
	// reproduction starts from zero (e.g. before re-sending the prompt that was
	// slow).
	Reset bool
}

// ResetArg reports whether a /perf line asks for the counters to be cleared
// ("/perf reset") rather than just the report ("/perf"). It is the command's only
// argument, shared here so the REPL and the TUI cannot drift on how it is read.
func ResetArg(line string) bool {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/perf")) == "reset"
}

// RunPerf renders the performance report to out.
func RunPerf(out io.Writer, in Input) {
	color := ui.Enabled()

	rep := perf.Snapshot()

	var buf strings.Builder
	rep.WriteText(&buf)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s (over %s, snapshot %s)\n",
		ui.Colorize(color, ui.Bold, "performance:"),
		perf.FormatDuration(rep.Captured.Sub(rep.Since)),
		rep.Captured.Format("15:04:05"))
	writeSession(out, color, in, rep)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Bold, "stages:"))
	// The table is rendered plain by perf: coloring individual numbers would
	// break the column alignment that makes it scannable.
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}

	fmt.Fprintln(out)
	for _, line := range strings.Split(strings.TrimRight(perf.WhatToLookFor, "\n"), "\n") {
		fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Dim, line))
	}

	fmt.Fprintf(out, "\n%s /perf reset clears the counters; PIGO_PERF=1 reports at the end of\n"+
		"every run (visible with PIGO_DEBUG=1 or in the TUI) and PIGO_PERF_LOG=<path> appends\n"+
		"each run's report as one JSON line.\n", ui.Colorize(color, ui.Dim, "tip:"))
	if in.Reset {
		perf.Reset()
		fmt.Fprintf(out, "%s counters cleared\n", ui.Colorize(color, ui.Dim, "·"))
	}
}

// writeSession prints the session facts the timings should be read against: the
// same prompt costs more with a longer history, and the context section is what
// turns "tui.reflow is slow" into "tui.reflow is slow because this conversation
// holds 3.1MB of transcript".
func writeSession(out io.Writer, color bool, in Input, rep perf.Report) {
	if in.SessionID == "" && in.Model == "" && in.Messages == 0 && in.ContextWindow == 0 {
		return
	}
	fmt.Fprintln(out)
	label := func(s string) string { return ui.Colorize(color, ui.Dim, s) }
	if in.SessionID != "" {
		fmt.Fprintf(out, "  %s %s\n", label("session:"), in.SessionID)
	}
	if in.Model != "" {
		model := in.Model
		if in.Provider != "" {
			model = in.Provider + "/" + model
		}
		fmt.Fprintf(out, "  %s %s\n", label("model:"), model)
	}
	if in.Messages > 0 {
		fmt.Fprintf(out, "  %s %d\n", label("messages:"), in.Messages)
	}
	if in.ContextWindow > 0 {
		// The token count comes from the last turn's estimate (the loop publishes
		// it as a gauge); before the first turn there is nothing to compare against
		// the window yet, so only the window is shown.
		if tokens := rep.Gauges["context.tokens"]; tokens > 0 {
			fmt.Fprintf(out, "  %s %s / %s tokens (%d%%)\n",
				label("context:"), perf.FormatGauge("", tokens),
				perf.FormatGauge("", int64(in.ContextWindow)),
				tokens*100/int64(in.ContextWindow))
		} else {
			fmt.Fprintf(out, "  %s %s tokens\n", label("context window:"), perf.FormatGauge("", int64(in.ContextWindow)))
		}
	}
	if blocks := rep.Gauges["tui.transcript.blocks"]; blocks > 0 {
		fmt.Fprintf(out, "  %s %d blocks / %s\n",
			label("transcript:"), blocks, perf.FormatBytes(rep.Gauges["tui.transcript.bytes"]))
	}
	if rate := rep.Gauges["stream.chars_per_sec"]; rate > 0 {
		fmt.Fprintf(out, "  %s %s (last turn)\n", label("stream:"), perf.FormatRate(rate))
	}
}
