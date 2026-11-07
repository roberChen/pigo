// This file implements the task_output tool: the model's handle on the async
// sub-agents it started. With no job id it lists every job with its status and
// current activity; with job_id it returns one job's report; with job_ids it reads
// or awaits a whole batch; with wait_all it waits for every job running at the time
// of the call. A batch read is the fan-in barrier after a parallel fan-out: the
// model gets every report in one result instead of one follow-up turn per child.
// Reports are normally delivered automatically as follow-up turns, so this tool is
// for polling, for waiting, and for re-reading a report that has already arrived.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// taskOutputArgs is the JSON argument shape for task_output. job_id (one) and
// job_ids (a batch) are alternative selectors; wait_all selects every running job.
type taskOutputArgs struct {
	JobID   string   `json:"job_id"`
	JobIDs  []string `json:"job_ids"`
	Wait    bool     `json:"wait"`
	WaitAll bool     `json:"wait_all"`
}

// TaskOutputTool exposes the background sub-agent job registry to the model.
type TaskOutputTool struct {
	Jobs *SubAgentJobs
}

// Name implements AgentTool.
func (t *TaskOutputTool) Name() string { return "task_output" }

// Description implements AgentTool.
func (t *TaskOutputTool) Description() string {
	return "Inspect the sub-agents you started with mode \"async\". With no job_id/job_ids it lists every job " +
		"(id, agent, status, activity). With job_id it returns one job's report; with job_ids it returns a whole " +
		"batch in the order given, reporting a failed job inline so one failure does not hide the others. Set wait " +
		"true to block until those jobs finish - the fan-in barrier after a parallel fan-out. Set wait_all true to " +
		"wait for every job running right now and return all their reports. Reading a finished job here marks it read, " +
		"so it is not delivered again as a follow-up message; reports also arrive automatically as follow-up messages " +
		"when you do not collect them."
}

// Schema implements AgentTool.
func (t *TaskOutputTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "job_id": {"type": "string", "description": "One background job id (for example job-1). Omit it (or use job_ids for a batch) to list every job."},
    "job_ids": {"type": "array", "items": {"type": "string"}, "description": "A batch of job ids to read or await together, e.g. every id returned by a parallel fan-out. Cannot be combined with job_id."},
    "wait": {"type": "boolean", "description": "Block until the requested job(s) finish before returning (default false). With job_ids this waits for all of them."},
    "wait_all": {"type": "boolean", "description": "Wait for every job running at the time of the call, then return all their reports. Cannot be combined with job_id/job_ids."}
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
	ids := cleanJobIDs(a.JobIDs)
	if id != "" && len(ids) > 0 {
		return agentcore.AgentToolResult{}, fmt.Errorf("task_output: pass either job_id or job_ids, not both (job_ids waits for the whole batch)")
	}
	if a.WaitAll && (id != "" || len(ids) > 0) {
		return agentcore.AgentToolResult{}, fmt.Errorf("task_output: wait_all already covers every job; do not combine it with job_id/job_ids")
	}
	if a.Wait && id == "" && len(ids) == 0 {
		return agentcore.AgentToolResult{}, fmt.Errorf("task_output: wait needs job_id or job_ids; use wait_all to await every running job")
	}
	if a.WaitAll {
		jobs := t.Jobs.List()
		if len(jobs) == 0 {
			return agentcore.AgentToolResult{}, fmt.Errorf("task_output: no background sub-agents have been started")
		}
		// The set is snapshotted before waiting: a job launched after this call
		// belongs to a later barrier, not this one.
		if !t.Jobs.waitJobs(ctx, jobs) && ctx.Err() != nil {
			return agentcore.AgentToolResult{}, fmt.Errorf("task_output: cancelled while waiting for the batch: %w", ctx.Err())
		}
		t.Jobs.markRead(jobs)
		return batchJobResult(jobs), nil
	}
	if len(ids) > 0 {
		jobs := make([]*subAgentJob, 0, len(ids))
		for _, jobID := range ids {
			job, ok := t.Jobs.Get(jobID)
			if !ok {
				return agentcore.AgentToolResult{}, fmt.Errorf("task_output: unknown job %q (use task_output without a job_id to list jobs)", jobID)
			}
			jobs = append(jobs, job)
		}
		if a.Wait {
			if !t.Jobs.waitJobs(ctx, jobs) && ctx.Err() != nil {
				return agentcore.AgentToolResult{}, fmt.Errorf("task_output: cancelled while waiting for the batch: %w", ctx.Err())
			}
		}
		t.Jobs.markRead(jobs)
		return batchJobResult(jobs), nil
	}
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
	// A settled report read through this tool counts as delivered: the model has
	// it in the transcript, so the follow-up seam must not inject it a second time.
	if status != JobRunning {
		t.Jobs.markRead([]*subAgentJob{job})
	}
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

// cleanJobIDs trims the requested ids, dropping blanks and duplicates while
// preserving the order the model asked for.
func cleanJobIDs(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// batchJobResult renders several jobs as one result: a header with the counts, then
// one section per job in the order given. A failed job is reported inline (with its
// error) rather than turning the whole call into an error, so one failure in a
// fan-out does not hide the reports the other children produced. Details carries
// the same information structurally.
func batchJobResult(jobs []*subAgentJob) agentcore.AgentToolResult {
	type jobEntry struct {
		job       *subAgentJob
		status    JobStatus
		activity  string
		result    string
		sessionID string
		err       error
		elapsed   time.Duration
	}
	entries := make([]jobEntry, 0, len(jobs))
	done, failed, running := 0, 0, 0
	for _, job := range jobs {
		status, activity, result, sessionID, err := job.Snapshot()
		switch status {
		case JobDone:
			done++
		case JobFailed:
			failed++
		default:
			running++
		}
		entries = append(entries, jobEntry{
			job: job, status: status, activity: activity, result: result,
			sessionID: sessionID, err: err,
			elapsed: time.Since(job.StartedAt()).Round(time.Second),
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "batch of %d sub-agent jobs: %d done", len(entries), done)
	if failed > 0 {
		fmt.Fprintf(&b, ", %d failed", failed)
	}
	if running > 0 {
		fmt.Fprintf(&b, ", %d still running", running)
	}
	b.WriteByte(':')
	details := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "%s (agent %s) %s after %s", e.job.ID(), e.job.Agent(), e.status, e.elapsed)
		if task := clipLine(e.job.Description(), 160); task != "" {
			fmt.Fprintf(&b, " — %s", task)
		}
		switch e.status {
		case JobRunning:
			if e.activity != "" {
				fmt.Fprintf(&b, "\nstill working (%s); its report will arrive as a follow-up message.", e.activity)
			} else {
				b.WriteString("\nstill working; its report will arrive as a follow-up message.")
			}
		case JobFailed:
			fmt.Fprintf(&b, "\nerror: %s", e.err)
		default:
			if e.result == "" {
				b.WriteString("\n(no output)")
			} else {
				b.WriteString("\nreport:\n" + e.result)
			}
		}
		entry := map[string]any{
			"jobId":  e.job.ID(),
			"agent":  e.job.Agent(),
			"status": string(e.status),
		}
		if e.sessionID != "" {
			entry["sessionId"] = e.sessionID
		}
		if e.err != nil {
			entry["error"] = e.err.Error()
		}
		details = append(details, entry)
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(b.String())},
		Details: map[string]any{"jobs": details},
	}
}
