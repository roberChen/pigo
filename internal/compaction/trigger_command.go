// This file implements the user-defined trigger command: the shell-command form
// of the pluggable compaction trigger (see trigger.go for the contract it fills).
//
// A command is the only policy form pigo needs to support: it can be written in
// any language, it reads the same bounded record of the upstream response the
// Go-level TriggerFunc sees, and it needs no build step. The wire contract is
// one JSON document in on stdin, one JSON decision out on stdout:
//
//	{"contextTokens":150000,"contextWindow":1000000,"inputBudget":512000,...}
//	{"compact":true,"reason":"remaining quota header says 12% left"}
//
// Failure is deliberately quiet and fail-open: a command that cannot start,
// times out, exits non-zero, or prints something unparseable contributes
// Decided=false, so the fixed threshold policy decides. A broken policy must
// never be able to stop a run from compacting when it is over budget — the
// provider would then reject the next request, which is a hard failure.
package compaction

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
)

// DefaultTriggerTimeout bounds one policy invocation. It is short on purpose:
// the command runs on the loop's goroutine before a request, so a hung policy
// delays the conversation. It is generous enough for a local script and for one
// small HTTP call to a gateway.
const DefaultTriggerTimeout = 10 * time.Second

// MaxTriggerOutputBytes caps the bytes read from the command's stdout. A
// decision is a few hundred bytes; anything beyond this is a runaway script.
const MaxTriggerOutputBytes = 64 << 10 // 64 KiB

// CommandTrigger runs a shell command as a trigger policy. The zero value is
// unusable (its command is empty); build one with NewCommandTrigger.
type CommandTrigger struct {
	// Command is handed to the shell with "-c".
	Command string
	// Dir is the working directory (usually the project root). Empty means the
	// process's current directory.
	Dir string
	// Shell defaults to "sh".
	Shell string
	// Timeout defaults to DefaultTriggerTimeout.
	Timeout time.Duration
	// Warn receives one line per failed invocation. nil discards them, which is
	// what a caller that has no diagnostic sink wants; the CLI passes diag.Diagf
	// so a broken policy is discoverable with PIGO_DEBUG.
	Warn func(format string, args ...any)
}

// NewCommandTrigger builds a trigger policy from a configured command string. It
// returns nil for an empty command, so "not configured" and "configured" stay
// distinguishable and the fixed policy alone runs in the common case.
func NewCommandTrigger(command, dir string) *CommandTrigger {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	return &CommandTrigger{Command: command, Dir: dir, Warn: nil}
}

// Func adapts the command to the TriggerFunc seam. A nil receiver yields nil, so
// callers can write exactly what they would for "no policy configured".
func (c *CommandTrigger) Func() TriggerFunc {
	if c == nil {
		return nil
	}
	return c.decide
}

// decide runs the command once. It never panics and never returns a decided
// answer it did not actually receive.
func (c *CommandTrigger) decide(ctx context.Context, in TriggerInput) TriggerDecision {
	payload, err := json.Marshal(commandInputFrom(in))
	if err != nil {
		c.warnf("compaction trigger: cannot encode input: %v", err)
		return TriggerDecision{}
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTriggerTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := c.Shell
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.CommandContext(runCtx, shell, "-c", c.Command)
	cmd.Dir = c.Dir
	cmd.Stdin = bytes.NewReader(payload)
	// WaitDelay bounds how long Run waits for a grandchild that inherited the
	// output pipes after the shell was killed on timeout (same reason as the
	// hook runner).
	cmd.WaitDelay = time.Second

	var stdout, stderr boundedBuffer
	stdout.limit = MaxTriggerOutputBytes
	stderr.limit = 4 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		c.warnf("compaction trigger: command timed out after %s", timeout)
		return TriggerDecision{}
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = runErr.Error()
		}
		c.warnf("compaction trigger: command failed: %s", detail)
		return TriggerDecision{}
	}

	dec, ok := parseCommandDecision(stdout.Bytes())
	if !ok {
		c.warnf("compaction trigger: command printed no usable decision (want {\"compact\":true|false})")
		return TriggerDecision{}
	}
	return dec
}

// warnf reports a failed invocation to the configured sink.
func (c *CommandTrigger) warnf(format string, args ...any) {
	if c.Warn != nil {
		c.Warn(format, args...)
	}
}

// commandInput is the JSON document written to the command's stdin. It is a
// flat, stable view of TriggerInput: the response metadata is sent as its
// snapshot (the live record holds a mutex and is not marshalable), and the
// numeric fields keep the names a script would expect.
type commandInput struct {
	ContextTokens int                             `json:"contextTokens"`
	ContextWindow int                             `json:"contextWindow"`
	InputBudget   int                             `json:"inputBudget"`
	ReserveTokens int                             `json:"reserveTokens"`
	TriggerPoint  int                             `json:"triggerPoint"`
	Messages      int                             `json:"messages"`
	Model         string                          `json:"model"`
	Provider      string                          `json:"provider"`
	Usage         *agentcore.Usage                `json:"usage,omitempty"`
	Response      *agentcore.ResponseMetaSnapshot `json:"response,omitempty"`
}

// commandInputFrom projects a TriggerInput onto the wire shape.
func commandInputFrom(in TriggerInput) commandInput {
	return commandInput{
		ContextTokens: in.ContextTokens,
		ContextWindow: in.ContextWindow,
		InputBudget:   in.InputBudget,
		ReserveTokens: in.ReserveTokens,
		TriggerPoint:  in.TriggerPoint,
		Messages:      in.Messages,
		Model:         in.Model,
		Provider:      in.Provider,
		Usage:         in.Usage,
		Response:      in.ResponseMeta.Snapshot(),
	}
}

// commandDecision is what a policy prints. Decide defaults to "yes, I decided"
// when the field is absent: a script that only prints {"compact":true} clearly
// answered the question. An explicit false is how a policy says "no opinion,
// use the fixed policy", which is what a model-specific policy wants for every
// model it does not recognize.
type commandDecision struct {
	Decide  *bool  `json:"decide"`
	Compact bool   `json:"compact"`
	Reason  string `json:"reason"`
}

// parseCommandDecision reads the first JSON object in the command's output. A
// policy may print diagnostics around it (SSE-style noise, a log line), so
// leading non-JSON text is tolerated; an output with no object at all is not.
func parseCommandDecision(b []byte) (TriggerDecision, bool) {
	obj, ok := firstJSONObject(b)
	if !ok {
		return TriggerDecision{}, false
	}
	var dec commandDecision
	if err := json.Unmarshal(obj, &dec); err != nil {
		return TriggerDecision{}, false
	}
	if dec.Decide != nil && !*dec.Decide {
		return TriggerDecision{}, true
	}
	return TriggerDecision{Decided: true, Compact: dec.Compact, Reason: dec.Reason}, true
}

// firstJSONObject returns the first balanced {...} span in b, or false. It
// tracks string literals and escapes so a brace inside a reason string does not
// end the object early.
func firstJSONObject(b []byte) ([]byte, bool) {
	start := bytes.IndexByte(b, '{')
	if start < 0 {
		return nil, false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(b); i++ {
		c := b[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return b[start : i+1], true
			}
		}
	}
	return nil, false
}

// boundedBuffer collects output up to a limit and silently drops the rest. It
// exists so a runaway policy cannot grow the process's memory.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

// Write consumes all of p, keeping only the bytes that fit under the limit. It
// reports the length of the input, not of the kept prefix: io.Writer's contract
// is that a short count means an error (os/exec maps it to ErrShortWrite), so
// returning the truncated length would turn an over-long-but-correct decision
// into a failed invocation.
func (w *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if w.limit > 0 && w.buf.Len() >= w.limit {
		w.over = true
		return n, nil
	}
	if w.limit > 0 && w.buf.Len()+len(p) > w.limit {
		p = p[:w.limit-w.buf.Len()]
		w.over = true
	}
	w.buf.Write(p)
	return n, nil
}

// Bytes returns the collected prefix.
func (w *boundedBuffer) Bytes() []byte { return w.buf.Bytes() }

// String returns the collected prefix as a string.
func (w *boundedBuffer) String() string { return w.buf.String() }
