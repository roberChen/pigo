package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// mockLLM is a real OpenAI-compatible SSE server. It never calls a provider and
// is safe for concurrent requests.
//
// Every request is validated first (validate.go); a violation is evidence of a
// host-side defect, so it is recorded and answered with HTTP 400. A valid request
// is answered from the deterministic plan (plan.go) derived from the marker token
// in the latest user prompt: some thinking, some text, and zero or more tool
// calls, so a turn exercises whatever combination its token selects. A request
// without a marker (a compaction summarization prompt) gets a plain summary reply.
type mockLLM struct {
	server    *http.Server
	listener  net.Listener
	baseURL   string
	latency   time.Duration
	jitter    time.Duration
	errorRate float64
	// opts is what a healthy request must look like for this run.
	opts validateOptions

	rng   *rand.Rand
	rngMu sync.Mutex
	// failedTokens records the prompt token of every request that was answered
	// with an injected 500. It is the only process that knows a failure was
	// deliberate, so it is the only one that can tell a client "this turn was cut
	// short by the harness, not by a defect" — a turn whose provider failed
	// cannot be judged against the full plan.
	//
	// It is bounded: a multi-hour run at a high error rate would otherwise grow
	// this map without limit, and the harness measures heap growth. failedOrder
	// holds the insertion order used to evict the oldest entry.
	failedTokens map[string]bool
	failedOrder  []string
	requests     atomic.Int64
	failures     atomic.Int64
	invalid      atomic.Int64
	active       atomic.Int64
	peak         atomic.Int64
	log          *eventLog
	summary      atomic.Int64
}

// chatRequest is the subset of the OpenAI request the harness needs.
type chatRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// startMockLLM binds an ephemeral loopback port and starts serving immediately.
func startMockLLM(latency, jitter time.Duration, errorRate float64, opts validateOptions, log *eventLog) (*mockLLM, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mock llm listen: %w", err)
	}
	m := &mockLLM{
		listener:     ln,
		baseURL:      "http://" + ln.Addr().String() + "/v1",
		latency:      latency,
		jitter:       jitter,
		errorRate:    errorRate,
		opts:         opts,
		rng:          rand.New(rand.NewSource(1)),
		failedTokens: make(map[string]bool),
		log:          log,
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

// handleCompletion validates one request, then streams either a scripted HTTP 500,
// a 400 naming the violation, or the plan-derived reply.
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
		m.reject(w, "request body is not a chat completions object")
		return
	}

	if err := validateCompletionRequest(r, body, m.opts); err != nil {
		m.invalid.Add(1)
		m.log.write("mock_invalid_request", map[string]any{
			"error": err.Error(), "bytes": len(body),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	prompt := latestUserText(req)
	stage := assistantMessagesSinceLastUser(req)
	if planToken := promptToken(prompt); planToken != "" {
		m.log.write("llm_turn", map[string]any{"token": planToken, "stage": stage})
	}

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
	if failure {
		// Remember the token before answering: the client asks after the turn
		// unwinds, which is always after this point.
		if token := promptToken(prompt); token != "" && !m.failedTokens[token] {
			m.failedTokens[token] = true
			m.failedOrder = append(m.failedOrder, token)
			if len(m.failedOrder) > maxFailedTokens {
				delete(m.failedTokens, m.failedOrder[0])
				m.failedOrder = m.failedOrder[1:]
			}
		}
	}
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
	if flusher != nil {
		flusher.Flush()
	}

	if planToken := promptToken(prompt); planToken == "" {
		// A request without a marker is a summarization prompt. Acknowledging it
		// keeps compaction working; its output must never look like a turn reply.
		m.summary.Add(1)
		m.streamReply(w, flusher, replyPlan{
			Stage: stage,
			Text:  "SUMMARY-MARKER (longstay mock summary)",
		})
		return
	}

	token := promptToken(prompt)
	plan := planFor(token, stage, m.opts.Tools)
	if !m.streamReply(w, flusher, plan) {
		m.log.write("llm_stream_aborted", map[string]any{"token": token, "stage": stage})
	}
}

// maxFailedTokens bounds the injected-failure record. A client asks about a turn
// right after it settles, so only the most recent tokens are ever consulted.
const maxFailedTokens = 4096

// injectedFailure reports whether any request of this turn was answered with a
// deliberate 500. A turn that saw one may legitimately stop before the end of its
// plan, so the strict reply/plan comparison is skipped for it (the fault is
// already counted in -llm-error-rate, and the client records it separately).
func (m *mockLLM) injectedFailure(token string) bool {
	m.rngMu.Lock()
	defer m.rngMu.Unlock()
	return m.failedTokens[token]
}

// reject answers a request the mock cannot even decode.
func (m *mockLLM) reject(w http.ResponseWriter, reason string) {
	m.invalid.Add(1)
	m.log.write("mock_invalid_request", map[string]any{"error": reason})
	http.Error(w, reason, http.StatusBadRequest)
}

// streamReply writes plan as an OpenAI SSE stream: a role chunk, thinking deltas,
// text deltas in uneven chunks, tool-call arguments split across chunks, the
// finish reason, a usage chunk and [DONE]. It reports whether the client was still
// reading.
func (m *mockLLM) streamReply(w io.Writer, flusher http.Flusher, plan replyPlan) bool {
	if !writeSSE(w, flusher, chunk(map[string]any{"role": "assistant"}, "")) {
		return false
	}
	for _, part := range unevenChunks(plan.Thinking, hashToken(plan.Text, plan.Stage)>>5) {
		if !writeSSE(w, flusher, chunk(map[string]any{"reasoning_content": part}, "")) {
			return false
		}
	}
	for _, part := range unevenChunks(plan.Text, hashToken(plan.Text, plan.Stage)) {
		if !writeSSE(w, flusher, chunk(map[string]any{"content": part}, "")) {
			return false
		}
	}
	for i, call := range plan.Calls {
		// The header and the first argument fragment normally share a chunk; the
		// rest arrive with an index only, which is what the decoder must merge.
		if !writeSSE(w, flusher, toolChunk(i, map[string]any{
			"index": i, "id": toolCallID(plan, i), "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": ""},
		})) {
			return false
		}
		for _, part := range unevenChunks(call.Arguments, hashToken(call.Arguments, i)) {
			if !writeSSE(w, flusher, toolChunk(i, map[string]any{
				"index":    i,
				"function": map[string]any{"arguments": part},
			})) {
				return false
			}
		}
	}
	finish := "stop"
	if len(plan.Calls) > 0 {
		finish = "tool_calls"
	}
	if !writeSSE(w, flusher, map[string]any{
		"id": "longstay", "model": m.opts.Model,
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}},
	}) {
		return false
	}
	// Usage rides its own final chunk with an empty choices array, exactly as
	// OpenAI does when stream_options.include_usage is set.
	if !writeSSE(w, flusher, map[string]any{
		"id": "longstay", "model": m.opts.Model, "choices": []any{},
		"usage": map[string]any{"prompt_tokens": 16, "completion_tokens": 8},
	}) {
		return false
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return err == nil
}

// chunk builds a chat.completion.chunk carrying one delta.
func chunk(delta map[string]any, finish string) map[string]any {
	choice := map[string]any{"delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{
		"id": "longstay", "model": "mock-chat",
		"choices": []any{choice},
	}
}

// toolChunk builds a chunk carrying a tool-call fragment for call index i.
func toolChunk(i int, call map[string]any) map[string]any {
	return chunk(map[string]any{"tool_calls": []any{call}}, "")
}

// toolCallID derives a replayable, unique-per-conversation id for a planned call.
// Stability matters: the host echoes the id back in the tool result, and the next
// request's validator requires every result to pair with a call.
func toolCallID(plan replyPlan, i int) string {
	return "call_" + strconv.FormatUint(hashToken(plan.Text, plan.Stage+i)%1e12, 10) + "_" + strconv.Itoa(i)
}

// unevenChunks splits s into chunks of 1..7 bytes so the host sees irregular
// partial messages rather than one uniform delta per word. It never splits a UTF-8
// rune, because the decoder would then surface replacement text for a partially
// received character.
func unevenChunks(s string, hash uint64) []string {
	if s == "" {
		return nil
	}
	var out []string
	for i := 0; i < len(s); {
		size := 1 + int(hash%7)
		hash >>= 4
		if hash == 0 {
			hash = 0x9e3779b97f4a7c15
		}
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		for end < len(s) && !utf8Start(s[end]) {
			end++
		}
		out = append(out, s[i:end])
		i = end
	}
	return out
}

// utf8Start reports whether b can start a UTF-8 sequence (not a continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// latestUserText returns the last user message, accepting both OpenAI string
// content and a content-block array.
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

// assistantMessagesSinceLastUser is the plan stage: how many assistant responses
// this user turn has already produced. 0 is the first response to the prompt.
func assistantMessagesSinceLastUser(req chatRequest) int {
	stage := 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		switch req.Messages[i].Role {
		case "user":
			return stage
		case "assistant":
			stage++
		case "system":
			// A system-only prefix cannot precede a tool answer of this turn.
			return stage
		}
	}
	return stage
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
