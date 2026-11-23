// This file defines the typed payloads for every host method and event. The
// envelope layer (wire.go) carries them as raw JSON; hostclient/hostserver
// (de)serialize these structs. Keeping them in one file makes the protocol
// reviewable at a glance and forces a deliberate change whenever the contract
// moves.
package hostapi

import (
	"encoding/json"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
)

// HelloParams is the first message a client sends. Token must match the host's
// token file; Client is a free-form label ("attach", "tui", "web") used only for
// status output.
type HelloParams struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	Client  string `json:"client,omitempty"`
}

// HelloResult reports the negotiated protocol and the host's identity.
type HelloResult struct {
	Version int       `json:"version"`
	PID     int       `json:"pid"`
	Build   string    `json:"build,omitempty"`
	Started time.Time `json:"startedAt"`
}

// ServerStatusResult is the answer to server.status. Sessions counts live
// (attached) sessions, not persisted ones.
type ServerStatusResult struct {
	PID      int       `json:"pid"`
	Build    string    `json:"build,omitempty"`
	Started  time.Time `json:"startedAt"`
	Sessions int       `json:"sessions"`
	Socket   string    `json:"socket,omitempty"`
	Daemon   bool      `json:"daemon"`
}

// SessionCreateParams starts a new conversation on the host. Cwd defaults to the
// host's own working directory when empty; it is recorded on the session header
// and roots the built-in file/shell tools.
type SessionCreateParams struct {
	Cwd   string `json:"cwd,omitempty"`
	Model string `json:"model,omitempty"`
	// Title is an optional human label shown in session.list. It is not the
	// model-facing prompt.
	Title string `json:"title,omitempty"`
}

// SessionInfo is the metadata a UI shows in a session picker. It never carries
// messages.
type SessionInfo struct {
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd,omitempty"`
	Model     string    `json:"model,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Running reports whether a turn is currently in flight for the session.
	Running bool `json:"running,omitempty"`
	// TurnID identifies the in-flight turn when Running is true.
	TurnID string `json:"turnId,omitempty"`
}

// SessionCreateResult returns the new session's id and metadata.
type SessionCreateResult struct {
	Session SessionInfo `json:"session"`
}

// SessionListResult lists the sessions owned by the calling connection. A
// connection cannot see another UI's sessions: isolation is the point of the
// host, so the listing is per-connection by contract.
type SessionListResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionResumeParams attaches to a persisted session. Ownership is per
// connection; a session created by another connection is rejected with
// CodeForbidden rather than silently shared.
type SessionResumeParams struct {
	ID string `json:"id"`
}

// SessionResumeResult returns the metadata plus the replayed conversation (the
// root-to-leaf path since the last compaction checkpoint, exactly what a resumed
// local run would seed into its context).
type SessionResumeResult struct {
	Session  SessionInfo           `json:"session"`
	Messages agentcore.MessageList `json:"messages"`
	// TurnID and EventSeq identify the running turn when a client reattaches to
	// an ownerless, in-flight session. Events replays the retained log; live
	// events continue with Seq values greater than EventSeq.
	TurnID   string         `json:"turnId,omitempty"`
	EventSeq uint64         `json:"eventSeq,omitempty"`
	Events   []SessionEvent `json:"events,omitempty"`
	// EventsTruncated means the bounded replay log lost its prefix. The client
	// must use the final persisted transcript for an authoritative history.
	EventsTruncated bool `json:"eventsTruncated,omitempty"`
}

// SessionCloseParams ends a session on the host. The conversation is already
// persisted, so closing is not destructive.
type SessionCloseParams struct {
	ID string `json:"id"`
}

// SessionSetModelParams switches the model of an existing session. The
// conversation, tools and memory stay; only the model (and the provider that
// serves it) changes, so the next prompt runs on the new model. A session with a
// turn in flight is rejected with CodeBusy: switching mid-turn would make the
// turn's already-decided budget disagree with the model answering it.
//
// Model accepts the same forms as `pigo --model` and `/model <id>`: a concrete
// id, or a bare provider name for that provider's default model.
type SessionSetModelParams struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model"`
}

// SessionSetModelResult reports the session as it now is plus the budget the
// host resolved for the new model. The budget is returned rather than recomputed
// by the client because only the host knows the layered configuration
// ([checkpoint]/[compaction], a per-model compact_at, a policy command) that
// decides where auto-compaction fires — a client that re-derived it could
// disagree with the loop it is driving.
type SessionSetModelResult struct {
	Session SessionInfo `json:"session"`
	// ContextWindow is the model's combined budget (input + output).
	ContextWindow int `json:"contextWindow,omitempty"`
	// InputBudget is what a single request may carry (the input ceiling when the
	// provider publishes one smaller than the window, else the window).
	InputBudget int `json:"inputBudget,omitempty"`
	// TriggerPoint is where automatic compaction fires, in tokens, measured
	// against InputBudget. 0 means automatic compaction is off for this model.
	TriggerPoint int `json:"triggerPoint,omitempty"`
	// TriggerSource names the cap that decided the trigger point (ladder,
	// reserve, configured, off) so a client can explain the number.
	TriggerSource string `json:"triggerSource,omitempty"`
}

// PromptParams submits one user turn. Text is the literal prompt (images are not
// part of v1 of the protocol; a UI that needs them can attach in a later
// revision).
type PromptParams struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
}

// PromptResult is the terminal answer for a prompt request. Streaming deltas
// arrive as session_event notifications while the request is outstanding, so a
// UI can render progress; the result repeats the final text so a client that
// ignores events still works.
type PromptResult struct {
	SessionID  string `json:"sessionId"`
	Text       string `json:"text"`
	StopReason string `json:"stopReason,omitempty"`
	// Error is set when the turn ended in a failure the host recovered from
	// (for example the provider returned an error message). A transport-level
	// failure is reported as an envelope error instead.
	Error string `json:"error,omitempty"`
}

// CancelParams asks the host to abort the session's in-flight turn.
type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// PermissionDecideParams answers a permission_request. RequestID is the id from
// the event; Always grants session-scoped trust for the session's directory so
// later side-effect calls skip the prompt.
type PermissionDecideParams struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Approve   bool   `json:"approve"`
	Always    bool   `json:"always,omitempty"`
}

// PermissionRequest is pushed to the owning connection when a side-effect tool
// wants to run in an untrusted directory. Summary is a one-line preview,
// produced by the same renderer the REPL's confirmation prompt uses.
type PermissionRequest struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	TurnID    string `json:"turnId,omitempty"`
	ToolName  string `json:"toolName"`
	Summary   string `json:"summary,omitempty"`
	// TimeoutMS is how long the host will wait before treating the request as
	// denied. 0 means "until the client answers or the turn is cancelled".
	TimeoutMS int `json:"timeoutMs,omitempty"`
}

// SessionClosedEvent tells the owner that a session ended (explicitly via
// session.close, or because the host is shutting down).
type SessionClosedEvent struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason,omitempty"`
}

// ServerShutdownEvent is broadcast before the host exits.
type ServerShutdownEvent struct {
	Reason string `json:"reason,omitempty"`
}

// Session event discriminants carried by SessionEvent.Type.
const (
	SessionEventAgentStart   = "agent_start"
	SessionEventAgentEnd     = "agent_end"
	SessionEventTextDelta    = "text_delta"
	SessionEventThinking     = "thinking_delta"
	SessionEventTurnEnd      = "turn_end"
	SessionEventToolStart    = "tool_start"
	SessionEventToolUpdate   = "tool_update"
	SessionEventToolEnd      = "tool_end"
	SessionEventCompaction   = "compaction"
	SessionEventSubAgent     = "subagent_progress"
	SessionEventSubAgentDone = "subagent_end"
)

// EventQueueOverflow is emitted locally by hostclient when its bounded event
// queue dropped older streaming events. The final RPC result is authoritative.
const EventQueueOverflow = "event_queue_overflow"

// QueueOverflowEvent reports how many events were discarded before this notice.
type QueueOverflowEvent struct {
	Dropped uint64 `json:"dropped"`
}

// SessionEvent is one increment of a running session, pushed as an
// EventSession notification. Only the fields relevant to Type are populated,
// which keeps the wire shape small and the mapper trivial.
type SessionEvent struct {
	SessionID string `json:"sessionId"`
	Type      string `json:"type"`
	TurnID    string `json:"turnId,omitempty"`
	Seq       uint64 `json:"seq,omitempty"`

	// Text deltas (text_delta / thinking_delta) and the final turn text.
	Text string `json:"text,omitempty"`

	// Tool lifecycle.
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Result     string          `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`

	// Turn end.
	Message *agentcore.AssistantMessage `json:"message,omitempty"`

	// Compaction.
	TokensBefore    int    `json:"tokensBefore,omitempty"`
	TokensAfter     int    `json:"tokensAfter,omitempty"`
	SummarizedCount int    `json:"summarizedCount,omitempty"`
	KeptCount       int    `json:"keptCount,omitempty"`
	CompactionError string `json:"compactionError,omitempty"`

	// Sub-agent progress.
	Description string `json:"description,omitempty"`
	Activity    string `json:"activity,omitempty"`
}
