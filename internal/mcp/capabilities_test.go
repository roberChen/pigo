package mcp

// Tests for the capabilities beyond tools (resources and prompts), server-initiated
// messages, live tool refresh, the legacy SSE transport and the OAuth flow.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
)

// capabilityTransport is a fake MCP server advertising every capability.
func capabilityTransport() *fakeTransport {
	f := newFakeTransport()
	f.handlers["initialize"] = func(json.RawMessage) (any, error) {
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]any{"name": "full"},
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": true},
				"resources": map[string]any{"listChanged": true},
				"prompts":   map[string]any{"listChanged": true},
			},
		}, nil
	}
	f.handlers["tools/list"] = func(json.RawMessage) (any, error) {
		return map[string]any{"tools": []map[string]any{{"name": "search"}}}, nil
	}
	f.handlers["resources/list"] = func(json.RawMessage) (any, error) {
		return map[string]any{"resources": []map[string]any{
			{"uri": "file:///a.txt", "name": "a.txt", "mimeType": "text/plain"},
		}}, nil
	}
	f.handlers["resources/read"] = func(params json.RawMessage) (any, error) {
		if !strings.Contains(string(params), "file:///a.txt") {
			return nil, fmt.Errorf("unexpected uri: %s", params)
		}
		return map[string]any{"contents": []map[string]any{
			{"uri": "file:///a.txt", "mimeType": "text/plain", "text": "hello resource"},
		}}, nil
	}
	f.handlers["prompts/list"] = func(json.RawMessage) (any, error) {
		return map[string]any{"prompts": []map[string]any{{
			"name":        "review",
			"description": "Review a change",
			"arguments":   []map[string]any{{"name": "path", "required": true}},
		}}}, nil
	}
	f.handlers["prompts/get"] = func(params json.RawMessage) (any, error) {
		if !strings.Contains(string(params), "main.go") {
			return nil, fmt.Errorf("argument not forwarded: %s", params)
		}
		return map[string]any{
			"description": "Review prompt",
			"messages": []map[string]any{
				{"role": "user", "content": map[string]any{"type": "text", "text": "review main.go"}},
			},
		}, nil
	}
	return f
}

// TestResourcesAndPromptsExposed verifies a server advertising resources and
// prompts contributes the list/read and list/get adapters, and that they map onto
// pigo results.
func TestResourcesAndPromptsExposed(t *testing.T) {
	original := transportFactory
	t.Cleanup(func() { transportFactory = original })
	transportFactory = func(ServerConfig, io.Writer) (Transport, error) { return capabilityTransport(), nil }

	m := Connect(context.Background(), []ServerConfig{{Name: "full", Command: "x"}}, nil)
	defer m.Close()

	tools := map[string]agentcore.AgentTool{}
	for _, tool := range m.Tools() {
		tools[tool.Name()] = tool
	}
	for _, want := range []string{
		"mcp__full__search",
		"mcp__full__list_resources",
		"mcp__full__read_resource",
		"mcp__full__list_prompts",
		"mcp__full__get_prompt",
	} {
		if _, ok := tools[want]; !ok {
			t.Fatalf("tool %q missing; got %v", want, toolNames(m.Tools()))
		}
	}

	res, err := tools["mcp__full__list_resources"].Execute(context.Background(), "c", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("list_resources: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "file:///a.txt") || !strings.Contains(got, "text/plain") {
		t.Errorf("list_resources = %q", got)
	}

	res, err = tools["mcp__full__read_resource"].Execute(context.Background(), "c", json.RawMessage(`{"uri":"file:///a.txt"}`), nil)
	if err != nil {
		t.Fatalf("read_resource: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); got != "hello resource" {
		t.Errorf("read_resource = %q", got)
	}
	if _, err := tools["mcp__full__read_resource"].Execute(context.Background(), "c", json.RawMessage(`{}`), nil); err == nil {
		t.Error("read_resource without a uri should error")
	}

	res, err = tools["mcp__full__list_prompts"].Execute(context.Background(), "c", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("list_prompts: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "review") || !strings.Contains(got, "path!") {
		t.Errorf("list_prompts = %q", got)
	}

	res, err = tools["mcp__full__get_prompt"].Execute(context.Background(), "c", json.RawMessage(`{"name":"review","arguments":{"path":"main.go"}}`), nil)
	if err != nil {
		t.Fatalf("get_prompt: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "review main.go") || !strings.Contains(got, "Review prompt") {
		t.Errorf("get_prompt = %q", got)
	}
}

// TestToolsOnlyServerHasNoResourceTools verifies capabilities gate the extra tools.
func TestToolsOnlyServerHasNoResourceTools(t *testing.T) {
	original := transportFactory
	t.Cleanup(func() { transportFactory = original })
	fake := newFakeTransport()
	fake.handlers["initialize"] = func(json.RawMessage) (any, error) {
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}, nil
	}
	fake.handlers["tools/list"] = func(json.RawMessage) (any, error) {
		return map[string]any{"tools": []map[string]any{{"name": "run"}}}, nil
	}
	transportFactory = func(ServerConfig, io.Writer) (Transport, error) { return fake, nil }

	m := Connect(context.Background(), []ServerConfig{{Name: "tiny", Command: "x"}}, nil)
	defer m.Close()
	if got := toolNames(m.Tools()); len(got) != 1 || got[0] != "mcp__tiny__run" {
		t.Fatalf("tools = %v, want only the server tool", got)
	}
}

// TestServerRequestsAnswered verifies the client answers lifecycle requests and
// declines sampling.
func TestServerRequestsAnswered(t *testing.T) {
	client, err := NewClient(context.Background(), ServerConfig{Name: "s"}, func() *fakeTransport {
		f := newFakeTransport()
		f.handlers["initialize"] = func(json.RawMessage) (any, error) {
			return map[string]any{"protocolVersion": ProtocolVersion}, nil
		}
		return f
	}())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if res, err := client.HandleRequest(context.Background(), "ping", nil); err != nil || res == nil {
		t.Errorf("ping = %v, %v", res, err)
	}
	res, err := client.HandleRequest(context.Background(), "roots/list", nil)
	if err != nil {
		t.Fatalf("roots/list: %v", err)
	}
	roots, _ := res.(map[string]any)["roots"].([]map[string]any)
	if len(roots) != 1 || !strings.HasPrefix(roots[0]["uri"].(string), "file://") {
		t.Errorf("roots = %v", res)
	}
	if _, err := client.HandleRequest(context.Background(), "sampling/createMessage", nil); err == nil {
		t.Error("sampling should be declined")
	}
	if _, err := client.HandleRequest(context.Background(), "unknown/method", nil); err == nil {
		t.Error("unknown methods should error")
	}
}

// TestListChangedRefresh verifies a tools/list_changed notification makes the
// manager re-list, and that new tools are registered for execution.
func TestListChangedRefresh(t *testing.T) {
	original := transportFactory
	t.Cleanup(func() { transportFactory = original })

	var mu sync.Mutex
	page := 1
	fake := newFakeTransport()
	fake.handlers["initialize"] = func(json.RawMessage) (any, error) {
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		}, nil
	}
	fake.handlers["tools/list"] = func(json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		tools := []map[string]any{{"name": "first"}}
		if page > 1 {
			tools = append(tools, map[string]any{"name": "second"})
		}
		return map[string]any{"tools": tools}, nil
	}
	transportFactory = func(ServerConfig, io.Writer) (Transport, error) { return fake, nil }

	reg := agenttool.NewToolRegistry()
	m := Connect(context.Background(), []ServerConfig{{Name: "dyn", Command: "x"}}, nil)
	defer m.Close()
	m.BindRegistry(reg)
	if got := toolNames(m.Tools()); len(got) != 1 {
		t.Fatalf("tools = %v, want first only", got)
	}

	// Nothing changed: Refresh is a no-op.
	if m.Refresh(context.Background()) {
		t.Fatal("Refresh without a notification should report no change")
	}

	mu.Lock()
	page = 2
	mu.Unlock()
	client := m.clients[0]
	client.HandleNotification("notifications/tools/list_changed", nil)
	if !m.Refresh(context.Background()) {
		t.Fatal("Refresh should report the changed tool set")
	}
	if got := toolNames(m.Tools()); len(got) != 2 {
		t.Fatalf("tools after refresh = %v, want both", got)
	}
	if _, ok := reg.Get("mcp__dyn__second"); !ok {
		t.Error("the refreshed tool was not registered for execution")
	}
}

// TestSSETransport verifies the legacy HTTP+SSE transport end to end: the endpoint
// event, POSTed requests, and replies arriving on the event stream.
func TestSSETransport(t *testing.T) {
	var mu sync.Mutex
	pendingReplies := make(chan string, 8)
	var postEndpoint string

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", srv.URL)
			if flusher != nil {
				flusher.Flush()
			}
			for {
				select {
				case reply := <-pendingReplies:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", reply)
					if flusher != nil {
						flusher.Flush()
					}
				case <-r.Context().Done():
					return
				}
			}
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(body, &req)
			postEndpoint = r.URL.Path
			w.WriteHeader(http.StatusAccepted)
			if req.ID == 0 {
				return // notification
			}
			switch req.Method {
			case "initialize":
				pendingReplies <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"capabilities":{"tools":{}}}}`, req.ID, ProtocolVersion)
			case "tools/list":
				pendingReplies <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"sse_tool"}]}}`, req.ID)
			case "tools/call":
				pendingReplies <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"from sse"}]}}`, req.ID)
			}
		}
	}))
	defer srv.Close()

	tr, err := newSSETransport(context.Background(), ServerConfig{Name: "legacy", URL: srv.URL, Transport: "sse"}, nil)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	client, err := NewClient(context.Background(), ServerConfig{Name: "legacy", URL: srv.URL, Transport: "sse"}, tr)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "sse_tool" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	res, err := client.CallTool(context.Background(), "sse_tool", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := agentcore.ContentToText(pluckContent(res.Content)); !strings.Contains(got, "from sse") {
		t.Errorf("call result = %q", got)
	}
	mu.Lock()
	endpoint := postEndpoint
	mu.Unlock()
	if endpoint != "/message" {
		t.Errorf("requests posted to %q, want /message", endpoint)
	}
}

// TestOAuthFlow verifies the authorization flow against a fake authorization
// server: discovery, dynamic registration, PKCE redirect, token exchange, the
// authenticated retry, and token persistence for the next launch.
func TestOAuthFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)

	var srv *httptest.Server
	mux := http.NewServeMux()
	// Resource document + authorization server metadata, all on one host.
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"resource":              srv.URL,
			"authorization_servers": []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"registration_endpoint":  srv.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"client_id": "client-1"})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
			http.Error(w, "missing pkce", http.StatusBadRequest)
			return
		}
		target := q.Get("redirect_uri") + "?code=code-1&state=" + q.Get("state")
		http.Redirect(w, r, target, http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "code-1" || r.Form.Get("code_verifier") == "" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	})
	// The MCP endpoint requires the granted token.
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"capabilities":{"tools":{}}}}`, req.ID, ProtocolVersion)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"secure"}]}}`, req.ID)
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	// Follow the authorization URL in-process instead of opening a browser.
	originalBrowser := openBrowserFunc
	t.Cleanup(func() { openBrowserFunc = originalBrowser })
	openBrowserFunc = func(u string) error {
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	tr, err := newHTTPTransport(ServerConfig{Name: "secure", URL: srv.URL + "/mcp", OAuth: true})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	client, err := NewClient(context.Background(), ServerConfig{Name: "secure", URL: srv.URL + "/mcp", OAuth: true}, tr)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "secure" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}

	// The token was cached for the next launch.
	data, err := os.ReadFile(filepath.Join(home, "mcp-oauth", "secure.json"))
	if err != nil {
		t.Fatalf("token not persisted: %v", err)
	}
	if !strings.Contains(string(data), "refresh-1") || !strings.Contains(string(data), "access-1") {
		t.Errorf("token record = %s", data)
	}

	// A second provider loads the cached token and needs no new authorization.
	browserCalls := 0
	openBrowserFunc = func(string) error { browserCalls++; return nil }
	tr2, err := newHTTPTransport(ServerConfig{Name: "secure", URL: srv.URL + "/mcp", OAuth: true})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	client2, err := NewClient(context.Background(), ServerConfig{Name: "secure", URL: srv.URL + "/mcp", OAuth: true}, tr2)
	if err != nil {
		t.Fatalf("NewClient (cached): %v", err)
	}
	defer client2.Close()
	if browserCalls != 0 {
		t.Errorf("a cached token should not prompt again (browser opened %d times)", browserCalls)
	}
}
