package tui

// Tests for /compact in the TUI: the command must run the summarizer off the tea
// loop, replace the context with the checkpoint + retained tail, report the
// compaction the way a live one is reported (card, status line, context readout),
// persist it, and refuse to run while a run is in flight — the pump goroutine owns
// the message list then, so compacting it would race the loop.

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/provider"
)

// summaryProvider is a Provider whose single streamed turn is a fixed summary,
// standing in for the summarization model behind /compact.
type summaryProvider struct{ text string }

func (p *summaryProvider) Name() string             { return "fake" }
func (p *summaryProvider) Models() []provider.Model { return nil }

func (p *summaryProvider) StreamCompletion(ctx context.Context, _ provider.CompletionRequest) (*provider.AssistantMessageEventStream, error) {
	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonEndTurn,
		Content:    agentcore.ContentList{agentcore.NewTextContent(p.text)},
	}
	s := provider.NewAssistantMessageEventStream(0)
	go func() { _ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg}); s.Close() }()
	return s, nil
}

// fatHistory builds n user messages of chars each, so the history is well past
// KeepRecentTokens (20k) and leaves a prefix worth summarizing.
func fatHistory(n, chars int) agentcore.MessageList {
	body := strings.Repeat("x", chars)
	msgs := make(agentcore.MessageList, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(body)},
		})
	}
	return msgs
}

// compactSession builds a session whose summarizer is the fake provider above,
// with a history long enough to compact.
func compactSession(t *testing.T) *runSession {
	t.Helper()
	s, _, err := newRunSessionWithStore(newTestStore(t), Options{
		Model:        "m",
		ProviderName: "fake",
		Provider:     &summaryProvider{text: "## Goal\nfolded"},
	})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	s.agentCtx.Messages = fatHistory(40, 4000)
	return s
}

// TestCompactCommandCompactsContext drives /compact end to end: the slash line
// pins the spinner and flips to running, the Cmd it returns produces a
// compactDoneMsg, and applying that message installs the card, the status line and
// the shrunk readout.
func TestCompactCommandCompactsContext(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	s := compactSession(t)
	m.session = s
	m.input.SetValue("/compact")

	next, cmd := m.submit()
	m = next.(Model)
	if !m.running {
		t.Fatal("/compact should flip to running while the summarizer works")
	}
	if m.spinner.pinned != "Compacting conversation" {
		t.Errorf("spinner label = %q, want the compaction label", m.spinner.pinned)
	}
	if len(s.agentCtx.Messages) != 40 {
		t.Errorf("the context must not change before the Cmd runs: %d messages", len(s.agentCtx.Messages))
	}
	if cmd == nil {
		t.Fatal("/compact should return the compaction Cmd")
	}

	msg := findCompactDone(t, cmd)
	if msg.err != nil {
		t.Fatalf("compaction failed: %v", msg.err)
	}

	m = apply(t, m, msg)
	if m.running {
		t.Error("a finished compaction should clear the running state")
	}
	if m.spinner.pinned != "" {
		t.Errorf("a finished compaction should unpin the spinner, label = %q", m.spinner.pinned)
	}
	// The context is now summary + tail.
	if got := s.agentCtx.Messages[0].Role(); got != agentcore.RoleCompaction {
		t.Errorf("context[0] role = %q, want %q", got, agentcore.RoleCompaction)
	}
	if len(s.agentCtx.Messages) >= 40 {
		t.Errorf("context should shrink, still %d messages", len(s.agentCtx.Messages))
	}
	if s.persisted != len(s.agentCtx.Messages) {
		t.Errorf("persisted cursor = %d, want %d (the handler persists immediately)",
			s.persisted, len(s.agentCtx.Messages))
	}
	if s.compacted {
		t.Error("the immediate persist should have consumed the compacted flag")
	}
	// Reported like a live compaction: a card carrying the checkpoint body, the
	// status line, and the meter moved.
	card := m.toolCards["compaction-1"]
	if card == nil {
		t.Fatal("a manual compaction should add a compaction card")
	}
	if got := responseText(card); !strings.Contains(got, "folded") {
		t.Errorf("card body should carry the summary, got %q", got)
	}
	if !strings.Contains(transcriptText(m.transcript), "context compacted") {
		t.Errorf("transcript should report the compaction:\n%s", transcriptText(m.transcript))
	}
	if m.ctxMeter.compactions != 1 {
		t.Errorf("compactions = %d, want 1", m.ctxMeter.compactions)
	}
	// And persisted: the session file is what a resume replays.
	if _, msgs, err := s.store.Load(s.header.ID); err != nil {
		t.Fatalf("Load: %v", err)
	} else if len(msgs) == 0 || msgs[0].Role() != agentcore.RoleCompaction {
		t.Errorf("persisted session should start at the checkpoint, got %d messages", len(msgs))
	}
}

// TestCompactCommandRefusedDuringRun verifies a mid-run /compact is refused with a
// note instead of compacting a message list the pump goroutine owns. Reaching it
// needs a drawer click or a remote-control line (the composer is blurred while
// running), so the guard cannot be assumed away.
func TestCompactCommandRefusedDuringRun(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	s := compactSession(t)
	m.session = s
	m.running = true
	before := len(s.agentCtx.Messages)

	next, cmd := m.runSlash("/compact")
	m = next.(Model)
	if cmd != nil {
		t.Error("a refused /compact must not start a compaction Cmd")
	}
	if len(s.agentCtx.Messages) != before {
		t.Error("a refused /compact must leave the context alone")
	}
	if !strings.Contains(transcriptText(m.transcript), "unavailable while a run is in progress") {
		t.Errorf("transcript should explain the refusal:\n%s", transcriptText(m.transcript))
	}
}

// TestCompactCommandNoOpReported verifies a history with nothing to fold says so
// and leaves the context untouched, instead of claiming a compaction happened.
func TestCompactCommandNoOpReported(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	s, _, err := newRunSessionWithStore(newTestStore(t), Options{Model: "m", ProviderName: "fake"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	s.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
	}
	m.session = s

	m = apply(t, m, compactDoneMsg{
		summary: "nothing to compact (12 tokens, 1 messages)",
		ev:      agentcore.CompactionEvent{Reason: "manual", TokensBefore: 12, TokensAfter: 12},
	})
	if len(m.toolCards) != 0 {
		t.Errorf("a no-op compaction should not add a card, got %d", len(m.toolCards))
	}
	if !strings.Contains(transcriptText(m.transcript), "nothing to compact") {
		t.Errorf("no-op compaction should report itself:\n%s", transcriptText(m.transcript))
	}

	m = apply(t, m, compactDoneMsg{err: errCompact("summarizer down")})
	if !strings.Contains(transcriptText(m.transcript), "compaction failed: summarizer down") {
		t.Errorf("a failed compaction should report the error:\n%s", transcriptText(m.transcript))
	}
	if !strings.Contains(transcriptText(m.transcript), "context left unchanged") {
		t.Errorf("a failed compaction should say the context is intact:\n%s", transcriptText(m.transcript))
	}
}

// TestContextDrawerCompactAction verifies the context drawer offers the action and
// that clicking it goes through the command (_not_ a second implementation of
// compacting), which is what keeps the click and "/compact" from drifting.
func TestContextDrawerCompactAction(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	m.statusPanel.toggle(sectionContext)

	rows := m.contextDrawerRows()
	var row *statusPanelRow
	for i := range rows {
		if rows[i].action == rowCompactNow {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("context drawer offers no compact action: %+v", rows)
	}
	if row.label != "compact" {
		t.Errorf("action row label = %q, want compact", row.label)
	}

	next, _ := m.activateStatusRow(*row)
	m = next.(Model)
	if m.statusPanel.open() {
		t.Error("clicking the action should close the drawer")
	}
	// No session in this model, so the command reports itself unavailable — proof
	// that the click reaches /compact rather than a private code path.
	if !strings.Contains(transcriptText(m.transcript), "compact unavailable") {
		t.Errorf("the click should run /compact:\n%s", transcriptText(m.transcript))
	}
}

// findCompactDone runs cmd (a batch of the compaction plus the spinner tick) and
// returns the compactDoneMsg it produces, ignoring the tick.
func findCompactDone(t *testing.T, cmd tea.Cmd) compactDoneMsg {
	t.Helper()
	return drainForCompactDone(t, cmd())
}

func drainForCompactDone(t *testing.T, msg tea.Msg) compactDoneMsg {
	t.Helper()
	switch v := msg.(type) {
	case compactDoneMsg:
		return v
	case tea.BatchMsg:
		for _, c := range v {
			if c == nil {
				continue
			}
			if got, ok := tryCompactDone(c()); ok {
				return got
			}
		}
	}
	t.Fatalf("no compactDoneMsg produced, last msg %T", msg)
	return compactDoneMsg{}
}

// tryCompactDone reports whether msg is (or carries) a compactDoneMsg.
func tryCompactDone(msg tea.Msg) (compactDoneMsg, bool) {
	switch v := msg.(type) {
	case compactDoneMsg:
		return v, true
	case tea.BatchMsg:
		for _, c := range v {
			if c == nil {
				continue
			}
			if got, ok := tryCompactDone(c()); ok {
				return got, true
			}
		}
	}
	return compactDoneMsg{}, false
}

// errCompact is a tiny error type for building a failed compactDoneMsg.
type errCompact string

func (e errCompact) Error() string { return string(e) }

// TestCompactCommandRejectsArgs verifies "/compact foo" says so instead of either
// ignoring the argument or silently compacting.
func TestCompactCommandRejectsArgs(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	s := compactSession(t)
	m.session = s
	before := len(s.agentCtx.Messages)

	next, cmd := m.runSlash("/compact keep")
	m = next.(Model)
	if cmd != nil {
		t.Error("an argument-taking /compact must not start a compaction")
	}
	if len(s.agentCtx.Messages) != before {
		t.Error("the context must be untouched")
	}
	if !strings.Contains(transcriptText(m.transcript), "usage: /compact") {
		t.Errorf("transcript should show the usage line:\n%s", transcriptText(m.transcript))
	}
}
