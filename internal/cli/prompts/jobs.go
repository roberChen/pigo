// This file implements the /jobs slash command: the user's view of everything
// still running in the background. The model has per-family tools for this
// (bash_jobs for shell commands, task_output for sub-agents); the user typing in
// the REPL or TUI has neither, and after a few detaches it is easy to forget what
// is running. /jobs lists both registries in one place, with the exact command to
// read or stop each entry.
//
// It is registered by the driver (not by BuildSlashRegistry) because it closes over
// the live registries, which only the driver's assembly owns — the same reason /mcp
// and /trust are installed separately.
package prompts

import (
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/ui"
	"github.com/smallnest/pigo/internal/runtime"
)

// RegisterJobsCommand installs /jobs against the session's background registries.
// Either store may be nil (the tool set has no shell or no sub-agents, e.g. under
// --no-tools or a restricted --allowed-tools boundary); the command then reports
// only the families it knows about. Calling it with both nil still registers the
// command, so /help stays stable across tool configurations.
func RegisterJobsCommand(reg *runtime.SlashRegistry, bashJobs *agenttool.BashJobStore, subJobs *runtime.SubAgentJobs) {
	reg.AddBuiltin(runtime.SlashCommand{
		Name:         "jobs",
		ArgumentHint: "[running]",
		Description:  "list background work (shell commands and sub-agents): id, status, runtime; /jobs running hides finished ones",
		Action: func(args string) string {
			return FormatJobs(strings.TrimSpace(args) == "running", bashJobs, subJobs)
		},
	})
}

// FormatJobs renders the background registries for /jobs. runningOnly filters out
// finished entries in both families. It is exported so the REPL/TUI tests can
// assert the layout without going through the registry.
func FormatJobs(runningOnly bool, bashJobs *agenttool.BashJobStore, subJobs *runtime.SubAgentJobs) string {
	color := ui.Enabled()

	type bashRow struct {
		id      string
		running bool
		detail  string
	}
	var bashRows []bashRow
	if bashJobs != nil {
		for _, job := range bashJobs.List() {
			running := job.Status() == agenttool.BashRunning
			if runningOnly && !running {
				continue
			}
			status := "running"
			if !running {
				status = fmt.Sprintf("exited code %d", job.ExitCode())
			}
			detail := fmt.Sprintf("%-16s  %-8s  %s",
				status, job.Elapsed().Round(time.Second), clipOneLine(job.Command, 90))
			if running {
				if n := job.UnreadBytes(); n > 0 {
					detail += fmt.Sprintf("  (%d unread bytes)", n)
				}
			}
			bashRows = append(bashRows, bashRow{id: job.ID, running: running, detail: detail})
		}
	}

	type subRow struct {
		id      string
		running bool
		detail  string
	}
	var subRows []subRow
	if subJobs != nil {
		for _, job := range subJobs.List() {
			status, activity, _, _, err := job.Snapshot()
			running := status == runtime.JobRunning
			if runningOnly && !running {
				continue
			}
			state := string(status)
			if running && activity != "" {
				state += " (" + clipOneLine(activity, 40) + ")"
			}
			detail := fmt.Sprintf("%-8s  %-20s  %-8s  %s",
				job.Agent(), state, time.Since(job.StartedAt()).Round(time.Second), clipOneLine(job.Description(), 70))
			if err != nil {
				detail += "  [" + clipOneLine(err.Error(), 60) + "]"
			}
			subRows = append(subRows, subRow{id: job.ID(), running: running, detail: detail})
		}
	}

	running := 0
	for _, r := range bashRows {
		if r.running {
			running++
		}
	}
	for _, r := range subRows {
		if r.running {
			running++
		}
	}

	var b strings.Builder
	if len(bashRows) == 0 && len(subRows) == 0 {
		if runningOnly {
			return "no background jobs are still running"
		}
		return "no background jobs have been started in this session\n" +
			"launch one with bash {run_in_background: true} (a foreground command that times out is moved here), or with task {background: true}"
	}

	header := fmt.Sprintf("background jobs: %d running, %d total", running, len(bashRows)+len(subRows))
	b.WriteString(ui.Colorize(color, ui.Bold, header))

	if len(bashRows) > 0 {
		b.WriteString("\n\n" + ui.Colorize(color, ui.Dim, "shell commands (bash)"))
		for _, r := range bashRows {
			b.WriteString("\n  " + ui.Colorize(color, ui.Cyan, r.id) + "  " + r.detail)
		}
	}
	if len(subRows) > 0 {
		b.WriteString("\n\n" + ui.Colorize(color, ui.Dim, "sub-agents (task)"))
		for _, r := range subRows {
			b.WriteString("\n  " + ui.Colorize(color, ui.Cyan, r.id) + "  " + r.detail)
		}
	}

	b.WriteString("\n\n" + ui.Colorize(color, ui.Dim, "follow up:"))
	if len(bashRows) > 0 {
		b.WriteString("\n  " + ui.Colorize(color, ui.Dim, `bash_output {"bash_id": "<id>"}  (add wait_ms / wait_for_exit to wait) · kill_bash {"bash_id": "<id>"}`))
	}
	if len(subRows) > 0 {
		b.WriteString("\n  " + ui.Colorize(color, ui.Dim, `task_output {"job_id": "<id>"}  (add "wait": true to block until it finishes)`))
	}
	return b.String()
}

// clipOneLine collapses whitespace and truncates s to max runes, so every entry
// stays on one terminal row.
func clipOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if max <= 0 || len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
