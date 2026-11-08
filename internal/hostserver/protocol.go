// This file keeps the host server's protocol vocabulary in one place: the wire
// envelope, the error type, the method/event names and the error codes all come
// from internal/hostapi, and are aliased here so the connection code reads
// without an import prefix. Aliasing (rather than redefining) means the two
// sides of the socket cannot drift: a change in hostapi is a change here.
package hostserver

import "github.com/smallnest/pigo/internal/hostapi"

// Envelope and its error type.
type envelope = hostapi.Envelope

type protoError = hostapi.Error

// Methods.
const (
	methodHello            = hostapi.MethodHello
	methodServerStatus     = hostapi.MethodServerStatus
	methodServerShutdown   = hostapi.MethodServerShutdown
	methodSessionCreate    = hostapi.MethodSessionCreate
	methodSessionList      = hostapi.MethodSessionList
	methodSessionResume    = hostapi.MethodSessionResume
	methodSessionClose     = hostapi.MethodSessionClose
	methodPrompt           = hostapi.MethodPrompt
	methodCancel           = hostapi.MethodCancel
	methodPermissionDecide = hostapi.MethodPermissionDecide
)

// Events.
const (
	eventSession        = hostapi.EventSession
	eventPermission     = hostapi.EventPermission
	eventSessionClosed  = hostapi.EventSessionClosed
	eventServerShutdown = hostapi.EventServerShutdown
)

// Error codes.
const (
	codeParse          = hostapi.CodeParse
	codeInvalidRequest = hostapi.CodeInvalidRequest
	codeMethodNotFound = hostapi.CodeMethodNotFound
	codeInvalidParams  = hostapi.CodeInvalidParams
	codeInternal       = hostapi.CodeInternal
	codeUnauthorized   = hostapi.CodeUnauthorized
	codeNotFound       = hostapi.CodeNotFound
	codeBusy           = hostapi.CodeBusy
	codeForbidden      = hostapi.CodeForbidden
	codeUnavailable    = hostapi.CodeUnavailable
)
