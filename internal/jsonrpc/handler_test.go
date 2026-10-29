package jsonrpc

// Tests for server->client message handling: a client answers a peer request with
// a result or a JSON-RPC error, and an unhandled method is answered with
// "method not found" so the peer never waits forever.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
)

// readMessage reads one JSON line written by the client.
func readMessage(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		t.Fatalf("no response written: %v", sc.Err())
	}
	var msg map[string]any
	if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return msg
}

// newHandlerTestClient builds a client whose stdin is a pipe, for exercising answer.
func newHandlerTestClient(onRequest func(context.Context, string, json.RawMessage) (any, error)) (*Client, *io.PipeReader, *io.PipeWriter) {
	pr, pw := io.Pipe()
	c := &Client{
		stdin:     pw,
		pending:   make(map[string]chan res),
		done:      make(chan struct{}),
		onRequest: onRequest,
	}
	return c, pr, pw
}

func TestAnswerServerRequest(t *testing.T) {
	c, pr, pw := newHandlerTestClient(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method != "ping" {
			t.Fatalf("method = %q", method)
		}
		return map[string]any{"pong": true}, nil
	})
	id := NumID(7)
	go c.answer(inbound{JSONRPC: Version, ID: &id, Method: "ping"})
	msg := readMessage(t, pr)
	pw.Close()
	if msg["id"] != float64(7) || msg["result"] == nil {
		t.Fatalf("response = %v", msg)
	}
}

func TestAnswerServerRequestError(t *testing.T) {
	c, pr, pw := newHandlerTestClient(func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &Error{Code: -32601, Message: "sampling is not supported"}
	})
	id := NumID(1)
	go c.answer(inbound{ID: &id, Method: "sampling/createMessage"})
	msg := readMessage(t, pr)
	pw.Close()
	errObj, ok := msg["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32601) || errObj["message"] != "sampling is not supported" {
		t.Fatalf("error response = %v", msg)
	}
}

func TestAnswerUnhandledMethod(t *testing.T) {
	c, pr, pw := newHandlerTestClient(nil) // no handler installed
	id := NumID(2)
	go c.answer(inbound{ID: &id, Method: "roots/list"})
	msg := readMessage(t, pr)
	pw.Close()
	errObj, ok := msg["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32601) {
		t.Fatalf("response = %v, want method-not-found", msg)
	}
}
