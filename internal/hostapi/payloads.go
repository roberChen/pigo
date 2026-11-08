// This file defines the typed payloads for every host method and event. The
// envelope layer (wire.go) carries them as raw JSON; hostclient/hostserver
// (de)serialize these structs. Keeping them in one file makes the protocol
// reviewable at a glance and forces a deliberate change whenever the contract
// moves.
package hostapi

import (
	"encoding/json"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
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
}

// SessionCloseParams ends a session on the host. The conversation is already
// persisted, so closing is not destructive.
type SessionCloseParams struct {
	ID string `json:"id"`
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

// SessionEvent is one increment of a running session, pushed as an
// EventSession notification. Only the fields relevant to Type are populated,
// which keeps the wire shape small and the mapper trivial.
type SessionEvent struct {
	SessionID string `json:"sessionId"`
	Type      string `json:"type"`

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
