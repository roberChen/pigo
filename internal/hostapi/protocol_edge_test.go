// This file tests the host wire protocol as a contract: every payload type must
// survive an envelope round trip, every JSON field name and method/event name
// must stay where the other side of the socket expects it, and malformed input
// must produce an error instead of a panic. The golden strings below are the
// wire format itself — if a rename reaches them, an older `pigo attach` breaks.
package hostapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// *Error is the protocol's error value; the hostserver and hostclient both carry
// it as a plain error.
var _ error = (*Error)(nil)

// Fixed timestamps keep the golden JSON stable.
var (
	wireT0 = time.Date(2024, 5, 1, 12, 30, 15, 0, time.UTC)
	wireT1 = wireT0.Add(time.Minute)
)

// wireMessage is a fully populated assistant message, used by the SessionEvent
// and SessionResumeResult cases.
func wireMessage() agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent("hi")},
		StopReason: agentcore.StopReasonEndTurn,
		Timestamp:  1714566615000,
	}
}

// roundTripParams and roundTripResult push v through the full encode/decode path
// (value → envelope → NDJSON line → envelope → value) and compare the re-encoded
// JSON. Comparing JSON rather than the values keeps time.Time's internal
// representation out of the assertion while still catching any field that fails
// to survive the trip.
func roundTripParams[T any](t *testing.T, v T) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	line, err := json.Marshal(Envelope{ID: "1", Method: MethodPrompt, Params: raw})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var decoded Envelope
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if decoded.IsEvent() || decoded.IsResponse() {
		t.Fatalf("request misclassified after round trip: %s", line)
	}
	var got T
	if err := decoded.DecodeParams(&got); err != nil {
		t.Fatalf("DecodeParams: %v", err)
	}
	again, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal params: %v", err)
	}
	if string(again) != string(raw) {
		t.Fatalf("params changed in transit:\n got %s\nwant %s", again, raw)
	}
}

func roundTripResult[T any](t *testing.T, v T) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	line, err := json.Marshal(Envelope{ID: "1", Result: raw})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var decoded Envelope
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !decoded.IsResponse() {
		t.Fatalf("response misclassified after round trip: %s", line)
	}
	var got T
	if err := decoded.DecodeResult(&got); err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	again, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	if string(again) != string(raw) {
		t.Fatalf("result changed in transit:\n got %s\nwant %s", again, raw)
	}
}

// TestPayloadRoundTrips covers every params and result type the protocol
// defines, with every field populated so a dropped field cannot hide behind
// omitempty.
func TestPayloadRoundTrips(t *testing.T) {
	t.Run("HelloParams", func(t *testing.T) {
		roundTripParams(t, HelloParams{Version: Version, Token: "tok", Client: "attach"})
	})
	t.Run("SessionCreateParams", func(t *testing.T) {
		roundTripParams(t, SessionCreateParams{Cwd: "/work", Model: "model-x", Title: "title"})
	})
	t.Run("SessionResumeParams", func(t *testing.T) {
		roundTripParams(t, SessionResumeParams{ID: "s1"})
	})
	t.Run("SessionCloseParams", func(t *testing.T) {
		roundTripParams(t, SessionCloseParams{ID: "s1"})
	})
	t.Run("PromptParams", func(t *testing.T) {
		roundTripParams(t, PromptParams{SessionID: "s1", Text: "hello"})
	})
	t.Run("CancelParams", func(t *testing.T) {
		roundTripParams(t, CancelParams{SessionID: "s1"})
	})
	t.Run("PermissionDecideParams", func(t *testing.T) {
		roundTripParams(t, PermissionDecideParams{SessionID: "s1", RequestID: "r1", Approve: true, Always: true})
	})

	t.Run("HelloResult", func(t *testing.T) {
		roundTripResult(t, HelloResult{Version: Version, PID: 42, Build: "dev", Started: wireT0})
	})
	t.Run("ServerStatusResult", func(t *testing.T) {
		roundTripResult(t, ServerStatusResult{
			PID: 42, Build: "dev", Started: wireT0, Sessions: 2, Socket: "/tmp/s.sock", Daemon: true,
		})
	})
	t.Run("SessionCreateResult", func(t *testing.T) {
		roundTripResult(t, SessionCreateResult{Session: wireSessionInfo()})
	})
	t.Run("SessionListResult", func(t *testing.T) {
		roundTripResult(t, SessionListResult{Sessions: []SessionInfo{wireSessionInfo(), {ID: "s2", CreatedAt: wireT0, UpdatedAt: wireT1}}})
	})
	t.Run("SessionListResultEmpty", func(t *testing.T) {
		// An empty list must stay a list: a UI iterating null would crash.
		roundTripResult(t, SessionListResult{Sessions: []SessionInfo{}})
	})
	t.Run("SessionResumeResult", func(t *testing.T) {
		roundTripResult(t, SessionResumeResult{
			Session: wireSessionInfo(),
			Messages: agentcore.MessageList{
				agentcore.UserMessage{
					RoleField: agentcore.RoleUser,
					Content:   agentcore.ContentList{agentcore.NewTextContent("hello")},
					Timestamp: 1,
				},
				wireMessage(),
				agentcore.ToolResultMessage{
					RoleField:  agentcore.RoleToolResult,
					ToolCallID: "c1",
					ToolName:   "bash",
					Content:    agentcore.ContentList{agentcore.NewTextContent("done")},
					Details:    map[string]any{"exit": float64(0)},
					IsError:    false,
					Timestamp:  2,
				},
				agentcore.CompactionMessage{
					RoleField:       agentcore.RoleCompaction,
					Summary:         "earlier turns",
					TokensBefore:    900,
					CoveredMessages: 12,
					Details:         json.RawMessage(`{"files":["a.go"]}`),
					Timestamp:       3,
				},
			},
		})
	})
	t.Run("PromptResult", func(t *testing.T) {
		roundTripResult(t, PromptResult{SessionID: "s1", Text: "hi", StopReason: "end_turn", Error: "recovered"})
	})
	t.Run("PermissionRequest", func(t *testing.T) {
		roundTripResult(t, PermissionRequest{
			SessionID: "s1", RequestID: "r1", ToolName: "bash", Summary: "ls", TimeoutMS: 5000,
		})
	})
	t.Run("SessionClosedEvent", func(t *testing.T) {
		roundTripResult(t, SessionClosedEvent{SessionID: "s1", Reason: "closed"})
	})
	t.Run("ServerShutdownEvent", func(t *testing.T) {
		roundTripResult(t, ServerShutdownEvent{Reason: "sigterm"})
	})
	t.Run("SessionEventFull", func(t *testing.T) {
		msg := wireMessage()
		roundTripResult(t, SessionEvent{
			SessionID: "s1", Type: SessionEventToolEnd, Text: "t", ToolCallID: "c1", ToolName: "bash",
			Arguments: json.RawMessage(`{"command":"ls"}`), Result: "r", IsError: true, Message: &msg,
			TokensBefore: 1, TokensAfter: 2, SummarizedCount: 3, KeptCount: 4, CompactionError: "e",
			Description: "d", Activity: "a",
		})
	})
	t.Run("SessionEventMinimal", func(t *testing.T) {
		roundTripResult(t, SessionEvent{SessionID: "s1", Type: SessionEventTextDelta, Text: "hi"})
	})
	t.Run("ErrorPayload", func(t *testing.T) {
		roundTripResult(t, &Error{Code: CodeParse, Message: "bad", Data: "detail"})
	})
}

func wireSessionInfo() SessionInfo {
	return SessionInfo{
		ID: "s1", Cwd: "/work", Model: "m", Provider: "p", Title: "t",
		CreatedAt: wireT0, UpdatedAt: wireT1, Running: true,
	}
}

// TestEnvelopeJSONShape pins the four envelope shapes. Params-before-event looks
// odd but is the struct order both sides already speak; changing it would be a
// silent wire break for any client matching exact lines.
func TestEnvelopeJSONShape(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
		want string
	}{
		{"zero", Envelope{}, `{}`},
		{"request", Envelope{ID: "1", Method: MethodPrompt, Params: json.RawMessage(`{"sessionId":"s","text":"hi"}`)},
			`{"id":"1","method":"prompt","params":{"sessionId":"s","text":"hi"}}`},
		{"response", Envelope{ID: "1", Result: json.RawMessage(`{"text":"hi"}`)},
			`{"id":"1","result":{"text":"hi"}}`},
		{"error", Envelope{ID: "2", Error: Err(CodeBusy, "busy")},
			`{"id":"2","error":{"code":1003,"message":"busy"}}`},
		{"event", Envelope{Event: EventSession, Params: json.RawMessage(`{"type":"text_delta"}`)},
			`{"params":{"type":"text_delta"},"event":"session_event"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.env)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("envelope shape changed:\n got %s\nwant %s", got, tc.want)
			}
			// The shape must also decode back to the same envelope, so the golden
			// is not describing something one-way.
			var back Envelope
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("unmarshal golden: %v", err)
			}
			again, err := json.Marshal(back)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(again) != tc.want {
				t.Fatalf("golden does not survive a decode: %s", again)
			}
		})
	}
}

// TestPayloadJSONShape is the same guard for the payload structs: these strings
// are what a non-Go client would have to parse.
func TestPayloadJSONShape(t *testing.T) {
	msg := wireMessage()
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"HelloParams", HelloParams{Version: 1, Token: "tok", Client: "attach"},
			`{"version":1,"token":"tok","client":"attach"}`},
		{"HelloResult", HelloResult{Version: 1, PID: 42, Build: "dev", Started: wireT0},
			`{"version":1,"pid":42,"build":"dev","startedAt":"2024-05-01T12:30:15Z"}`},
		{"ServerStatusResult", ServerStatusResult{PID: 42, Build: "dev", Started: wireT0, Sessions: 2, Socket: "/tmp/s.sock", Daemon: true},
			`{"pid":42,"build":"dev","startedAt":"2024-05-01T12:30:15Z","sessions":2,"socket":"/tmp/s.sock","daemon":true}`},
		{"SessionCreateParams", SessionCreateParams{Cwd: "/work", Model: "m", Title: "t"},
			`{"cwd":"/work","model":"m","title":"t"}`},
		{"SessionInfo", wireSessionInfo(),
			`{"id":"s1","cwd":"/work","model":"m","provider":"p","title":"t","createdAt":"2024-05-01T12:30:15Z","updatedAt":"2024-05-01T12:31:15Z","running":true}`},
		{"SessionCreateResult", SessionCreateResult{Session: SessionInfo{ID: "s1", CreatedAt: wireT0, UpdatedAt: wireT1}},
			`{"session":{"id":"s1","createdAt":"2024-05-01T12:30:15Z","updatedAt":"2024-05-01T12:31:15Z"}}`},
		{"SessionListResult", SessionListResult{Sessions: []SessionInfo{{ID: "s1", CreatedAt: wireT0, UpdatedAt: wireT1}}},
			`{"sessions":[{"id":"s1","createdAt":"2024-05-01T12:30:15Z","updatedAt":"2024-05-01T12:31:15Z"}]}`},
		{"SessionResumeParams", SessionResumeParams{ID: "s1"}, `{"id":"s1"}`},
		{"SessionResumeResult", SessionResumeResult{Session: SessionInfo{ID: "s1", CreatedAt: wireT0, UpdatedAt: wireT1}, Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hello")}, Timestamp: 1}}},
			`{"session":{"id":"s1","createdAt":"2024-05-01T12:30:15Z","updatedAt":"2024-05-01T12:31:15Z"},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}],"timestamp":1}]}`},
		{"SessionCloseParams", SessionCloseParams{ID: "s1"}, `{"id":"s1"}`},
		{"PromptParams", PromptParams{SessionID: "s1", Text: "hi"}, `{"sessionId":"s1","text":"hi"}`},
		{"PromptResult", PromptResult{SessionID: "s1", Text: "hi", StopReason: "end_turn", Error: "e"},
			`{"sessionId":"s1","text":"hi","stopReason":"end_turn","error":"e"}`},
		{"CancelParams", CancelParams{SessionID: "s1"}, `{"sessionId":"s1"}`},
		{"PermissionDecideParams", PermissionDecideParams{SessionID: "s1", RequestID: "r1", Approve: true, Always: true},
			`{"sessionId":"s1","requestId":"r1","approve":true,"always":true}`},
		{"PermissionRequest", PermissionRequest{SessionID: "s1", RequestID: "r1", ToolName: "bash", Summary: "ls", TimeoutMS: 5000},
			`{"sessionId":"s1","requestId":"r1","toolName":"bash","summary":"ls","timeoutMs":5000}`},
		{"SessionClosedEvent", SessionClosedEvent{SessionID: "s1", Reason: "done"}, `{"sessionId":"s1","reason":"done"}`},
		{"ServerShutdownEvent", ServerShutdownEvent{Reason: "sigterm"}, `{"reason":"sigterm"}`},
		{"SessionEventMinimal", SessionEvent{SessionID: "s1", Type: SessionEventTextDelta, Text: "hi"},
			`{"sessionId":"s1","type":"text_delta","text":"hi"}`},
		{"SessionEventFull", SessionEvent{
			SessionID: "s1", Type: SessionEventToolEnd, Text: "t", ToolCallID: "c1", ToolName: "bash",
			Arguments: json.RawMessage(`{"command":"ls"}`), Result: "r", IsError: true, Message: &msg,
			TokensBefore: 1, TokensAfter: 2, SummarizedCount: 3, KeptCount: 4, CompactionError: "e",
			Description: "d", Activity: "a",
		},
			`{"sessionId":"s1","type":"tool_end","text":"t","toolCallId":"c1","toolName":"bash","arguments":{"command":"ls"},"result":"r","isError":true,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"end_turn","timestamp":1714566615000},"tokensBefore":1,"tokensAfter":2,"summarizedCount":3,"keptCount":4,"compactionError":"e","description":"d","activity":"a"}`},
		{"Error", &Error{Code: CodeParse, Message: "bad", Data: "d"}, `{"code":-32700,"message":"bad","data":"d"}`},
		{"ErrorNoData", &Error{Code: CodeInternal, Message: "boom"}, `{"code":-32603,"message":"boom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("wire shape changed:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// wireFieldNames returns the JSON keys of a struct in declaration order.
func wireFieldNames(t reflect.Type) []string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}

// TestPayloadFieldNamesAreStable guards against a tag rename that the value-based
// tests would miss when the field happens to be empty.
func TestPayloadFieldNamesAreStable(t *testing.T) {
	want := map[string][]string{
		"Envelope":               {"id", "method", "params", "result", "error", "event"},
		"Error":                  {"code", "message", "data"},
		"HelloParams":            {"version", "token", "client"},
		"HelloResult":            {"version", "pid", "build", "startedAt"},
		"ServerStatusResult":     {"pid", "build", "startedAt", "sessions", "socket", "daemon"},
		"SessionCreateParams":    {"cwd", "model", "title"},
		"SessionInfo":            {"id", "cwd", "model", "provider", "title", "createdAt", "updatedAt", "running"},
		"SessionCreateResult":    {"session"},
		"SessionListResult":      {"sessions"},
		"SessionResumeParams":    {"id"},
		"SessionResumeResult":    {"session", "messages"},
		"SessionCloseParams":     {"id"},
		"PromptParams":           {"sessionId", "text"},
		"PromptResult":           {"sessionId", "text", "stopReason", "error"},
		"CancelParams":           {"sessionId"},
		"PermissionDecideParams": {"sessionId", "requestId", "approve", "always"},
		"PermissionRequest":      {"sessionId", "requestId", "toolName", "summary", "timeoutMs"},
		"SessionClosedEvent":     {"sessionId", "reason"},
		"ServerShutdownEvent":    {"reason"},
		"SessionEvent": {"sessionId", "type", "text", "toolCallId", "toolName", "arguments", "result",
			"isError", "message", "tokensBefore", "tokensAfter", "summarizedCount", "keptCount",
			"compactionError", "description", "activity"},
	}

	types := map[string]reflect.Type{
		"Envelope":               reflect.TypeOf(Envelope{}),
		"Error":                  reflect.TypeOf(Error{}),
		"HelloParams":            reflect.TypeOf(HelloParams{}),
		"HelloResult":            reflect.TypeOf(HelloResult{}),
		"ServerStatusResult":     reflect.TypeOf(ServerStatusResult{}),
		"SessionCreateParams":    reflect.TypeOf(SessionCreateParams{}),
		"SessionInfo":            reflect.TypeOf(SessionInfo{}),
		"SessionCreateResult":    reflect.TypeOf(SessionCreateResult{}),
		"SessionListResult":      reflect.TypeOf(SessionListResult{}),
		"SessionResumeParams":    reflect.TypeOf(SessionResumeParams{}),
		"SessionResumeResult":    reflect.TypeOf(SessionResumeResult{}),
		"SessionCloseParams":     reflect.TypeOf(SessionCloseParams{}),
		"PromptParams":           reflect.TypeOf(PromptParams{}),
		"PromptResult":           reflect.TypeOf(PromptResult{}),
		"CancelParams":           reflect.TypeOf(CancelParams{}),
		"PermissionDecideParams": reflect.TypeOf(PermissionDecideParams{}),
		"PermissionRequest":      reflect.TypeOf(PermissionRequest{}),
		"SessionClosedEvent":     reflect.TypeOf(SessionClosedEvent{}),
		"ServerShutdownEvent":    reflect.TypeOf(ServerShutdownEvent{}),
		"SessionEvent":           reflect.TypeOf(SessionEvent{}),
	}
	if len(types) != len(want) {
		t.Fatalf("table drift: %d types, %d expectations", len(types), len(want))
	}
	for name, typ := range types {
		got := wireFieldNames(typ)
		if !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s fields = %v, want %v", name, got, want[name])
		}
	}
}

// TestMethodAndEventNamesAreStable pins the dispatch strings: a renamed method
// is a client/server version break even though nothing fails to compile.
func TestMethodAndEventNamesAreStable(t *testing.T) {
	methods := map[string]string{
		"MethodHello":            MethodHello,
		"MethodServerStatus":     MethodServerStatus,
		"MethodServerShutdown":   MethodServerShutdown,
		"MethodSessionCreate":    MethodSessionCreate,
		"MethodSessionList":      MethodSessionList,
		"MethodSessionResume":    MethodSessionResume,
		"MethodSessionClose":     MethodSessionClose,
		"MethodPrompt":           MethodPrompt,
		"MethodCancel":           MethodCancel,
		"MethodPermissionDecide": MethodPermissionDecide,
	}
	wantMethods := map[string]string{
		"MethodHello":            "hello",
		"MethodServerStatus":     "server.status",
		"MethodServerShutdown":   "server.shutdown",
		"MethodSessionCreate":    "session.create",
		"MethodSessionList":      "session.list",
		"MethodSessionResume":    "session.resume",
		"MethodSessionClose":     "session.close",
		"MethodPrompt":           "prompt",
		"MethodCancel":           "cancel",
		"MethodPermissionDecide": "permission.decide",
	}
	events := map[string]string{
		"EventSession":        EventSession,
		"EventPermission":     EventPermission,
		"EventSessionClosed":  EventSessionClosed,
		"EventServerShutdown": EventServerShutdown,
	}
	wantEvents := map[string]string{
		"EventSession":        "session_event",
		"EventPermission":     "permission_request",
		"EventSessionClosed":  "session_closed",
		"EventServerShutdown": "server_shutdown",
	}
	sessionEvents := map[string]string{
		"SessionEventAgentStart":   SessionEventAgentStart,
		"SessionEventAgentEnd":     SessionEventAgentEnd,
		"SessionEventTextDelta":    SessionEventTextDelta,
		"SessionEventThinking":     SessionEventThinking,
		"SessionEventTurnEnd":      SessionEventTurnEnd,
		"SessionEventToolStart":    SessionEventToolStart,
		"SessionEventToolUpdate":   SessionEventToolUpdate,
		"SessionEventToolEnd":      SessionEventToolEnd,
		"SessionEventCompaction":   SessionEventCompaction,
		"SessionEventSubAgent":     SessionEventSubAgent,
		"SessionEventSubAgentDone": SessionEventSubAgentDone,
	}
	wantSessionEvents := map[string]string{
		"SessionEventAgentStart":   "agent_start",
		"SessionEventAgentEnd":     "agent_end",
		"SessionEventTextDelta":    "text_delta",
		"SessionEventThinking":     "thinking_delta",
		"SessionEventTurnEnd":      "turn_end",
		"SessionEventToolStart":    "tool_start",
		"SessionEventToolUpdate":   "tool_update",
		"SessionEventToolEnd":      "tool_end",
		"SessionEventCompaction":   "compaction",
		"SessionEventSubAgent":     "subagent_progress",
		"SessionEventSubAgentDone": "subagent_end",
	}

	checkStableNames(t, "method", methods, wantMethods)
	checkStableNames(t, "event", events, wantEvents)
	checkStableNames(t, "session event", sessionEvents, wantSessionEvents)
}

// checkStableNames asserts the name→value table matches the expectation, has no
// empty or duplicated value, and covers every expectation (so a new constant
// cannot be added without stating its wire string here).
func checkStableNames(t *testing.T, kind string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s table drift: %d constants, %d expectations", kind, len(got), len(want))
	}
	seen := make(map[string]string, len(got))
	for name, value := range got {
		if value != want[name] {
			t.Errorf("%s %s = %q, want %q", kind, name, value, want[name])
		}
		if value == "" {
			t.Errorf("%s %s is empty", kind, name)
			continue
		}
		if other, dup := seen[value]; dup {
			t.Errorf("%s value %q is shared by %s and %s", kind, value, other, name)
		}
		seen[value] = name
	}
}

func TestErrorCodes(t *testing.T) {
	// The numeric space mirrors JSON-RPC where it overlaps, so a generic client
	// can reuse its error handling; the 1xxx range is host-specific.
	spec := map[string]struct {
		got  int
		want int
	}{
		"CodeParse":          {CodeParse, -32700},
		"CodeInvalidRequest": {CodeInvalidRequest, -32600},
		"CodeMethodNotFound": {CodeMethodNotFound, -32601},
		"CodeInvalidParams":  {CodeInvalidParams, -32602},
		"CodeInternal":       {CodeInternal, -32603},
		"CodeUnauthorized":   {CodeUnauthorized, 1001},
		"CodeNotFound":       {CodeNotFound, 1002},
		"CodeBusy":           {CodeBusy, 1003},
		"CodeForbidden":      {CodeForbidden, 1004},
		"CodeUnavailable":    {CodeUnavailable, 1005},
	}
	seen := make(map[int]string, len(spec))
	for name, c := range spec {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", name, c.got, c.want)
		}
		if other, dup := seen[c.got]; dup {
			t.Errorf("%s and %s share code %d", other, name, c.got)
		}
		seen[c.got] = name
	}
	if Version != 1 {
		t.Fatalf("protocol Version = %d, want 1", Version)
	}
}

func TestErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"message_only", &Error{Code: CodeBusy, Message: "busy"}, "busy"},
		{"with_data", &Error{Code: CodeInvalidParams, Message: "bad params", Data: "sessionId required"}, "bad params: sessionId required"},
		{"empty_message", &Error{Code: CodeInternal}, ""},
		{"nil_receiver", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil *Error is reachable through an Envelope field, so Error() must
			// not panic and must not be mistaken for a real failure message.
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}

	built := Err(CodeNotFound, "no such session")
	if built.Code != CodeNotFound || built.Message != "no such session" || built.Data != "" {
		t.Fatalf("Err() = %+v", built)
	}
}

func TestEnvelopeClassification(t *testing.T) {
	cases := []struct {
		name        string
		env         Envelope
		wantEvent   bool
		wantRespond bool
	}{
		{"zero", Envelope{}, false, false},
		{"request", Envelope{ID: "1", Method: MethodHello}, false, false},
		{"request_no_id", Envelope{Method: MethodHello}, false, false},
		{"response_result", Envelope{ID: "1", Result: json.RawMessage(`{}`)}, false, true},
		{"response_null_result", Envelope{ID: "1", Result: json.RawMessage(`null`)}, false, true},
		{"response_error", Envelope{ID: "1", Error: Err(CodeInternal, "x")}, false, true},
		{"response_no_body", Envelope{ID: "1"}, false, false},
		{"event", Envelope{Event: EventSession}, true, false},
		{"event_with_params", Envelope{Event: EventPermission, Params: json.RawMessage(`{}`)}, true, false},
		// A line carrying both an event and a method is an event: the event field
		// is the discriminant a client switches on.
		{"event_and_method", Envelope{Event: EventSession, Method: MethodPrompt}, true, false},
		{"event_with_id", Envelope{Event: EventSessionClosed, ID: "9"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.env.IsEvent(); got != tc.wantEvent {
				t.Fatalf("IsEvent() = %v, want %v", got, tc.wantEvent)
			}
			if got := tc.env.IsResponse(); got != tc.wantRespond {
				t.Fatalf("IsResponse() = %v, want %v", got, tc.wantRespond)
			}
		})
	}
}

func TestDecodeParamsEdgeCases(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		p := PromptParams{SessionID: "keep", Text: "keep"}
		if err := (Envelope{Method: MethodCancel}).DecodeParams(&p); err != nil {
			t.Fatalf("absent params: %v", err)
		}
		if p.SessionID != "keep" || p.Text != "keep" {
			t.Fatalf("absent params mutated the value: %+v", p)
		}
	})
	t.Run("json_null", func(t *testing.T) {
		p := PromptParams{SessionID: "keep"}
		if err := (Envelope{Params: json.RawMessage(`null`)}).DecodeParams(&p); err != nil {
			t.Fatalf("null params: %v", err)
		}
		if p.SessionID != "keep" {
			t.Fatalf("null params mutated the value: %+v", p)
		}
	})
	t.Run("empty_object", func(t *testing.T) {
		var p SessionCreateParams
		if err := (Envelope{Params: json.RawMessage(`{}`)}).DecodeParams(&p); err != nil {
			t.Fatalf("empty object: %v", err)
		}
		if p != (SessionCreateParams{}) {
			t.Fatalf("empty object produced %+v", p)
		}
	})
	t.Run("whitespace_only", func(t *testing.T) {
		// Non-empty but not JSON: must be an error, and must not panic.
		var p PromptParams
		if err := (Envelope{Params: json.RawMessage(" \n")}).DecodeParams(&p); err == nil {
			t.Fatal("whitespace-only params decoded without error")
		}
	})
	t.Run("invalid_json", func(t *testing.T) {
		var p PromptParams
		if err := (Envelope{Params: json.RawMessage(`{"sessionId":`)}).DecodeParams(&p); err == nil {
			t.Fatal("truncated params decoded without error")
		}
	})
	t.Run("wrong_shape", func(t *testing.T) {
		var p PromptParams
		if err := (Envelope{Params: json.RawMessage(`[1,2,3]`)}).DecodeParams(&p); err == nil {
			t.Fatal("array params decoded into a struct without error")
		}
	})
	t.Run("nil_target", func(t *testing.T) {
		// A handler that passes a nil pointer must get an error rather than a
		// panic on the server's dispatch goroutine.
		if err := (Envelope{Params: json.RawMessage(`{"sessionId":"s"}`)}).DecodeParams(nil); err == nil {
			t.Fatal("DecodeParams(nil) returned no error")
		}
		if err := (Envelope{Result: json.RawMessage(`{"text":"x"}`)}).DecodeResult(nil); err == nil {
			t.Fatal("DecodeResult(nil) returned no error")
		}
	})
	t.Run("unknown_fields_ignored", func(t *testing.T) {
		var p PromptParams
		raw := `{"sessionId":"s","text":"hi","future":"field","nested":{"a":1}}`
		if err := (Envelope{Params: json.RawMessage(raw)}).DecodeParams(&p); err != nil {
			t.Fatalf("unknown fields: %v", err)
		}
		if p.SessionID != "s" || p.Text != "hi" {
			t.Fatalf("params = %+v", p)
		}
	})
	t.Run("result_absent", func(t *testing.T) {
		r := PromptResult{Text: "keep"}
		if err := (Envelope{ID: "1", Error: Err(CodeBusy, "busy")}).DecodeResult(&r); err != nil {
			t.Fatalf("absent result: %v", err)
		}
		if r.Text != "keep" {
			t.Fatalf("absent result mutated the value: %+v", r)
		}
	})
}

// TestUnknownEnvelopeValues ensures forward compatibility: a newer peer's method,
// event or session-event type decodes into the typed structs without panicking,
// so a version skew degrades to "ignored notification" instead of a crash.
func TestUnknownEnvelopeValues(t *testing.T) {
	line := `{"id":"7","method":"future.method","params":{"anything":true}}`
	var env Envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("unmarshal unknown method: %v", err)
	}
	if env.Method != "future.method" || env.IsEvent() || env.IsResponse() {
		t.Fatalf("unknown method envelope = %+v", env)
	}
	var generic map[string]any
	if err := env.DecodeParams(&generic); err != nil {
		t.Fatalf("decode unknown params: %v", err)
	}
	if generic["anything"] != true {
		t.Fatalf("unknown params = %v", generic)
	}

	eventLine := `{"event":"future_event","params":{"sessionId":"s1","type":"future_type","newField":1}}`
	var ev Envelope
	if err := json.Unmarshal([]byte(eventLine), &ev); err != nil {
		t.Fatalf("unmarshal unknown event: %v", err)
	}
	if !ev.IsEvent() {
		t.Fatalf("unknown event not classified as an event: %+v", ev)
	}
	var se SessionEvent
	if err := ev.DecodeParams(&se); err != nil {
		t.Fatalf("decode unknown session event: %v", err)
	}
	if se.SessionID != "s1" || se.Type != "future_type" {
		t.Fatalf("unknown session event = %+v", se)
	}
	// The client-side reaction is "not my type": nothing to render, nothing to do.
	if se.Type != SessionEventTextDelta && se.Text != "" {
		t.Fatalf("an unknown event carried text: %+v", se)
	}
}

func TestInvalidEnvelopeLine(t *testing.T) {
	lines := []string{
		``,
		`{`,
		`{"id":}`,
		`not json`,
		`{"id":"1"} trailing`,
		`{"id":1}`,
	}
	for _, line := range lines {
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err == nil {
			t.Fatalf("line %q decoded without error", line)
		}
	}
	// A zero envelope is safe to interrogate and to decode into: the server
	// builds one before it has any input.
	var zero Envelope
	if zero.IsEvent() || zero.IsResponse() {
		t.Fatal("zero envelope classified as something")
	}
	var p PromptParams
	if err := zero.DecodeParams(&p); err != nil {
		t.Fatalf("zero envelope DecodeParams: %v", err)
	}
	if err := zero.DecodeResult(&p); err != nil {
		t.Fatalf("zero envelope DecodeResult: %v", err)
	}
}

func TestBigPayloads(t *testing.T) {
	// A prompt near the practical size limit and a transcript-sized message list
	// must survive the trip; the envelope layer never inspects payload size, and
	// a regression to line-buffered parsing would show up here as truncation.
	big := strings.Repeat("x", 1<<20)
	roundTripParams(t, PromptParams{SessionID: "s1", Text: big})

	line, err := json.Marshal(Envelope{Event: EventSession, Params: mustJSON(t, SessionEvent{
		SessionID: "s1", Type: SessionEventTextDelta, Text: big,
	})})
	if err != nil {
		t.Fatalf("marshal big event: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshal big event: %v", err)
	}
	var se SessionEvent
	if err := env.DecodeParams(&se); err != nil {
		t.Fatalf("decode big event: %v", err)
	}
	if len(se.Text) != len(big) {
		t.Fatalf("big delta truncated to %d bytes", len(se.Text))
	}

	msgs := make(agentcore.MessageList, 0, 500)
	for i := 0; i < 500; i++ {
		msgs = append(msgs, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(big[:1024])},
			Timestamp: int64(i),
		})
	}
	roundTripResult(t, SessionResumeResult{Session: wireSessionInfo(), Messages: msgs})
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
