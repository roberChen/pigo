package tui

import (
	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/compaction"
)

// This file implements the live context-usage meter behind the status bar's
// context segment (#386, US-003). The bar originally learned its numbers only
// from the end-of-run TelemetryEvent, so the segment appeared late and then
// stayed frozen until the next run ended. The meter derives the same figure the
// loop's compaction accounting uses (internal/compaction.EstimateContextTokens)
// but incrementally, from the messages the event bridge already delivers plus
// the deltas streaming through it, so the segment tracks the conversation while
// it is still being written.
//
// It deliberately does NOT read the session's AgentContext during a run: the
// pump goroutine owns agentCtx.Messages from the moment a run starts until
// DrainStream returns, so reading it here (on the tea goroutine) would be a data
// race. Bridged values are copies, which makes every update race-free by
// construction; only the idle windows (session load, resume, run end, rebuild)
// seed from the message list directly.

// contextMeter accumulates context-token usage the way
// compaction.EstimateContextTokens does: the last provider-reported usage block
// wins, and only the messages after it are estimated.
type contextMeter struct {
	// window is the model's context-token budget; 0 means unknown.
	window int

	// usageTokens is the provider-reported input+output total of the most recent
	// usable assistant message (0 when the run has not reported any yet).
	usageTokens int
	// trailing is the estimated token count of everything appended after that
	// usage block (user prompts, tool results).
	trailing int
	// streamBytes counts the assistant text streamed since the last turn settled,
	// so the readout grows while the model is still generating. It is folded into
	// the assistant message at turn end and reset.
	streamBytes int
	// measured reports whether any count is known; before the first measurement
	// the bar shows its placeholder instead of a hard 0%.
	measured bool
}

// reset clears the meter for a new conversation with the given context window.
func (m *contextMeter) reset(window int) {
	*m = contextMeter{window: window}
}

// setWindow updates the model budget without disturbing the token counts (a
// model switch can change the window while the conversation stays the same).
func (m *contextMeter) setWindow(window int) {
	if window > 0 {
		m.window = window
	}
}

// seed recomputes the meter from a message list. It must only be called while no
// run is in flight (the run goroutine owns the list otherwise).
func (m *contextMeter) seed(msgs []agentcore.Message, window int) {
	if window > 0 {
		m.window = window
	}
	est := compaction.EstimateContextTokens(msgs)
	m.usageTokens = est.UsageTokens
	m.trailing = est.TrailingTokens
	m.streamBytes = 0
	m.measured = true
}

// addUserPrompt accounts for a submitted prompt before the run starts, so the
// bar moves the moment the user sends a message rather than only at the end of
// the run.
func (m *contextMeter) addUserPrompt(prompt string) {
	m.trailing += compaction.EstimateTokens(agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
	})
	m.measured = true
}

// addStream accounts for streamed assistant text, giving the readout a live
// estimate between provider usage reports.
func (m *contextMeter) addStream(delta string) {
	if delta == "" {
		return
	}
	m.streamBytes += len(delta)
	m.measured = true
}

// observeTurn folds one finished turn into the meter. A usable usage block
// replaces the running total (the provider's number is authoritative for the
// whole request); otherwise the assistant message is estimated and added. Tool
// results produced by the turn are estimated and appended after it.
func (m *contextMeter) observeTurn(msg agentcore.AssistantMessage, results []agentcore.ToolResultMessage) {
	m.streamBytes = 0
	// Reuse the compaction accounting on the single message: it decides whether
	// the usage block is usable (skipping aborted/error/zero-token responses).
	est := compaction.EstimateContextTokens([]agentcore.Message{msg})
	if est.LastUsageIndex >= 0 {
		m.usageTokens = est.UsageTokens
		m.trailing = 0
	} else {
		m.trailing += est.TrailingTokens
	}
	for _, r := range results {
		m.trailing += compaction.EstimateTokens(r)
	}
	m.measured = true
}

// observeCompaction collapses the count to the post-compaction size. after <= 0
// (an unknown result) leaves the meter untouched.
func (m *contextMeter) observeCompaction(after int) {
	if after <= 0 {
		return
	}
	m.usageTokens = after
	m.trailing = 0
	m.streamBytes = 0
	m.measured = true
}

// observeTelemetry adopts the run-end telemetry summary, which is authoritative:
// the loop computed it from the settled context after any compaction.
func (m *contextMeter) observeTelemetry(ev agentcore.TelemetryEvent) {
	if ev.ContextWindow > 0 {
		m.window = ev.ContextWindow
	}
	if ev.ContextTokens > 0 {
		m.usageTokens = ev.ContextTokens
		m.trailing = 0
		m.streamBytes = 0
		m.measured = true
	}
}

// tokens returns the meter's current context-token estimate.
func (m *contextMeter) tokens() int {
	return m.usageTokens + m.trailing + ceilDiv(m.streamBytes, charsPerToken)
}

// view projects the meter into the status bar's readout shape.
func (m *contextMeter) view() telemetryEventView {
	v := telemetryEventView{window: m.window, known: m.measured}
	if !m.measured {
		return v
	}
	v.tokens = m.tokens()
	if m.window > 0 {
		v.util = float64(v.tokens) / float64(m.window)
	}
	return v
}

// charsPerToken mirrors compaction's conservative characters-per-token divisor
// (the accounting package keeps its own copy unexported).
const charsPerToken = 4

// seedContextMeter recomputes the status bar's context readout from the live
// conversation and renders it. It must only run while no run is in flight (the
// pump goroutine owns the message list during a run): it is called on session
// bind, on /new and /resume, and after a manual /rebuild, all idle windows.
func (m *Model) seedContextMeter() {
	window := cli.DefaultContextWindow
	if m.session != nil && m.session.live != nil && m.session.live.ContextWindow > 0 {
		window = m.session.live.ContextWindow
	} else if m.live != nil && m.live.ContextWindow > 0 {
		window = m.live.ContextWindow
	}
	m.ctxMeter.reset(window)
	if m.session != nil {
		m.ctxMeter.seed(m.session.agentCtx.Messages, window)
	}
	m.statusBar.SetContext(m.ctxMeter.view())
}

// syncContextWindow adopts a changed context-window budget (a /model switch can
// point at a model with a different window) without disturbing the token counts,
// then refreshes the readout.
func (m *Model) syncContextWindow() {
	if m.live == nil {
		return
	}
	m.ctxMeter.setWindow(m.live.ContextWindow)
	m.statusBar.SetContext(m.ctxMeter.view())
}

// ceilDiv returns ceil(a/b) for non-negative a and positive b.
func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
