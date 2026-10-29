package mcp

// Tests for the MCP client: the initialize handshake, tool listing (with
// pagination), tool calling and content mapping, transport selection and validation,
// the HTTP (JSON and SSE) transport against a real server, and the manager's
// best-effort behaviour when a server is broken.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// fakeTransport is an in-memory MCP server for client tests.
type fakeTransport struct {
	mu       sync.Mutex
	calls    []string
	handlers map[string]func(params json.RawMessage) (any, error)
	closed   bool
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{handlers: map[string]func(json.RawMessage) (any, error){}}
}

func (f *fakeTransport) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, method)
	h := f.handlers[method]
	f.mu.Unlock()
	if h == nil {
		return nil, fmt.Errorf("unexpected method %q", method)
	}
	res, err := h(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func (f *fakeTransport) Notify(method string, _ any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// TestClientHandshakeListAndCall walks the protocol: initialize, initialized
// notification, paginated tools/list, and tools/call with content mapping.
func TestClientHandshakeListAndCall(t *testing.T) {
	fake := newFakeTransport()
	fake.handlers["initialize"] = func(json.RawMessage) (any, error) {
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]any{"name": "demo", "version": "1.2.3"},
		}, nil
	}
	page := 0
	fake.handlers["tools/list"] = func(params json.RawMessage) (any, error) {
		page++
		if page == 1 {
			if strings.Contains(string(params), "cursor") {
				t.Fatalf("first page should not send a cursor: %s", params)
			}
			return map[string]any{
				"tools": []map[string]any{{
					"name":        "search",
					"description": "Search the docs",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
				}},
				"nextCursor": "page-2",
			}, nil
		}
		if !strings.Contains(string(params), "page-2") {
			t.Fatalf("second page should send the cursor: %s", params)
		}
		return map[string]any{"tools": []map[string]any{{"name": "ping"}}}, nil
	}
	fake.handlers["tools/call"] = func(params json.RawMessage) (any, error) {
		var p callToolParams
		_ = json.Unmarshal(params, &p)
		switch p.Name {
		case "search":
			return map[string]any{"content": []map[string]any{
				{"type": "text", "text": "found 2 docs"},
				{"type": "image", "data": "aGk=", "mimeType": "image/png"},
				{"type": "resource", "resource": map[string]any{"uri": "file:///a.txt", "text": "hello"}},
			}}, nil
		case "ping":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "bad"}}, "isError": true}, nil
		}
		return nil, fmt.Errorf("unknown tool")
	}

	client, err := NewClient(context.Background(), ServerConfig{Name: "demo-server"}, fake)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if name, version := client.ServerInfo(); name != "demo" || version != "1.2.3" {
		t.Errorf("server info = %q %q", name, version)
	}
	if got := strings.Join(fake.calls[:2], ","); got != "initialize,notifications/initialized" {
		t.Errorf("handshake calls = %s", got)
	}

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "search" || tools[1].Name != "ping" {
		t.Fatalf("tools = %+v", tools)
	}
	if page != 2 {
		t.Errorf("tools/list pages = %d, want 2", page)
	}

	wrapped := wrapTools(client, tools)
	if len(wrapped) != 2 {
		t.Fatalf("wrapped tools = %d", len(wrapped))
	}
	search := wrapped[0]
	if search.Name() != "mcp__demo_server__search" {
		t.Errorf("tool name = %q, want mcp__demo_server__search", search.Name())
	}
	if !strings.Contains(search.Description(), `MCP server "demo-server", tool "search"`) {
		t.Errorf("description = %q", search.Description())
	}
	if !json.Valid(search.Schema()) || !strings.Contains(string(search.Schema()), `"q"`) {
		t.Errorf("schema not passed through: %s", search.Schema())
	}
	if search.ExecutionMode() != agentcore.ToolExecutionParallel {
		t.Errorf("execution mode = %v", search.ExecutionMode())
	}

	res, err := search.Execute(context.Background(), "call-1", json.RawMessage(`{"q":"go"}`), nil)
	if err != nil {
		t.Fatalf("Execute(search): %v", err)
	}
	var kinds []string
	for _, c := range res.Content {
		switch c.(type) {
		case agentcore.TextContent:
			kinds = append(kinds, "text")
		case agentcore.ImageContent:
			kinds = append(kinds, "image")
		default:
			kinds = append(kinds, "other")
		}
	}
	if strings.Join(kinds, ",") != "text,image,text" {
		t.Errorf("content kinds = %v, want text,image,text (resource rendered as text)", kinds)
	}
	text := agentcore.ContentToText(res.Content)
	if !strings.Contains(text, "found 2 docs") || !strings.Contains(text, "file:///a.txt") {
		t.Errorf("content text = %q", text)
	}

	// A tool-level failure is surfaced as a tool error carrying the message.
	if _, err := wrapped[1].Execute(context.Background(), "call-2", json.RawMessage(`{}`), nil); err == nil {
		t.Error("isError result should become a tool error")
	} else if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error = %v, want the server's message", err)
	}
}

// TestServerConfigValidation verifies the definition checks: a name is required and
// exactly one transport must be configured.
func TestServerConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
	}{
		{"no name", ServerConfig{Command: "x"}},
		{"neither transport", ServerConfig{Name: "a"}},
		{"both transports", ServerConfig{Name: "a", Command: "x", URL: "http://h"}},
	}
	for _, tc := range cases {
		if err := tc.cfg.validate(); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
	ok := []ServerConfig{
		{Name: "stdio", Command: "npx", Args: []string{"-y", "server"}},
		{Name: "http", URL: "http://127.0.0.1:9/mcp"},
	}
	for _, cfg := range ok {
		if err := cfg.validate(); err != nil {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
}

// TestManagerSkipsBrokenServers verifies a failing server is logged and skipped
// while the healthy ones still contribute tools, and that Close releases them.
func TestManagerSkipsBrokenServers(t *testing.T) {
	original := transportFactory
	t.Cleanup(func() { transportFactory = original })

	made := map[string]*fakeTransport{}
	transportFactory = func(cfg ServerConfig, _ io.Writer) (Transport, error) {
		if cfg.Name == "broken" {
			return nil, fmt.Errorf("cannot start")
		}
		fake := newFakeTransport()
		fake.handlers["initialize"] = func(json.RawMessage) (any, error) {
			return map[string]any{"protocolVersion": ProtocolVersion}, nil
		}
		fake.handlers["tools/list"] = func(json.RawMessage) (any, error) {
			return map[string]any{"tools": []map[string]any{{"name": "run"}}}, nil
		}
		made[cfg.Name] = fake
		return fake, nil
	}

	var log strings.Builder
	m := Connect(context.Background(), []ServerConfig{
		{Name: "good", Command: "x"},
		{Name: "broken", Command: "x"},
		{Name: "good", Command: "x"}, // duplicate
		{Name: "off", Command: "x", Disabled: true},
	}, &log)
	tools := m.Tools()
	if len(tools) != 1 || tools[0].Name() != "mcp__good__run" {
		t.Fatalf("tools = %v, want only mcp__good__run", toolNames(tools))
	}
	if got := m.Servers(); len(got) != 1 || got[0] != "good" {
		t.Errorf("servers = %v", got)
	}
	for _, want := range []string{`mcp server "broken" skipped: cannot start`, `mcp server "good" skipped: duplicate name`} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log.String())
		}
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !made["good"].closed {
		t.Error("Close did not close the connected server")
	}
	if len(m.Tools()) != 0 {
		t.Error("tools should be released after Close")
	}
}

// TestHTTPTransportJSON verifies the Streamable HTTP transport against a real
// server that replies with plain JSON and assigns a session id.
func TestHTTPTransportJSON(t *testing.T) {
	var sessions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "text/event-stream") {
			t.Errorf("Accept = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		sessions = append(sessions, r.Header.Get("Mcp-Session-Id"))
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"serverInfo":{"name":"http-demo"}}}`, req.ID, ProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`, req.ID)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"pong"}]}}`, req.ID)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient(context.Background(), ServerConfig{Name: "httpd", URL: srv.URL}, mustHTTPTransport(t, srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	if name, _ := client.ServerInfo(); name != "http-demo" {
		t.Errorf("server info = %q", name)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	res, err := client.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := agentcore.ContentToText(pluckContent(res.Content)); !strings.Contains(got, "pong") {
		t.Errorf("call result = %q", got)
	}
	// The session id from initialize must be echoed afterwards.
	if len(sessions) < 4 || sessions[0] != "" || sessions[1] != "sess-1" {
		t.Errorf("session header sequence = %v, want unset then sess-1", sessions)
	}
}

// TestHTTPTransportSSE verifies a server that answers with an event stream,
// including an unrelated reply that must be skipped.
func TestHTTPTransportSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q}}`, req.ID, ProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			// A notification, then a stale reply, then the real one.
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"stale\"}]}}\n\n", req.ID+100)
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"streamed\"}]}}\n\n", req.ID)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient(context.Background(), ServerConfig{Name: "sse", URL: srv.URL}, mustHTTPTransport(t, srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	res, err := client.CallTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := agentcore.ContentToText(pluckContent(res.Content)); !strings.Contains(got, "streamed") || strings.Contains(got, "stale") {
		t.Errorf("call result = %q, want the matching streamed reply", got)
	}
}

// TestHTTPTransportErrorReply verifies a JSON-RPC error object becomes a Go error.
func TestHTTPTransportErrorReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q}}`, req.ID, ProtocolVersion)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"no such tool"}}`, req.ID)
	}))
	defer srv.Close()

	client, err := NewClient(context.Background(), ServerConfig{Name: "err", URL: srv.URL}, mustHTTPTransport(t, srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	if _, err := client.CallTool(context.Background(), "missing", nil); err == nil {
		t.Fatal("expected an error for a JSON-RPC error reply")
	} else if !strings.Contains(err.Error(), "no such tool") {
		t.Errorf("error = %v", err)
	}
}

// TestToolNameSanitize verifies MCP tool naming: lowercase identifiers with the
// server/tool separator.
func TestToolNameSanitize(t *testing.T) {
	cases := map[string]string{
		"Demo-Server":  "demo_server",
		"with space":   "with_space",
		"??":           "unnamed",
		"a.b.c":        "a_b_c",
		"already_fine": "already_fine",
		"UPPER":        "upper",
	}
	for in, want := range cases {
		if got := sanitizeToolName(in); got != want {
			t.Errorf("sanitizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ToolName("GitHub", "create/issue"); got != "mcp__github__create_issue" {
		t.Errorf("ToolName = %q", got)
	}
}

// mustHTTPTransport builds the HTTP transport for a test server.
func mustHTTPTransport(t *testing.T, url string) Transport {
	t.Helper()
	tr, err := newHTTPTransport(ServerConfig{Name: "test", URL: url})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	return tr
}

// pluckContent converts MCP content blocks to pigo blocks for assertions.
func pluckContent(blocks []Content) agentcore.ContentList {
	out, _ := mcpContent(blocks)
	return out
}

// toolNames maps agent tools to names for assertions.
func toolNames(tools []agentcore.AgentTool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name()
	}
	return out
}
