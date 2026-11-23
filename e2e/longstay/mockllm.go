package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
// calls, so a turn exercises whatever combination its token selects. The stage
// comes from the latest marker-bearing user prompt, not a user-role reminder.
// The summarizer's distinct system prompt gets a plain summary reply.
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
	failedTokens        map[string]bool
	failedOrder         []string
	requests            atomic.Int64
	failures            atomic.Int64
	invalid             atomic.Int64
	active              atomic.Int64
	peak                atomic.Int64
	log                 *eventLog
	summary             atomic.Int64
	taskChildRequests   atomic.Int64
	taskChildCompleted  atomic.Int64
	taskChildReadChecks atomic.Int64
	taskChildFailures   atomic.Int64
	taskFollowUps       atomic.Int64
	// modelByToken records which model id answered a parent turn, keyed by the
	// turn's marker token. A model switch is only real if the very next request
	// for that session names the new id, and the client cannot see the request —
	// only the mock can. Bounded like failedTokens: only recently settled turns
	// are ever asked about.
	modelMu              sync.Mutex
	modelByToken         map[string]string
	modelOrder           []string
	taskChildMu          sync.Mutex
	taskChildByKey       map[string]int64
	taskChildFailedByKey map[string]int64
}

// chatRequest is the subset of the OpenAI request the harness needs.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
}

var dynamicArgumentPatterns = []struct {
	placeholder string
	toolName    string
	pattern     *regexp.Regexp
}{
	{placeholder: "{{LAST_BASH_ID}}", toolName: "bash", pattern: regexp.MustCompile(`bash_id: (bash_[0-9]+)`)},
	{placeholder: "{{LAST_SCHEDULE_ID}}", toolName: "schedule_create", pattern: regexp.MustCompile(`scheduled (sched-[0-9]+)`)},
}

func recordDynamicID(ids map[string]string, toolName, result string) {
	for _, dynamic := range dynamicArgumentPatterns {
		if dynamic.toolName != toolName {
			continue
		}
		matches := dynamic.pattern.FindAllStringSubmatch(result, -1)
		if len(matches) > 0 {
			ids[dynamic.placeholder] = matches[len(matches)-1][1]
		}
	}
}

func resolvePlannedArguments(arguments string, ids map[string]string) (string, error) {
	for _, dynamic := range dynamicArgumentPatterns {
		if !strings.Contains(arguments, dynamic.placeholder) {
			continue
		}
		id := ids[dynamic.placeholder]
		if id == "" {
			return "", fmt.Errorf("no current-turn %s result provides %s", dynamic.toolName, dynamic.placeholder)
		}
		arguments = strings.ReplaceAll(arguments, dynamic.placeholder, id)
	}
	for {
		start := strings.Index(arguments, "{{TASK_JOB:")
		if start < 0 {
			break
		}
		end := strings.Index(arguments[start:], "}}")
		if end < 0 {
			return "", fmt.Errorf("unterminated task job placeholder")
		}
		end += start + 2
		placeholder := arguments[start:end]
		id := ids[placeholder]
		if id == "" {
			return "", fmt.Errorf("no current-turn task result provides %s", placeholder)
		}
		arguments = strings.ReplaceAll(arguments, placeholder, id)
	}
	return arguments, nil
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
		modelByToken: make(map[string]string),
		log:          log,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", m.handleCompletion)
	mux.HandleFunc("/fixture", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "WEBFETCH-MARKER\n")
	})
	m.server = &http.Server{Handler: mux}
	go func() { _ = m.server.Serve(ln) }()
	return m, nil
}

// URL returns the OpenAI-compatible base URL.
func (m *mockLLM) URL() string { return m.baseURL }

// FixtureURL returns the local webfetch fixture endpoint.
func (m *mockLLM) FixtureURL() string {
	return strings.TrimSuffix(m.baseURL, "/v1") + "/fixture"
}

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

	childRequest := isTaskChildRequest(req)
	childToken, childStage := taskTokenAndStage(req)
	if childRequest {
		if childToken == "" {
			m.reject(w, "task child request has no longstay task marker")
			return
		}
		if err := validateTaskChildTools(body); err != nil {
			m.reject(w, "invalid task child tools: "+err.Error())
			return
		}
		m.taskChildRequests.Add(1)
		m.log.write("llm_child_turn", map[string]any{"token": childToken, "stage": childStage})
	}

	token, stage := planTokenAndStage(req)
	summaryRequest := isSummarizationRequest(req)
	if token != "" && !summaryRequest && !childRequest {
		m.recordTurnModel(token, req.Model)
		m.log.write("llm_turn", map[string]any{"token": token, "stage": stage, "model": req.Model})
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
	if childRequest {
		failure = strings.HasSuffix(childToken, "/error")
		if failure {
			m.taskChildFailures.Add(1)
		}
	}
	if failure {
		// Remember the token before answering: the client asks after the turn
		// unwinds, which is always after this point.
		if token != "" && !summaryRequest && !childRequest && !m.failedTokens[token] {
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
		if childRequest {
			m.recordTaskChildKey(childToken, true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"longstay injected upstream failure","type":"server_error"}}`)
		return
	}

	plan := replyPlan{Stage: stage, Text: "SUMMARY-MARKER (longstay mock summary)"}
	childFinal := childRequest && childStage > 0
	autoFollowUp := !childRequest && token != "" && stage == planRounds(token) && taskAutoFollowUp(token)
	switch {
	case childRequest:
		if childFinal {
			if !strings.Contains(taskTurnToolText(req), notesMarker) {
				m.reject(w, "task child final request lacks the real read marker")
				return
			}
			m.taskChildReadChecks.Add(1)
		}
		plan = taskPlanFor(childToken, childStage)
	case summaryRequest || token == "":
		if summaryRequest {
			m.summary.Add(1)
		}
	default:
		if autoFollowUp {
			if !autoTaskFollowUpSeen(req) {
				m.reject(w, "automatic task follow-up finished without its child report")
				return
			}
			m.taskFollowUps.Add(1)
		}
		plan = planFor(token, stage, m.opts.Tools)
		resolved, err := m.resolveDynamicArguments(plan, body)
		if err != nil {
			m.invalid.Add(1)
			m.log.write("mock_invalid_request", map[string]any{"error": err.Error(), "token": token, "stage": stage})
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		plan = resolved
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	streamed := m.streamReply(w, flusher, req.Model, plan)
	if !streamed && token != "" && !summaryRequest && !childRequest {
		m.log.write("llm_stream_aborted", map[string]any{"token": token, "stage": stage})
	}
	if !streamed && childRequest {
		m.log.write("llm_child_stream_aborted", map[string]any{"token": childToken, "stage": childStage})
	}
	if streamed && childFinal {
		m.taskChildCompleted.Add(1)
		m.recordTaskChildKey(childToken, false)
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

// maxTurnModels bounds the per-token model record. A client asks about a turn
// right after it settles, so only the most recent tokens are ever consulted.
const maxTurnModels = 4096

// recordTurnModel remembers the model id that answered a turn. A switch is
// verified against this, not against the client's own intention: the client
// asked for the switch, but only the host's request proves it landed.
func (m *mockLLM) recordTurnModel(token, model string) {
	if token == "" {
		return
	}
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	if m.modelByToken == nil {
		m.modelByToken = make(map[string]string)
	}
	if _, seen := m.modelByToken[token]; !seen {
		m.modelOrder = append(m.modelOrder, token)
		if len(m.modelOrder) > maxTurnModels {
			delete(m.modelByToken, m.modelOrder[0])
			m.modelOrder = m.modelOrder[1:]
		}
	}
	m.modelByToken[token] = model
}

// turnModel returns the model id that answered a turn, or "" when the mock has
// no record (a turn it never saw, or one evicted from the bound).
func (m *mockLLM) turnModel(token string) string {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	return m.modelByToken[token]
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
func (m *mockLLM) streamReply(w io.Writer, flusher http.Flusher, model string, plan replyPlan) bool {
	if model == "" {
		model = baseModel
	}
	if !writeSSE(w, flusher, chunk(model, map[string]any{"role": "assistant"}, "")) {
		return false
	}
	for _, part := range unevenChunks(plan.Thinking, hashToken(plan.Text, plan.Stage)>>5) {
		if !writeSSE(w, flusher, chunk(model, map[string]any{"reasoning_content": part}, "")) {
			return false
		}
	}
	for _, part := range unevenChunks(plan.Text, hashToken(plan.Text, plan.Stage)) {
		if !writeSSE(w, flusher, chunk(model, map[string]any{"content": part}, "")) {
			return false
		}
	}
	for i, call := range plan.Calls {
		// The header and the first argument fragment normally share a chunk; the
		// rest arrive with an index only, which is what the decoder must merge.
		if !writeSSE(w, flusher, toolChunk(model, i, map[string]any{
			"index": i, "id": toolCallID(plan, i), "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": ""},
		})) {
			return false
		}
		for _, part := range unevenChunks(call.Arguments, hashToken(call.Arguments, i)) {
			if !writeSSE(w, flusher, toolChunk(model, i, map[string]any{
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
		"id": "longstay", "model": model,
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}},
	}) {
		return false
	}
	// Usage rides its own final chunk with an empty choices array, exactly as
	// OpenAI does when stream_options.include_usage is set.
	if !writeSSE(w, flusher, map[string]any{
		"id": "longstay", "model": model, "choices": []any{},
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

// resolveDynamicArguments fills values only from tool results in the current
// user turn. Older turns may contain ids that were already deleted or stopped.
func (m *mockLLM) resolveDynamicArguments(plan replyPlan, body []byte) (replyPlan, error) {
	needsIDs := false
	for _, call := range plan.Calls {
		if strings.Contains(call.Arguments, "{{") {
			needsIDs = true
			break
		}
	}
	if !needsIDs {
		return plan, nil
	}
	ids, err := currentTurnDynamicIDs(body)
	if err != nil {
		return plan, err
	}
	for i := range plan.Calls {
		arguments, err := resolvePlannedArguments(plan.Calls[i].Arguments, ids)
		if err != nil {
			return plan, fmt.Errorf("%s stage %d: %w", plan.Text, plan.Stage, err)
		}
		plan.Calls[i].Arguments = arguments
	}
	return plan, nil
}

func currentTurnDynamicIDs(body []byte) (map[string]string, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode request for dynamic arguments: %w", err)
	}
	start := 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && promptToken(contentText(req.Messages[i].Content)) != "" {
			start = i + 1
			break
		}
	}
	ids := make(map[string]string)
	calls := make(map[string]wireToolCall)
	var results strings.Builder
	for _, message := range req.Messages[start:] {
		switch message.Role {
		case "assistant":
			for _, call := range message.ToolCalls {
				calls[call.ID] = call
			}
		case "tool":
			text := contentText(message.Content)
			results.WriteString(text)
			results.WriteByte('\n')
			if call, ok := calls[message.ToolCallID]; ok {
				recordDynamicID(ids, call.Function.Name, text)
				if call.Function.Name == "task" {
					recordTaskDynamicID(ids, call.Function.Arguments, text)
				}
			}
		}
	}
	for _, dynamic := range dynamicArgumentPatterns {
		recordDynamicID(ids, dynamic.toolName, results.String())
	}
	return ids, nil
}

// chunk builds a chat.completion.chunk carrying one delta. model is the id the
// request asked for, echoed back the way a real provider does — a session that
// switched models must see the new id on the wire, not the one it booted with.
func chunk(model string, delta map[string]any, finish string) map[string]any {
	choice := map[string]any{"delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{
		"id": "longstay", "model": model,
		"choices": []any{choice},
	}
}

// toolChunk builds a chunk carrying a tool-call fragment for call index i.
func toolChunk(model string, i int, call map[string]any) map[string]any {
	return chunk(model, map[string]any{"tool_calls": []any{call}}, "")
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

// planTokenAndStage finds the latest real prompt marker, skipping user-role
// system reminders appended by the runtime. The stage counts every assistant
// response after that prompt, including responses around those reminders.
func planTokenAndStage(req chatRequest) (string, int) {
	stage := 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		switch message.Role {
		case "assistant":
			stage++
		case "user":
			if token := promptToken(contentText(message.Content)); token != "" {
				return token, stage
			}
		}
	}
	return "", 0
}

func isSummarizationRequest(req chatRequest) bool {
	for _, message := range req.Messages {
		if message.Role == "system" {
			return strings.Contains(contentText(message.Content), "context summarization assistant")
		}
	}
	return false
}

func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
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
