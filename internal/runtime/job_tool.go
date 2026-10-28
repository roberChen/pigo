// This file implements the task_output tool: the model's handle on the background
// sub-agents it started. Without a job id it lists every job with its status and
// current activity; with a job id it returns that job's report (waiting for it when
// `wait` is set). Reports are normally delivered automatically as follow-up turns,
// so this tool is for polling, for waiting on a specific job, and for re-reading a
// report that has already arrived.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// taskOutputArgs is the JSON argument shape for task_output.
type taskOutputArgs struct {
	JobID string `json:"job_id"`
	Wait  bool   `json:"wait"`
}

// TaskOutputTool exposes the background sub-agent job registry to the model.
type TaskOutputTool struct {
	Jobs *SubAgentJobs
}

// Name implements AgentTool.
func (t *TaskOutputTool) Name() string { return "task_output" }

// Description implements AgentTool.
func (t *TaskOutputTool) Description() string {
	return "Inspect the background sub-agents you started. With no job_id it lists every job " +
		"(id, agent, status, activity). With a job_id it returns that job's report, or its current " +
		"status while it still runs; set wait true to block until it finishes. Reports also arrive " +
		"automatically as a follow-up message, so use this only to check progress, to wait for a " +
		"specific job, or to re-read a report."
}

// Schema implements AgentTool.
func (t *TaskOutputTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "job_id": {"type": "string", "description": "Background job id (for example job-1). Omit to list every job."},
    "wait": {"type": "boolean", "description": "Wait for the job to finish before returning (default false)."}
  },
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Waiting must not interleave with another
// tool's prompt, so the batch runs sequentially.
func (t *TaskOutputTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

// Execute implements AgentTool.
func (t *TaskOutputTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var a taskOutputArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return agentcore.AgentToolResult{}, fmt.Errorf("task_output: decode args: %w", err)
		}
	}
	if t.Jobs == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("task_output: background sub-agents are not configured for this run")
	}
	id := strings.TrimSpace(a.JobID)
	if id == "" {
		lines := t.Jobs.statusLines()
		if len(lines) == 0 {
			return agentcore.AgentToolResult{}, fmt.Errorf("task_output: no background sub-agents have been started")
		}
		return agentcore.AgentToolResult{Content: agentcore.ContentList{
			agentcore.NewTextContent("background sub-agents:\n  " + strings.Join(lines, "\n  ")),
		}}, nil
	}
	job, ok := t.Jobs.Get(id)
	if !ok {
		return agentcore.AgentToolResult{}, fmt.Errorf("task_output: unknown job %q (use task_output without a job_id to list jobs)", id)
	}
	if a.Wait {
		if !job.wait(ctx) {
			if ctx.Err() != nil {
				return agentcore.AgentToolResult{}, fmt.Errorf("task_output: cancelled while waiting for %s: %w", id, ctx.Err())
			}
		}
	}
	status, activity, result, sessionID, err := job.Snapshot()
	if status == JobRunning {
		line := fmt.Sprintf("job %s (agent %s) is still running", id, job.Agent())
		if activity != "" {
			line += " (" + activity + ")"
		}
		line += fmt.Sprintf(" after %s; its report will arrive as a follow-up message.", time.Since(job.StartedAt()).Round(time.Second))
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(line)}}, nil
	}
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("job %s (agent %s) failed: %w", id, job.Agent(), err)
	}
	var details any
	if sessionID != "" {
		details = map[string]any{"jobId": id, "agent": job.Agent(), "sessionId": sessionID}
	} else {
		details = map[string]any{"jobId": id, "agent": job.Agent()}
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(result)}, Details: details}, nil
}
