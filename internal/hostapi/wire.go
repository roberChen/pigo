// Package hostapi defines the wire protocol shared by pigo's host daemon
// (`pigo serve`) and every UI that drives it (`pigo attach`, and later the TUI
// and a web UI). See docs/architecture.md §6 for the design.
//
// The protocol is newline-delimited JSON over a unix socket: every line is one
// JSON object, either a request, a response, or a server-pushed event. Requests
// and responses are correlated by ID; events carry an event name and no ID.
//
// This package is deliberately dependency-light (agentcore + stdlib): it is the
// contract both sides compile against, so the ordering/discriminant helpers
// live here and the transport lives in hostserver/hostclient.
package hostapi

import "encoding/json"

// Methods accepted by the host. A request's Method must be one of these; the
// server answers an unknown method with CodeMethodNotFound rather than closing
// the connection, so a newer client degrades instead of breaking.
const (
	MethodHello            = "hello"
	MethodServerStatus     = "server.status"
	MethodServerShutdown   = "server.shutdown"
	MethodSessionCreate    = "session.create"
	MethodSessionList      = "session.list"
	MethodSessionResume    = "session.resume"
	MethodSessionClose     = "session.close"
	MethodPrompt           = "prompt"
	MethodCancel           = "cancel"
	MethodPermissionDecide = "permission.decide"
)

// Event names the server pushes. Every event is delivered as an Envelope with
// Event set and ID empty.
const (
	EventSession        = "session_event"
	EventPermission     = "permission_request"
	EventSessionClosed  = "session_closed"
	EventServerShutdown = "server_shutdown"
)

// Error codes. The numeric space mirrors JSON-RPC where it overlaps so a
// generic client can reuse its error handling; the 1xxx range is host-specific.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	// CodeUnauthorized means the connection has not completed hello with the
	// right token (or sent hello twice). The server closes after reporting it.
	CodeUnauthorized = 1001
	// CodeNotFound means the referenced session does not exist or is not owned
	// by this connection.
	CodeNotFound = 1002
	// CodeBusy means the session already has a turn in flight. Prompts are not
	// queued: a UI that wants to wait must observe the running turn's events.
	CodeBusy = 1003
	// CodeForbidden means the session exists but this connection may not touch
	// it (a different UI owns it).
	CodeForbidden = 1004
	// CodeUnavailable means a required capability is missing on this host (for
	// example, shutting down a daemon that was started in the foreground).
	CodeUnavailable = 1005
)

// Version is the protocol version carried by hello. The server refuses a client
// whose version is newer than its own, so a rolling upgrade never misparses.
const Version = 1

// Envelope is one NDJSON line. Exactly one shape is present:
//
//	request   ID + Method (+ Params)
//	response  ID + (Result | Error)
//	event     Event (+ Params)
//
// Params and Result stay raw so the envelope layer never needs to know a
// method's payload; the typed structs in this package decode them on demand.
type Envelope struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
}

// Error is a structured failure. Message is user-facing; Data is optional and
// method-specific.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Data != "" {
		return e.Message + ": " + e.Data
	}
	return e.Message
}

// Err builds an *Error for a code/message pair.
func Err(code int, message string) *Error { return &Error{Code: code, Message: message} }

// IsEvent reports whether env is a server-pushed event.
func (env Envelope) IsEvent() bool { return env.Event != "" }

// IsResponse reports whether env answers a request: it carries an ID and either
// a result or an error, and no method.
func (env Envelope) IsResponse() bool {
	return env.Method == "" && env.ID != "" && (len(env.Result) > 0 || env.Error != nil)
}

// DecodeParams decodes an envelope's raw params into v. A nil/empty params
// payload leaves v untouched, so a method that takes no arguments works without
// the client having to send "{}".
func (env Envelope) DecodeParams(v any) error {
	if len(env.Params) == 0 {
		return nil
	}
	return json.Unmarshal(env.Params, v)
}

// DecodeResult decodes an envelope's raw result into v.
func (env Envelope) DecodeResult(v any) error {
	if len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, v)
}
