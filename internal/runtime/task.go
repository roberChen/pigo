// This file implements the generic `task` tool (US-002, #454): a general-purpose
// sub-agent the model can dispatch with a free-form prompt to fan out work in a
// single assistant message. It is a thin specialization of SubAgentTool - it
// reuses executeGoroutine's child-loop driving - configured with a generic
// system prompt (the delegated task comes from the call arguments at runtime),
// a shared concurrency semaphore, and a child tool set from which `task` itself
// is excluded (the nesting guard, wired in internal/cli/run).
package runtime

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// DefaultMaxSubagents is the concurrency cap applied when PIGO_MAX_SUBAGENTS is
// unset or invalid. It bounds how many task sub-agents run at once so a fan-out
// cannot overwhelm the provider rate limit.
const DefaultMaxSubagents = 4

// taskDescription is advertised to the parent model so it knows when to delegate
// work to a generic sub-agent.
const taskDescription = "Dispatch a general-purpose sub-agent to autonomously complete a delegated task. " +
	"The sub-agent runs its own agent loop with a fresh context and the standard tool set, then returns its final report. " +
	"Provide a complete, self-contained prompt since the sub-agent shares none of this conversation's context. " +
	"Every call must state its mode: \"sync\" waits for the report inside the tool call (multiple sync calls in one " +
	"message run in parallel and are awaited together), while \"async\" returns a job id immediately and delivers the " +
	"report as a follow-up message (collect a batch with task_output job_ids + wait, or wait_all). " +
	"A sub-agent cannot ask the user anything: if it needs a decision it says so in its report. Ask the user yourself " +
	"with question, then continue that sub-agent with session_id set to the id its result reported."

// taskSystemPrompt seeds every generic sub-agent's context. It is intentionally
// generic (the actual work arrives as the runtime prompt) and mirrors the
// parent agent's operating posture so a delegated task is carried out the same
// way the parent would.
const taskSystemPrompt = "You are a focused sub-agent working on one delegated task. " +
	"You have your own fresh context and the standard tool set, but you cannot spawn further sub-agents and you cannot ask the user questions. " +
	"Complete the task fully using the tools available. If a decision is genuinely blocked on the user, stop and say exactly what you " +
	"need decided: the agent that dispatched you will ask, then continue this same session with the answer. " +
	"Finish with a concise report of what you did and any key findings; your final message is returned verbatim to the agent that dispatched you."

// taskSchema is the JSON Schema for a task invocation: a required self-contained
// prompt plus the required sync/async mode and an optional short description used
// for status display.
var taskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {
      "type": "string",
      "description": "A short (3-5 word) description of the task, for status display."
    },
    "prompt": {
      "type": "string",
      "description": "The full task for the sub-agent to perform. It must be self-contained since the sub-agent runs with a fresh context and shares none of this conversation."
    },
    "mode": {
      "type": "string",
      "enum": ["sync", "async"],
      "description": "Required. \"sync\" blocks this tool call until the sub-agent finishes and returns its report; several sync calls in one message run in parallel and are awaited together. \"async\" returns a job id immediately and delivers the report as a follow-up message - collect a batch with task_output job_ids + wait, or wait_all."
    },
    "session_id": {
      "type": "string",
      "description": "Optional: continue an earlier sub-agent run by passing the session id its result reported, instead of starting from a fresh context."
    }
  },
  "required": ["prompt", "mode"],
  "additionalProperties": false
}`)

// NewTaskTool builds the generic `task` sub-agent tool. factory produces a fresh
// child RunConfig per spawn (reusing the parent's provider stream/model and a
// child tool registry that must exclude `task` for the nesting guard); sem is a
// shared buffered channel bounding concurrent task runs (nil disables limiting);
// jobs is the run's background-job registry (nil disables `mode: "async"`). The
// child prompt comes from the call arguments at runtime, so a single generic spec
// serves every delegated task.
func NewTaskTool(factory func() RunConfig, sem chan struct{}, jobs *SubAgentJobs) *SubAgentTool {
	return NewSubAgentTool(SubAgentSpec{
		Name:         "task",
		Description:  taskDescription,
		SystemPrompt: taskSystemPrompt,
		Schema:       taskSchema,
		NewRunConfig: factory,
		Sem:          sem,
		Jobs:         jobs,
	})
}

// MaxSubagents resolves the concurrency cap for task sub-agents from
// PIGO_MAX_SUBAGENTS: absent or unparseable yields DefaultMaxSubagents (4), and
// a parsed value below 1 is floored to 1 so the semaphore always admits at least
// one runner.
func MaxSubagents() int {
	v := strings.TrimSpace(os.Getenv("PIGO_MAX_SUBAGENTS"))
	if v == "" {
		return DefaultMaxSubagents
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return DefaultMaxSubagents
	}
	if n < 1 {
		return 1
	}
	return n
}

// NewSubagentSemaphore builds the shared concurrency semaphore for task
// sub-agents, sized by MaxSubagents. One instance per run is created and shared
// across every task call so the cap is enforced run-wide.
func NewSubagentSemaphore() chan struct{} {
	return make(chan struct{}, MaxSubagents())
}

// activityOf maps a child sub-agent event to the display verb surfaced in a
// SubAgentProgressEvent (D-8: tool name / phase, no argument summary). A child
// ToolExecutionStartEvent maps by tool name; a TurnStartEvent (a fresh turn with
// no tool in progress) maps to "Thinking". Every other event maps to "" so the
// caller emits nothing — progress is reported only at these activity boundaries,
// keeping event volume proportional to the child's tool calls rather than its
// text deltas (D-7).
func activityOf(ev agentcore.AgentEvent) string {
	switch e := ev.(type) {
	case agentcore.ToolExecutionStartEvent:
		switch e.ToolName {
		case "read", "view_image":
			return "Reading"
		case "edit", "write":
			return "Editing"
		case "bash":
			return "Running bash"
		case "grep", "find", "ls":
			return "Searching"
		case "webfetch":
			return "Fetching"
		default:
			return ""
		}
	case agentcore.TurnStartEvent:
		return "Thinking"
	default:
		return ""
	}
}

// estimateTokens gives a coarse output-token estimate from a running character
// count of the child's streamed text (~4 chars per token). It rides along on
// each progress event as a rough "↓ tokens" figure; 0 means unknown (no text
// streamed yet).
func estimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars / 4
}
