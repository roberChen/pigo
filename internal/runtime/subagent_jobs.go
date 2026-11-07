// This file implements async sub-agent jobs: a sub-agent dispatched with
// `mode: "async"` starts on its own goroutine, the tool call returns at once
// with a job id, and the child's report is delivered as a follow-up user turn when
// it finishes (RunConfig.GetFollowUpMessages). The model can also poll, or wait for
// one job or a whole batch, explicitly with the task_output tool.
//
// Lifetime: a job runs on the parent RUN's context, not the tool call's. It keeps
// running while the parent does other work, and is cancelled when the run is
// (Esc / Ctrl+C / a cancelled run). Because the follow-up seam waits for the next
// pending completion before letting the run settle, work the model started is
// never silently dropped: the run ends only once every background job has been
// reported (or the run is cancelled).
//
// Persistence is unchanged: an async child still writes its own session (parented
// to this conversation), and its report carries the session id so the parent can
// continue it later.
package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// JobStatus is the lifecycle state of a background sub-agent job.
type JobStatus string

const (
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
)

// subAgentJob is one background sub-agent run.
type subAgentJob struct {
	id          string
	agent       string
	description string
	prompt      string
	startedAt   time.Time

	mu        sync.Mutex
	status    JobStatus
	activity  string
	sessionID string
	result    string
	err       error
	delivered bool

	done chan struct{}
}

// SubAgentJobs is the per-run registry of background sub-agent jobs. It is created
// by the tool assembly and shared by every sub-agent tool plus the follow-up seam.
type SubAgentJobs struct {
	mu      sync.Mutex
	seq     int
	jobs    map[string]*subAgentJob
	order   []string
	changed chan struct{} // closed and replaced on every state change (broadcast)
}

// NewSubAgentJobs returns an empty registry.
func NewSubAgentJobs() *SubAgentJobs {
	return &SubAgentJobs{jobs: make(map[string]*subAgentJob), changed: make(chan struct{})}
}

// Start launches run on its own goroutine and returns the job handle. ctx is the
// parent RUN's context (not the tool call's), so the job survives the tool call and
// dies with the run. run receives the job id (used as the progress-event id) and
// must return the child's report text, its session id ("") and an error.
func (r *SubAgentJobs) Start(agent, description, prompt string, ctx context.Context, run func(ctx context.Context, jobID string) (text, sessionID string, err error)) *subAgentJob {
	r.mu.Lock()
	r.seq++
	job := &subAgentJob{
		id:          fmt.Sprintf("job-%d", r.seq),
		agent:       agent,
		description: strings.TrimSpace(description),
		prompt:      prompt,
		startedAt:   time.Now(),
		status:      JobRunning,
		activity:    "Starting",
		done:        make(chan struct{}),
	}
	r.jobs[job.id] = job
	r.order = append(r.order, job.id)
	r.broadcastLocked()
	r.mu.Unlock()

	go func() {
		text, sessionID, err := run(ctx, job.id)
		r.finish(job, text, sessionID, err)
	}()
	return job
}

// finish records a job's outcome and wakes anyone waiting on the registry.
func (r *SubAgentJobs) finish(job *subAgentJob, text, sessionID string, err error) {
	job.mu.Lock()
	job.result = text
	job.sessionID = sessionID
	job.err = err
	if err != nil {
		job.status = JobFailed
	} else {
		job.status = JobDone
	}
	job.mu.Unlock()
	close(job.done)
	r.mu.Lock()
	r.broadcastLocked()
	r.mu.Unlock()
}

// broadcastLocked closes the current change channel and installs a fresh one, the
// usual broadcast idiom. Callers hold r.mu.
func (r *SubAgentJobs) broadcastLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// Get returns a job by id.
func (r *SubAgentJobs) Get(id string) (*subAgentJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	return job, ok
}

// List returns the jobs in start order.
func (r *SubAgentJobs) List() []*subAgentJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*subAgentJob, 0, len(r.order))
	for _, id := range r.order {
		if job, ok := r.jobs[id]; ok {
			out = append(out, job)
		}
	}
	return out
}

// SubAgentJobsFromTools returns the registry a tool set's sub-agent tools share, or
// nil when the set has none. Drivers use it to wire the follow-up seam without
// threading the registry through their own options.
func SubAgentJobsFromTools(tools []agentcore.AgentTool) *SubAgentJobs {
	for _, tool := range tools {
		if st, ok := tool.(*SubAgentTool); ok && st.spec.Jobs != nil {
			return st.spec.Jobs
		}
	}
	return nil
}

// ID returns the job's id.
func (j *subAgentJob) ID() string { return j.id }

// StartedAt returns when the job was launched.
func (j *subAgentJob) StartedAt() time.Time { return j.startedAt }

// Agent returns the agent name ("task" for the generic sub-agent).
func (j *subAgentJob) Agent() string { return j.agent }

// Description returns the short task description, falling back to the prompt.
func (j *subAgentJob) Description() string {
	if j.description != "" {
		return j.description
	}
	return j.prompt
}

// Snapshot returns the job's current status, activity, report and error.
func (j *subAgentJob) Snapshot() (status JobStatus, activity, result, sessionID string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.activity, j.result, j.sessionID, j.err
}

// setActivity records what the child is doing, for the status panel and
// task_output.
func (j *subAgentJob) setActivity(activity string) {
	if activity == "" {
		return
	}
	j.mu.Lock()
	j.activity = activity
	j.mu.Unlock()
}

// wait blocks until the job settles or ctx is done. It reports whether the job
// finished.
func (j *subAgentJob) wait(ctx context.Context) bool {
	select {
	case <-j.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitJobs waits until every job in jobs has settled or ctx is done, and reports
// whether all of them settled. The jobs are awaited together, not one after the
// other: the loop wakes on any registry state change, so a batch barrier returns
// as soon as the last job finishes, whatever order they complete in. The caller's
// slice is a snapshot - a job started after the call is not part of this barrier.
func (r *SubAgentJobs) waitJobs(ctx context.Context, jobs []*subAgentJob) bool {
	for {
		// Capture the change channel before inspecting the jobs: a completion
		// broadcasts by closing the captured channel, so a finish that races this
		// check either leaves the job settled (loop exits) or closes the channel we
		// are about to wait on (loop re-checks) - never a missed wakeup.
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()
		pending := false
		for _, job := range jobs {
			if job == nil {
				continue
			}
			job.mu.Lock()
			running := job.status == JobRunning
			job.mu.Unlock()
			if running {
				pending = true
				break
			}
		}
		if !pending {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// markRead marks settled jobs as delivered so their report is not injected again as
// a follow-up turn. A job still running stays eligible: the caller only saw status.
func (r *SubAgentJobs) markRead(jobs []*subAgentJob) {
	for _, job := range jobs {
		if job == nil {
			continue
		}
		job.mu.Lock()
		if job.status != JobRunning {
			job.delivered = true
		}
		job.mu.Unlock()
	}
}

// deliverCompleted returns the finished, not-yet-delivered jobs' reports as user
// messages and marks them delivered.
func (r *SubAgentJobs) deliverCompleted() []agentcore.AgentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var msgs []agentcore.AgentMessage
	for _, id := range r.order {
		job := r.jobs[id]
		if job == nil {
			continue
		}
		job.mu.Lock()
		if job.delivered || job.status == JobRunning {
			job.mu.Unlock()
			continue
		}
		job.delivered = true
		msg := job.describeMessageLocked()
		job.mu.Unlock()
		msgs = append(msgs, msg)
	}
	return msgs
}

// describeMessageLocked renders the follow-up user message for a finished job.
// Callers hold job.mu.
func (j *subAgentJob) describeMessageLocked() agentcore.AgentMessage {
	var b strings.Builder
	b.WriteString("A background sub-agent you started has finished.\n")
	fmt.Fprintf(&b, "job: %s (agent: %s, ran %s)\n", j.id, j.agent, time.Since(j.startedAt).Round(time.Second))
	if task := clipLine(j.Description(), 160); task != "" {
		fmt.Fprintf(&b, "task: %s\n", task)
	}
	if j.err != nil {
		b.WriteString("status: failed\n")
		b.WriteString("error: " + j.err.Error())
	} else {
		b.WriteString("status: done\n")
		b.WriteString("report:\n" + j.result)
	}
	// Origin marks this as a harness-injected turn, not something the human typed:
	// the model must see it as input, but a resumed session must not replay the
	// child's report behind a "you" gutter as if the user had said it.
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(b.String())},
		Origin:    agentcore.UserMessageOriginSubAgentReport,
	}
}

// pending reports whether any job is still running.
func (r *SubAgentJobs) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range r.jobs {
		job.mu.Lock()
		running := job.status == JobRunning
		job.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

// FollowUpMessages is the RunConfig.GetFollowUpMessages seam for background jobs.
// It delivers every finished job's report as a user turn, and, when work is still
// outstanding, waits for the next completion so the run does not settle while the
// model has sub-agents running. A cancelled run stops waiting.
func (r *SubAgentJobs) FollowUpMessages(ctx context.Context, _ *agentcore.AgentContext) []agentcore.AgentMessage {
	if r == nil {
		return nil
	}
	if msgs := r.deliverCompleted(); len(msgs) > 0 {
		return msgs
	}
	if !r.pending() {
		return nil
	}
	// Wait for the next state change (a completion), bounded by the run's ctx.
	r.mu.Lock()
	changed := r.changed
	r.mu.Unlock()
	select {
	case <-changed:
	case <-ctx.Done():
		return nil
	}
	return r.deliverCompleted()
}

// statusLines renders the registry as a table for task_output (no job id given).
func (r *SubAgentJobs) statusLines() []string {
	jobs := r.List()
	if len(jobs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(jobs))
	for _, job := range jobs {
		status, activity, result, _, err := job.Snapshot()
		line := fmt.Sprintf("%s  %-6s  %-10s  %-8s  %s", job.id, job.agent, status, time.Since(job.StartedAt()).Round(time.Second), clipLine(job.Description(), 80))
		if status == JobRunning && activity != "" {
			line += " (" + activity + ")"
		}
		if err != nil {
			line += " [" + clipLine(err.Error(), 80) + "]"
		}
		if status == JobDone && len(result) == 0 {
			line += " (no output)"
		}
		lines = append(lines, line)
	}
	return lines
}

// sortedJobIDs returns the registry's job ids sorted, for deterministic tests and
// diagnostics.
func (r *SubAgentJobs) sortedJobIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, len(r.order))
	copy(ids, r.order)
	sort.Strings(ids)
	return ids
}

// clipLine collapses whitespace and truncates s to max runes, for one-line status
// and message rendering.
func clipLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
