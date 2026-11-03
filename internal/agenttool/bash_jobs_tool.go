// This file implements the bash_jobs tool: the model's single-step handle on every
// background shell command in the session. bash_output answers "what did job X
// print?" for one id the model already remembers; this answers "what is still
// running, and what is it?" after a few detaches, which is otherwise only
// recoverable from scrollback. It pairs with the /jobs slash command, which shows
// the same registry (plus background sub-agents) to the user.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// bashJobsArgs is the decoded argument shape for BashJobsTool.
type bashJobsArgs struct {
	// RunningOnly hides jobs that already exited.
	RunningOnly bool `json:"running_only,omitempty"`
}

// BashJobsTool lists the background bash jobs held by the shared BashJobStore.
type BashJobsTool struct {
	Jobs *BashJobStore
}

// Name implements AgentTool.
func (t *BashJobsTool) Name() string { return "bash_jobs" }

// Description implements AgentTool.
func (t *BashJobsTool) Description() string {
	return "List the background commands this session has running (or finished). " +
		"Shows each bash_id, whether it is running or exited, how long it has run, " +
		"its exit code, how much output bash_output has not returned yet, and the " +
		"command line. Use it to recover ids you no longer have in context before " +
		"calling bash_output or kill_bash; set running_only=true to hide finished jobs."
}

// Schema implements AgentTool.
func (t *BashJobsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "running_only": {"type": "boolean", "description": "Only list jobs that are still running (default false lists every job)."}
  },
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Listing is read-only.
func (t *BashJobsTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements AgentTool. It renders one line per job plus the follow-up
// calls, so a fresh look at the registry is actionable without another round trip.
func (t *BashJobsTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[bashJobsArgs](args, "bash_jobs")
	if bad != nil {
		return *bad, nil
	}
	if t.Jobs == nil {
		return errorResult("bash_jobs: background jobs are not available in this environment"), nil
	}

	jobs := t.Jobs.List()
	var lines []string
	details := make([]map[string]any, 0, len(jobs))
	running := 0
	for _, job := range jobs {
		status := job.Status()
		if status == BashRunning {
			running++
		}
		if a.RunningOnly && status != BashRunning {
			continue
		}
		lines = append(lines, bashJobLine(job))
		details = append(details, bashJobDetails(job))
	}

	if len(lines) == 0 {
		msg := "no background bash jobs have been started in this session"
		if a.RunningOnly {
			msg = "no background bash jobs are still running"
		}
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent(msg + "\nstart one with bash {run_in_background: true}, or a foreground command that times out is moved here automatically.")},
			Details: map[string]any{"count": 0, "running": 0},
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "background bash jobs: %d shown, %d running\n", len(lines), running)
	for _, line := range lines {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("\nfollow up with:\n")
	b.WriteString("  bash_output {\"bash_id\": \"<id>\"}                      — read new output (add wait_ms or wait_for_exit to wait)\n")
	b.WriteString("  kill_bash  {\"bash_id\": \"<id>\"}                      — stop it\n")

	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(b.String())},
		Details: map[string]any{"count": len(lines), "running": running, "jobs": details},
	}, nil
}

// bashJobLine renders one job as a compact status row: id, status, elapsed, exit
// code or unread-output hint, and the (clipped) command line.
func bashJobLine(job *BashJob) string {
	status := job.Status()
	state := "running"
	if status == BashExited {
		state = fmt.Sprintf("exited code %d", job.ExitCode())
	}
	line := fmt.Sprintf("%-8s  %-16s  %-8s  %s",
		job.ID, state, job.Elapsed().Round(time.Second), clipCommand(job.Command, 100))
	if status == BashRunning {
		if n := job.UnreadBytes(); n > 0 {
			line += fmt.Sprintf("  (%d unread bytes; bash_output)", n)
		}
	} else if errText := job.ErrorText(); errText != "" && job.ExitCode() != 0 {
		line += "  [" + clipCommand(errText, 80) + "]"
	}
	return line
}

// bashJobDetails is the structured form of one job row, so a front-end can render
// the listing without parsing text.
func bashJobDetails(job *BashJob) map[string]any {
	status := job.Status()
	return map[string]any{
		"bash_id":     job.ID,
		"command":     job.Command,
		"status":      string(status),
		"exitCode":    job.ExitCode(),
		"startedAt":   job.StartedAt.Format(time.RFC3339),
		"elapsedMs":   job.Elapsed().Milliseconds(),
		"unreadBytes": job.UnreadBytes(),
	}
}

// clipCommand collapses whitespace (a multi-line command must stay one row) and
// truncates to max runes.
func clipCommand(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if max <= 0 || len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
