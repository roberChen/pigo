// This file implements the two companion tools for background bash jobs:
// bash_output drains a job's new output, optionally waiting for more output or
// for the job to finish, and reports its status; kill_bash terminates a running
// job. Both address a job by the bash_id returned from a `bash` call with
// run_in_background=true (or from a foreground call that timed out and was moved
// to the background), sharing the same BashJobStore so a job launched by the
// bash tool is visible here. This mirrors Claude Code's BashOutput/KillShell
// tools.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// bashOutputDefaultWait bounds a wait_for_exit call that does not pass wait_ms,
// so "wait until it exits" cannot block the turn for the full job lifetime.
const bashOutputDefaultWait = 30 * time.Second

// BashOutputTool reads the output a background job has produced since the last
// read and reports whether it is still running or has exited. Jobs is the shared
// store the bash tool populates.
type BashOutputTool struct {
	Jobs *BashJobStore
}

// bashOutputArgs is the decoded argument shape for BashOutputTool.
type bashOutputArgs struct {
	// BashID is the job handle returned by a background `bash` call.
	BashID string `json:"bash_id"`
	// WaitMs is how long to wait for new output or completion before returning
	// whatever is available (milliseconds). Zero returns immediately.
	WaitMs int `json:"wait_ms,omitempty"`
	// WaitForExit keeps waiting until the job exits (bounded by wait_ms, or by
	// bashOutputDefaultWait when wait_ms is unset) instead of returning on the
	// first chunk of new output.
	WaitForExit bool `json:"wait_for_exit,omitempty"`
}

// Name implements AgentTool.
func (t *BashOutputTool) Name() string { return "bash_output" }

// Description implements AgentTool.
func (t *BashOutputTool) Description() string {
	return "Read new output from a background command started with bash " +
		"run_in_background=true, addressed by its bash_id. Returns output " +
		"accumulated since the last read plus the command's status (running or " +
		"exited, with exit code). Set wait_ms to block briefly for new output " +
		"instead of polling, or wait_for_exit=true to wait until the command " +
		"finishes. Call repeatedly to stream a long job's output."
}

// Schema implements AgentTool.
func (t *BashOutputTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "bash_id": {"type": "string", "description": "The bash_id returned by a background bash call."},
    "wait_ms": {"type": "integer", "description": "Wait up to this many milliseconds for new output or completion (0 = return immediately, capped at 10 minutes).", "minimum": 0},
    "wait_for_exit": {"type": "boolean", "description": "Wait until the command exits instead of returning on the first new output; bounded by wait_ms (default 30s when wait_ms is unset)."}
  },
  "required": ["bash_id"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Reading output has no side effects.
func (t *BashOutputTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements AgentTool. It returns the job's new output and status,
// optionally waiting wait_ms for output (returning as soon as some arrives) or
// wait_for_exit for completion. Waiting is interruptible: a canceled turn stops
// the wait and returns whatever has been collected so far.
func (t *BashOutputTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[bashOutputArgs](args, "bash_output")
	if bad != nil {
		return *bad, nil
	}
	if t.Jobs == nil {
		return errorResult("bash_output: background jobs are not available in this environment"), nil
	}
	job, ok := t.Jobs.Get(a.BashID)
	if !ok {
		return errorResult(fmt.Sprintf("bash_output: no background command with id %q", a.BashID)), nil
	}

	wait := time.Duration(a.WaitMs) * time.Millisecond
	if wait < 0 {
		wait = 0
	}
	if wait > bashMaxTimeout {
		wait = bashMaxTimeout
	}
	if a.WaitForExit && wait == 0 {
		wait = bashOutputDefaultWait
	}
	started := time.Now()
	deadline := started.Add(wait)

	// Drain first and only then decide whether to wait, so output that is already
	// buffered is returned immediately rather than after a pointless sleep. When
	// waiting for exit, keep collecting output so nothing produced during the
	// wait is lost.
	var collected strings.Builder
	var status BashJobStatus
	var exitCode int
	var errMsg string
	for {
		collected.WriteString(job.readNew())
		status, exitCode, errMsg = job.snapshot()
		if status == BashExited || wait <= 0 || !time.Now().Before(deadline) || ctx.Err() != nil {
			break
		}
		if !a.WaitForExit && collected.Len() > 0 {
			break
		}
		job.waitForActivity(ctx, time.Until(deadline), a.WaitForExit)
	}

	// The job may have written its final lines between the read and the status
	// check above; drain once more so an exited job's last output is not left for
	// a follow-up call.
	collected.WriteString(job.readNew())
	out := truncateBashOutput(collected.String())

	var statusLine string
	if status == BashRunning {
		statusLine = fmt.Sprintf("[%s: running]", a.BashID)
	} else if errMsg != "" && exitCode != 0 {
		statusLine = fmt.Sprintf("[%s: exited code %d: %s]", a.BashID, exitCode, errMsg)
	} else {
		statusLine = fmt.Sprintf("[%s: exited code %d]", a.BashID, exitCode)
	}

	text := statusLine
	if out != "" {
		text = out + "\n" + statusLine
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(text)},
		Details: map[string]any{"bash_id": a.BashID, "status": string(status), "exitCode": exitCode, "waitedMs": time.Since(started).Milliseconds()},
	}, nil
}

// BashKillTool terminates a running background job by canceling its context.
// Jobs is the shared store the bash tool populates.
type BashKillTool struct {
	Jobs *BashJobStore
}

// bashKillArgs is the decoded argument shape for BashKillTool.
type bashKillArgs struct {
	// BashID is the job handle returned by a background `bash` call.
	BashID string `json:"bash_id"`
}

// Name implements AgentTool.
func (t *BashKillTool) Name() string { return "kill_bash" }

// Description implements AgentTool.
func (t *BashKillTool) Description() string {
	return "Terminate a background command started with bash " +
		"run_in_background=true, addressed by its bash_id. The command's " +
		"process is killed; already-exited jobs report that they were not running."
}

// Schema implements AgentTool.
func (t *BashKillTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "bash_id": {"type": "string", "description": "The bash_id returned by a background bash call."}
  },
  "required": ["bash_id"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Killing a process is a side effect.
func (t *BashKillTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

// Execute implements AgentTool. It kills the job and reports whether it had been
// running.
func (t *BashKillTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[bashKillArgs](args, "kill_bash")
	if bad != nil {
		return *bad, nil
	}
	if t.Jobs == nil {
		return errorResult("kill_bash: background jobs are not available in this environment"), nil
	}
	job, ok := t.Jobs.Get(a.BashID)
	if !ok {
		return errorResult(fmt.Sprintf("kill_bash: no background command with id %q", a.BashID)), nil
	}

	if job.kill() {
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent(fmt.Sprintf("killed background command %s", a.BashID))},
			Details: map[string]any{"bash_id": a.BashID, "killed": true},
		}, nil
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(fmt.Sprintf("background command %s was not running", a.BashID))},
		Details: map[string]any{"bash_id": a.BashID, "killed": false},
	}, nil
}
