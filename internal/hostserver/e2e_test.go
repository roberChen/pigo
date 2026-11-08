// This file is the host's end-to-end suite. Unlike server_test.go (which scripts
// the Factory to isolate the machine), everything here goes through the
// production path — DefaultFactory → run.SetupEnvIn → the real tool registry and
// executors → the real runtime loop → a real provider speaking HTTP/SSE. Only
// the model server is fake, because "the model" is the one thing that cannot be
// deterministic.
//
// The distinction matters for an architecture split: the scripted tests prove the
// socket/protocol machine, while these prove that a session created over the
// wire actually gets tools rooted in its own cwd, its own system prompt, its
// hooks, its permission gate and its on-disk transcript — the whole reason the
// host exists.
package hostserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// ---- fake OpenAI-compatible model --------------------------------------

// llmStep is one scripted model reply. At most one of Text/Tool/Status is used.
type llmStep struct {
	// Text is streamed as content deltas and ends with finish_reason=stop.
	Text string
	// Tool asks the loop to run a tool and ends with finish_reason=tool_calls.
	Tool *llmToolCall
	// Status, when non-zero, answers with that HTTP status instead of a stream.
	Status int
	Body   string
	// Delay is applied between streamed text chunks.
	Delay time.Duration
	// Hold blocks mid-stream (after the first delta) so a test can cancel a turn
	// that is genuinely in flight. It returns as soon as the client disconnects.
	Hold time.Duration
}

// llmToolCall is a tool invocation the fake model asks for.
type llmToolCall struct {
	ID   string
	Name string
	// Args is the raw JSON arguments object.
	Args string
}

// llmRequest is one recorded call the loop made, decoded enough to assert on.
type llmRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools"`
	Raw      []byte           `json:"-"`
}

// fakeLLM is an OpenAI-compatible streaming endpoint driven by a script. Each
// request consumes the next step; a request past the end of the script answers
// with a plain text reply so an unexpected extra turn is visible but harmless.
type fakeLLM struct {
	t  *testing.T
	mu sync.Mutex
	// steps is the reply script, consumed in order.
	steps []llmStep
	// requests records every body the loop sent, in arrival order.
	requests []llmRequest
	// stepIndex is how many replies have been served.
	stepIndex int
}

// newFakeLLM starts an httptest server that speaks the Chat Completions SSE
// format and returns it with the base URL to hand DefaultFactory.
func newFakeLLM(t *testing.T, steps ...llmStep) (*fakeLLM, string) {
	t.Helper()
	f := &fakeLLM{t: t, steps: steps}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1"
}

// serve answers one streaming completion request.
func (f *fakeLLM) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request: %v", err)
		return
	}
	var req llmRequest
	req.Raw = body
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	var step llmStep
	if f.stepIndex < len(f.steps) {
		step = f.steps[f.stepIndex]
	}
	f.stepIndex++
	f.mu.Unlock()

	if step.Status != 0 {
		body := step.Body
		if body == "" {
			body = `{"error":{"message":"scripted upstream failure"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.Status)
		_, _ = w.Write([]byte(body))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	if step.Tool != nil {
		id := step.Tool.ID
		if id == "" {
			id = "call_1"
		}
		// Split the arguments across two chunks: a single-chunk test would pass
		// even if the decoder's accumulation path were broken.
		half := len(step.Tool.Args) / 2
		f.writeSSE(w, fmt.Sprintf(
			`{"id":"c1","model":"e2e","choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
			id, step.Tool.Name, step.Tool.Args[:half]))
		flush()
		f.writeSSE(w, fmt.Sprintf(
			`{"id":"c1","model":"e2e","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`,
			step.Tool.Args[half:]))
		flush()
		f.writeSSE(w, `{"id":"c1","model":"e2e","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	} else {
		text := step.Text
		if text == "" {
			text = "ok"
		}
		for i, part := range splitEvery(text, 3) {
			f.writeSSE(w, fmt.Sprintf(
				`{"id":"c1","model":"e2e","choices":[{"delta":{"content":%q}}]}`, part))
			flush()
			if i == 0 && step.Hold > 0 {
				if !sleepOrDone(r.Context(), step.Hold) {
					return
				}
			}
			if step.Delay > 0 {
				if !sleepOrDone(r.Context(), step.Delay) {
					return
				}
			}
		}
		f.writeSSE(w, `{"id":"c1","model":"e2e","choices":[{"delta":{},"finish_reason":"stop"}]}`)
	}
	f.writeSSE(w, `{"id":"c1","model":"e2e","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
	f.writeSSE(w, `[DONE]`)
}

// sleepOrDone waits for d, returning false early if the request context ends
// (the client cancelled), so a held-open stream does not pin the test server.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// writeSSE writes one data frame.
func (f *fakeLLM) writeSSE(w http.ResponseWriter, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}

// requestCount is how many completions the loop requested.
func (f *fakeLLM) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// requestAt returns the i-th recorded request (0-based).
func (f *fakeLLM) requestAt(i int) llmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.requests) {
		f.t.Fatalf("no request %d (only %d recorded)", i, len(f.requests))
	}
	return f.requests[i]
}

// waitRequests blocks until the loop has made n requests or fails the test.
func (f *fakeLLM) waitRequests(n int) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.requestCount() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %d requests (have %d)", n, f.requestCount())
}

// splitEvery splits s into n roughly equal non-empty parts.
func splitEvery(s string, n int) []string {
	if len(s) <= n {
		parts := make([]string, 0, len(s))
		for _, r := range s {
			parts = append(parts, string(r))
		}
		if len(parts) == 0 {
			return []string{""}
		}
		return parts
	}
	size := (len(s) + n - 1) / n
	var out []string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// ---- fixtures ----------------------------------------------------------

// e2eEnv prepares the process environment a real run reads and returns a Factory
// wired to the fake model. Every test gets its own PIGO_HOME and skills dir so
// nothing touches the developer's state (AGENTS.md).
func e2eEnv(t *testing.T, baseURL string, mutate ...func(*DefaultFactory)) *DefaultFactory {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	t.Setenv("PIGO_SKILLS_DIR", filepath.Join(home, "skills"))
	// The explicit APIKey on the factory overrides the environment; set a dummy
	// one anyway so a provider that reads env directly still starts.
	t.Setenv("OPENAI_API_KEY", "e2e-key")

	f := &DefaultFactory{
		Model:    "e2e-model",
		BaseURL:  baseURL,
		Protocol: "openai",
		Provider: "openai",
		APIKey:   "e2e-key",
		NoSkills: true,
		Memory:   false,
	}
	for _, m := range mutate {
		m(f)
	}
	return f
}

// startHost starts a host with the given factory and returns its fixture. The
// store and trust manager are temp files, so a test can neither read nor write
// the developer's real state.
func startHost(t *testing.T, f Factory, mutate ...func(*Config)) *fixture {
	t.Helper()
	return newFixture(t, f, mutate...)
}

// startHostOn starts a server over an explicit store and trust manager, so two
// servers can share the state a real restart would preserve.
func startHostOn(t *testing.T, f Factory, store *session.Store, mgr *trust.Manager) *fixture {
	t.Helper()
	dir := t.TempDir()
	srv, err := NewServer(Config{Dir: dir, Factory: f, Store: store, Trust: mgr, Build: "e2e"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		srv.Shutdown("test cleanup")
		<-done
	})
	token, err := ReadToken(dir)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	return &fixture{srv: srv, token: token, store: store, dir: dir}
}

// createSession creates a session rooted at cwd and returns its id.
func createSession(t *testing.T, c *testClient, cwd string) string {
	t.Helper()
	res := decodeResult[hostapi.SessionCreateResult](t,
		c.mustCall(hostapi.MethodSessionCreate, hostapi.SessionCreateParams{Cwd: cwd}))
	if res.Session.ID == "" {
		t.Fatal("session id is empty")
	}
	return res.Session.ID
}

// prompt runs one turn and returns the result envelope.
func prompt(t *testing.T, c *testClient, sid, text string) *hostapi.Envelope {
	t.Helper()
	return c.mustCall(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: text})
}

// streamedText concatenates every text_delta event currently buffered, without
// consuming them (safe to poll).
func streamedText(c *testClient) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, env := range c.events {
		if env.Event != hostapi.EventSession {
			continue
		}
		var ev hostapi.SessionEvent
		if err := env.DecodeParams(&ev); err != nil || ev.Type != hostapi.SessionEventTextDelta {
			continue
		}
		b.WriteString(ev.Text)
	}
	return b.String()
}

// takeTextDeltas drains and concatenates the buffered text deltas.
func takeTextDeltas(c *testClient) string {
	var b strings.Builder
	for _, ev := range c.collectEvents(hostapi.SessionEventTextDelta) {
		b.WriteString(ev.Text)
	}
	return b.String()
}

// toolEndByName drains tool_end events until one for name arrives.
func toolEndByName(t *testing.T, c *testClient, name string) hostapi.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, ev := range c.collectEvents(hostapi.SessionEventToolEnd) {
			if ev.ToolName == name {
				return ev
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no tool_end for %q", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// toolNames on the wire, in order.
func wireToolNames(req llmRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if fn, ok := tool["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// wireContainsToolResult reports whether a recorded request carries needle in a
// role:"tool" message.
func wireContainsToolResult(req llmRequest, needle string) bool {
	for _, m := range req.Messages {
		if m["role"] != "tool" {
			continue
		}
		if content, _ := m["content"].(string); strings.Contains(content, needle) {
			return true
		}
	}
	return false
}

// wireRoles renders the role sequence of a recorded request.
func wireRoles(req llmRequest) string {
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		roles = append(roles, fmt.Sprint(m["role"]))
	}
	return strings.Join(roles, ",")
}

// ---- tests -------------------------------------------------------------

// TestE2E_RealFactoryStreamsAndPersists is the baseline: a session created over
// the wire runs a real provider and lands on disk.
func TestE2E_RealFactoryStreamsAndPersists(t *testing.T) {
	llm, baseURL := newFakeLLM(t, llmStep{Text: "Hello from the fake model"})
	cwd := t.TempDir()
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, cwd)

	res := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "say hello"))
	if res.Text != "Hello from the fake model" {
		t.Fatalf("final text = %q", res.Text)
	}
	if res.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if got := takeTextDeltas(c); got != "Hello from the fake model" {
		t.Fatalf("streamed deltas = %q", got)
	}
	if n := llm.requestCount(); n != 1 {
		t.Fatalf("provider requests = %d, want 1", n)
	}

	// The assembled system prompt reached the wire and names the session cwd.
	req := llm.requestAt(0)
	if len(req.Messages) == 0 || req.Messages[0]["role"] != "system" {
		t.Fatalf("first wire message is not the system prompt: %+v", req.Messages)
	}
	if sp, _ := req.Messages[0]["content"].(string); !strings.Contains(sp, cwd) {
		t.Fatalf("system prompt does not mention the session cwd %q: %q", cwd, sp)
	}
	if req.Model != "e2e-model" {
		t.Fatalf("wire model = %q", req.Model)
	}

	// The transcript is on disk with both turns.
	header, msgs, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if header.Cwd != cwd {
		t.Fatalf("header cwd = %q, want %q", header.Cwd, cwd)
	}
	if len(msgs) != 2 {
		t.Fatalf("persisted messages = %d, want 2 (user + assistant): %+v", len(msgs), msgs)
	}
	if msgs[1].Role() != agentcore.RoleAssistant {
		t.Fatalf("second message role = %q", msgs[1].Role())
	}
	if a, ok := msgs[1].(agentcore.AssistantMessage); !ok || agentcore.ContentToText(a.Content) != "Hello from the fake model" {
		t.Fatalf("persisted assistant message = %+v", msgs[1])
	}
}

// TestE2E_RealToolCallRoundTrip drives a real tool through the real registry and
// verifies the result is fed back to the model on the next request. A scripted
// factory cannot catch a broken registry/tool wiring; this can.
func TestE2E_RealToolCallRoundTrip(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "data.txt"), []byte("the secret is 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	llm, baseURL := newFakeLLM(t,
		llmStep{Tool: &llmToolCall{ID: "call_read", Name: "read", Args: `{"path":"data.txt"}`}},
		llmStep{Text: "The secret is 42."},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, cwd)

	res := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "read data.txt"))
	if res.Text != "The secret is 42." {
		t.Fatalf("final text = %q", res.Text)
	}

	if ev, ok := c.takeEvent(hostapi.SessionEventToolStart); !ok || ev.ToolName != "read" {
		t.Fatalf("missing tool_start for read: %+v (found=%v)", ev, ok)
	}
	end := toolEndByName(t, c, "read")
	if end.IsError {
		t.Fatalf("read failed: %s", end.Result)
	}
	if !strings.Contains(end.Result, "the secret is 42") {
		t.Fatalf("tool_end result = %q", end.Result)
	}

	llm.waitRequests(2)
	if !wireContainsToolResult(llm.requestAt(1), "the secret is 42") {
		t.Fatalf("tool result did not reach the model: %+v", llm.requestAt(1).Messages)
	}
}

// TestE2E_PermissionApproveWritesFile is the security-critical path end to end:
// an untrusted directory makes the host ask, and the answer lets the *real*
// write tool mutate the real file.
func TestE2E_PermissionApproveWritesFile(t *testing.T) {
	cwd := t.TempDir() // untrusted: fresh dir + temp trust store
	_, baseURL := newFakeLLM(t,
		llmStep{Tool: &llmToolCall{Name: "write", Args: `{"path":"out.txt","content":"approved\n"}`}},
		llmStep{Text: "wrote it"},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, cwd)

	type callResult struct {
		resp *hostapi.Envelope
		err  error
	}
	done := make(chan callResult, 1)
	go func() {
		resp, err := c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "write out.txt"})
		done <- callResult{resp, err}
	}()

	env := c.waitEvent(t, hostapi.EventPermission)
	var req hostapi.PermissionRequest
	if err := env.DecodeParams(&req); err != nil {
		t.Fatalf("decode permission: %v", err)
	}
	if req.ToolName != "write" {
		t.Fatalf("permission asked for %q", req.ToolName)
	}
	if !strings.Contains(req.Summary, "out.txt") {
		t.Fatalf("permission summary lacks the path: %q", req.Summary)
	}
	dec := decodeResult[struct {
		OK bool `json:"ok"`
	}](t, c.mustCall(hostapi.MethodPermissionDecide, hostapi.PermissionDecideParams{
		SessionID: sid, RequestID: req.RequestID, Approve: true,
	}))
	if !dec.OK {
		t.Fatal("permission.decide returned ok=false")
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("prompt: %v", r.err)
	}
	res := decodeResult[hostapi.PromptResult](t, r.resp)
	if res.Text != "wrote it" {
		t.Fatalf("final text = %q", res.Text)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "out.txt"))
	if err != nil {
		t.Fatalf("approved write did not create the file: %v", err)
	}
	if string(data) != "approved\n" {
		t.Fatalf("file content = %q", data)
	}
}

// TestE2E_PermissionDenyPreventsWrite is the fail-closed counterpart: no answer
// means the tool never runs, and the model is told why.
func TestE2E_PermissionDenyPreventsWrite(t *testing.T) {
	cwd := t.TempDir()
	_, baseURL := newFakeLLM(t,
		llmStep{Tool: &llmToolCall{Name: "write", Args: `{"path":"out.txt","content":"nope\n"}`}},
		llmStep{Text: "blocked"},
	)
	// A short timeout makes "no answer" reachable without a second client.
	fx := startHost(t, e2eEnv(t, baseURL), func(cfg *Config) {
		cfg.PermissionTimeout = 150 * time.Millisecond
	})
	c := fx.dial(t)
	sid := createSession(t, c, cwd)

	res := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "write out.txt"))
	if res.Text != "blocked" {
		t.Fatalf("final text = %q", res.Text)
	}
	end := toolEndByName(t, c, "write")
	if !end.IsError {
		t.Fatalf("denied write reported success: %+v", end)
	}
	if !strings.Contains(end.Result, "not trusted") {
		t.Fatalf("denial reason = %q", end.Result)
	}
	if _, err := os.Stat(filepath.Join(cwd, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write created a file: %v", err)
	}
}

// TestE2E_BashToolRunsRealProcess covers the other side of the gate: a trusted
// directory lets a real process run through the real executor.
func TestE2E_BashToolRunsRealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash is not available on windows")
	}
	cwd := t.TempDir()
	_, baseURL := newFakeLLM(t,
		llmStep{Tool: &llmToolCall{Name: "bash", Args: `{"command":"printf e2e-ran > marker.txt"}`}},
		llmStep{Text: "done"},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	// The fixture's own trust manager is what the server consults; granting
	// session trust here is the in-process equivalent of --approve.
	fx.srv.trust.SetSessionTrust(cwd)

	c := fx.dial(t)
	sid := createSession(t, c, cwd)
	res := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "run it"))
	if res.Text != "done" {
		t.Fatalf("final text = %q", res.Text)
	}
	end := toolEndByName(t, c, "bash")
	if end.IsError {
		t.Fatalf("bash failed: %s", end.Result)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "marker.txt"))
	if err != nil {
		t.Fatalf("bash did not create the file: %v", err)
	}
	if string(data) != "e2e-ran" {
		t.Fatalf("marker = %q", data)
	}
}

// TestE2E_ToolPolicyIsStructural proves a --allowed-tools boundary holds through
// the host: the tool is absent from the wire schema AND a model that asks for it
// anyway is rejected by the registry.
func TestE2E_ToolPolicyIsStructural(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "data.txt"), []byte("visible\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	llm, baseURL := newFakeLLM(t,
		// The model ignores the schema and asks for a tool it was not given.
		llmStep{Tool: &llmToolCall{Name: "write", Args: `{"path":"evil.txt","content":"x"}`}},
		llmStep{Text: "cannot"},
	)
	fx := startHost(t, e2eEnv(t, baseURL, func(f *DefaultFactory) {
		f.Policy = run.NewToolPolicy([]string{"read"}, nil)
	}))
	c := fx.dial(t)
	sid := createSession(t, c, cwd)

	decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "try to write"))

	names := wireToolNames(llm.requestAt(0))
	if len(names) != 1 || names[0] != "read" {
		t.Fatalf("tool policy did not bound the schema: %v", names)
	}

	end := toolEndByName(t, c, "write")
	if !end.IsError || !strings.Contains(end.Result, "unknown tool") {
		t.Fatalf("out-of-policy tool was not rejected: %+v", end)
	}
	if _, err := os.Stat(filepath.Join(cwd, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("out-of-policy tool wrote a file: %v", err)
	}
}

// TestE2E_ProviderHTTPErrorSurfacesAndSessionSurvives checks that a 500 is
// reported to the client without poisoning the session: the next turn works.
func TestE2E_ProviderHTTPErrorSurfacesAndSessionSurvives(t *testing.T) {
	llm, baseURL := newFakeLLM(t,
		llmStep{Status: http.StatusInternalServerError},
		llmStep{Text: "recovered"},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, t.TempDir())

	res := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "boom"))
	if res.StopReason != agentcore.StopReasonError {
		t.Fatalf("stop reason = %q, want error", res.StopReason)
	}
	if !strings.Contains(res.Error, "500") {
		t.Fatalf("in-band error does not name the status: %q", res.Error)
	}
	// The failed turn must not be persisted as if the model had answered: the
	// transcript holds the user message and the error turn, and a resume replays
	// both rather than silently dropping the question.
	_, msgs, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Role() != agentcore.RoleUser {
		t.Fatalf("failed turn transcript = %+v", msgs)
	}

	// Same session, new turn: the agent was not torn down by the failure.
	res = decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "again"))
	if res.Text != "recovered" {
		t.Fatalf("recovery turn text = %q", res.Text)
	}
	if n := llm.requestCount(); n != 2 {
		t.Fatalf("provider requests = %d, want 2", n)
	}
}

// TestE2E_CancelMidStream aborts a real in-flight turn and keeps the session
// usable. The provider holds the stream open, so the transport must be cancelled
// rather than merely ignored.
func TestE2E_CancelMidStream(t *testing.T) {
	_, baseURL := newFakeLLM(t,
		llmStep{Text: "a long partial answer", Hold: 30 * time.Second},
		llmStep{Text: "after cancel"},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sid := createSession(t, c, t.TempDir())

	type callResult struct {
		resp *hostapi.Envelope
		err  error
	}
	done := make(chan callResult, 1)
	go func() {
		resp, err := c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "talk"})
		done <- callResult{resp, err}
	}()

	// Wait until the model has actually started answering.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(streamedText(c), "long") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(streamedText(c), "long") {
		t.Fatal("the fake model never streamed; cannot test a mid-stream cancel")
	}

	start := time.Now()
	c.mustCall(hostapi.MethodCancel, hostapi.CancelParams{SessionID: sid})
	r := <-done
	if r.err != nil {
		t.Fatalf("cancel returned a transport error: %v", r.err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancel took %v; the stream was not aborted", elapsed)
	}
	res := decodeResult[hostapi.PromptResult](t, r.resp)
	if res.StopReason != agentcore.StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted", res.StopReason)
	}
	if res.Error != "" {
		t.Fatalf("a user cancel was reported as a failure: %q", res.Error)
	}
	// The partial answer the user already saw is in the reply (and in the
	// transcript), so the client can keep rendering it after the abort banner.
	if res.Text == "" || !strings.HasPrefix("a long partial answer", res.Text) {
		t.Fatalf("aborted turn lost its partial text: %q (stop=%q err=%q streamed=%q)", res.Text, res.StopReason, res.Error, streamedText(c))
	}

	// The partial turn is persisted: a cancelled turn is still history.
	_, msgs, err := fx.store.Load(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("cancelled turn was not persisted at all")
	}
	if got := agentcore.ContentToText(msgs[0].(agentcore.UserMessage).Content); got != "talk" {
		t.Fatalf("persisted first message = %q", got)
	}
	last := msgs[len(msgs)-1].(agentcore.AssistantMessage)
	if last.StopReason != agentcore.StopReasonAborted {
		t.Fatalf("persisted stop reason = %q, want aborted", last.StopReason)
	}
	if agentcore.ContentToText(last.Content) != res.Text {
		t.Fatalf("persisted partial = %q, reply partial = %q", agentcore.ContentToText(last.Content), res.Text)
	}

	// And the session still works.
	res2 := decodeResult[hostapi.PromptResult](t, prompt(t, c, sid, "continue"))
	if res2.Text != "after cancel" {
		t.Fatalf("post-cancel text = %q", res2.Text)
	}
}

// TestE2E_ResumeAfterHostRestart persists a session, kills the host, starts a
// new host on the same store and resumes: the replayed history must reach the
// model on the next turn.
func TestE2E_ResumeAfterHostRestart(t *testing.T) {
	llm, baseURL := newFakeLLM(t,
		llmStep{Text: "first answer"},
		llmStep{Text: "second answer"},
	)
	factory := e2eEnv(t, baseURL)

	// The store lives outside the host state dir so a second host reuses it, which
	// is exactly what a restart does.
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := trust.NewManager(filepath.Join(t.TempDir(), "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	first := startHostOn(t, factory, store, mgr)
	c1 := first.dial(t)
	sid := createSession(t, c1, cwd)
	decodeResult[hostapi.PromptResult](t, prompt(t, c1, sid, "first question"))
	c1.close()
	first.srv.Shutdown("restart")

	second := startHostOn(t, factory, store, mgr)
	c2 := second.dial(t)
	rr := decodeResult[hostapi.SessionResumeResult](t,
		c2.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}))
	if len(rr.Messages) != 2 {
		t.Fatalf("resumed messages = %d, want 2: %+v", len(rr.Messages), rr.Messages)
	}
	if rr.Session.Cwd != cwd {
		t.Fatalf("resumed cwd = %q, want %q", rr.Session.Cwd, cwd)
	}

	res := decodeResult[hostapi.PromptResult](t, prompt(t, c2, sid, "second question"))
	if res.Text != "second answer" {
		t.Fatalf("post-resume text = %q", res.Text)
	}
	// The restarted host must have replayed the whole conversation to the model.
	if got := wireRoles(llm.requestAt(1)); got != "system,user,assistant,user" {
		t.Fatalf("resumed wire history = %s", got)
	}
}

// TestE2E_SessionsAreRootedAtTheirOwnCwd proves the SetupEnvIn contract: one
// host serves sessions in different directories without mixing their tools,
// prompts or transcripts.
func TestE2E_SessionsAreRootedAtTheirOwnCwd(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dirA, "who.txt"), []byte("FILE-A"), 0o644))
	must(os.WriteFile(filepath.Join(dirB, "who.txt"), []byte("FILE-B"), 0o644))
	must(os.WriteFile(filepath.Join(dirA, "AGENTS.md"), []byte("PROJECT-A-RULES"), 0o644))
	must(os.WriteFile(filepath.Join(dirB, "AGENTS.md"), []byte("PROJECT-B-RULES"), 0o644))

	llm, baseURL := newFakeLLM(t,
		llmStep{Tool: &llmToolCall{Name: "read", Args: `{"path":"who.txt"}`}},
		llmStep{Text: "A"},
		llmStep{Tool: &llmToolCall{Name: "read", Args: `{"path":"who.txt"}`}},
		llmStep{Text: "B"},
	)
	fx := startHost(t, e2eEnv(t, baseURL))
	c := fx.dial(t)
	sidA := createSession(t, c, dirA)
	sidB := createSession(t, c, dirB)

	decodeResult[hostapi.PromptResult](t, prompt(t, c, sidA, "which project?"))
	decodeResult[hostapi.PromptResult](t, prompt(t, c, sidB, "which project?"))

	// Requests 0/1 belong to A, 2/3 to B (the two turns ran sequentially).
	reqA, reqB := llm.requestAt(0), llm.requestAt(2)
	if sp, _ := reqA.Messages[0]["content"].(string); !strings.Contains(sp, "PROJECT-A-RULES") || strings.Contains(sp, "PROJECT-B-RULES") {
		t.Fatalf("session A system prompt leaked the wrong AGENTS.md: %q", sp)
	}
	if sp, _ := reqB.Messages[0]["content"].(string); !strings.Contains(sp, "PROJECT-B-RULES") || strings.Contains(sp, "PROJECT-A-RULES") {
		t.Fatalf("session B system prompt leaked the wrong AGENTS.md: %q", sp)
	}
	if !wireContainsToolResult(llm.requestAt(1), "FILE-A") {
		t.Fatal("session A read the wrong file")
	}
	if !wireContainsToolResult(llm.requestAt(3), "FILE-B") {
		t.Fatal("session B read the wrong file")
	}
}

// TestE2E_UserPromptSubmitHookBlocks installs a real shell hook through the
// global config layer and checks the host honors its blocking decision.
func TestE2E_UserPromptSubmitHookBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook commands assume a POSIX shell")
	}
	home := t.TempDir()
	llm, baseURL := newFakeLLM(t, llmStep{Text: "should not run"})
	cfg := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo blocked by policy 1>&2; exit 2"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIGO_HOME", home)
	t.Setenv("PIGO_SKILLS_DIR", filepath.Join(home, "skills"))
	t.Setenv("OPENAI_API_KEY", "e2e-key")

	f := &DefaultFactory{
		Model: "e2e-model", BaseURL: baseURL, Protocol: "openai",
		Provider: "openai", APIKey: "e2e-key", NoSkills: true,
	}
	fx := startHost(t, f)
	c := fx.dial(t)
	sid := createSession(t, c, t.TempDir())

	resp := prompt(t, c, sid, "hello")
	if resp.Error == nil {
		t.Fatalf("blocking hook did not fail the prompt: %s", resp.Result)
	}
	if !strings.Contains(resp.Error.Message, "blocked by policy") {
		t.Fatalf("hook reason not surfaced: %s", resp.Error.Message)
	}
	if n := llm.requestCount(); n != 0 {
		t.Fatalf("provider was called despite the block (%d requests)", n)
	}
}

// TestE2E_ConcurrentSessionsStayIsolated runs several sessions at once, each
// with its own real agent. A shared mutable factory that leaked across sessions
// would show up as crossed answers.
func TestE2E_ConcurrentSessionsStayIsolated(t *testing.T) {
	const n = 4
	steps := make([]llmStep, 0, n)
	for i := 0; i < n; i++ {
		steps = append(steps, llmStep{Text: fmt.Sprintf("answer-%d", i), Delay: 5 * time.Millisecond})
	}
	llm, baseURL := newFakeLLM(t, steps...)
	fx := startHost(t, e2eEnv(t, baseURL))

	// Connections and sessions are created on the test goroutine: *testing.T
	// helpers must not be called from the worker goroutines.
	type job struct {
		c   *testClient
		sid string
	}
	jobs := make([]job, n)
	for i := 0; i < n; i++ {
		c := fx.dial(t)
		jobs[i] = job{c: c, sid: createSession(t, c, t.TempDir())}
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := jobs[i].c.call(hostapi.MethodPrompt, hostapi.PromptParams{
				SessionID: jobs[i].sid,
				Text:      fmt.Sprintf("q%d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			if resp.Error != nil {
				errs <- fmt.Errorf("session %d: %v", i, resp.Error)
				return
			}
			var res hostapi.PromptResult
			if err := resp.DecodeResult(&res); err != nil {
				errs <- err
				return
			}
			if !strings.HasPrefix(res.Text, "answer-") {
				errs <- fmt.Errorf("session %d got %q", i, res.Text)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := llm.requestCount(); n != 4 {
		t.Fatalf("provider requests = %d, want 4", n)
	}
	if got := fx.srv.liveSessionCount(); got != n {
		t.Fatalf("live sessions = %d, want %d", got, n)
	}
}

// TestE2E_DisconnectKeepsTheTurnRunning is the split's headline promise over the
// real stack: dropping the socket does not abort a turn. The turn finishes, its
// answer is persisted, and the session is then closed because nobody re-attached.
func TestE2E_DisconnectKeepsTheTurnRunning(t *testing.T) {
	llm, baseURL := newFakeLLM(t, llmStep{Text: "kept running", Hold: 300 * time.Millisecond})
	fx := startHost(t, e2eEnv(t, baseURL))

	c := fx.dial(t)
	cwd := t.TempDir()
	sid := createSession(t, c, cwd)

	type callResult struct {
		resp *hostapi.Envelope
		err  error
	}
	done := make(chan callResult, 1)
	go func() {
		resp, err := c.call(hostapi.MethodPrompt, hostapi.PromptParams{SessionID: sid, Text: "long task"})
		done <- callResult{resp, err}
	}()
	// Sever the socket while the model is still holding the stream open.
	llm.waitRequests(1)
	c.close()
	r := <-done
	// The request fails at the transport (there is nobody to answer), but that is
	// not the turn's fate: the assertion is on what lands on disk below.
	if r.err == nil && r.resp != nil && r.resp.Error == nil {
		t.Log("prompt reply survived the disconnect; the transcript check is authoritative")
	}

	// The host must finish the turn on its own and persist the answer with no
	// client attached at all.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, msgs, err := fx.store.Load(sid)
		if err == nil && len(msgs) >= 2 {
			if a, ok := msgs[1].(agentcore.AssistantMessage); ok &&
				agentcore.ContentToText(a.Content) == "kept running" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish and persist after the client disconnected")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The orphan is reaped once its turn ends: a dead client must not pin an
	// agent (and its provider connection) in the host forever.
	reap := time.Now().Add(10 * time.Second)
	for fx.srv.liveSessionCount() != 0 {
		if time.Now().After(reap) {
			t.Fatalf("orphaned session still live after its turn finished (sessions=%d)", fx.srv.liveSessionCount())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And the history is resumable from a fresh connection.
	c2 := fx.dial(t)
	rr := decodeResult[hostapi.SessionResumeResult](t,
		c2.mustCall(hostapi.MethodSessionResume, hostapi.SessionResumeParams{ID: sid}))
	if len(rr.Messages) < 2 {
		t.Fatalf("resume after disconnect returned %d messages", len(rr.Messages))
	}
}
