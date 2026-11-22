package tui

// Tests for the sticky compacting state (compactstate.go): once a compaction
// starts, a failed or aborted attempt does not end it — the session may only
// retry, run a read-only command, or exit. The composer must not accept a prompt
// (the context cannot fit one), the retry key must start a fresh attempt that
// discards the previous partial output, and the escape hatches must stay open so
// a summarizer that is down for good cannot pin the program.

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/provider"
)

// compactingModel builds a sized model stuck in the compacting state after one
// failed attempt, which is the state the operator actually sits in.
func compactingModel(t *testing.T) (Model, *runSession) {
	t.Helper()
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	s := compactSession(t)
	m = m.withSession(s, nil)
	m.beginCompact()
	m = apply(t, m, compactDoneMsg{err: errCompact("summarizer down")})
	if !m.compacting.active {
		t.Fatal("the failed attempt should leave the compacting state on")
	}
	return m, s
}

// typeKey builds a printable key press carrying its text, which is how the
// terminal delivers ordinary typing.
func typeKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// TestCompactingStateBlocksPrompts pins the core rule: while the session is
// compacting, an ordinary message cannot even be composed. The keystroke is
// dropped at the source, so there is no buffer to submit and nothing can reach a
// context that is still over budget.
func TestCompactingStateBlocksPrompts(t *testing.T) {
	m, _ := compactingModel(t)

	m = apply(t, m, typeKey('h'))
	m = apply(t, m, typeKey('i'))
	if got := m.input.Value(); got != "" {
		t.Errorf("composer took a prompt while compacting: %q", got)
	}
	// Enter on an empty composer is the retry key, not a submit: with no typed
	// text it must start an attempt rather than send an empty message.
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if !m.running {
		t.Error("Enter on the waiting screen should retry the compaction")
	}
	if m.compacting.attempt != 2 {
		t.Errorf("attempt = %d, want 2 after a retry", m.compacting.attempt)
	}
	if cmd == nil {
		t.Error("the retry should return the compaction Cmd")
	}
}

// TestCompactingStateAllowsReadOnlyCommands pins the allowlist: a read-only
// command still works while waiting (the operator can inspect the session they
// are stuck in), while anything that replaces the session or sends a prompt is
// refused.
func TestCompactingStateAllowsReadOnlyCommands(t *testing.T) {
	m, _ := compactingModel(t)

	// "/" opens a command even though arbitrary typing is refused.
	m = apply(t, m, typeKey('/'))
	for _, r := range "session" {
		m = apply(t, m, typeKey(r))
	}
	if got := m.input.Value(); got != "/session" {
		t.Fatalf("a command should be composable while compacting, composer = %q", got)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if !strings.Contains(transcriptText(m.transcript), "messages:") {
		t.Errorf("/session should have run:\n%s", transcriptText(m.transcript))
	}
	if !m.compacting.active {
		t.Error("a read-only command must not end the compacting state")
	}

	// /new replaces the session, which is exactly what must not happen here.
	m = m.withInputValue("/new")
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		t.Error("a session-replacing command must not run while compacting")
	}
	if !strings.Contains(transcriptText(m.transcript), "only /status") {
		t.Errorf("the refusal should list what is allowed:\n%s", transcriptText(m.transcript))
	}
	if !m.compacting.active {
		t.Error("a refused command must not end the compacting state")
	}
}

// TestCompactingStateRefusesPromptSubmission covers the path that does not come
// from typing: a drawer click or a remote-control line can put text in the
// composer. A prompt smuggled that way is still refused, with a note saying why.
func TestCompactingStateRefusesPromptSubmission(t *testing.T) {
	m, s := compactingModel(t)
	before := len(s.agentCtx.Messages)

	m = m.withInputValue("please keep going")
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		t.Error("a prompt must not start a run while compacting")
	}
	if len(s.agentCtx.Messages) != before {
		t.Error("a refused prompt must not touch the context")
	}
	if !strings.Contains(transcriptText(m.transcript), "a message cannot be sent") {
		t.Errorf("the refusal should explain itself:\n%s", transcriptText(m.transcript))
	}
}

// TestCompactingStateRetryDiscardsFailedAttempt pins "start over, do not
// continue": a retry after a failure begins from attempt N+1 with the failure
// cleared, and the summarizer runs again over the whole history — the partial
// output of the failed attempt is gone, not appended to.
func TestCompactingStateRetryDiscardsFailedAttempt(t *testing.T) {
	m, s := compactingModel(t)
	waiting := m.compacting
	if waiting.lastErr == "" {
		t.Fatal("the waiting state should carry the failure")
	}

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if m.compacting.lastErr != "" {
		t.Errorf("a retry should clear the previous failure, got %q", m.compacting.lastErr)
	}
	if m.compacting.attempt != waiting.attempt+1 {
		t.Errorf("attempt = %d, want %d", m.compacting.attempt, waiting.attempt+1)
	}
	// The retry runs against the untouched history: a failed attempt installs
	// nothing, so the new attempt summarizes the same messages from scratch.
	if len(s.agentCtx.Messages) != 40 {
		t.Errorf("the failed attempt left %d messages, want the original 40", len(s.agentCtx.Messages))
	}
	msg := findCompactDone(t, cmd)
	if msg.err != nil {
		t.Fatalf("the retry should succeed: %v", msg.err)
	}
	m = apply(t, m, msg)
	if m.compacting.active {
		t.Error("a successful retry should end the compacting state")
	}
	if got := s.agentCtx.Messages[0].Role(); got != agentcore.RoleCompaction {
		t.Errorf("context[0] role = %q, want the checkpoint", got)
	}
}

// TestCompactingStateNoOpAlsoEndsIt verifies that "nothing to fold" is a
// resolution, not a failure: the state exists to get the context under budget,
// and a cut point with no prefix means there is nothing left to do. Leaving the
// operator stuck there would be a trap with no way out but exit.
func TestCompactingStateNoOpAlsoEndsIt(t *testing.T) {
	m, s := compactingModel(t)
	s.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
	}
	m = apply(t, m, compactDoneMsg{
		summary: "nothing to compact (12 tokens, 1 messages)",
		ev:      agentcore.CompactionEvent{Reason: "manual", TokensBefore: 12, TokensAfter: 12},
		noop:    true,
	})
	if m.compacting.active {
		t.Error("a no-op compaction resolves the state")
	}
}

// TestCompactingStateInterruptIsTwoStage pins the escape hatch: with an attempt
// in flight, Ctrl+C aborts that attempt and stays (the context is still over
// budget); with nothing in flight the same key quits. Being stuck in a failing
// compaction must never require killing the terminal.
func TestCompactingStateInterruptIsTwoStage(t *testing.T) {
	m, _ := compactingModel(t)
	interrupted := 0
	m.interruptFn = func() { interrupted++ }

	// Attempt in flight: the press aborts the attempt, not the program.
	m.running = true
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if cmd != nil {
		t.Error("aborting an attempt must not quit the program")
	}
	if interrupted != 1 {
		t.Errorf("interruptFn called %d times, want 1", interrupted)
	}
	if !m.compacting.active {
		t.Error("aborting an attempt is not the same as being done compacting")
	}

	// Nothing in flight: the same key is the way out.
	m.running = false
	next, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("Esc on the waiting screen should quit")
	}
	if !m.quitting {
		t.Error("Esc with nothing in flight should leave the program")
	}
}

// TestCompactingStateCtrlDStillExits verifies the hard exit stays hard even
// mid-attempt: a summarizer that hangs must not be able to hold the terminal.
func TestCompactingStateCtrlDStillExits(t *testing.T) {
	m, _ := compactingModel(t)
	m.running = true

	next, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	m = next.(Model)
	if cmd == nil || !m.quitting {
		t.Error("Ctrl+D must quit even while a compaction attempt is in flight")
	}
}

// TestCompactingStateSlashCompactRetries checks the command spelling of the
// retry key: reaching for /compact again while stuck means "try again", not
// "start a second compaction on top of the first".
func TestCompactingStateSlashCompactRetries(t *testing.T) {
	m, _ := compactingModel(t)
	m = m.withInputValue("/compact")

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(Model)
	if !m.running || cmd == nil {
		t.Fatal("/compact while stuck should retry the compaction")
	}
	if m.compacting.attempt != 2 {
		t.Errorf("attempt = %d, want 2", m.compacting.attempt)
	}
}

// TestCompactingWaitingRowIsVisible pins the UI half of "sticky": between
// attempts the spinner is parked, so without this row the state would be
// invisible and the composer would appear to silently swallow input.
func TestCompactingWaitingRowIsVisible(t *testing.T) {
	m, _ := compactingModel(t)
	// The row wraps at the narrow test width, so compare on whitespace-collapsed
	// text rather than on the rendered line breaks.
	view := strings.Join(strings.Fields(stripANSI(m.View().Content)), " ")
	for _, want := range []string{
		"compaction failed (attempt 1)",
		"summarizer down",
		"context left unchanged",
		"Enter retries",
		"Esc quits",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("waiting screen should show %q:\n%s", want, view)
		}
	}
	// The row is reserved, so the layout does not shift between a failed attempt
	// and a running one.
	withRow := m.transcript.vp.Height()
	m.running = true
	m.relayout()
	if got := m.transcript.vp.Height(); got != withRow {
		t.Errorf("the waiting row and the spinner should occupy the same row: %d vs %d",
			withRow, got)
	}
}

// TestCompactingCommandAllowlist pins the allowlist itself, including the
// in-flight split: commands that read the message list are only safe between
// attempts, because an attempt owns it while running.
func TestCompactingCommandAllowlist(t *testing.T) {
	cases := []struct {
		line      string
		inFlight  bool
		wantAllow bool
	}{
		{"/exit", true, true},
		{"/quit", false, true},
		{"/help", true, true},
		{"/status", false, true},
		{"/status", true, false},
		{"/session", false, true},
		{"/memory", true, false},
		{"/perf reset", false, true},
		{"/new", false, false},
		{"/resume", false, false},
		{"/rebuild", false, false},
		{"/model x", false, false},
		{"hello", false, false},
	}
	for _, c := range cases {
		if got := compactingCommandAllows(c.line, c.inFlight); got != c.wantAllow {
			t.Errorf("compactingCommandAllows(%q, inFlight=%v) = %v, want %v",
				c.line, c.inFlight, got, c.wantAllow)
		}
	}
}

// TestCompactingAttemptIsCancellable verifies the session records a cancel func
// for the in-flight manual attempt, so the interrupt above actually has
// something to cancel instead of a request that keeps running unseen.
func TestCompactingAttemptIsCancellable(t *testing.T) {
	s := compactSession(t)
	// A summarizer that blocks until its context is cancelled, so the attempt is
	// genuinely in flight when the interrupt lands.
	started := make(chan struct{})
	s.live.Provider = blockingProvider{started: started}
	cmd := s.compactCmd()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the summarizer never started")
	}
	if s.cancelCompact == nil {
		t.Fatal("an in-flight attempt should record a cancel func")
	}
	s.interrupt()

	select {
	case msg := <-done:
		if got, ok := msg.(compactDoneMsg); !ok || got.err == nil {
			t.Errorf("an aborted attempt should report an error, got %#v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupting the attempt should end it")
	}
}

// blockingProvider is a Provider whose stream blocks until the run's context is
// cancelled, so a test can observe an attempt that is genuinely in flight.
type blockingProvider struct{ started chan struct{} }

func (p blockingProvider) Name() string             { return "fake" }
func (p blockingProvider) Models() []provider.Model { return nil }

func (p blockingProvider) StreamCompletion(ctx context.Context, _ provider.CompletionRequest) (*provider.AssistantMessageEventStream, error) {
	close(p.started)
	s := provider.NewAssistantMessageEventStream(0)
	go func() {
		<-ctx.Done()
		_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			StopReason: agentcore.StopReasonAborted,
		}})
		s.Close()
	}()
	return s, nil
}

// withInputValue sets the composer's text the way a drawer click or a remote line
// does, bypassing the keystroke gate.
func (m Model) withInputValue(v string) Model {
	m.input.SetValue(v)
	return m
}
