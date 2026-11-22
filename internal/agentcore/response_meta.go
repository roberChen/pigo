// Package agentcore defines the types every layer shares. This file implements
// ResponseMeta: a bounded, secret-free record of what an upstream actually
// returned to a request.
//
// It exists for the pluggable compaction trigger. pigo understands the standard
// usage block, but a non-standard gateway may account for tokens differently —
// a custom usage field, a remaining-quota header, a per-request budget in the
// body — and only a user-written policy can interpret that. Handing the policy
// the raw record, rather than pigo's interpretation of it, is what makes such a
// gateway supportable without patching the harness.
//
// Two rules keep this safe to expose:
//
//   - It is BOUNDED. The body is a tail of at most MaxResponseBodyBytes, and the
//     number of SSE payloads remembered is capped, so a long stream cannot grow
//     the process's memory through this channel.
//   - It is SECRET-FREE. Credential-shaped headers (authorization, cookies, API
//     keys) are dropped when the record is captured, so a policy — or a debug
//     dump of it — cannot leak a key.
package agentcore

import (
	"strings"
	"sync"
)

// MaxResponseBodyBytes bounds the raw response bytes ResponseMeta retains. A
// usage-bearing payload is small; the tail is kept because in SSE the final
// events (usage, finish reason) arrive last.
const MaxResponseBodyBytes = 16 << 10 // 16 KiB

// MaxResponsePayloads bounds how many individual SSE payloads are remembered.
const MaxResponsePayloads = 32

// redactedHeaderValue replaces a credential-shaped header's value.
const redactedHeaderValue = "[redacted]"

// sensitiveHeaderFragments are matched (case-insensitively) as substrings of a
// header name. A response header whose name contains any of them is recorded as
// redacted rather than dropped, so a policy can still see that the header was
// present without learning its value.
var sensitiveHeaderFragments = []string{
	"authorization",
	"api-key",
	"apikey",
	"cookie",
	"token",
	"secret",
	"session-id",
	"x-amz-security",
}

// ResponseMeta is a bounded record of one upstream response. The zero value is
// empty and safe to use; a nil *ResponseMeta means "not captured".
//
// It is written by the transport goroutine and read by the loop after the
// stream has closed, so all access goes through the embedded mutex; copy it with
// Snapshot rather than by assignment.
type ResponseMeta struct {
	mu sync.RWMutex
	// status is the upstream HTTP status of the response that carried the body.
	status int
	// headers holds the sanitized response headers.
	headers map[string][]string
	// payloads holds the most recent raw SSE payloads, oldest first.
	payloads []string
	// bodyTail holds the tail of the raw response body (non-SSE responses, or
	// the trailing bytes of an SSE body when payload boundaries were not seen).
	bodyTail string
	// bodies counts how many response bodies were captured into this record.
	bodies int
}

// NewResponseMeta returns an empty record ready to be filled by a transport.
func NewResponseMeta() *ResponseMeta {
	return &ResponseMeta{}
}

// SetStatus records the response status code.
func (m *ResponseMeta) SetStatus(code int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = code
}

// SetHeaders records the response headers, keeping the shape but redacting the
// value of any credential-shaped header. It replaces a previous header set, so a
// retried request reports its own response rather than a merge.
func (m *ResponseMeta) SetHeaders(h map[string][]string) {
	if m == nil {
		return
	}
	sanitized := make(map[string][]string, len(h))
	for name, values := range h {
		if isSensitiveHeader(name) {
			sanitized[name] = []string{redactedHeaderValue}
			continue
		}
		sanitized[name] = append([]string(nil), values...)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headers = sanitized
}

// AddPayload records one raw SSE payload, keeping only the most recent
// MaxResponsePayloads entries.
func (m *ResponseMeta) AddPayload(payload string) {
	if m == nil || payload == "" {
		return
	}
	if len(payload) > MaxResponseBodyBytes {
		payload = payload[len(payload)-MaxResponseBodyBytes:]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payloads = append(m.payloads, payload)
	if len(m.payloads) > MaxResponsePayloads {
		m.payloads = m.payloads[len(m.payloads)-MaxResponsePayloads:]
	}
}

// AppendBodyTail records raw body bytes, keeping only the last
// MaxResponseBodyBytes. It is used for a response that is not parsed as SSE, so
// its content is still available to a policy.
func (m *ResponseMeta) AppendBodyTail(b []byte) {
	if m == nil || len(b) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodyTail += string(b)
	if len(m.bodyTail) > MaxResponseBodyBytes {
		m.bodyTail = m.bodyTail[len(m.bodyTail)-MaxResponseBodyBytes:]
	}
}

// CountBody records that one more response body was captured.
func (m *ResponseMeta) CountBody() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies++
}

// Reset clears the record so the next request starts from a clean state. The
// loop calls it before each turn, so the record always describes the most recent
// response rather than a merge of every response in the session.
func (m *ResponseMeta) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = 0
	m.headers = nil
	m.payloads = nil
	m.bodyTail = ""
	m.bodies = 0
}

// ResponseMetaSnapshot is an immutable, JSON-friendly copy of a ResponseMeta.
type ResponseMetaSnapshot struct {
	Status   int                 `json:"status,omitempty"`
	Headers  map[string][]string `json:"headers,omitempty"`
	Payloads []string            `json:"payloads,omitempty"`
	BodyTail string              `json:"bodyTail,omitempty"`
	Bodies   int                 `json:"bodies,omitempty"`
}

// Snapshot returns a copy that is safe to read after the transport goroutine has
// moved on. It returns nil for a nil record, so "not captured" stays
// distinguishable from "captured nothing".
func (m *ResponseMeta) Snapshot() *ResponseMetaSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap := &ResponseMetaSnapshot{
		Status:   m.status,
		BodyTail: m.bodyTail,
		Bodies:   m.bodies,
	}
	if len(m.headers) > 0 {
		snap.Headers = make(map[string][]string, len(m.headers))
		for k, v := range m.headers {
			snap.Headers[k] = append([]string(nil), v...)
		}
	}
	if len(m.payloads) > 0 {
		snap.Payloads = append([]string(nil), m.payloads...)
	}
	return snap
}

// isSensitiveHeader reports whether a response header's value must be redacted.
func isSensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, frag := range sensitiveHeaderFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}
