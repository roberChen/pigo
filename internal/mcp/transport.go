// This file implements the two MCP transports: stdio (a server process speaking
// line-delimited JSON-RPC on its stdin/stdout, via internal/jsonrpc) and Streamable
// HTTP (JSON-RPC POSTed to one endpoint, with a JSON or an SSE response, and an
// Mcp-Session-Id header echoed back once the server assigns one).
//
// The legacy HTTP+SSE transport (2024-03-26: GET /sse, then POST to an advertised
// endpoint) is not supported; a server that only speaks it fails to initialize and
// is reported, like any other unreachable server.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smallnest/pigo/internal/jsonrpc"
)

// Transport is the wire half of an MCP connection: one request/reply channel plus
// notifications.
type Transport interface {
	// Call sends a request and returns its result payload.
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Notify sends a notification (no reply expected).
	Notify(method string, params any) error
	// Close releases the transport.
	Close() error
}

// transportHandler receives the messages a server sends on its own initiative
// (requests and notifications).
type transportHandler interface {
	HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error)
	HandleNotification(method string, params json.RawMessage)
}

// handlerSetter is implemented by transports that can route server->client
// messages to a handler.
type handlerSetter interface {
	SetHandler(handler transportHandler)
}

// defaultTransport picks the transport from the server definition.
func defaultTransport(cfg ServerConfig, logw io.Writer) (Transport, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case "", "http":
		if strings.TrimSpace(cfg.URL) != "" {
			return newHTTPTransport(cfg)
		}
		return newStdioTransport(cfg, logw)
	case "stdio":
		return newStdioTransport(cfg, logw)
	case "sse":
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("the sse transport requires a url")
		}
		// Bound waiting for the server to announce its message endpoint.
		setupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return newSSETransport(setupCtx, cfg, logw)
	default:
		return nil, fmt.Errorf("unknown transport %q (want http, sse or stdio)", cfg.Transport)
	}
}

// stdioTransport speaks MCP over a subprocess's stdio using internal/jsonrpc, which
// already handles id correlation, concurrent calls and graceful shutdown.
type stdioTransport struct {
	client  *jsonrpc.Client
	mu      sync.Mutex
	handler transportHandler
}

// SetHandler installs the server-message handler (called by the MCP client).
func (t *stdioTransport) SetHandler(h transportHandler) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

// currentHandler returns the installed handler (nil before the client attaches).
func (t *stdioTransport) currentHandler() transportHandler {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.handler
}

// newStdioTransport launches the server process. The child inherits the parent
// environment with the definition's env entries layered on top, so PATH and proxy
// settings still work.
func newStdioTransport(cfg ServerConfig, logw io.Writer) (Transport, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("stdio server requires a command")
	}
	t := &stdioTransport{}
	client, err := jsonrpc.NewClient(jsonrpc.Config{
		Command: cfg.Command,
		Args:    cfg.Args,
		Env:     mergeEnv(os.Environ(), cfg.Env),
		Stderr:  logw,
		// The handler is looked up per message so it can be installed after the
		// transport exists (the MCP client is built from it).
		OnRequest: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
			if h := t.currentHandler(); h != nil {
				return h.HandleRequest(ctx, method, params)
			}
			return nil, &jsonrpc.Error{Code: -32601, Message: "method not found: " + method}
		},
		OnNotification: func(method string, params json.RawMessage) {
			if h := t.currentHandler(); h != nil {
				h.HandleNotification(method, params)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	t.client = client
	return t, nil
}

// Call delegates to the JSON-RPC client.
func (t *stdioTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return t.client.Call(ctx, method, params)
}

// Notify delegates to the JSON-RPC client.
func (t *stdioTransport) Notify(method string, params any) error {
	return t.client.Notify(method, params)
}

// Close terminates the child process.
func (t *stdioTransport) Close() error { return t.client.Close() }

// mergeEnv layers add onto base (os/exec form), replacing keys that already exist.
// Entries are sorted so the child environment is deterministic.
func mergeEnv(base []string, add map[string]string) []string {
	if len(add) == 0 {
		return base
	}
	index := make(map[string]int, len(base))
	out := make([]string, len(base))
	copy(out, base)
	for i, entry := range out {
		if name, _, ok := strings.Cut(entry, "="); ok {
			index[name] = i
		}
	}
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry := k + "=" + add[k]
		if i, ok := index[k]; ok {
			out[i] = entry
			continue
		}
		out = append(out, entry)
	}
	return out
}

// httpTransport speaks Streamable HTTP: JSON-RPC requests are POSTed to one
// endpoint and the reply arrives as either a JSON body or an SSE stream. The
// session id a server returns on initialize is echoed on later requests.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	nextID  atomic.Int64
	oauth   *oauthProvider

	mu        sync.Mutex
	sessionID string
}

// newHTTPTransport builds the HTTP transport. No request is made until initialize.
func newHTTPTransport(cfg ServerConfig) (Transport, error) {
	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		return nil, fmt.Errorf("http server requires a url")
	}
	t := &httpTransport{
		url:     url,
		headers: cfg.Headers,
		client:  &http.Client{},
	}
	if cfg.OAuth {
		t.oauth = newOAuthProvider(cfg, nil)
	}
	return t, nil
}

// Call posts one request and decodes its response.
func (t *httpTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp http: marshal request: %w", err)
	}
	resp, err := t.post(ctx, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && t.oauth != nil {
		// The token was missing or rejected: run (or re-run) the authorization flow
		// and retry once.
		resp.Body.Close()
		t.oauth.Invalidate()
		if err := t.oauth.authorize(ctx); err != nil {
			return nil, err
		}
		if resp, err = t.post(ctx, body); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mcp http %s: %s: %s", method, resp.Status, strings.TrimSpace(string(msg)))
	}
	t.captureSession(resp)

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		return readSSEResponse(resp.Body, id)
	}
	return decodeJSONRPCResponse(resp.Body, id)
}

// Notify posts a notification and drains the reply (servers answer 202).
func (t *httpTransport) Notify(method string, params any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("mcp http: marshal notification: %w", err)
	}
	resp, err := t.post(context.Background(), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	t.captureSession(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mcp http %s: %s", method, resp.Status)
	}
	return nil
}

// Close ends the session (best effort) and releases idle connections.
func (t *httpTransport) Close() error {
	t.mu.Lock()
	session := t.sessionID
	t.mu.Unlock()
	if session != "" {
		req, err := http.NewRequest(http.MethodDelete, t.url, nil)
		if err == nil {
			t.applyHeaders(req)
			if resp, err := t.client.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
				resp.Body.Close()
			}
		}
	}
	t.client.CloseIdleConnections()
	return nil
}

// post sends body with the JSON-RPC content negotiation Streamable HTTP requires.
func (t *httpTransport) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp http: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.applyHeaders(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp http: %w", err)
	}
	return resp, nil
}

// applyHeaders sets the configured headers plus any known session id.
func (t *httpTransport) applyHeaders(req *http.Request) {
	t.mu.Lock()
	session := t.sessionID
	t.mu.Unlock()
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if t.oauth != nil {
		if token, err := t.oauth.Authorization(req.Context()); err == nil && token != "" {
			req.Header.Set("Authorization", token)
		}
	}
}

// captureSession records a server-assigned session id for later requests.
func (t *httpTransport) captureSession(resp *http.Response) {
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
}

// jsonrpcReply is the envelope shared by JSON and SSE replies.
type jsonrpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// decodeJSONRPCResponse decodes a single JSON body reply.
func decodeJSONRPCResponse(r io.Reader, id int64) (json.RawMessage, error) {
	var reply jsonrpcReply
	if err := json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("mcp http: decode response: %w", err)
	}
	return replyResult(reply, id)
}

// readSSEResponse reads SSE events until the response with the matching id arrives,
// answering server pings along the way.
func readSSEResponse(r io.Reader, id int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() == 0 {
				continue
			}
			raw := data.String()
			data.Reset()
			var reply jsonrpcReply
			if err := json.Unmarshal([]byte(raw), &reply); err != nil {
				continue
			}
			if reply.Method != "" && reply.ID == nil {
				continue // server notification inside the stream
			}
			result, err := replyResult(reply, id)
			if err == nil {
				return result, nil
			}
			if strings.Contains(err.Error(), "different id") {
				continue // a reply to an earlier request; keep reading
			}
			return nil, err
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		default:
			// event:, id:, retry: and comments are irrelevant here.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp http: read event stream: %w", err)
	}
	return nil, fmt.Errorf("mcp http: event stream ended before the response arrived")
}

// replyResult unwraps a JSON-RPC reply, checking the id and surfacing errors.
func replyResult(reply jsonrpcReply, id int64) (json.RawMessage, error) {
	if reply.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	if len(reply.ID) > 0 {
		var got int64
		if err := json.Unmarshal(reply.ID, &got); err == nil && got != id {
			return nil, fmt.Errorf("mcp http: reply has a different id (%d != %d)", got, id)
		}
	}
	if len(reply.Result) == 0 {
		return nil, fmt.Errorf("mcp http: reply has no result")
	}
	return reply.Result, nil
}
