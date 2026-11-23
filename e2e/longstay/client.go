package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/hostapi"
	"github.com/roberChen/pigo/internal/hostclient"
)

// turnOutcome is the result of one asynchronous prompt. It carries the attempt
// it belongs to: a client may exit mid-turn, reconnect and start a new prompt
// before the abandoned turn's goroutine returns, and without the token that late
// result would be attributed to (and corrupt the state of) the newer attempt.
type turnOutcome struct {
	attempt int
	token   string
	client  *hostclient.Client
	// model is the model the session was believed to run on when this turn was
	// launched. The mock reports which id actually answered, so comparing the two
	// proves an in-session switch reached the wire instead of only changing the
	// host's header.
	model  string
	result hostapi.PromptResult
	err    error
}

// resumeOwnershipGrace is how long a rejected resume is retried before it is
// reported. Ownership handoff after a dropped connection is normal; staying
// owned by a connection that never went away is not.
const resumeOwnershipGrace = 2 * time.Second

// turnTrace is the stream/plan verification state of one attempt. The mock's
// reply is a pure function of the prompt token, so the client can check the
// authoritative turn_end messages and tool results it receives against the plan
// it derives for the same token. Nothing is shared between the two processes.
type turnTrace struct {
	token string
	// stage is how many assistant messages matched their planned shape, in order.
	stage int
	// turnID/maxSeq dedupe replay: a reconnect re-delivers events this trace has
	// already counted, and counting them twice would look like an extra stage.
	turnID string
	maxSeq uint64
	// pending maps a tool call id of the current stage to the call it must be.
	pending map[string]plannedCall
	// results buffers tool_end events that arrived before the turn_end naming
	// their calls: the executor emits each tool's result as it completes, which
	// is one event ahead of the TurnEndEvent carrying the assistant message, so
	// a result can never be matched to a plan at the moment it arrives.
	results map[string]hostapi.SessionEvent
	// dynamicIDs binds placeholders to ids produced earlier in this same turn.
	dynamicIDs map[string]string
	// active is false once the run settled (agent_end) or the client exited.
	active  bool
	summary bool
}

// clientState is one independent UI lifecycle. Prompt execution, event delivery
// and connection loss are all asynchronous; the state machine never waits for a
// turn to finish before allowing an exit or reconnect.
type clientState struct {
	runner *runner
	id     int
	rng    *rand.Rand
	cwd    string

	mu          sync.Mutex
	client      *hostclient.Client
	sessionID   string
	connected   bool
	everJoined  bool
	needsResume bool
	// resumePendingAt marks the first resume this reconnection attempt had
	// rejected because the previous connection was still the session's owner.
	resumePendingAt time.Time
	observed        bool
	running         bool
	// model is the session's current model as this client last saw it. It is
	// updated from every create/resume/switch response, so a turn's expected model
	// is never a local guess.
	model string
	// nextSwitchTurn is the prompt count at which the next switch is due. It
	// advances by ModelSwitchEvery after each switch; a modulo test on turn would
	// retrigger immediately, because a switch does not itself consume a turn.
	nextSwitchTurn int
	exitAt         time.Time
	reconnectAt    time.Time
	nextPrompt     time.Time
	outcomes       chan struct{}
	cancel         context.CancelFunc
	turn           int
	// attempt numbers prompts across reconnects. A late outcome whose attempt no
	// longer matches is discarded instead of being applied to a newer turn.
	attempt int
	// trace is the in-flight attempt's verification state, or nil when this
	// client is only observing a turn it did not start.
	trace *turnTrace

	// pending holds finished prompts until the loop drains them, and outcomeSignal
	// wakes it. A prompt goroutine must never block on delivery (the loop may have
	// stopped while its turn was in flight) and must never drop a result (a lost
	// live outcome would leave running=true forever and stall the client), so the
	// hand-off is an unbounded under-lock queue rather than a channel.
	outcomeMu     sync.Mutex
	pending       []turnOutcome
	outcomeSignal chan struct{}

	seen    map[string]uint64
	missing map[string]map[uint64]bool
}

// newClientState allocates a client slot with an isolated workspace and RNG.
func newClientState(r *runner, id int) *clientState {
	cwd := r.workspaceFor(id)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		r.recordProtocol("create_client_workspace", err)
	}
	return &clientState{
		runner:        r,
		id:            id,
		rng:           rand.New(rand.NewSource(r.cfg.Seed + int64(id)*7919)),
		cwd:           cwd,
		outcomeSignal: make(chan struct{}, 1),
		seen:          make(map[string]uint64),
		missing:       make(map[string]map[uint64]bool),
	}
}

// run drives this client until the harness stops.
func (s *clientState) run(ctx context.Context) {
	defer s.runner.clients.Done()
	ticker := time.NewTicker(s.runner.cfg.ActionInterval)
	defer ticker.Stop()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.exit("run complete")
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.outcomeSignal:
			for _, outcome := range s.takeOutcomes() {
				s.handleOutcome(outcome)
			}
		case <-ticker.C:
			s.step()
		}
	}
}

// deliverOutcome queues a finished prompt and wakes the loop. It never blocks, so
// a prompt goroutine cannot leak when the loop has already stopped.
func (s *clientState) deliverOutcome(outcome turnOutcome) {
	s.outcomeMu.Lock()
	s.pending = append(s.pending, outcome)
	s.outcomeMu.Unlock()
	select {
	case s.outcomeSignal <- struct{}{}:
	default:
		// A wake-up is already queued, which is enough for the whole batch.
	}
}

// takeOutcomes drains the queue in completion order.
func (s *clientState) takeOutcomes() []turnOutcome {
	s.outcomeMu.Lock()
	defer s.outcomeMu.Unlock()
	outcomes := s.pending
	s.pending = nil
	return outcomes
}

// step performs at most one lifecycle transition.
func (s *clientState) step() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.client == nil {
		if now.Before(s.reconnectAt) {
			return
		}
		s.connect()
		return
	}
	if now.After(s.exitAt) {
		s.exit("lifetime elapsed")
		return
	}
	if s.sessionID == "" {
		s.createSession()
		return
	}
	if s.running {
		return
	}
	if s.needsResume {
		s.resume()
		return
	}
	if now.After(s.nextPrompt) {
		// A switch is attempted between turns, never during one: the host rejects
		// a mid-turn switch with CodeBusy precisely so a turn's budget cannot
		// disagree with the model answering it.
		if s.shouldSwitchModel() {
			s.switchModel()
			return
		}
		s.startPrompt()
	}
}

// shouldSwitchModel reports whether this session is due for a model switch. It
// counts prompts rather than wall time so a short run still exercises the seam,
// and it keeps no RNG state, so a seeded run switches at the same points.
func (s *clientState) shouldSwitchModel() bool {
	if s.runner.cfg.ModelSwitchEvery <= 0 {
		return false
	}
	return s.turn >= s.nextSwitchTurn
}

// scheduleNextSwitch arms the following switch. It is called after a switch
// lands, so a rejected one is retried on the next tick instead of being pushed
// a whole interval into the future.
func (s *clientState) scheduleNextSwitch() {
	if s.runner.cfg.ModelSwitchEvery <= 0 {
		return
	}
	s.nextSwitchTurn = s.turn + s.runner.cfg.ModelSwitchEvery
}

// switchModel moves the session to the next model in the rotation and checks what
// the host resolved for it.
//
// The budget check is the point of the scenario. Only the host knows the layered
// configuration that decides where auto-compaction fires, so it returns the
// resolved window/budget/trigger and this side compares them against pigo's
// built-in limit table. A switch that changes the model id but leaves the old
// model's plan in place (the bug this covers) is invisible in the RPC's success
// and shows up exactly here.
func (s *clientState) switchModel() {
	client, session := s.client, s.sessionID
	from := s.model
	if from == "" {
		from = baseModel
	}
	to := nextLongstayModel(from)
	res, err := client.SetModel(session, to)
	if err != nil {
		// Every one of these is a legitimate race with the lifecycle, not a host
		// defect: a turn this client is only observing is still running, or the
		// previous connection's ownership has not been released yet.
		switch {
		case isCode(err, hostapi.CodeBusy):
			s.nextPrompt = time.Now().Add(50 * time.Millisecond)
		case isCode(err, hostapi.CodeNotFound):
			// Nothing persisted yet, or the session was closed under us.
			s.sessionID = ""
		case isCode(err, hostapi.CodeForbidden):
			s.needsResume = true
			s.nextPrompt = time.Now().Add(50 * time.Millisecond)
		default:
			s.runner.recordProtocol("session_set_model", err)
		}
		return
	}
	if res.Session.ID != session {
		s.runner.recordProtocol("model_switch_session", fmt.Errorf("client %d switched %s, host answered for %s", s.id, session, res.Session.ID))
		return
	}
	if err := verifySwitchBudget(res); err != nil {
		s.runner.recordProtocol("model_switch_budget", fmt.Errorf("client %d %s -> %s: %w", s.id, from, to, err))
		return
	}
	if res.Session.Model != to {
		s.runner.recordProtocol("model_switch_identity", fmt.Errorf("client %d asked for %q, session reports %q", s.id, to, res.Session.Model))
		return
	}
	if res.Session.Provider != providerNameOpenAI {
		s.runner.recordProtocol("model_switch_provider", fmt.Errorf("client %d switch changed provider to %q", s.id, res.Session.Provider))
	}
	s.model = res.Session.Model
	s.scheduleNextSwitch()
	s.runner.metrics.modelSwitchVerified.Add(1)
	// Prompt immediately: the next turn is what proves the new model is the one
	// answering, and keeping the gap small keeps the check attributable.
	s.nextPrompt = time.Now()
	s.runner.log.write("model_switched", map[string]any{
		"client": s.id, "session": session, "from": from, "to": s.model,
		"contextWindow": res.ContextWindow, "inputBudget": res.InputBudget,
		"triggerPoint": res.TriggerPoint, "triggerSource": res.TriggerSource,
	})
}

// connect establishes a fresh host connection and installs event callbacks
// before resuming, so live events cannot be missed during the handshake.
func (s *clientState) connect() {
	client, err := hostclient.Connect(hostclient.Options{
		Dir: s.runner.homeDir, Client: fmt.Sprintf("longstay-%03d", s.id),
		CallTimeout: s.runner.cfg.CallTimeout,
	})
	if err != nil {
		s.reconnectAt = time.Now().Add(s.delay(s.runner.cfg.MinReconnectDelay, s.runner.cfg.MaxReconnectDelay))
		s.runner.metrics.transportErrors.Add(1)
		s.runner.log.write("connect_error", map[string]any{"client": s.id, "error": err.Error()})
		return
	}
	s.client = client
	s.client.OnEvent("", s.onEvent)
	s.client.OnEvent(hostapi.EventPermission, s.onPermission)
	s.client.OnEvent(hostapi.EventQueueOverflow, func(hostapi.Envelope) {
		s.runner.metrics.queueOverflows.Add(1)
	})
	s.connected = true
	if s.everJoined {
		s.runner.metrics.reconnects.Add(1)
	} else {
		s.everJoined = true
	}
	s.runner.metrics.clientsStarted.Add(1)
	countPeak(&s.runner.metrics.peakClients, s.runner.connectedClients.Add(1))
	s.needsResume = s.sessionID != ""
	s.exitAt = time.Now().Add(s.delay(s.runner.cfg.MinClientLifetime, s.runner.cfg.MaxClientLifetime))
	s.runner.log.write("client_connected", map[string]any{"client": s.id, "session": s.sessionID})
}

// exit closes the current connection without waiting for the turn. The host may
// continue running it; a later resume is what restores observation and writing.
func (s *clientState) exit(reason string) {
	if s.client == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.client.Close()
	s.client = nil
	s.connected = false
	s.running = false
	s.cancel = nil
	// The attempt's events can no longer arrive on this connection. Its trace is
	// dropped rather than merged into whatever the next lifetime observes, and
	// pending gaps are forgotten: a turn this client walked away from cannot be
	// repaired by a replay it is not entitled to, so holding the gap against the
	// host would blame a disconnect on the server.
	s.trace = nil
	clear(s.missing)
	s.reconnectAt = time.Now().Add(s.delay(s.runner.cfg.MinReconnectDelay, s.runner.cfg.MaxReconnectDelay))
	s.runner.connectedClients.Add(-1)
	s.runner.log.write("client_exited", map[string]any{"client": s.id, "session": s.sessionID, "reason": reason})
}

// createSession starts a conversation in this client's own directory.
func (s *clientState) createSession() {
	info, err := s.client.CreateSession(s.cwd, "mock-chat", fmt.Sprintf("longstay-%03d", s.id))
	if err != nil {
		s.runner.recordProtocol("session_create", err)
		return
	}
	s.sessionID = info.ID
	s.model = info.Model
	s.nextSwitchTurn = s.runner.cfg.ModelSwitchEvery
	s.needsResume = false
	s.observed = false
	s.turn = 0
	s.nextPrompt = time.Now()
	s.runner.log.write("session_created", map[string]any{"client": s.id, "session": info.ID, "cwd": s.cwd})
}

// resume inspects an existing session. Running sessions are observed only;
// idle sessions are adopted and can receive the next prompt.
func (s *clientState) resume() {
	rr, err := s.client.Resume(s.sessionID)
	if err != nil {
		if isCode(err, hostapi.CodeNotFound) {
			// A session that never persisted a turn has no file to resume.
			s.sessionID = ""
			return
		}
		if isCode(err, hostapi.CodeBusy) {
			// Final persistence and tombstone removal race with turn_end.
			s.nextPrompt = time.Now().Add(50 * time.Millisecond)
			return
		}
		// The host releases ownership when its read loop notices the socket the
		// harness just dropped, which is a few milliseconds after the client's
		// resume arrives. Rejecting the resume in that window is correct host
		// behaviour, so it is retried for a bounded stretch and only counted as a
		// violation if the session stays owned by a connection that never died.
		if isCode(err, hostapi.CodeForbidden) && strings.Contains(err.Error(), "attached to another client") {
			if s.resumePendingAt.IsZero() {
				s.resumePendingAt = time.Now()
			}
			if time.Since(s.resumePendingAt) < resumeOwnershipGrace {
				s.nextPrompt = time.Now().Add(50 * time.Millisecond)
				return
			}
		}
		s.runner.recordProtocol("session_resume", err)
		return
	}
	s.replay(rr.Events)
	if rr.EventsTruncated {
		s.runner.metrics.replayTruncated.Add(1)
	}
	if rr.Session.Running {
		s.observed = true
		s.nextPrompt = time.Now().Add(50 * time.Millisecond)
		return
	}
	s.observed = false
	s.needsResume = false
	s.resumePendingAt = time.Time{}
	if rr.Session.Model != "" {
		// The header is authoritative: a switch made by an earlier lifetime of this
		// slot (or by anything else) must not leave a stale expectation here.
		s.model = rr.Session.Model
	}
	// A resumed idle session keeps its history; the plan is keyed by the token in
	// each user prompt, so the next turn verifies exactly like the first one did.
	s.runner.metrics.adoptions.Add(1)
	s.nextPrompt = time.Now().Add(s.delay(s.runner.cfg.MinPromptDelay, s.runner.cfg.MaxPromptDelay))
	s.runner.log.write("session_adopted", map[string]any{"client": s.id, "session": s.sessionID, "messages": len(rr.Messages)})
}

// startPrompt sends one random prompt carrying a marker token and returns
// immediately, allowing a lifetime expiry or disconnect to happen while the
// provider is still running. It is called with s.mu held.
func (s *clientState) startPrompt() {
	s.turn++
	s.attempt++
	token := fmt.Sprintf("client-%03d/turn-%d/attempt-%d", s.id, s.turn, s.attempt)
	// Prepare this turn's editable fixture before the first tool call, so edit
	// never depends on an earlier write that the plan did not select.
	for _, call := range sequence(token) {
		if call.Name == "edit" {
			path := filepath.Join(s.cwd, "editable-"+artifactName(token)+".txt")
			if err := os.WriteFile(path, []byte("WRITE-MARKER\n"), 0o644); err != nil {
				s.runner.recordProtocol("prepare_edit_fixture", err)
			}
		}
	}
	// Capture the connection and session for this attempt: both fields are
	// cleared by exit() under the same mutex, and reading them from the goroutine
	// would both race and let a reply be sent on the next attempt's connection.
	client, session := s.client, s.sessionID
	prompt := promptFor(s.rng, token)
	outcome := turnOutcome{attempt: s.attempt, token: token, client: client, model: s.model}
	s.trace = &turnTrace{
		token:      token,
		pending:    make(map[string]plannedCall),
		results:    make(map[string]hostapi.SessionEvent),
		dynamicIDs: make(map[string]string),
		active:     true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true
	s.runner.metrics.prompts.Add(1)
	s.runner.log.write("prompt_started", map[string]any{"client": s.id, "session": session, "turn": s.turn, "token": token})
	go func() {
		outcome.result, outcome.err = client.Prompt(ctx, session, prompt)
		s.deliverOutcome(outcome)
	}()
}

// handleOutcome distinguishes expected provider faults and disconnects from
// actual protocol failures or crossed replies.
func (s *clientState) handleOutcome(outcome turnOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if outcome.attempt != s.attempt {
		// A late result from an abandoned attempt (the client exited mid-turn and
		// has since reconnected). It says nothing about the current attempt, so it
		// must not clear its running flag or move its next prompt.
		s.runner.log.write("stale_outcome", map[string]any{"client": s.id, "attempt": outcome.attempt, "current": s.attempt})
		return
	}
	s.running = false
	s.cancel = nil
	if outcome.err != nil {
		if s.client == nil || s.client != outcome.client {
			// Expected when exit() closed the connection during the turn.
			return
		}
		if isCode(outcome.err, hostapi.CodeBusy) || isCode(outcome.err, hostapi.CodeForbidden) {
			// Observer-to-owner adoption can race with the old turn's final write.
			s.needsResume = true
			s.nextPrompt = time.Now().Add(50 * time.Millisecond)
			return
		}
		s.runner.metrics.transportErrors.Add(1)
		s.runner.log.write("prompt_transport_error", map[string]any{"client": s.id, "session": s.sessionID, "error": outcome.err.Error()})
		return
	}
	if outcome.result.Error != "" {
		// Only the mock knows whether this failure was the one it injected. An
		// unexpected provider error is evidence of a defect, not a scenario.
		if s.runner.injectedFailure(outcome.token) {
			s.runner.metrics.providerErrors.Add(1)
			s.runner.log.write("provider_error", map[string]any{"client": s.id, "session": s.sessionID, "turn": s.turn, "injected": true})
		} else {
			s.runner.recordProtocol("unexpected_provider_error", fmt.Errorf("token %s: %s", outcome.token, outcome.result.Error))
		}
	} else if outcome.result.StopReason == "aborted" {
		s.runner.metrics.aborted.Add(1)
	} else if s.trace != nil && s.trace.token == outcome.token && s.trace.summary {
		s.runner.metrics.turnsCompacted.Add(1)
		s.runner.log.write("turn_compacted", map[string]any{"client": s.id, "session": s.sessionID, "token": outcome.token})
	} else if s.runner.injectedFailure(outcome.token) {
		// The turn saw a deliberate 500. pigo's repair path may have degraded a
		// tool result and finished on a shorter path than the plan describes, so
		// the reply cannot be compared against the full plan; the verified stages
		// and the persisted transcript still are.
		s.runner.metrics.turnsDegraded.Add(1)
		s.runner.log.write("turn_degraded", map[string]any{"client": s.id, "session": s.sessionID, "token": outcome.token})
	} else {
		s.runner.metrics.completed.Add(1)
		s.verifyTurnModel(outcome)
		// Compaction may replace the user-visible tail with a mock summary. The
		// persisted plan stages are still checked by verifyTranscript.
		if !strings.HasPrefix(outcome.result.Text, "SUMMARY-MARKER") {
			if want := expectedFinalText(outcome.token, s.runner.toolsEnabled()); outcome.result.Text != want {
				s.runner.recordProtocol("reply_mismatch", fmt.Errorf("got %q, want %q", outcome.result.Text, want))
			}
		}
	}
	s.nextPrompt = time.Now().Add(s.delay(s.runner.cfg.MinPromptDelay, s.runner.cfg.MaxPromptDelay))
}

// verifyTurnModel checks that the model the mock actually saw on the wire is the
// one this client believed the session ran on.
//
// A successful session.setModel only proves the host accepted the request. The
// budget it returned could still be computed for a model that never answers, so
// the authoritative signal is the mock's record of the request: it is the only
// party that sees the model id pigo sends. A mismatch means the switch changed
// the session header without reaching the loop's streaming config.
func (s *clientState) verifyTurnModel(outcome turnOutcome) {
	// Child-agent requests are keyed by their own tokens and never carry the
	// parent's, so only parent turns are consulted here.
	if outcome.model == "" {
		return
	}
	answered := s.runner.turnModel(outcome.token)
	if answered == "" {
		// The mock saw no parent turn for this token. It may have been evicted from
		// the bound, or the turn degraded before the request; neither is evidence
		// about the model.
		return
	}
	if answered != outcome.model {
		s.runner.metrics.modelSwitchTurnsWrong.Add(1)
		s.runner.recordProtocol("turn_model_mismatch", fmt.Errorf("client %d token %s answered by %q, expected %q", s.id, outcome.token, answered, outcome.model))
	}
}

// onPermission answers a side-effect request only for this client's session.
func (s *clientState) onPermission(env hostapi.Envelope) {
	var req hostapi.PermissionRequest
	if env.DecodeParams(&req) != nil {
		return
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		_ = client.Decide(req.SessionID, req.RequestID, true, false)
	}
}

// onEvent validates event ownership and sequence continuity, including events
// recovered by a later replay. Missing sequence numbers are filled out of order.
func (s *clientState) onEvent(env hostapi.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEvent(env)
}

// recordEvent is called with s.mu held, by either a live callback or replay.
func (s *clientState) recordEvent(env hostapi.Envelope) {
	if env.Event != hostapi.EventSession {
		return
	}
	var ev hostapi.SessionEvent
	if env.DecodeParams(&ev) != nil {
		s.runner.recordProtocol("decode_session_event", errors.New("invalid session event"))
		return
	}
	if s.sessionID != "" && ev.SessionID != s.sessionID {
		s.runner.recordProtocol("cross_session_event", fmt.Errorf("client %d got %s, own %s", s.id, ev.SessionID, s.sessionID))
		return
	}
	if ev.TurnID == "" || ev.Seq == 0 {
		return
	}
	previous := s.seen[ev.TurnID]
	if ev.Seq > previous {
		if previous > 0 && ev.Seq > previous+1 {
			if s.missing[ev.TurnID] == nil {
				s.missing[ev.TurnID] = make(map[uint64]bool)
			}
			for seq := previous + 1; seq < ev.Seq; seq++ {
				s.missing[ev.TurnID][seq] = true
			}
		}
		s.seen[ev.TurnID] = ev.Seq
		// Only a newly seen event is verified: a replay re-delivers events this
		// client already counted.
		s.verifyEvent(ev)
	} else {
		s.runner.metrics.duplicates.Add(1)
	}
	if missing := s.missing[ev.TurnID]; missing != nil {
		delete(missing, ev.Seq)
		if len(missing) == 0 {
			delete(s.missing, ev.TurnID)
		}
	}
	if ev.Type == hostapi.SessionEventAgentEnd && s.observed {
		// The run settled. agent_end, not turn_end, is the settle point: turn_end
		// fires for every assistant message, including the ones inside a tool loop.
		s.needsResume = true
	}
}

// verifyEvent checks one newly seen event against the in-flight attempt's plan.
// It is called with s.mu held.
func (s *clientState) verifyEvent(ev hostapi.SessionEvent) {
	t := s.trace
	if t == nil || !t.active {
		return
	}
	if t.turnID == "" {
		t.turnID = ev.TurnID
	} else if t.turnID != ev.TurnID {
		return // a later attempt's events; the trace belongs to an older turn
	}
	if ev.Seq <= t.maxSeq {
		return
	}
	t.maxSeq = ev.Seq
	switch ev.Type {
	case hostapi.SessionEventTurnEnd:
		if ev.Message != nil {
			s.verifyStage(t, *ev.Message)
		}
	case hostapi.SessionEventToolEnd:
		s.bufferToolResult(t, ev)
	case hostapi.SessionEventSubAgent:
		if ev.ToolCallID == "" || ev.Activity == "" {
			s.runner.recordProtocol("subagent_progress_mismatch", fmt.Errorf("token %s: invalid progress event", t.token))
			return
		}
		s.runner.metrics.subAgentProgress.Add(1)
	case hostapi.SessionEventSubAgentDone:
		if ev.ToolCallID == "" {
			s.runner.recordProtocol("subagent_done_mismatch", fmt.Errorf("token %s: missing id", t.token))
			return
		}
		s.runner.metrics.subAgentDone.Add(1)
	case hostapi.SessionEventAgentEnd:
		s.finishTrace(t)
	}
}

// verifyStage compares one assistant message with the plan's next stage: its
// thinking, its text, and the names and arguments of its tool calls.
func (s *clientState) verifyStage(t *turnTrace, msg agentcore.AssistantMessage) {
	if !s.connected {
		// Exiting mid-turn is an intended lifecycle event, not a protocol error.
		return
	}
	gotText := agentcore.ContentToText(msg.Content)
	if strings.HasPrefix(gotText, "SUMMARY-MARKER") {
		// Compaction summaries are provider work, not a planned assistant stage.
		t.summary = true
		t.active = false
		return
	}
	if msg.StopReason == agentcore.StopReasonAborted || msg.StopReason == agentcore.StopReasonError {
		// A cancelled or failed turn legitimately stops before the plan's end.
		return
	}
	want := planFor(t.token, t.stage, s.runner.toolsEnabled())
	if s.runner.toolsEnabled() && len(want.Calls) > 0 && msg.StopReason != agentcore.StopReasonToolUse {
		if s.runner.injectedFailure(t.token) {
			// The repair path degraded this turn's output; it is allowed to stop
			// before the plan's round count.
			return
		}
		s.runner.recordProtocol("stage_missing_tool_calls", fmt.Errorf("token %s stage %d stopped with %q", t.token, t.stage, msg.StopReason))
		return
	}
	gotThinking := agentcore.ThinkingText(msg.Content)
	if gotText != want.Text {
		s.runner.recordProtocol("stage_text_mismatch", fmt.Errorf("token %s stage %d: got %q, want %q", t.token, t.stage, gotText, want.Text))
		return
	}
	if gotThinking != want.Thinking {
		s.runner.recordProtocol("stage_thinking_mismatch", fmt.Errorf("token %s stage %d: got %q, want %q", t.token, t.stage, gotThinking, want.Thinking))
		return
	}
	calls := msg.ToolCalls()
	if len(calls) != len(want.Calls) {
		s.runner.recordProtocol("stage_call_count_mismatch", fmt.Errorf("token %s stage %d: got %d calls, want %d", t.token, t.stage, len(calls), len(want.Calls)))
		return
	}
	t.pending = make(map[string]plannedCall, len(calls))
	for i, call := range calls {
		planned := want.Calls[i]
		expectedArgs, err := resolvePlannedArguments(planned.Arguments, t.dynamicIDs)
		if err != nil {
			s.runner.recordProtocol("stage_call_mismatch", fmt.Errorf("token %s stage %d call %d: %w", t.token, t.stage, i, err))
			return
		}
		if call.Name != planned.Name || string(call.Arguments) != expectedArgs {
			s.runner.recordProtocol("stage_call_mismatch", fmt.Errorf("token %s stage %d call %d: got %s%s, want %s%s",
				t.token, t.stage, i, call.Name, call.Arguments, planned.Name, expectedArgs))
			return
		}
		t.pending[call.ID] = planned
		// The result normally arrived while this message was still streaming.
		if ev, ok := t.results[call.ID]; ok {
			delete(t.pending, call.ID)
			delete(t.results, call.ID)
			s.checkToolResult(t, call.ID, planned, ev)
		}
	}
	if len(want.Calls) > 0 {
		s.runner.log.write("stage_verified", map[string]any{"client": s.id, "token": t.token, "stage": t.stage, "calls": len(want.Calls)})
	}
	t.stage++
}

// bufferToolResult holds a completed call's result until the turn_end that names
// the call arrives; the ordering is fixed by the runtime (see turnTrace.results).
// A result for a call already planned is verified at once, which keeps verification
// correct if that ordering ever changes.
func (s *clientState) bufferToolResult(t *turnTrace, ev hostapi.SessionEvent) {
	t.results[ev.ToolCallID] = ev
	if planned, ok := t.pending[ev.ToolCallID]; ok {
		delete(t.pending, ev.ToolCallID)
		delete(t.results, ev.ToolCallID)
		s.checkToolResult(t, ev.ToolCallID, planned, ev)
	}
}

// checkToolResult proves a planned call really executed inside a workspace: the
// result must carry the marker that only that file contains.
//
// The one tolerated error is a tool result the runtime itself degraded into an
// abnormal placeholder after the provider rejected the request: that is the
// documented recovery path for an injected upstream failure (the harness runs
// with a nonzero -llm-error-rate on purpose), not a defect in the call. The
// original output is kept ahead of the placeholder, so the marker check still
// proves the tool ran.
func (s *clientState) checkToolResult(t *turnTrace, callID string, planned plannedCall, ev hostapi.SessionEvent) {
	degraded := strings.Contains(ev.Result, agentcore.AbnormalToolResultMarker)
	if ev.IsError && !degraded && !planned.AllowError {
		s.runner.recordProtocol("tool_call_error", fmt.Errorf("token %s tool %s: %s", t.token, ev.ToolName, strings.TrimSpace(ev.Result)))
		return
	}
	matched := false
	for _, want := range planned.ExpectAny {
		if strings.Contains(ev.Result, want) {
			matched = true
			break
		}
	}
	if !matched {
		s.runner.recordProtocol("tool_result_mismatch", fmt.Errorf("token %s tool %s: result %q lacks any of %v", t.token, ev.ToolName, truncate(ev.Result, 160), planned.ExpectAny))
		return
	}
	recordDynamicID(t.dynamicIDs, ev.ToolName, ev.Result)
	if planned.Name == "task" {
		recordTaskDynamicID(t.dynamicIDs, planned.Arguments, ev.Result)
	}
	if planned.CheckResult != nil {
		if err := planned.CheckResult(ev.Result); err != nil {
			s.runner.recordProtocol("tool_result_mismatch", fmt.Errorf("token %s tool %s: %w", t.token, ev.ToolName, err))
			return
		}
	}
	if planned.Name == "task_output" {
		switch {
		case planned.Arguments == `{}`:
			s.runner.metrics.taskOutputList.Add(1)
		case strings.Contains(planned.Arguments, `"wait_all":true`):
			s.runner.metrics.taskOutputWaitAll.Add(1)
		case strings.Contains(planned.Arguments, `"job_ids"`):
			s.runner.metrics.taskOutputBatch.Add(1)
		default:
			s.runner.metrics.taskOutputSingle.Add(1)
		}
	}
	if (planned.Name == "task" || planned.Name == "task_output") && strings.Contains(ev.Result, taskReportMarker) {
		s.verifyTaskSessionIDs(t.token, ev.Result)
	}
	s.verifyWorkspaceEffect(t, planned)
	if degraded {
		s.runner.metrics.degradedResults.Add(1)
	}
	s.runner.metrics.toolVerifications.Add(1)
}

// verifyWorkspaceEffect checks the postcondition that cannot be inferred from a
// tool's textual result alone: a write/edit actually changed the intended file.
func (s *clientState) verifyWorkspaceEffect(t *turnTrace, planned plannedCall) {
	if planned.Effect == nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.cwd, planned.Effect.Path))
	if err != nil {
		s.runner.recordProtocol("workspace_effect", fmt.Errorf("token %s tool %s: read %s: %w", t.token, planned.Name, planned.Effect.Path, err))
		return
	}
	if !strings.Contains(string(data), planned.Effect.Contains) {
		s.runner.recordProtocol("workspace_effect", fmt.Errorf("token %s tool %s: %s lacks %q", t.token, planned.Name, planned.Effect.Path, planned.Effect.Contains))
	}
}

func (s *clientState) verifyTaskSessionIDs(token, result string) {
	matches := successfulTaskSessionMatches(result)
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		id := match[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if id == "" || filepath.Base(id) != id {
			s.runner.recordProtocol("task_session_mismatch", fmt.Errorf("token %s: invalid child session id %q", token, id))
			continue
		}
		path := filepath.Join(s.runner.homeDir, "sessions", id+".jsonl")
		data, err := os.ReadFile(path)
		if err != nil {
			s.runner.recordProtocol("task_session_mismatch", fmt.Errorf("token %s: read child %s: %w", token, id, err))
			continue
		}
		text := string(data)
		required := []string{
			`"parentSession":"` + s.sessionID + `"`,
			`"subAgent":"task"`,
			`"toolName":"read"`,
			notesMarker,
			taskReportMarker,
		}
		for _, marker := range required {
			if !strings.Contains(text, marker) {
				s.runner.recordProtocol("task_session_mismatch", fmt.Errorf("token %s child %s lacks %q", token, id, marker))
			}
		}
		s.runner.metrics.taskSessionsVerified.Add(1)
	}
}

// finishTrace closes the attempt's verification and counts it when every planned
// stage was seen. A turn cut short by an exit is not a failure: the client's own
// `aborted`/`stale_outcome` handling accounts for those separately.
func (s *clientState) finishTrace(t *turnTrace) {
	if t.summary || !s.connected {
		return
	}
	t.active = false
	for callID, planned := range t.pending {
		if ev, ok := t.results[callID]; ok {
			s.checkToolResult(t, callID, planned, ev)
			continue
		}
		s.runner.metrics.turnsUnverified.Add(1)
		s.runner.log.write("tool_result_missing", map[string]any{"client": s.id, "token": t.token, "call": callID})
	}
	stages := 1
	if s.runner.toolsEnabled() {
		stages += planRounds(t.token)
	}
	if t.stage == stages {
		s.runner.metrics.turnsVerified.Add(1)
		return
	}
	if s.runner.injectedFailure(t.token) {
		// A turn cut short by the harness's own 500 is an expected outcome; it is
		// counted, not treated as an unexplained incomplete turn.
		s.runner.metrics.turnsDegraded.Add(1)
		s.runner.log.write("turn_short_after_injection", map[string]any{"client": s.id, "token": t.token, "stages": t.stage, "want": stages})
		return
	}
	s.runner.metrics.turnsUnverified.Add(1)
	s.runner.log.write("turn_incomplete", map[string]any{"client": s.id, "token": t.token, "stages": t.stage, "want": stages})
}

// replay feeds retained events through the same continuity checks as live ones.
func (s *clientState) replay(events []hostapi.SessionEvent) {
	for _, ev := range events {
		s.recordEvent(hostapi.Envelope{Event: hostapi.EventSession, Params: mustJSON(ev)})
	}
}

// verifyReconnect confirms that a session with persisted history is resumable
// and that every sequence gap seen by this client has been repaired. It runs
// after the client goroutine has stopped, so it may reconnect and replay
// directly; every slot is accounted for in the summary either way, so a silent
// skip cannot look like a passing verification.
func (s *clientState) verifyReconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == "" {
		s.runner.metrics.verificationSkips.Add(1)
		s.runner.log.write("verification_skipped", map[string]any{"client": s.id, "reason": "no session"})
		return
	}
	if s.turn == 0 {
		s.runner.metrics.verificationSkips.Add(1)
		s.runner.log.write("verification_skipped", map[string]any{"client": s.id, "reason": "no turn sent"})
		return
	}
	s.runner.metrics.verifications.Add(1)
	if s.client == nil {
		s.connect()
	}
	if s.client == nil {
		s.runner.recordProtocol("final_reconnect", errors.New("could not reconnect"))
		return
	}
	rr, err := s.client.Resume(s.sessionID)
	if err != nil {
		if !isCode(err, hostapi.CodeBusy) {
			s.runner.recordProtocol("final_resume", err)
		}
	} else {
		s.replay(rr.Events)
		if rr.EventsTruncated {
			s.runner.metrics.replayTruncated.Add(1)
		}
		// The persisted transcript is the authoritative record of the session:
		// every assistant message in it must still be a planned stage of the
		// prompt it answers, including the ones from turns this lifetime never saw.
		s.verifyTranscript(rr.Messages)
	}
	missing := 0
	for _, seqs := range s.missing {
		missing += len(seqs)
	}
	if missing != 0 {
		s.runner.metrics.missingEvents.Add(int64(missing))
		s.runner.recordProtocol("unrepaired_event_gap", fmt.Errorf("client %d has %d missing events", s.id, missing))
	}
	s.runner.log.write("verification_complete", map[string]any{"client": s.id, "session": s.sessionID, "turns": s.turn})
}

// verifyTranscript re-derives every turn's plan from the markers in the persisted
// user messages and checks the assistant messages that answer them. It is the
// end-of-run, authoritative complement to the live event checks.
func (s *clientState) verifyTranscript(msgs agentcore.MessageList) {
	var token string
	stages := 0
	dynamicIDs := make(map[string]string)
	taskCalls := make(map[string]agentcore.ToolCallContent)
	tools := s.runner.toolsEnabled()
	for _, m := range msgs {
		switch msg := m.(type) {
		case agentcore.UserMessage:
			text := agentcore.ContentToText(msg.Content)
			if next := promptToken(text); next != "" {
				token = next
				stages = 0
				dynamicIDs = make(map[string]string)
				taskCalls = make(map[string]agentcore.ToolCallContent)
			} else if msg.Origin == agentcore.UserMessageOriginSubAgentReport && token != "" {
				if taskAutoFollowUp(token) {
					if !strings.Contains(text, "task: longstay auto") || !strings.Contains(text, taskReportMarker) {
						s.runner.recordProtocol("task_followup_mismatch", fmt.Errorf("token %s: invalid automatic child report", token))
					} else {
						s.runner.metrics.taskFollowUpsVerified.Add(1)
					}
				}
			} else {
				token, stages = "", 0
				dynamicIDs = make(map[string]string)
				taskCalls = make(map[string]agentcore.ToolCallContent)
			}
		case agentcore.ToolResultMessage:
			result := agentcore.ContentToText(msg.Content)
			recordDynamicID(dynamicIDs, msg.ToolName, result)
			if call, ok := taskCalls[msg.ToolCallID]; ok && msg.ToolName == "task" {
				recordTaskDynamicID(dynamicIDs, string(call.Arguments), result)
			}
			if (msg.ToolName == "task" || msg.ToolName == "task_output") && strings.Contains(result, taskReportMarker) {
				s.verifyTaskSessionIDs(token, result)
			}
		case agentcore.CompactionMessage:
			// The checkpoint stands in for the history before it, so nothing
			// before it can answer a later turn's plan.
			token, stages = "", 0
			dynamicIDs = make(map[string]string)
			taskCalls = make(map[string]agentcore.ToolCallContent)
		case agentcore.AssistantMessage:
			if token == "" {
				// Not a harness turn (a compacted summary, or a message from a
				// session this client did not create).
				continue
			}
			if strings.HasPrefix(agentcore.ContentToText(msg.Content), "SUMMARY-MARKER") {
				continue
			}
			if msg.StopReason == agentcore.StopReasonAborted || msg.StopReason == agentcore.StopReasonError {
				continue
			}
			want := planFor(token, stages, tools)
			if got := agentcore.ContentToText(msg.Content); got != want.Text {
				s.runner.recordProtocol("transcript_text_mismatch", fmt.Errorf("token %s stage %d: got %q, want %q", token, stages, got, want.Text))
			}
			if got := agentcore.ThinkingText(msg.Content); got != want.Thinking {
				s.runner.recordProtocol("transcript_thinking_mismatch", fmt.Errorf("token %s stage %d: got %q, want %q", token, stages, got, want.Thinking))
			}
			calls := msg.ToolCalls()
			for _, call := range calls {
				taskCalls[call.ID] = call
			}
			if len(calls) != len(want.Calls) {
				s.runner.recordProtocol("transcript_calls_mismatch", fmt.Errorf("token %s stage %d: got %d calls, want %d", token, stages, len(calls), len(want.Calls)))
			} else {
				for i, call := range calls {
					expectedArgs, err := resolvePlannedArguments(want.Calls[i].Arguments, dynamicIDs)
					if err != nil {
						s.runner.recordProtocol("transcript_call_mismatch", fmt.Errorf("token %s stage %d call %d: %w", token, stages, i, err))
						continue
					}
					if call.Name != want.Calls[i].Name || string(call.Arguments) != expectedArgs {
						s.runner.recordProtocol("transcript_call_mismatch", fmt.Errorf("token %s stage %d call %d", token, stages, i))
					}
				}
			}
			stages++
		}
	}
	s.runner.metrics.transcriptsVerified.Add(1)
}

// delay chooses a duration uniformly in [min,max].
func (s *clientState) delay(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(s.rng.Int63n(int64(max-min)+1))
}

// truncate shortens s for a log line without splitting a rune.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// isCode reports whether err is a protocol error with the given code.
func isCode(err error, code int) bool {
	var protocolError *hostapi.Error
	return errors.As(err, &protocolError) && protocolError.Code == code
}

// mustJSON is used only for locally replayed values known to be serializable.
func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// workspaceFor maps a client slot onto a workspace directory. With
// -workspace-pool N, N>0, several clients share one directory, which is the
// "two sessions in one workspace" case; N=0 gives every client its own.
func (r *runner) workspaceFor(id int) string {
	index := id
	if r.cfg.WorkspacePool > 0 {
		index = (id-1)%r.cfg.WorkspacePool + 1
	}
	return filepath.Join(r.workDir, fmt.Sprintf("client-%03d", index))
}
