package tui

// Tests for the TUI side of the question tool: the panel the operator sees, the
// answer paths (numbered option, multi-select, typed text, cancel), the review step
// that must be confirmed before answers are released, and the asker that bridges the
// run goroutine to the message loop.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
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

// askOneQuestion asks a single question and returns the model plus the reply channel.
func askOneQuestion(t *testing.T, question string, options ...string) (Model, chan agentcore.AskResponse) {
	t.Helper()
	m := askTestModel(t)
	reply := make(chan agentcore.AskResponse, 1)
	m = apply(t, m, questionAskedMsg{
		questions: []agentcore.AskQuestion{{Question: question, Options: options}},
		reply:     reply,
	})
	if m.ask == nil {
		t.Fatal("questionAskedMsg should open the panel")
	}
	return m, reply
}

// received drains the reply channel, failing when the panel has not submitted.
func received(t *testing.T, reply chan agentcore.AskResponse) agentcore.AskResponse {
	t.Helper()
	select {
	case got := <-reply:
		return got
	case <-time.After(50 * time.Millisecond):
		t.Fatal("the panel did not submit an answer")
		return agentcore.AskResponse{}
	}
}

// notSubmitted asserts the panel is still holding the answers.
func notSubmitted(t *testing.T, reply chan agentcore.AskResponse) {
	t.Helper()
	select {
	case got := <-reply:
		t.Fatalf("the panel submitted before review was confirmed: %+v", got)
	default:
	}
}

// hasText reports whether content contains want, ignoring the runs of spaces the
// review block uses to align its columns.
func hasText(content, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(content), " "), want)
}

// TestAskPanelShowsQuestionAndAnswersWithOption verifies the panel renders the
// question with its suggested answers and that a digit picks that option. The digit
// alone must not submit: the batch goes through the review screen first.
func TestAskPanelShowsQuestionAndAnswersWithOption(t *testing.T) {
	m, reply := askOneQuestion(t, "Deploy now?", "yes", "no")

	content := stripANSI(m.View().Content)
	for _, want := range []string{"Deploy now?", "1) yes", "2) no", "Enter review"} {
		if !hasText(content, want) {
			t.Fatalf("panel missing %q:\n%s", want, content)
		}
	}

	m = apply(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	notSubmitted(t, reply)

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // to the review
	if m.ask == nil || m.ask.stage != askStageReviewing {
		t.Fatalf("Enter should move to the review stage, stage = %v", m.ask.stage)
	}
	review := stripANSI(m.View().Content)
	if !hasText(review, "→ no") {
		t.Fatalf("review should show the chosen option:\n%s", review)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // submit
	if got := received(t, reply); len(got.Answers) != 1 || strings.Join(got.Answers[0].Selected, ",") != "no" {
		t.Errorf("answer = %+v, want option no", got)
	}
	if m.ask != nil {
		t.Error("the panel should close once submitted")
	}
	if !m.input.Focused() || m.ask != nil {
		t.Error("the composer should be handed back after submission")
	}
}

// TestAskPanelAcceptsTypedAnswer verifies free-text answering: keys edit the
// composer and the typed buffer rides along as the answer's text.
func TestAskPanelAcceptsTypedAnswer(t *testing.T) {
	m, reply := askOneQuestion(t, "Which branch?")

	m = typeText(t, m, "release/2.0")
	if got := m.input.Value(); got != "release/2.0" {
		t.Fatalf("composer = %q, want the typed answer", got)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // review
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // submit

	got := received(t, reply)
	if len(got.Answers) != 1 || got.Answers[0].Text != "release/2.0" {
		t.Errorf("answer = %+v, want the typed text", got)
	}
	if m.input.Value() != "" {
		t.Errorf("composer should clear after answering, got %q", m.input.Value())
	}
}

// TestAskPanelMultiSelect verifies a multi-select question toggles options with
// Space and numbers, keeps the menu order, and reports every chosen option.
func TestAskPanelMultiSelect(t *testing.T) {
	m := askTestModel(t)
	reply := make(chan agentcore.AskResponse, 1)
	m = apply(t, m, questionAskedMsg{
		questions: []agentcore.AskQuestion{{
			Question: "Which features?",
			Options:  []string{"auth", "billing", "reports"},
			Multi:    true,
		}},
		reply: reply,
	})

	// Space on the highlighted first option, then the number for the third.
	m = apply(t, m, tea.KeyPressMsg{Code: ' ', Text: " "})
	m = apply(t, m, tea.KeyPressMsg{Code: '3', Text: "3"})
	content := stripANSI(m.View().Content)
	for _, want := range []string{"[x] auth", "[ ] billing", "[x] reports"} {
		if !hasText(content, want) {
			t.Fatalf("multi-select rendering missing %q:\n%s", want, content)
		}
	}
	// Toggling the same option twice deselects it.
	m = apply(t, m, tea.KeyPressMsg{Code: '3', Text: "3"})
	if content := stripANSI(m.View().Content); strings.Contains(content, "[x] reports") {
		t.Fatalf("re-toggling should deselect the option:\n%s", content)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: '3', Text: "3"})

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	got := received(t, reply)
	if len(got.Answers) != 1 || strings.Join(got.Answers[0].Selected, ",") != "auth,reports" {
		t.Errorf("answer = %+v, want auth and reports", got)
	}
}

// TestAskPanelSingleSelectReplaces verifies a second pick replaces the first in a
// single-select question, so it can never submit two options.
func TestAskPanelSingleSelectReplaces(t *testing.T) {
	m, reply := askOneQuestion(t, "Deploy now?", "yes", "no")
	m = apply(t, m, tea.KeyPressMsg{Code: '1', Text: "1"})
	m = apply(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := received(t, reply); strings.Join(got.Answers[0].Selected, ",") != "no" {
		t.Errorf("answer = %+v, want only the last picked option", got)
	}
}

// TestAskPanelBatchAnswersEachQuestion verifies a batch asks the questions in order,
// keeps every answer, and submits them together.
func TestAskPanelBatchAnswersEachQuestion(t *testing.T) {
	m := askTestModel(t)
	reply := make(chan agentcore.AskResponse, 1)
	m = apply(t, m, questionAskedMsg{
		questions: []agentcore.AskQuestion{
			{Question: "Which database?", Options: []string{"postgres", "sqlite"}},
			{Question: "Anything else?"},
		},
		reply: reply,
	})

	if content := stripANSI(m.View().Content); !strings.Contains(content, "question 1/2") {
		t.Fatalf("header should count the batch:\n%s", content)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: '1', Text: "1"})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // next question
	if content := stripANSI(m.View().Content); !strings.Contains(content, "question 2/2") {
		t.Fatalf("panel should have moved to the second question:\n%s", content)
	}
	m = typeText(t, m, "add an index")
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // review
	if content := stripANSI(m.View().Content); !hasText(content, "→ postgres") || !hasText(content, "→ add an index") {
		t.Fatalf("review should list both answers:\n%s", content)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // submit

	got := received(t, reply)
	if len(got.Answers) != 2 {
		t.Fatalf("answers = %+v, want two", got.Answers)
	}
	if strings.Join(got.Answers[0].Selected, ",") != "postgres" || got.Answers[1].Text != "add an index" {
		t.Errorf("answers = %+v, want them aligned with their questions", got.Answers)
	}
}

// TestAskPanelReviewEditGoesBack verifies the review is a checkpoint: "e" returns to
// the highlighted question and the correction replaces the earlier answer.
func TestAskPanelReviewEditGoesBack(t *testing.T) {
	m, reply := askOneQuestion(t, "Which branch?", "main", "release")
	m = apply(t, m, tea.KeyPressMsg{Code: '1', Text: "1"}) // main
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = apply(t, m, tea.KeyPressMsg{Code: 'e'}) // back to the question
	if m.ask == nil || m.ask.stage != askStageAnswering {
		t.Fatalf("e should return to the question, stage = %v", m.ask.stage)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: '2', Text: "2"}) // release
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := received(t, reply); strings.Join(got.Answers[0].Selected, ",") != "release" {
		t.Errorf("answer = %+v, want the corrected option", got.Answers)
	}
}

// TestAskPanelCancelInterruptsRun verifies Esc abandons the batch and interrupts the
// run, which unblocks the question tool with a cancellation error.
func TestAskPanelCancelInterruptsRun(t *testing.T) {
	m, reply := askOneQuestion(t, "Delete the branch?")
	interrupted := false
	m.interruptFn = func() { interrupted = true }

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.ask != nil {
		t.Error("Esc should dismiss the panel")
	}
	if !interrupted {
		t.Error("Esc while a question is pending should interrupt the run")
	}
	notSubmitted(t, reply)
}

// TestAskPanelCtrlCCancels verifies Ctrl+C is the same abort as Esc while the panel
// is up, instead of being swallowed as text input.
func TestAskPanelCtrlCCancels(t *testing.T) {
	m, _ := askOneQuestion(t, "Delete the branch?")
	interrupted := false
	m.interruptFn = func() { interrupted = true }
	m = apply(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.ask != nil || !interrupted {
		t.Errorf("Ctrl+C should cancel the question (ask=%v interrupted=%v)", m.ask != nil, interrupted)
	}
}

// TestAskPanelClearedOnRunEnd verifies a run that ends (canceled elsewhere, or
// errored) never leaves a dangling panel over the composer.
func TestAskPanelClearedOnRunEnd(t *testing.T) {
	m, _ := askOneQuestion(t, "Still there?")
	m = apply(t, m, runEndMsg{})
	if m.ask != nil {
		t.Error("run end should clear the pending question")
	}
}

// TestAskPanelReservesRows verifies the panel takes exactly its rows out of the
// transcript so the shell keeps exactly Height rows while it is up.
func TestAskPanelReservesRows(t *testing.T) {
	m := apply(t, NewModel(Options{}), tea.WindowSizeMsg{Width: 80, Height: 20})
	base := m.transcript.viewportHeight()

	m.ask = newAskPrompt([]agentcore.AskQuestion{{Question: "Deploy now?", Options: []string{"yes", "no"}}}, make(chan agentcore.AskResponse, 1))
	m.relayout()
	rows := m.ask.rows(m.theme, m.width, m.height)
	if rows != 5 { // header, question, two options, hint
		t.Errorf("panel rows = %d, want 5", rows)
	}
	if got := m.transcript.viewportHeight(); got != base-rows {
		t.Errorf("transcript height with the panel up = %d, want %d (base %d - %d rows)", got, base-rows, base, rows)
	}
	if got := strings.Count(m.View().Content, "\n"); got != 19 {
		t.Errorf("newline count = %d, want 19 (20 rows)", got)
	}
}

// TestAskPanelBudget verifies the panel is capped on a tall terminal and shrinks on
// a short one instead of squeezing the transcript to nothing.
func TestAskPanelBudget(t *testing.T) {
	cases := []struct{ height, want int }{
		{4, 2}, {10, 6}, {20, 10}, {60, 18}, {100, 18},
	}
	for _, tc := range cases {
		if got := askPanelRows(tc.height); got != tc.want {
			t.Errorf("askPanelRows(%d) = %d, want %d", tc.height, got, tc.want)
		}
	}
}

// TestAskPanelScrollsLongQuestion verifies a question longer than the panel is
// scrollable rather than clipped: PgUp/PgDn move the window and stop at the ends.
func TestAskPanelScrollsLongQuestion(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	m, _ := askOneQuestion(t, strings.TrimSpace(b.String()))

	if content := stripANSI(m.View().Content); !strings.Contains(content, "line 01") {
		t.Fatalf("the first line should be visible:\n%s", content)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	content := stripANSI(m.View().Content)
	if strings.Contains(content, "line 01") {
		t.Fatalf("PgDn should scroll the question text:\n%s", content)
	}
	// Scrolling to the very end clamps instead of running past the text, and the
	// hint tells the operator there is more to see.
	for i := 0; i < 40; i++ {
		m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	content = stripANSI(m.View().Content)
	if !strings.Contains(content, "line 40") {
		t.Fatalf("scrolling to the end should reveal the last line:\n%s", content)
	}
	for i := 0; i < 40; i++ {
		m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	content = stripANSI(m.View().Content)
	if !strings.Contains(content, "line 01") {
		t.Fatalf("scrolling back should reveal the first line:\n%s", content)
	}
}

// TestAskPanelEmptyEnterNeedsReview verifies a stray Enter cannot tell the model the
// user said nothing: it moves on, and the empty answer is visible on the review
// screen, which must be confirmed separately.
func TestAskPanelEmptyEnterNeedsReview(t *testing.T) {
	m, reply := askOneQuestion(t, "Which branch?")
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.ask == nil || m.ask.stage != askStageReviewing {
		t.Fatalf("Enter should reach the review, stage = %v", m.ask.stage)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "(no answer)") {
		t.Fatalf("the review should call out the unanswered question:\n%s", content)
	}
	notSubmitted(t, reply)
}

// TestAskPanelDigitRange verifies only digits within the option list are treated as
// picks; anything else types into the composer.
func TestAskPanelDigitRange(t *testing.T) {
	m, _ := askOneQuestion(t, "Deploy now?", "yes", "no")
	m = typeText(t, m, "5")
	if got := m.input.Value(); got != "5" {
		t.Errorf("an out-of-range digit should be typed, composer = %q", got)
	}
}

// TestAskViaChannelRoundTrip verifies the bridge asker: the batch arrives on the run
// channel, the answers flow back, and a canceled run unblocks it instead of waiting
// forever.
func TestAskViaChannelRoundTrip(t *testing.T) {
	ch := newEventChan()
	ask := askViaChannel(ch)

	done := make(chan agentcore.AskResponse, 1)
	var askErr error
	go func() {
		resp, err := ask(context.Background(), agentcore.AskRequest{Questions: []agentcore.AskQuestion{{Question: "Which one?"}}})
		askErr = err
		done <- resp
	}()

	msg, ok := (<-ch).(questionAskedMsg)
	if !ok {
		t.Fatalf("expected a questionAskedMsg on the run channel")
	}
	if len(msg.questions) != 1 || msg.questions[0].Question != "Which one?" {
		t.Errorf("questions = %+v, want the asked question", msg.questions)
	}
	msg.reply <- agentcore.AskResponse{Answers: []agentcore.AskAnswer{{Selected: []string{"that one"}}}}

	select {
	case resp := <-done:
		if askErr != nil || len(resp.Answers) != 1 || strings.Join(resp.Answers[0].Selected, ",") != "that one" {
			t.Fatalf("asker returned (%+v, %v), want the answer", resp, askErr)
		}
	case <-time.After(time.Second):
		t.Fatal("asker did not return after the answer was delivered")
	}

	// A canceled run must not leave the tool waiting for an answer.
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		_, err := ask(ctx, agentcore.AskRequest{Questions: []agentcore.AskQuestion{{Question: "abandoned"}}})
		canceled <- err
	}()
	<-ch // the batch reaches the UI, but the operator never answers
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

// TestAskPanelEnterCommitsHighlight verifies Enter on a menu answers with the
// highlighted option (what a menu implies), while typed text wins over it because
// it is the explicit answer.
func TestAskPanelEnterCommitsHighlight(t *testing.T) {
	m, reply := askOneQuestion(t, "Deploy now?", "yes", "no")
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // highlight "no"
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := received(t, reply); strings.Join(got.Answers[0].Selected, ",") != "no" {
		t.Errorf("answer = %+v, want the highlighted option", got.Answers)
	}

	m, reply = askOneQuestion(t, "Which branch?", "main", "release")
	m = typeText(t, m, "feature/x")
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	got := received(t, reply)
	if got.Answers[0].Text != "feature/x" || len(got.Answers[0].Selected) != 0 {
		t.Errorf("answer = %+v, want the typed text only", got.Answers)
	}
}
