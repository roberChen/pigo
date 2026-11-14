package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// mockLLM is a real OpenAI-compatible SSE server. It never calls a provider and
// is safe for concurrent requests. Its reply is deterministic from the latest
// user prompt, which lets the harness detect crossed or corrupted turns.
type mockLLM struct {
	server    *http.Server
	listener  net.Listener
	baseURL   string
	latency   time.Duration
	jitter    time.Duration
	errorRate float64

	rng      *rand.Rand
	rngMu    sync.Mutex
	requests atomic.Int64
	failures atomic.Int64
	active   atomic.Int64
	peak     atomic.Int64
	log      *eventLog
}

// chatRequest is the subset of the OpenAI request the harness needs.
type chatRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// startMockLLM binds an ephemeral loopback port and starts serving immediately.
func startMockLLM(latency, jitter time.Duration, errorRate float64, log *eventLog) (*mockLLM, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mock llm listen: %w", err)
	}
	m := &mockLLM{
		listener:  ln,
		baseURL:   "http://" + ln.Addr().String() + "/v1",
		latency:   latency,
		jitter:    jitter,
		errorRate: errorRate,
		rng:       rand.New(rand.NewSource(1)),
		log:       log,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", m.handleCompletion)
	m.server = &http.Server{Handler: mux}
	go func() { _ = m.server.Serve(ln) }()
	return m, nil
}

// URL returns the OpenAI-compatible base URL.
func (m *mockLLM) URL() string { return m.baseURL }

// Close stops the mock listener and active connections.
func (m *mockLLM) Close(ctx context.Context) error {
	return m.server.Shutdown(ctx)
}

// handleCompletion streams either a scripted HTTP 500 or a deterministic reply.
func (m *mockLLM) handleCompletion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.requests.Add(1)
	active := m.active.Add(1)
	defer m.active.Add(-1)
	for {
		peak := m.peak.Load()
		if active <= peak || m.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	defer func() {
		m.log.write("llm_request", map[string]any{
			"bytes": len(body), "active": active, "requests": m.requests.Load(),
		})
	}()

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	prompt := latestUserText(req)
	delay := m.latency
	if m.jitter > 0 {
		m.rngMu.Lock()
		delay += time.Duration(m.rng.Int63n(int64(m.jitter) + 1))
		m.rngMu.Unlock()
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	m.rngMu.Lock()
	failure := m.rng.Float64() < m.errorRate
	m.rngMu.Unlock()
	if failure {
		m.failures.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"longstay injected upstream failure","type":"server_error"}}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	reply := "mock:" + prompt
	const chunks = 8
	for i := 0; i < chunks; i++ {
		start := len(reply) * i / chunks
		end := len(reply) * (i + 1) / chunks
		if !writeSSE(w, flusher, map[string]any{
			"id": "longstay", "model": "mock-chat",
			"choices": []any{map[string]any{"delta": map[string]any{"content": reply[start:end]}}},
		}) {
			return
		}
	}
	writeSSE(w, flusher, map[string]any{
		"id": "longstay", "model": "mock-chat",
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// latestUserText returns the last user message, accepting both OpenAI string
// content and a simple content-block array.
func latestUserText(req chatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(req.Messages[i].Content, &text) == nil {
			return text
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(req.Messages[i].Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "text" {
					return block.Text
				}
			}
		}
		return ""
	}
	return ""
}

// writeSSE writes one data frame and reports whether the client is still there.
func writeSSE(w io.Writer, flusher http.Flusher, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
}
