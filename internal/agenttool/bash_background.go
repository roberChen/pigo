// This file implements background bash execution: a BashJobStore holding the
// commands launched with run_in_background (and commands a foreground call moved
// to the background when it timed out), and the BashJob state each one carries.
// A background job is detached from the turn context (which is canceled when the
// turn ends) and runs under its own cancelable context until it exits or
// kill_bash stops it. Its combined stdout/stderr accumulates in a buffer that
// bash_output drains incrementally, mirroring Claude Code's background shells +
// BashOutput/KillShell.
package agenttool

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// BashJobStatus is the lifecycle state of a background bash job.
type BashJobStatus string

const (
	// BashRunning means the command is still executing.
	BashRunning BashJobStatus = "running"
	// BashExited means the command finished (successfully or not) or was killed.
	BashExited BashJobStatus = "exited"
)

// BashJob is a single background command: its identity, growing combined output,
// and terminal status. All fields are guarded by mu so the running command's
// writer, bash_output reads (including its wait window), and kill_bash can touch
// it concurrently.
type BashJob struct {
	// ID is the stable handle (e.g. "bash_1") bash_output/kill_bash address. It is
	// empty until the job is registered with a store; a foreground command that
	// finishes in time never gets one.
	ID string
	// Command is the shell command line, kept for listing/display.
	Command string
	// StartedAt is when the command was launched.
	StartedAt time.Time

	mu       sync.Mutex
	buf      bytes.Buffer
	cursor   int // bytes of buf already returned by bash_output
	status   BashJobStatus
	exitCode int
	errMsg   string
	finished time.Time
	cancel   context.CancelFunc
	// onUpdate streams partial output to the in-flight tool call. It is set only
	// while a foreground call owns the job and cleared when the job is detached,
	// so late output never tries to update a turn that already ended.
	onUpdate agentcore.ToolUpdateFunc
	// changed is closed and replaced whenever new output arrives, letting a
	// waiting bash_output wake immediately instead of polling.
	changed chan struct{}
	// done is closed once the command has exited, so a waiting bash_output can
	// block for completion.
	done chan struct{}
}

// newBashJob returns a running, unregistered job for command. Registration (and
// id assignment) is a separate step so the bash tool can run a command as a
// normal foreground call and only adopt it into the store if it times out.
func newBashJob(command string) *BashJob {
	return &BashJob{
		Command:   command,
		StartedAt: time.Now(),
		status:    BashRunning,
		changed:   make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// jobWriter adapts a BashJob to io.Writer so it can be a command's Stdout/Stderr;
// each write appends to the job's combined buffer under its lock, wakes any
// waiting bash_output, and forwards a growing snapshot to the streaming callback.
type jobWriter struct{ job *BashJob }

func (w jobWriter) Write(p []byte) (int, error) {
	j := w.job
	j.mu.Lock()
	j.buf.Write(p)
	onUpdate := j.onUpdate
	var snapshot string
	if onUpdate != nil {
		snapshot = j.buf.String()
	}
	// Signal new output: close the current channel and replace it, so every
	// waiter that captured the old one wakes and the next waiter blocks afresh.
	close(j.changed)
	j.changed = make(chan struct{})
	j.mu.Unlock()

	if onUpdate != nil {
		onUpdate(agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(snapshot)}})
	}
	return len(p), nil
}

// writer returns an io.Writer that appends to the job's output buffer.
func (j *BashJob) writer() jobWriter { return jobWriter{job: j} }

// setOnUpdate installs (or, with nil, clears) the streaming callback. It is
// cleared when a foreground call detaches the job into the background.
func (j *BashJob) setOnUpdate(fn agentcore.ToolUpdateFunc) {
	j.mu.Lock()
	j.onUpdate = fn
	j.mu.Unlock()
}

// finish records the command's terminal state from the error returned by
// cmd.Wait (nil = success) and wakes anyone waiting for completion. It is
// idempotent: a second call (e.g. after a kill) does not re-close done.
func (j *BashJob) finish(exitCode int, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == BashExited {
		return
	}
	j.status = BashExited
	j.exitCode = exitCode
	j.errMsg = errMsg
	j.finished = time.Now()
	close(j.changed)
	j.changed = make(chan struct{})
	close(j.done)
}

// kill cancels the job's context (terminating the process tree) and reports
// whether the job was running when called.
func (j *BashJob) kill() bool {
	j.mu.Lock()
	running := j.status == BashRunning
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return running
}

// readNew returns the output accumulated since the last read and advances the
// cursor, so successive bash_output calls stream the command's output without
// repeating what was already seen.
func (j *BashJob) readNew() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	all := j.buf.Bytes()
	if j.cursor > len(all) {
		j.cursor = len(all)
	}
	out := string(all[j.cursor:])
	j.cursor = len(all)
	return out
}

// fullOutput returns everything the command has written so far without moving
// the drain cursor. It is used to report the partial output of a foreground
// command that timed out (the output is then also available to bash_output).
func (j *BashJob) fullOutput() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.String()
}

// snapshot returns the job's current status fields for reporting without exposing
// the mutex-guarded internals.
func (j *BashJob) snapshot() (status BashJobStatus, exitCode int, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.exitCode, j.errMsg
}

// Status returns whether the command is still running or has exited.
func (j *BashJob) Status() BashJobStatus {
	status, _, _ := j.snapshot()
	return status
}

// ExitCode returns the command's exit code; it is meaningless until Status
// reports BashExited.
func (j *BashJob) ExitCode() int {
	_, code, _ := j.snapshot()
	return code
}

// ErrorText returns the wait error of a command that exited non-zero ("" for a
// clean exit).
func (j *BashJob) ErrorText() string {
	_, _, msg := j.snapshot()
	return msg
}

// Elapsed returns how long the command ran, or has been running.
func (j *BashJob) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == BashExited && !j.finished.IsZero() {
		return j.finished.Sub(j.StartedAt)
	}
	return time.Since(j.StartedAt)
}

// UnreadBytes returns how much output has accumulated that bash_output has not
// returned yet, so a status listing can hint whether there is something to read.
func (j *BashJob) UnreadBytes() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.Len() - j.cursor
}

// waitForActivity blocks until there is something a waiter cares about, the
// context is canceled, or timeout elapses. With forExit it waits for the command
// to finish (output alone does not wake it); otherwise it wakes as soon as new
// output arrives or the job exits. It returns immediately when the relevant
// event has already happened, so callers can loop: read, wait, read.
func (j *BashJob) waitForActivity(ctx context.Context, timeout time.Duration, forExit bool) {
	if timeout <= 0 {
		return
	}
	j.mu.Lock()
	exited := j.status == BashExited
	pending := j.cursor < j.buf.Len()
	changed := j.changed
	done := j.done
	j.mu.Unlock()

	if exited {
		return
	}
	if !forExit && pending {
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	if forExit {
		select {
		case <-done:
		case <-ctx.Done():
		case <-timer.C:
		}
		return
	}
	select {
	case <-changed:
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// BashJobStore holds a session's background bash jobs. A single store is shared by
// the bash, bash_output and kill_bash tools so a job launched by one is visible to
// the others. It is safe for concurrent use.
type BashJobStore struct {
	mu   sync.Mutex
	jobs map[string]*BashJob
	seq  int
}

// NewBashJobStore returns an empty store.
func NewBashJobStore() *BashJobStore {
	return &BashJobStore{jobs: map[string]*BashJob{}}
}

// register adopts a running job, assigning its sequential id and the cancel func
// that stops it. It is how a foreground command that outlived its timeout becomes
// addressable as bash_<n>.
func (s *BashJobStore) register(job *BashJob, cancel context.CancelFunc) *BashJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	job.mu.Lock()
	job.ID = fmt.Sprintf("bash_%d", s.seq)
	job.cancel = cancel
	job.mu.Unlock()
	s.jobs[job.ID] = job
	return job
}

// create registers a new running job for command with its cancel func, assigning
// a readable sequential id.
func (s *BashJobStore) create(command string, cancel context.CancelFunc) *BashJob {
	return s.register(newBashJob(command), cancel)
}

// Get returns the job with the given id, or (nil, false).
func (s *BashJobStore) Get(id string) (*BashJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

// List returns the jobs in creation order.
func (s *BashJobStore) List() []*BashJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*BashJob, 0, len(s.jobs))
	for i := 1; i <= s.seq; i++ {
		if j, ok := s.jobs[fmt.Sprintf("bash_%d", i)]; ok {
			out = append(out, j)
		}
	}
	return out
}

// KillAll cancels every still-running job. It is intended for session shutdown so
// background processes are not orphaned.
func (s *BashJobStore) KillAll() {
	for _, j := range s.List() {
		j.kill()
	}
}
