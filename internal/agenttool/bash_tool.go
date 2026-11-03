// This file implements the bash tool (US-018): run a shell command, streaming
// stdout/stderr back as tool_execution_update partials, honoring a timeout and
// context cancellation (which kills the child's process group). A command that
// exceeds its timeout is moved to the background by the job store rather than
// killed. A non-zero exit is surfaced as an error (isError) whose message
// carries the captured output.
package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/smallnest/pigo/internal/agentcore"
)

// bashDefaultTimeout bounds a command that does not specify one.
const bashDefaultTimeout = 2 * time.Minute

// bashMaxTimeout caps any requested timeout.
const bashMaxTimeout = 10 * time.Minute

// bashMaxOutputBytes caps how many bytes of combined stdout/stderr the bash tool
// returns to the model. A single command can emit megabytes (build logs, a big
// cat), which — unlike the timeout cap — would otherwise flow into context whole
// and blow the window. Output past this size is truncated to a head + tail
// preview (see truncateBashOutput), mirroring search's searchMaxResults/"[truncated
// …]" convention. This is the tool's own inner cap; a later executor-layer budget
// may impose a stricter outer limit.
const bashMaxOutputBytes = 30_000

// truncateBashOutput caps s at bashMaxOutputBytes using the shared
// truncateToBudget idiom (head + "[truncated N bytes]" marker + tail, cut on
// UTF-8 rune boundaries). It is the bash tool's own inner cap; the executor
// layer applies a separate, uniform outer budget afterward.
func truncateBashOutput(s string) string {
	return truncateToBudget(s, bashMaxOutputBytes)
}

// trimUTF8Prefix drops trailing bytes of s that form an incomplete rune, so the
// returned prefix ends on a rune boundary.
func trimUTF8Prefix(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// trimUTF8Suffix drops leading bytes of s that form an incomplete rune, so the
// returned suffix starts on a rune boundary.
func trimUTF8Suffix(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[1:]
			continue
		}
		break
	}
	return s
}

// BashTool runs shell commands. Dir bounds the working directory (empty = the
// process CWD). Shell selects the interpreter (empty = "bash -c").
type BashTool struct {
	// Dir is the working directory for commands. Empty uses the process CWD.
	Dir string
	// Shell is the interpreter path. Empty defaults to "bash".
	Shell string
	// Jobs holds background jobs launched with run_in_background. When nil,
	// run_in_background is rejected (the front-end did not wire a store).
	Jobs *BashJobStore
}

// bashToolArgs is the decoded argument shape for BashTool.
type bashToolArgs struct {
	// Command is the shell command line to run.
	Command string `json:"command"`
	// TimeoutMs optionally overrides the default timeout (milliseconds).
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// RunInBackground detaches the command from the turn: it keeps running after
	// Execute returns, and its output is drained later via bash_output. A
	// background command has no default timeout (so dev servers/watchers run
	// indefinitely); timeout_ms still caps it if given.
	RunInBackground bool `json:"run_in_background,omitempty"`
}

// Name implements AgentTool.
func (t *BashTool) Name() string { return "bash" }

// Description implements AgentTool.
func (t *BashTool) Description() string {
	return "Run a shell command, streaming stdout/stderr. Supports a timeout " +
		"and cancellation. A non-zero exit code is reported as an error. " +
		"Set run_in_background=true for long-running commands (dev servers, " +
		"watchers): it returns immediately with a bash_id you drain with " +
		"bash_output and stop with kill_bash. A foreground command that exceeds " +
		"its timeout is automatically moved to the background instead of killed, " +
		"so you can keep draining its output and decide whether to stop it. " +
		"On Windows the command runs under bash if available (Git Bash/WSL), " +
		"else PowerShell, else cmd — prefer portable commands."
}

// Schema implements AgentTool.
func (t *BashTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command":    {"type": "string", "description": "Shell command line to run."},
    "timeout_ms": {"type": "integer", "description": "Timeout in milliseconds (capped at 10 minutes). A foreground command that reaches it is moved to the background; a background command is killed at it.", "minimum": 0},
    "run_in_background": {"type": "boolean", "description": "Run detached and return immediately with a bash_id; drain output with bash_output, stop with kill_bash."}
  },
  "required": ["command"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Commands can have side effects → sequential.
func (t *BashTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

// shellLookPath resolves a program on PATH. It is a package var so tests can
// simulate a Windows box with or without bash installed.
var shellLookPath = exec.LookPath

// resolveShell picks the interpreter and the flag that makes it read the command
// from the next argument. An explicit shell (BashTool.Shell) is always honored as
// a POSIX-style "<shell> -c <command>".
//
// On Windows with no explicit shell, the naive "bash -c" hardcode fails on stock
// machines that have no bash on PATH — the model then retries bash blindly and
// every call errors (issue #518). So we prefer a real bash when one is present
// (Git Bash / WSL / MSYS), since commands are authored in bash syntax, and fall
// back to PowerShell, then cmd, so a command still runs on a bare Windows box.
func resolveShell(explicit, goos string, lookPath func(string) (string, error)) (shell, flag string) {
	if explicit != "" {
		return explicit, "-c"
	}
	if goos == "windows" {
		if p, err := lookPath("bash"); err == nil {
			return p, "-c"
		}
		if p, err := lookPath("powershell"); err == nil {
			return p, "-Command"
		}
		return "cmd", "/C"
	}
	return "bash", "-c"
}

// Execute implements AgentTool. It streams combined stdout/stderr via onUpdate,
// enforces a timeout, and kills the process tree on context cancellation
// (Ctrl-C / a canceled run). A non-zero exit returns a Go error (→ isError)
// carrying the exit code and output.
//
// A command that outlives its timeout is not killed: it is adopted by the job
// store and keeps running in the background, returning a bash_id the model can
// drain with bash_output. That is what makes a slow build or a forgotten server
// usable instead of a hard failure; a tool without a wired store (tests, an
// embedded use) keeps the historical timeout error.
func (t *BashTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[bashToolArgs](args, "bash")
	if bad != nil {
		return *bad, nil
	}
	if a.Command == "" {
		return errorResult("bash: command is required"), nil
	}

	if a.RunInBackground {
		return t.startBackground(a)
	}

	timeout := bashDefaultTimeout
	if a.TimeoutMs > 0 {
		timeout = time.Duration(a.TimeoutMs) * time.Millisecond
	}
	if timeout > bashMaxTimeout {
		timeout = bashMaxTimeout
	}
	return t.runForeground(ctx, a, timeout, onUpdate)
}

// foregroundCommand builds a command that is deliberately NOT bound to ctx: a
// timeout hands the still-running process to the job store, and the turn context
// is canceled when that turn ends, which would kill a background job immediately.
// Cancellation is therefore handled explicitly in runForeground.
func (t *BashTool) foregroundCommand(command string) *exec.Cmd {
	shell, flag := resolveShell(t.Shell, runtime.GOOS, shellLookPath)
	cmd := exec.Command(shell, flag, command)
	if t.Dir != "" {
		cmd.Dir = t.Dir
	}
	// Own process group so a Ctrl-C/timeout kill takes the whole tree.
	configureProcessGroup(cmd)
	return cmd
}

// runForeground runs command and waits for it, a timeout, or cancellation.
func (t *BashTool) runForeground(ctx context.Context, a bashToolArgs, timeout time.Duration, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	cmd := t.foregroundCommand(a.Command)

	job := newBashJob(a.Command)
	job.setOnUpdate(onUpdate)
	w := job.writer()
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		job.finish(-1, err.Error())
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("")}},
				fmt.Errorf("bash: could not start shell %q: %v. On Windows install Git Bash or WSL (or configure a shell); commands are bash syntax", cmd.Args[0], execErr.Err)
		}
		return errorResult(fmt.Sprintf("bash: could not start command: %v", err)), nil
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-waitCh:
		return finishForegroundJob(job, err)
	case <-ctx.Done():
		// A command that finished at the same moment wins the race: report its
		// real result instead of a cancellation it did not suffer.
		select {
		case err := <-waitCh:
			return finishForegroundJob(job, err)
		default:
		}
		return cancelForeground(cmd, job, waitCh)
	case <-timer.C:
		select {
		case err := <-waitCh:
			return finishForegroundJob(job, err)
		default:
		}
		return t.backgroundAfterTimeout(cmd, job, waitCh, timeout)
	}
}

// finishForegroundJob records the terminal state and shapes the ordinary
// success/failure result.
func finishForegroundJob(job *BashJob, err error) (agentcore.AgentToolResult, error) {
	code, msg := bashExitInfo(err)
	job.finish(code, msg)
	output := truncateBashOutput(job.fullOutput())

	if err != nil {
		return agentcore.AgentToolResult{
				Content: agentcore.ContentList{agentcore.NewTextContent(output)},
				Details: map[string]any{"exitCode": code},
			},
			fmt.Errorf("bash: command exited with code %d\n%s", code, output)
	}

	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(output)},
		Details: map[string]any{"exitCode": 0},
	}, nil
}

// cancelForeground stops a foreground command because its context was canceled
// (Ctrl-C, or the run being torn down) and reports the captured output. The whole
// process group is killed so a helper the shell spawned does not keep running
// after the tool call has failed.
func cancelForeground(cmd *exec.Cmd, job *BashJob, waitCh <-chan error) (agentcore.AgentToolResult, error) {
	job.setOnUpdate(nil)
	killProcessTree(cmd)
	code, msg := bashExitInfo(<-waitCh)
	job.finish(code, msg)
	output := truncateBashOutput(job.fullOutput())
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(output)}},
		fmt.Errorf("bash: command canceled\n%s", output)
}

// backgroundAfterTimeout keeps a foreground command that outlived timeout alive:
// it moves the job into the store so bash_output can drain it and kill_bash can
// stop it. Without a store there is nothing to address it by, so it is killed
// and reported as a timeout exactly as before.
func (t *BashTool) backgroundAfterTimeout(cmd *exec.Cmd, job *BashJob, waitCh <-chan error, timeout time.Duration) (agentcore.AgentToolResult, error) {
	job.setOnUpdate(nil)

	if t.Jobs == nil {
		killProcessTree(cmd)
		code, msg := bashExitInfo(<-waitCh)
		job.finish(code, msg)
		output := truncateBashOutput(job.fullOutput())
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(output)}},
			fmt.Errorf("bash: command timed out after %s\n%s", timeout, output)
	}

	t.Jobs.register(job, func() { killProcessTree(cmd) })
	go func() {
		code, msg := bashExitInfo(<-waitCh)
		job.finish(code, msg)
	}()

	reason := fmt.Sprintf("bash: TIMEOUT — the command was still running after %s, so it was moved to the background instead of being killed (it keeps running).", timeout)
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(backgroundNotice(reason, job))},
		Details: map[string]any{"bash_id": job.ID, "background": true, "timedOut": true},
	}, nil
}

// backgroundNotice formats the result of a command that is now running in the
// background: what happened to it (a timeout, or an explicit run_in_background
// request), its bash_id, and the exact calls that follow it. A foreground
// command that outlived its timeout has already consumed the turn's worth of
// time, so this notice is the only place the model learns the job still exists
// and how to reach its output or wait for it — write it as instructions, not as
// decoration.
func backgroundNotice(reason string, job *BashJob) string {
	var b strings.Builder
	b.WriteString(reason)
	fmt.Fprintf(&b, "\nbash_id: %s\n\n", job.ID)
	b.WriteString("Follow-up calls:\n")
	fmt.Fprintf(&b, "  bash_output {\"bash_id\": %q}  — output collected so far, returns immediately\n", job.ID)
	fmt.Fprintf(&b, "  bash_output {\"bash_id\": %q, \"wait_ms\": 30000}  — wait up to 30s for new output, then return\n", job.ID)
	fmt.Fprintf(&b, "  bash_output {\"bash_id\": %q, \"wait_for_exit\": true, \"wait_ms\": 120000}  — wait until it finishes\n", job.ID)
	fmt.Fprintf(&b, "  kill_bash {\"bash_id\": %q}  — stop it\n", job.ID)
	if partial := truncateBashOutput(job.fullOutput()); partial != "" {
		b.WriteString("\npartial output so far:\n")
		b.WriteString(partial)
	}
	return b.String()
}

// bashExitInfo maps the error from cmd.Wait to an exit code and message.
func bashExitInfo(err error) (code int, msg string) {
	if err == nil {
		return 0, ""
	}
	code = -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return code, err.Error()
}

// startBackground launches the command detached from the turn context and
// returns immediately with a bash_id. The job runs under its own cancelable
// context (rooted at context.Background(), not the turn ctx which is canceled
// when the turn ends), so it survives past Execute. A background command has no
// default timeout — a dev server or watcher is expected to run indefinitely —
// but an explicit timeout_ms still caps it. Its combined output accumulates in
// the job's buffer for bash_output to drain; kill_bash cancels its context.
func (t *BashTool) startBackground(a bashToolArgs) (agentcore.AgentToolResult, error) {
	if t.Jobs == nil {
		return errorResult("bash: run_in_background is not available in this environment"), nil
	}

	var jobCtx context.Context
	var cancelCtx context.CancelFunc
	if a.TimeoutMs > 0 {
		timeout := time.Duration(a.TimeoutMs) * time.Millisecond
		if timeout > bashMaxTimeout {
			timeout = bashMaxTimeout
		}
		jobCtx, cancelCtx = context.WithTimeout(context.Background(), timeout)
	} else {
		jobCtx, cancelCtx = context.WithCancel(context.Background())
	}

	shell, flag := resolveShell(t.Shell, runtime.GOOS, shellLookPath)
	cmd := exec.CommandContext(jobCtx, shell, flag, a.Command)
	if t.Dir != "" {
		cmd.Dir = t.Dir
	}
	configureProcessGroup(cmd)
	// CommandContext's default Cancel kills only the shell; kill the group too so
	// an explicit timeout_ms does not leave helpers behind.
	cmd.Cancel = func() error {
		killProcessTree(cmd)
		return nil
	}

	// kill_bash/KillAll go through this func so stopping a job takes the whole
	// process tree with it.
	job := t.Jobs.create(a.Command, func() {
		cancelCtx()
		killProcessTree(cmd)
	})
	w := job.writer()
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		cancelCtx()
		job.finish(-1, err.Error())
		return errorResult(fmt.Sprintf("bash: could not start background command: %v", err)), nil
	}

	go func() {
		err := cmd.Wait()
		cancelCtx()
		code, msg := bashExitInfo(err)
		job.finish(code, msg)
	}()

	reason := fmt.Sprintf("bash: started in the background (no default timeout; it keeps running until it exits or kill_bash stops it): %s", a.Command)
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(backgroundNotice(reason, job))},
		Details: map[string]any{"bash_id": job.ID, "background": true},
	}, nil
}
