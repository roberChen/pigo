package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
)

// This file tests the live context meter and the permanent context segment
// (#386, US-003): the readout must exist from the first frame and must move as
// the conversation grows, not only when a run ends.

func usageAssistant(text string, in, out int) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		Usage:      &agentcore.Usage{InputTokens: in, OutputTokens: out},
		StopReason: agentcore.StopReasonEndTurn,
	}
}

// TestContextMeterPrefersUsage checks the provider-reported usage wins over the
// character estimate, matching compaction.EstimateContextTokens.
func TestContextMeterPrefersUsage(t *testing.T) {
	var m contextMeter
	m.reset(200_000)
	m.addUserPrompt("hello there") // estimated before any usage exists
	m.observeTurn(usageAssistant(strings.Repeat("x", 4000), 10_000, 500), nil)

	if got, want := m.tokens(), 10_500; got != want {
		t.Fatalf("tokens = %d, want %d (provider usage)", got, want)
	}
	v := m.view()
	if !v.known || v.window != 200_000 {
		t.Fatalf("view = %+v, want a known measurement at the 200k window", v)
	}
	if v.tokens != 10_500 {
		t.Errorf("view tokens = %d, want 10500", v.tokens)
	}
	if got, want := int(v.util*100+0.5), 5; got != want {
		t.Errorf("utilization = %d%%, want %d%%", got, want)
	}
}

// TestContextMeterEstimatesWithoutUsage checks a usage-less turn is estimated
// and appended to the trailing total rather than discarded.
func TestContextMeterEstimatesWithoutUsage(t *testing.T) {
	var m contextMeter
	m.reset(100_000)
	m.addUserPrompt("abcdefgh") // 8 chars -> 2 tokens
	m.observeTurn(agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content:   agentcore.ContentList{agentcore.NewTextContent("abcdefgh")}, // 2 tokens
	}, nil)

	if got, want := m.tokens(), 4; got != want {
		t.Fatalf("tokens = %d, want %d (prompt + estimated turn)", got, want)
	}
}

// TestContextMeterCountsToolResults verifies tool results produced by a turn are
// counted after the assistant message that requested them.
func TestContextMeterCountsToolResults(t *testing.T) {
	var m contextMeter
	m.reset(100_000)
	m.observeTurn(agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content:   agentcore.ContentList{agentcore.NewTextContent("abcdefgh")},
	}, []agentcore.ToolResultMessage{{
		RoleField: agentcore.RoleToolResult,
		Content:   agentcore.ContentList{agentcore.NewTextContent(strings.Repeat("y", 40))}, // 10 tokens
	}})

	if got, want := m.tokens(), 12; got != want {
		t.Fatalf("tokens = %d, want %d (assistant + tool result)", got, want)
	}
}

// TestContextMeterStreamsBetweenTurns verifies streamed text grows the readout
// before the turn settles, and that settling the turn does not double count it.
func TestContextMeterStreamsBetweenTurns(t *testing.T) {
	var m contextMeter
	m.reset(100_000)
	m.observeTurn(usageAssistant("", 1_000, 100), nil)
	base := m.tokens()

	m.addStream(strings.Repeat("z", 400)) // 100 tokens of in-flight text
	if got := m.tokens(); got != base+100 {
		t.Fatalf("streaming tokens = %d, want %d", got, base+100)
	}

	// The settled turn carries authoritative usage, so the stream estimate is
	// replaced rather than added again.
	m.observeTurn(usageAssistant(strings.Repeat("z", 400), 1_100, 100), nil)
	if got, want := m.tokens(), 1_200; got != want {
		t.Fatalf("after settle tokens = %d, want %d (usage only)", got, want)
	}
	if m.streamBytes != 0 {
		t.Errorf("streamBytes = %d, want reset to 0", m.streamBytes)
	}
}

// TestContextMeterCompactionAndTelemetry verifies a compaction collapses the
// count and a telemetry summary adopts the loop's authoritative numbers.
func TestContextMeterCompactionAndTelemetry(t *testing.T) {
	var m contextMeter
	m.reset(200_000)
	m.observeTurn(usageAssistant("", 90_000, 1_000), nil)

	m.observeCompaction(12_000)
	if got, want := m.tokens(), 12_000; got != want {
		t.Fatalf("after compaction tokens = %d, want %d", got, want)
	}

	m.observeTelemetry(agentcore.TelemetryEvent{ContextTokens: 15_000, ContextWindow: 128_000, ContextUtilization: 15_000.0 / 128_000})
	if got, want := m.tokens(), 15_000; got != want {
		t.Fatalf("after telemetry tokens = %d, want %d", got, want)
	}
	if m.view().window != 128_000 {
		t.Errorf("window = %d, want the telemetry-reported 128000", m.view().window)
	}
}

// TestContextMeterUnknownIsNotZero checks an unknown meter reports known=false so
// the bar renders its placeholder rather than a misleading 0%.
func TestContextMeterUnknownIsNotZero(t *testing.T) {
	var m contextMeter
	m.reset(128_000)
	v := m.view()
	if v.known {
		t.Errorf("fresh meter should be unknown, got %+v", v)
	}
	if v.tokens != 0 {
		t.Errorf("fresh meter tokens = %d, want 0", v.tokens)
	}
}

// TestStatusBarUpdatesOnTurnEnd drives a session-bound model and asserts the
// status bar's context segment tracks turns and streamed text rather than only
// the end-of-run telemetry.
func TestStatusBarUpdatesOnTurnEnd(t *testing.T) {
	store := newTestStore(t)
	s, _, err := newRunSessionWithStore(store, Options{Model: "meter-model", ProviderName: "meter-provider"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	m := NewModel(Options{}).withSession(s, nil)
	m = apply(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})

	// Before any turn the permanent segment shows its placeholder.
	bar := stripANSI(m.statusBar.Render(200))
	if !strings.Contains(bar, glyphCtx) {
		t.Fatalf("context segment missing from a fresh bar: %q", bar)
	}

	// Streaming text moves the readout immediately.
	m.running = true
	m.runCh = make(chan tea.Msg)
	m = apply(t, m, textDeltaMsg{delta: strings.Repeat("a", 400)})
	if bar := stripANSI(m.statusBar.Render(200)); !strings.Contains(bar, glyphCtx) {
		t.Fatalf("context segment vanished while streaming: %q", bar)
	}

	// A settled turn with provider usage sets an authoritative count.
	m = apply(t, m, turnEndMsg{msg: usageAssistant("done", 40_000, 1_000)})
	got := m.statusBar.tokens
	if got != 41_000 {
		t.Fatalf("status bar tokens = %d, want 41000 from the turn's usage", got)
	}
	if got, want := m.statusBar.contextPct, 32; got != want { // 41000 / 128000
		t.Errorf("contextPct = %d, want %d", got, want)
	}

	// Compaction shrinks it without waiting for the run-end telemetry.
	m = apply(t, m, compactionMsg{tokensAfter: 5_000})
	if got := m.statusBar.tokens; got != 5_000 {
		t.Errorf("after compaction tokens = %d, want 5000", got)
	}
}

// TestStatusBarContextSurvivesModelSwitch verifies a /model switch keeps the
// measured tokens while adopting the (default) window budget.
func TestStatusBarContextSurvivesModelSwitch(t *testing.T) {
	m := NewModel(Options{})
	m.live.ContextWindow = cli.DefaultContextWindow
	m.ctxMeter.reset(cli.DefaultContextWindow)
	m.ctxMeter.addUserPrompt(strings.Repeat("q", 4000)) // 1000 tokens
	m = apply(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	m.syncContextWindow()

	if got := m.statusBar.tokens; got != 1000 {
		t.Fatalf("tokens after window sync = %d, want 1000", got)
	}
	if m.statusBar.contextPct < 0 {
		t.Error("context percentage should stay measured across a window sync")
	}
}

// TestCompactionMsgCarriesTokensAfter checks the bridge forwards the loop's
// post-compaction token count into compactionMsg.
func TestCompactionMsgCarriesTokensAfter(t *testing.T) {
	ch := newEventChan()
	h := newStreamHandler(ch, nil)
	h.OnEvent(agentcore.CompactionEvent{Reason: "threshold", TokensBefore: 90_000, TokensAfter: 12_000})

	msg, ok := (<-ch).(compactionMsg)
	if !ok {
		t.Fatalf("got %T, want compactionMsg", msg)
	}
	if msg.tokensAfter != 12_000 {
		t.Errorf("tokensAfter = %d, want 12000", msg.tokensAfter)
	}
}

// TestSeedContextMeterFromResumedHistory verifies binding a session with prior
// history seeds the readout immediately, so a resumed conversation shows its
// usage without waiting for a new turn.
func TestSeedContextMeterFromResumedHistory(t *testing.T) {
	store := newTestStore(t)
	s, _, err := newRunSessionWithStore(store, Options{Model: "m", ProviderName: "p"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	s.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("q1")}},
		usageAssistant("a1", 20_000, 500),
	}

	m := NewModel(Options{}).withSession(s, nil)
	if got, want := m.statusBar.tokens, 20_500; got != want {
		t.Fatalf("seeded tokens = %d, want %d from the resumed history", got, want)
	}
	if m.statusBar.contextPct < 0 {
		t.Error("a resumed conversation should render a measured percentage")
	}
}
