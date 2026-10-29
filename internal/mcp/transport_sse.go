// This file implements the legacy HTTP+SSE MCP transport (protocol revision
// 2024-03-26): the client opens one long-lived GET stream, the server announces the
// POST endpoint with an `endpoint` event, requests are POSTed there, and both
// replies and server-initiated messages arrive on the GET stream.
//
// It is selected with `transport = "sse"` (or `mcp add ... --transport sse`). The
// Streamable HTTP transport (`transport = "http"` or the default for a url) is
// preferred when the server supports it.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/smallnest/pigo/internal/jsonrpc"
)

// sseTransport is the legacy HTTP+SSE MCP transport.
type sseTransport struct {
	streamURL string
	headers   map[string]string
	client    *http.Client
	nextID    atomic.Int64
	logw      io.Writer
	oauth     *oauthProvider

	body   io.ReadCloser
	cancel context.CancelFunc // cancels the event-stream request context

	mu       sync.Mutex
	endpoint string
	ready    chan struct{} // closed once the endpoint event arrives
	pending  map[string]chan sseReply
	handler  transportHandler
	closed   bool
	err      error
}

// sseMessage is one decoded JSON-RPC line on the event stream (a reply, a server
// request, or a notification).
type sseMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *sseError       `json:"error,omitempty"`
}

// sseError is the JSON-RPC error object on the wire.
type sseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// sseReply carries one reply (or transport error) to a waiting call.
type sseReply struct {
	result json.RawMessage
	err    error
}

// newSSETransport opens the event stream and waits until the server announces its
// message endpoint (or ctx expires).
func newSSETransport(ctx context.Context, cfg ServerConfig, logw io.Writer) (Transport, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	t := &sseTransport{
		streamURL: u.String(),
		headers:   cfg.Headers,
		client:    &http.Client{},
		logw:      logw,
		pending:   make(map[string]chan sseReply),
		ready:     make(chan struct{}),
	}
	if cfg.OAuth {
		t.oauth = newOAuthProvider(cfg, logw)
	}

	// The event stream must outlive the setup call, so it gets its own cancellable
	// context; ctx only bounds waiting for the endpoint announcement below.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	t.cancel = streamCancel
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, t.streamURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	t.applyHeaders(ctx, req)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open event stream: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized && t.oauth != nil {
		resp.Body.Close()
		if err := t.oauth.authorize(ctx); err != nil {
			return nil, err
		}
		req, err = http.NewRequestWithContext(streamCtx, http.MethodGet, t.streamURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")
		t.applyHeaders(ctx, req)
		if resp, err = t.client.Do(req); err != nil {
			return nil, fmt.Errorf("open event stream: %w", err)
		}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("event stream: %s", resp.Status)
	}
	t.body = resp.Body
	go t.readLoop(resp.Body)

	// Wait for the endpoint announcement.
	select {
	case <-t.ready:
	case <-ctx.Done():
		t.Close()
		return nil, fmt.Errorf("waiting for the message endpoint: %w", ctx.Err())
	}
	t.mu.Lock()
	err = t.err
	t.mu.Unlock()
	if err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// SetHandler installs the server-message handler.
func (t *sseTransport) SetHandler(h transportHandler) {
	t.mu.Lock()
	t.handler = h
	t.mu.Unlock()
}

// applyHeaders adds configured headers plus an OAuth token when one is available.
func (t *sseTransport) applyHeaders(ctx context.Context, req *http.Request) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.oauth != nil {
		if token, err := t.oauth.Authorization(ctx); err == nil && token != "" {
			req.Header.Set("Authorization", token)
		}
	}
}

// readLoop parses the event stream until it closes or the transport is closed.
func (t *sseTransport) readLoop(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	event := ""
	var data strings.Builder
	dispatch := func() {
		if data.Len() == 0 {
			event = ""
			return
		}
		payload := data.String()
		data.Reset()
		kind := event
		event = ""
		switch kind {
		case "endpoint":
			t.setEndpoint(payload)
		default:
			t.handleMessage(payload)
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data:"))
		}
	}
	dispatch()
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	t.failAll(fmt.Errorf("event stream closed: %w", err))
}

// setEndpoint resolves and records the announced message endpoint.
func (t *sseTransport) setEndpoint(raw string) {
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		t.failAll(fmt.Errorf("invalid endpoint %q: %w", raw, err))
		return
	}
	base, err := url.Parse(t.streamURL)
	if err != nil {
		t.failAll(err)
		return
	}
	t.mu.Lock()
	t.endpoint = base.ResolveReference(ref).String()
	if !t.endpointSeen() {
		close(t.ready)
	}
	t.mu.Unlock()
}

// endpointSeen reports whether the endpoint was already announced (callers hold mu).
func (t *sseTransport) endpointSeen() bool {
	select {
	case <-t.ready:
		return true
	default:
		return false
	}
}

// handleMessage dispatches one JSON-RPC message arriving on the stream.
func (t *sseTransport) handleMessage(payload string) {
	var in sseMessage
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		return
	}
	switch {
	case in.Method != "" && len(in.ID) == 0:
		t.mu.Lock()
		h := t.handler
		t.mu.Unlock()
		if h != nil {
			go h.HandleNotification(in.Method, in.Params)
		}
	case in.Method != "":
		t.mu.Lock()
		h := t.handler
		t.mu.Unlock()
		if h == nil {
			return
		}
		go func() {
			result, err := h.HandleRequest(context.Background(), in.Method, in.Params)
			resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(in.ID)}
			if err != nil {
				var rpcErr *jsonrpc.Error
				if errors.As(err, &rpcErr) {
					resp["error"] = rpcErr
				} else {
					resp["error"] = &sseError{Code: -32603, Message: err.Error()}
				}
			} else {
				resp["result"] = result
			}
			_ = t.postJSON(context.Background(), resp, false)
		}()
	default:
		t.deliver(responseKey(in.ID), in.Result, in.Error)
	}
}

// responseKey renders a reply id the way pending calls key it.
func responseKey(id json.RawMessage) string {
	return strings.TrimSpace(string(id))
}

// deliver hands a reply to its waiter.
func (t *sseTransport) deliver(id string, result json.RawMessage, rpcErr *sseError) {
	t.mu.Lock()
	ch, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	if rpcErr != nil {
		ch <- sseReply{err: fmt.Errorf("mcp error %d: %s", rpcErr.Code, rpcErr.Message)}
		return
	}
	ch <- sseReply{result: result}
}

// failAll fails every pending call (stream closed).
func (t *sseTransport) failAll(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.err = err
	pending := t.pending
	t.pending = make(map[string]chan sseReply)
	t.mu.Unlock()
	for _, ch := range pending {
		ch <- sseReply{err: err}
	}
}

// Call posts a request and waits for its reply on the event stream.
func (t *sseTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	ch := make(chan sseReply, 1)
	key := fmt.Sprintf("%d", id)
	t.mu.Lock()
	if t.closed {
		err := t.err
		t.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("transport closed")
		}
		return nil, err
	}
	t.pending[key] = ch
	t.mu.Unlock()

	if err := t.postJSON(ctx, payload, true); err != nil {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Notify posts a notification.
func (t *sseTransport) Notify(method string, params any) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		payload["params"] = params
	}
	return t.postJSON(context.Background(), payload, false)
}

// postJSON POSTs one message to the announced endpoint. `expectReply` retries once
// through OAuth when the endpoint answers 401.
func (t *sseTransport) postJSON(ctx context.Context, payload any, expectReply bool) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	t.mu.Lock()
	endpoint := t.endpoint
	t.mu.Unlock()
	if endpoint == "" {
		return fmt.Errorf("message endpoint not announced yet")
	}
	post := func() (*http.Response, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		t.applyHeaders(ctx, req)
		return t.client.Do(req)
	}
	resp, err := post()
	if err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized && t.oauth != nil {
		resp.Body.Close()
		if err := t.oauth.authorize(ctx); err != nil {
			return err
		}
		if resp, err = post(); err != nil {
			return fmt.Errorf("post message: %w", err)
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("post message: %s", resp.Status)
	}
	return nil
}

// Close terminates the event stream.
func (t *sseTransport) Close() error {
	if t.cancel != nil {
		t.cancel()
	}
	t.failAll(fmt.Errorf("transport closed"))
	if t.body != nil {
		return t.body.Close()
	}
	return nil
}
