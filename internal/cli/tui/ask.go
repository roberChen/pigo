// This file implements the TUI's side of the human-in-the-loop seam: the question
// tool's batch is delivered as a questionAskedMsg on the run's bridge channel, the
// operator's answers are sent back on a reply channel, and the tool resumes with
// them. The run keeps draining while the prompt is up, so streaming, the spinner and
// the cancel path stay live.
//
// The panel is a small state machine: it answers the questions one at a time (the
// question text is fully reachable by scrolling, long option lists are windowed),
// then shows a review of every answer, and only submitting the review releases the
// tool. That last step is the point — a batch carries several answers, so the
// operator gets to see all of them at once before they are sent.
package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/ui"
)

// askStage is which part of the ask panel is on screen.
type askStage int

const (
	// askStageAnswering shows one question with its options.
	askStageAnswering askStage = iota
	// askStageReviewing shows every answer for a final look before submission.
	askStageReviewing
)

// Panel sizing. The question panel is capped so it can never take over the screen:
// a question may be enormous, but it is scrolled rather than allowed to push the
// transcript to zero rows or overflow the terminal. The minimum keeps a header, a
// couple of option rows and the hint line visible.
const (
	askPanelMinRows  = 6
	askPanelMaxRows  = 18
	askMaxOptionRows = 10
)

// questionAskedMsg carries one batch of questions from the question tool to the UI.
// The tool blocks on the answering goroutine until reply receives the operator's
// answers or the run is canceled, so the message must be handed to the UI without
// dropping it (the bridge channel provides the back-pressure).
type questionAskedMsg struct {
	questions []agentcore.AskQuestion
	reply     chan<- agentcore.AskResponse
}

// askPrompt is the pending batch shown just above the composer. While it is set the
// composer becomes the free-text field for the current question (see
// model.handleKey).
type askPrompt struct {
	questions []agentcore.AskQuestion
	answers   []agentcore.AskAnswer
	// index is the question being answered; scroll is the first visible line of its
	// text, and cursor the highlighted option. review is the highlighted question on
	// the review screen.
	index  int
	cursor int
	scroll int
	review int
	stage  askStage
	reply  chan<- agentcore.AskResponse
}

// newAskPrompt builds the panel for a batch, with one empty answer per question.
func newAskPrompt(questions []agentcore.AskQuestion, reply chan<- agentcore.AskResponse) *askPrompt {
	return &askPrompt{
		questions: questions,
		answers:   make([]agentcore.AskAnswer, len(questions)),
		reply:     reply,
	}
}

// current returns the question being answered.
func (a *askPrompt) current() agentcore.AskQuestion {
	if a == nil || a.index < 0 || a.index >= len(a.questions) {
		return agentcore.AskQuestion{}
	}
	return a.questions[a.index]
}

// askPanelRows is the number of rows the panel may occupy on a screen of height
// rows. It leaves the transcript room to stay useful on a short terminal.
func askPanelRows(height int) int {
	budget := height / 2
	if budget < askPanelMinRows {
		budget = askPanelMinRows
	}
	if budget > askPanelMaxRows {
		budget = askPanelMaxRows
	}
	if height > 2 && budget > height-2 {
		budget = height - 2
	}
	if budget < 1 {
		budget = 1
	}
	return budget
}

// view renders the panel within its row budget: the answering screen for the
// current question, or the review screen once every question has an answer.
func (a *askPrompt) view(theme Theme, width, height int) string {
	if a == nil {
		return ""
	}
	if width <= 0 {
		width = 1
	}
	budget := askPanelRows(height)
	if a.stage == askStageReviewing {
		return a.viewReview(theme, width, budget)
	}
	return a.viewQuestion(theme, width, budget)
}

// viewQuestion renders the header, as much of the question text as fits, the
// visible options and the key hint. The question text wraps and scrolls instead of
// being clipped, so an arbitrarily long question stays fully readable.
func (a *askPrompt) viewQuestion(theme Theme, width, budget int) string {
	q := a.current()
	var lines []string
	header := "?"
	if len(a.questions) > 1 {
		header = fmt.Sprintf("? question %d/%d", a.index+1, len(a.questions))
	}
	if q.Multi {
		header += " · select all that apply"
	}
	lines = append(lines, theme.Accent.Render(TruncateToWidth(header, width)))

	// Split the remaining rows between the question text and the option window.
	// Options never take more than half, so a 20-option question cannot hide the
	// question that explains it.
	body := budget - 2 // header + hint
	if body < 0 {
		body = 0
	}
	optionRows := 0
	if len(q.Options) > 0 {
		optionRows = min(len(q.Options), askMaxOptionRows, max(body/2, 1))
	}
	textRows := body - optionRows
	if textRows < 0 {
		textRows = 0
	}

	textLines := wrapQuestionText(q.Question, max(width-2, 1))
	maxScroll := max(len(textLines)-textRows, 0)
	scroll := a.scroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	for i := scroll; i < scroll+textRows && i < len(textLines); i++ {
		// The question is the model speaking to the operator, so it reads as prose
		// (the header and the highlighted option carry the accent).
		lines = append(lines, theme.User.Render("  "+textLines[i]))
	}

	if optionRows > 0 {
		start, end := pickerWindow(a.cursor, len(q.Options), optionRows)
		selected := make(map[string]bool, len(a.answers[a.index].Selected))
		for _, s := range a.answers[a.index].Selected {
			selected[s] = true
		}
		for i := start; i < end; i++ {
			opt := q.Options[i]
			var label string
			switch {
			case q.Multi && selected[opt]:
				label = fmt.Sprintf("%d) [x] %s", i+1, opt)
			case q.Multi:
				label = fmt.Sprintf("%d) [ ] %s", i+1, opt)
			default:
				label = fmt.Sprintf("%d) %s", i+1, opt)
			}
			prefix := "  "
			switch {
			case i == a.cursor:
				prefix = "› "
			case selected[opt]:
				prefix = "• "
			}
			row := TruncateToWidth(prefix+label, width)
			if i == a.cursor {
				lines = append(lines, theme.Accent.Render(row))
			} else {
				lines = append(lines, theme.System.Render(row))
			}
		}
	}

	lines = append(lines, theme.System.Render(TruncateToWidth(a.hint(q, len(textLines), textRows), width)))
	return strings.Join(lines, "\n")
}

// hint describes the keys that matter for this question, most important first so a
// narrow terminal truncates the tail (paging, extra hints) rather than Esc.
func (a *askPrompt) hint(q agentcore.AskQuestion, textLines, textRows int) string {
	var parts []string
	if a.index == len(a.questions)-1 {
		parts = append(parts, "Enter review")
	} else {
		parts = append(parts, "Enter next")
	}
	parts = append(parts, "Esc cancel")
	if len(q.Options) > 0 {
		parts = append(parts, "↑/↓ move")
		if q.Multi {
			parts = append(parts, "Space toggle")
		} else {
			parts = append(parts, "Space pick")
		}
		parts = append(parts, "type for other", "number pick")
	} else {
		parts = append(parts, "type your answer")
	}
	if len(a.questions) > 1 {
		parts = append(parts, "Tab next")
	}
	if textLines > textRows {
		parts = append(parts, "PgUp/PgDn scroll")
	}
	return strings.Join(parts, " · ")
}

// viewReview renders every answer in one screen so the batch can be checked before
// it is submitted. Long batches window around the highlight rather than growing
// past the panel budget.
func (a *askPrompt) viewReview(theme Theme, width, budget int) string {
	header := fmt.Sprintf("review %d answer(s) · ↑/↓ choose · e edit · Enter submit · Esc cancel", len(a.questions))
	var lines []string
	lines = append(lines, theme.Accent.Render(TruncateToWidth(header, width)))
	rows := budget - 1
	if rows < 1 {
		rows = 1
	}
	start, end := pickerWindow(a.review, len(a.questions), rows)
	for i := start; i < end; i++ {
		row := reviewRow(width, i, a.questions[i].Question, a.answers[i], i == a.review)
		if i == a.review {
			lines = append(lines, theme.Accent.Render(row))
		} else {
			lines = append(lines, theme.System.Render(row))
		}
	}
	return strings.Join(lines, "\n")
}

// reviewRow lays out one review line. The answer is what the operator is checking,
// so it keeps up to half the row and the question text is clipped around it; a long
// question must not push its own answer off the screen.
func reviewRow(width, index int, question string, answer agentcore.AskAnswer, selected bool) string {
	marker := "  "
	if selected {
		marker = "› "
	}
	num := fmt.Sprintf("%d. ", index+1)
	arrow := "  →  "
	text := answerSummary(answer)
	answerW := min(ui.Width(text), max(width/2, 10))
	qW := width - ui.Width(marker) - ui.Width(num) - ui.Width(arrow) - answerW
	if qW < 8 {
		qW = 8
	}
	return marker + num + TruncateToWidth(firstLineOf(question), qW) + arrow + TruncateToWidth(text, answerW)
}

// answerSummary renders one answer for the review row, marking an unanswered
// question explicitly so it cannot be submitted unnoticed.
func answerSummary(a agentcore.AskAnswer) string {
	if a.IsEmpty() {
		return "(no answer)"
	}
	return a.Display()
}

// rows reports how many terminal rows the panel occupies, so relayout can reserve
// exactly that space above the composer.
func (a *askPrompt) rows(theme Theme, width, height int) int {
	if a == nil {
		return 0
	}
	return strings.Count(a.view(theme, width, height), "\n") + 1
}

// matchOption maps a key press to the option it addresses: a digit selects the
// option at that position, so a yes/no style question is answered with one
// keystroke. Only the first nine options are reachable this way; the cursor covers
// the rest of a long list.
func (a *askPrompt) matchOption(key string) (int, bool) {
	if a == nil {
		return 0, false
	}
	n := len(a.current().Options)
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return 0, false
	}
	i := int(key[0] - '0')
	if i > n {
		return 0, false
	}
	return i - 1, true
}

// moveCursor shifts the option highlight, wrapping at the ends.
func (a *askPrompt) moveCursor(delta int) {
	n := len(a.current().Options)
	if n == 0 {
		a.scroll += delta
		return
	}
	a.cursor = ((a.cursor+delta)%n + n) % n
}

// toggleOption applies the highlighted option: multi-select flips it, single-select
// replaces the whole selection so the question stays a pick-one.
func (a *askPrompt) toggleOption() {
	q := a.current()
	if a.cursor < 0 || a.cursor >= len(q.Options) {
		return
	}
	opt := q.Options[a.cursor]
	answer := &a.answers[a.index]
	if !q.Multi {
		answer.Selected = []string{opt}
		return
	}
	// Rebuild from the option list so the selection stays in menu order (and can
	// never hold duplicates), which is how the review and the tool result read.
	selected := make(map[string]bool, len(answer.Selected)+1)
	for _, s := range answer.Selected {
		selected[s] = true
	}
	selected[opt] = !selected[opt]
	answer.Selected = answer.Selected[:0]
	for _, o := range q.Options {
		if selected[o] {
			answer.Selected = append(answer.Selected, o)
		}
	}
}

// pickOption selects (or toggles, for multi-select) the option at a 0-based index.
func (a *askPrompt) pickOption(idx int) {
	q := a.current()
	if idx < 0 || idx >= len(q.Options) {
		return
	}
	a.cursor = idx
	a.toggleOption()
}

// scrollText moves the question text window by delta lines, clamped to the text.
func (a *askPrompt) scrollText(delta, width, height int) {
	textRows := askPanelRows(height) - 2
	if textRows < 1 {
		textRows = 1
	}
	lines := wrapQuestionText(a.current().Question, max(width-2, 1))
	maxScroll := max(len(lines)-textRows, 0)
	a.scroll += delta
	if a.scroll < 0 {
		a.scroll = 0
	}
	if a.scroll > maxScroll {
		a.scroll = maxScroll
	}
}

// askViaChannel builds the run's asker for the TUI: it hands the batch to the UI on
// the bridge channel and waits for the answers. The reply channel is buffered so an
// answer to an already-canceled run cannot block the UI goroutine.
func askViaChannel(ch chan tea.Msg) agentcore.AskUserFunc {
	return func(ctx context.Context, req agentcore.AskRequest) (agentcore.AskResponse, error) {
		reply := make(chan agentcore.AskResponse, 1)
		select {
		case ch <- questionAskedMsg{questions: req.Questions, reply: reply}:
		case <-ctx.Done():
			return agentcore.AskResponse{}, ctx.Err()
		}
		select {
		case resp := <-reply:
			return resp, nil
		case <-ctx.Done():
			return agentcore.AskResponse{}, ctx.Err()
		}
	}
}

// submitAnswers hands the reviewed answers to the blocked question tool and returns
// the shell to its normal running state. The reply send is non-blocking: the run may
// be gone already (a canceled run), and the UI goroutine must never wait on it.
func (m Model) submitAnswers() (tea.Model, tea.Cmd) {
	if m.ask == nil {
		return m, nil
	}
	// On the answering screen the composer holds this question's free text; fold it
	// in so text typed before the review is not lost. The review screen clears the
	// composer, so it must not overwrite an already committed answer.
	if m.ask.stage == askStageAnswering {
		m.askSaveCurrentText()
	}
	resp := agentcore.AskResponse{Answers: m.ask.answers}
	reply := m.ask.reply
	m.ask = nil
	m.spinner.unpin()
	m.input.Clear()
	// The run continues, so the composer goes back to being the prompt field.
	cmd := m.input.Focus()
	m.relayout()
	select {
	case reply <- resp:
	default:
	}
	return m, cmd
}

// cancelAsk abandons a pending batch and interrupts the run: the asker unblocks on
// the canceled context and the question tool reports the abort to the model, which
// then ends the run as usual.
func (m Model) cancelAsk() (tea.Model, tea.Cmd) {
	m.ask = nil
	m.spinner.unpin()
	m.input.Clear()
	m.input.Blur()
	m.relayout()
	if m.interruptFn != nil {
		m.interruptFn()
	}
	return m, nil
}

// handleAskKey routes a key press while a question batch is pending. Answering and
// reviewing have separate key maps: the answering stage edits answers, the review
// stage only navigates and submits, so a stray keystroke at the review cannot
// change an answer it was meant to check.
func (m Model) handleAskKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.ask == nil {
		return m, nil
	}
	key := msg.String()
	if key == "esc" || key == "ctrl+c" {
		return m.cancelAsk()
	}
	if m.ask.stage == askStageReviewing {
		return m.handleAskReviewKey(key)
	}
	hasOptions := len(m.ask.current().Options) > 0
	switch key {
	case "enter":
		return m.askAdvance(true)
	case "tab":
		return m.askAdvance(false)
	case "shift+tab":
		if m.ask.index > 0 {
			m.askGoto(m.ask.index - 1)
		}
		return m, nil
	case "up", "down":
		delta := 1
		if key == "up" {
			delta = -1
		}
		if hasOptions {
			m.ask.moveCursor(delta)
		} else {
			m.ask.scrollText(delta, m.width, m.height)
		}
		m.relayout()
		return m, nil
	case "pgup", "pgdown":
		delta := askPanelRows(m.height) / 2
		if key == "pgup" {
			delta = -delta
		}
		m.ask.scrollText(delta, m.width, m.height)
		m.relayout()
		return m, nil
	case "space":
		if hasOptions {
			m.ask.toggleOption()
			m.relayout()
			return m, nil
		}
	}
	if hasOptions {
		if idx, ok := m.ask.matchOption(key); ok {
			m.ask.pickOption(idx)
			m.relayout()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleAskReviewKey drives the review screen: Enter submits the batch, ↑/↓ picks a
// question, and "e" (or that question's number) goes back to edit it.
func (m Model) handleAskReviewKey(key string) (tea.Model, tea.Cmd) {
	n := len(m.ask.questions)
	switch key {
	case "enter":
		return m.submitAnswers()
	case "up":
		m.ask.review = wrapIndex(m.ask.review-1, n)
		return m, nil
	case "down":
		m.ask.review = wrapIndex(m.ask.review+1, n)
		return m, nil
	case "e":
		return m.askEditHighlighted()
	}
	if idx, ok := digitIndex(key, n); ok {
		m.ask.review = idx
		return m.askEditHighlighted()
	}
	return m, nil
}

// askSaveCurrentText folds the composer into the current question's answer. The
// composer is the free-text "other" field, so whatever is typed when the operator
// moves on is part of that answer.
func (m *Model) askSaveCurrentText() {
	if m.ask == nil || m.ask.index < 0 || m.ask.index >= len(m.ask.answers) {
		return
	}
	m.ask.answers[m.ask.index].Text = strings.TrimSpace(m.input.Value())
}

// askGoto moves the panel to question idx, saving the composer first and restoring
// the target's stored free text, so going back to an answered question is lossless.
func (m *Model) askGoto(idx int) {
	if m.ask == nil || idx < 0 || idx >= len(m.ask.questions) || idx == m.ask.index {
		return
	}
	m.askSaveCurrentText()
	m.ask.index = idx
	m.ask.cursor = 0
	m.ask.scroll = 0
	m.input.SetValue(m.ask.answers[idx].Text)
	m.relayout()
}

// askAdvance commits the current answer and moves to the next question, or to the
// review screen once every question has been visited. With pick set (Enter), an
// empty composer commits the highlighted option, which is what a menu implies; typed
// text always wins because it is the explicit answer.
func (m Model) askAdvance(pick bool) (tea.Model, tea.Cmd) {
	if pick && strings.TrimSpace(m.input.Value()) == "" {
		if q := m.ask.current(); len(q.Options) > 0 && !q.Multi {
			m.ask.toggleOption()
		}
	}
	m.askSaveCurrentText()
	if m.ask.index >= len(m.ask.questions)-1 {
		m.ask.stage = askStageReviewing
		m.ask.review = 0
		m.input.Clear()
		m.relayout()
		return m, nil
	}
	m.askGoto(m.ask.index + 1)
	return m, nil
}

// askEditHighlighted returns from the review to the highlighted question.
func (m Model) askEditHighlighted() (tea.Model, tea.Cmd) {
	if m.ask == nil || m.ask.stage != askStageReviewing {
		return m, nil
	}
	idx := m.ask.review
	m.ask.stage = askStageAnswering
	m.askGoto(idx)
	cmd := m.input.Focus()
	m.relayout()
	return m, cmd
}

// wrapIndex wraps i into [0,n) so ↑/↓ loops through the questions.
func wrapIndex(i, n int) int {
	if n <= 0 {
		return 0
	}
	return ((i % n) + n) % n
}

// digitIndex maps a single digit key to a 0-based index below n.
func digitIndex(key string, n int) (int, bool) {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return 0, false
	}
	i := int(key[0] - '0')
	if i > n {
		return 0, false
	}
	return i - 1, true
}

// remoteAskEcho renders the pending questions for a paired browser, which cannot
// answer them but should see why the run paused.
func remoteAskEcho(questions []agentcore.AskQuestion) string {
	var b strings.Builder
	if len(questions) == 1 {
		b.WriteString("\n? " + questions[0].Question + "\n")
		return b.String()
	}
	for i, q := range questions {
		fmt.Fprintf(&b, "\n? (%d/%d) %s\n", i+1, len(questions), q.Question)
	}
	return b.String()
}

// wrapQuestionText wraps question prose at spaces, keeping the author's own line
// breaks and blank lines. Ordinary prose therefore never splits mid-word; a single
// token wider than the panel is hard-broken so no line can overflow it.
func wrapQuestionText(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, para := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		out = append(out, wrapWords(para, width)...)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// wrapWords wraps one newline-free paragraph into lines no wider than width.
func wrapWords(line string, width int) []string {
	if strings.TrimSpace(line) == "" {
		return []string{""}
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(line) {
		switch {
		case cur == "":
			cur = word
		case ui.Width(cur)+1+ui.Width(word) <= width:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
		}
		for ui.Width(cur) > width {
			head, tail := splitAtWidth(cur, width)
			out = append(out, head)
			cur = tail
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// splitAtWidth cuts s into the widest prefix that fits width cells and the rest.
func splitAtWidth(s string, width int) (string, string) {
	used := 0
	for i, r := range s {
		rw := runeWidth(r)
		if used+rw > width {
			return s[:i], s[i:]
		}
		used += rw
	}
	return s, ""
}
