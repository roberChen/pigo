package tui

// Tests for the TUI side of the question tool: the prompt the operator sees, the
// answer paths (numbered option, typed text, cancel), and the asker that bridges
// the run goroutine to the message loop.

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
)

// askTestModel builds a sized model and puts it in the running state a real run
// would have while a question is pending.
func askTestModel(t *testing.T) Model {
	t.Helper()
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 20})
	m.running = true
	m.spinner.begin(time.Now(), "")
	m.relayout()
	return m
}

// typeText feeds one key press per rune into the model, the way a user types.
func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m = apply(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

// TestAskPromptShowsQuestionAndAnswersWithOption verifies the prompt renders the
// question with its suggested answers and that a single digit answers it, handing
// the chosen option back to the blocked tool.
func TestAskPromptShowsQuestionAndAnswersWithOption(t *testing.T) {
	m := askTestModel(t)
	reply := make(chan string, 1)
	m = apply(t, m, questionAskedMsg{
		question: "Deploy now?",
		options:  []string{"yes", "no"},
		reply:    reply,
	})
	if m.ask == nil {
		t.Fatal("questionAskedMsg should open the answer prompt")
	}

	content := stripANSI(m.View().Content)
	for _, want := range []string{"Deploy now?", "1) yes", "2) no"} {
		if !strings.Contains(content, want) {
			t.Fatalf("prompt missing %q:\n%s", want, content)
		}
	}

	m = apply(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	select {
	case got := <-reply:
		if got != "no" {
			t.Errorf("answer = %q, want %q", got, "no")
		}
	default:
		t.Fatal("the chosen option was not sent back to the question tool")
	}
	if m.ask != nil {
		t.Error("the prompt should close once answered")
	}
}

// TestAskPromptAcceptsTypedAnswer verifies free-text answering: keys edit the
// composer and Enter submits the buffer as the answer.
func TestAskPromptAcceptsTypedAnswer(t *testing.T) {
	m := askTestModel(t)
	reply := make(chan string, 1)
	m = apply(t, m, questionAskedMsg{question: "Which branch?", reply: reply})

	m = typeText(t, m, "release/2.0")
	if got := m.input.Value(); got != "release/2.0" {
		t.Fatalf("composer = %q, want the typed answer", got)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case got := <-reply:
		if got != "release/2.0" {
			t.Errorf("answer = %q, want %q", got, "release/2.0")
		}
	default:
		t.Fatal("Enter did not send the typed answer")
	}
	if m.input.Value() != "" {
		t.Errorf("composer should clear after answering, got %q", m.input.Value())
	}
}

// TestAskPromptCancelInterruptsRun verifies Esc abandons the prompt and interrupts
// the run, which unblocks the question tool with a cancellation error.
func TestAskPromptCancelInterruptsRun(t *testing.T) {
	m := askTestModel(t)
	interrupted := false
	m.interruptFn = func() { interrupted = true }
	m = apply(t, m, questionAskedMsg{question: "Delete the branch?", reply: make(chan string, 1)})

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.ask != nil {
		t.Error("Esc should dismiss the prompt")
	}
	if !interrupted {
		t.Error("Esc while a question is pending should interrupt the run")
	}
}

// TestAskPromptClearedOnRunEnd verifies a run that ends (canceled elsewhere, or
// errored) never leaves a dangling prompt over the composer.
func TestAskPromptClearedOnRunEnd(t *testing.T) {
	m := askTestModel(t)
	m = apply(t, m, questionAskedMsg{question: "Still there?", reply: make(chan string, 1)})
	m = apply(t, m, runEndMsg{})
	if m.ask != nil {
		t.Error("run end should clear the pending question")
	}
}

// TestAskPromptReservesRows verifies the prompt takes its rows out of the
// transcript so the shell keeps exactly Height rows while it is up.
func TestAskPromptReservesRows(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 20})
	base := m.transcript.viewportHeight()

	m.ask = &askPrompt{question: "Deploy now?", options: []string{"yes", "no"}, reply: make(chan string, 1)}
	m.relayout()
	if got := m.transcript.viewportHeight(); got != base-2 {
		t.Errorf("transcript height with the prompt up = %d, want %d (base %d - 2 rows)", got, base-2, base)
	}
	if got := strings.Count(m.View().Content, "\n"); got != 19 {
		t.Errorf("newline count = %d, want 19 (20 rows)", got)
	}
}

// TestAskViaChannelRoundTrip verifies the bridge asker: the question arrives on the
// run channel, the answer flows back, and a canceled run unblocks it instead of
// waiting forever.
func TestAskViaChannelRoundTrip(t *testing.T) {
	ch := newEventChan()
	ask := askViaChannel(ch)

	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := ask(context.Background(), agentcore.AskRequest{Question: "Which one?"})
		done <- result{answer: answer, err: err}
	}()

	msg, ok := (<-ch).(questionAskedMsg)
	if !ok {
		t.Fatalf("expected a questionAskedMsg on the run channel")
	}
	if msg.question != "Which one?" {
		t.Errorf("question = %q, want %q", msg.question, "Which one?")
	}
	msg.reply <- "that one"

	select {
	case res := <-done:
		if res.err != nil || res.answer != "that one" {
			t.Fatalf("asker returned (%q, %v), want (that one, nil)", res.answer, res.err)
		}
	case <-time.After(time.Second):
		t.Fatal("asker did not return after the answer was delivered")
	}

	// A canceled run must not leave the tool waiting for an answer.
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		_, err := ask(ctx, agentcore.AskRequest{Question: "abandoned"})
		canceled <- err
	}()
	<-ch // the question reaches the UI, but the operator never answers
	cancel()
	select {
	case err := <-canceled:
		if err == nil {
			t.Error("a canceled run should report an error instead of a blank answer")
		}
	case <-time.After(time.Second):
		t.Fatal("asker did not unblock when the run was canceled")
	}
}

// TestAskPromptIgnoresEmptyEnter verifies an empty Enter does not answer the
// question (Esc is the way to abandon it), so a stray keystroke cannot tell the
// model the user said nothing.
func TestAskPromptIgnoresEmptyEnter(t *testing.T) {
	m := askTestModel(t)
	reply := make(chan string, 1)
	m = apply(t, m, questionAskedMsg{question: "Which branch?", reply: reply})

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.ask == nil {
		t.Fatal("empty Enter should leave the question open")
	}
	select {
	case got := <-reply:
		t.Fatalf("empty Enter answered the question with %q", got)
	default:
	}
}
