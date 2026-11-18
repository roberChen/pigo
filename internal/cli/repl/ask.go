// This file implements the line REPL's side of the human-in-the-loop seam: when
// the question tool asks the operator something, each question is printed in full
// and the answers are read from the same buffered input the prompt and the
// tool-call confirmation use. Reads are serialized with the confirmation prompt's
// mutex so two prompts can never interleave on the shared reader.
//
// Nothing is clipped: a question may be several paragraphs long and an option list
// may be longer than a page, so long option lists are paged and the question text is
// printed verbatim (the terminal wraps it). After every question is answered the
// whole batch is echoed back for confirmation, because a batch is submitted once
// and the operator must be able to see all of it before it is.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/ui"
)

// replOptionsPerPage is how many options one page shows. A menu longer than this
// is paged so a 20-option question does not scroll the question itself off screen;
// "n"/"p" move between pages.
const replOptionsPerPage = 10

// errDeclined reports that the operator reviewed the batch and chose not to submit
// it. It is distinct and actionable: the model is told the user declined rather
// than that the question could not be asked.
var errDeclined = errors.New("the user declined to answer")

// askUserREPL asks every question in the batch, then shows the collected answers
// for confirmation. It returns an error on EOF, on a canceled read, or when the
// operator declines the review, which the question tool turns into an error result.
//
// A run canceled while this is blocked (Ctrl+C) cannot interrupt the read itself —
// the same limitation the tool-confirmation prompt has — so the answer is simply
// discarded once it arrives.
func askUserREPL(out io.Writer, deps replDeps, req agentcore.AskRequest) (agentcore.AskResponse, error) {
	if deps.confirmMu != nil {
		deps.confirmMu.Lock()
		defer deps.confirmMu.Unlock()
	}
	if deps.in == nil {
		return agentcore.AskResponse{}, fmt.Errorf("no input available")
	}
	answers := make([]agentcore.AskAnswer, len(req.Questions))
	for i, q := range req.Questions {
		answer, err := askOneREPL(out, deps, i, len(req.Questions), q, answers[i])
		if err != nil {
			return agentcore.AskResponse{}, err
		}
		answers[i] = answer
	}
	return reviewAnswersREPL(out, deps, req.Questions, answers)
}

// askOneREPL prints one question (in full) with its options and reads the answer.
// An existing answer is shown as the starting state, so editing a question from the
// review screen does not silently discard what was already chosen.
func askOneREPL(out io.Writer, deps replDeps, index, total int, q agentcore.AskQuestion, existing agentcore.AskAnswer) (agentcore.AskAnswer, error) {
	color := ui.Enabled()
	header := "?"
	if total > 1 {
		header = fmt.Sprintf("? question %d/%d", index+1, total)
	}
	lead := ui.Colorize(color, ui.Cyan, header)
	if hint := questionHint(q); hint != "" {
		lead += " " + hint
	}
	writeQuestionText(out, lead+" ", "  ", q.Question, color)

	answer := existing
	if q.Multi {
		answer.Selected = append([]string(nil), existing.Selected...)
	} else if len(existing.Selected) > 1 {
		answer.Selected = existing.Selected[:1]
	}

	if len(q.Options) == 0 {
		fmt.Fprint(out, "answer: ")
		line, err := readAnswerLine(deps)
		if err != nil {
			return agentcore.AskAnswer{}, err
		}
		answer.Text = line
		return answer, nil
	}

	page := 0
	for {
		printOptionsPage(out, q, answer, page, color)
		if q.Multi {
			fmt.Fprint(out, "  select (numbers/range, a=all, Enter=done, n/p=page, text=other): ")
		} else {
			fmt.Fprint(out, "  answer (number, n/p=page, or text): ")
		}
		line, err := readAnswerLine(deps)
		if err != nil {
			return agentcore.AskAnswer{}, err
		}
		switch {
		case q.Multi && line == "":
			return answer, nil
		case line == "":
			// An empty reply is an explicit "I have no answer"; the review step
			// shows it so nothing is submitted unseen.
			return answer, nil
		case isPageCommand(line):
			page = nextOptionPage(page, len(q.Options), line)
			continue
		case q.Multi && strings.EqualFold(line, "a"):
			answer.Selected = append([]string(nil), q.Options...)
			continue
		}
		if picked, ok := parseOptionSelection(line, len(q.Options)); ok {
			if q.Multi {
				toggleOptions(&answer, q.Options, picked)
				continue
			}
			answer.Selected = []string{q.Options[picked[0]-1]}
			answer.Text = ""
			return answer, nil
		}
		if q.Multi {
			// Any other text is the free-form "other" answer; keep whatever was
			// toggled and finish this question.
			answer.Text = line
			return answer, nil
		}
		answer.Text = line
		answer.Selected = nil
		return answer, nil
	}
}

// reviewAnswersREPL prints every question with its answer and asks the operator to
// submit, decline, or edit one question by number. It loops until one of those is
// chosen, so a typo at the prompt does not throw the batch away.
func reviewAnswersREPL(out io.Writer, deps replDeps, questions []agentcore.AskQuestion, answers []agentcore.AskAnswer) (agentcore.AskResponse, error) {
	color := ui.Enabled()
	out0 := func() {
		fmt.Fprintf(out, "\n%s\n", ui.Colorize(color, ui.Bold, "review answers before submitting"))
		for i, q := range questions {
			fmt.Fprintf(out, "  %d. %s\n", i+1, firstLineOfText(q.Question))
			fmt.Fprintf(out, "     %s %s\n", ui.Colorize(color, ui.Green, "→"), answerLine(answers[i]))
		}
	}
	out0()
	for {
		fmt.Fprint(out, "  submit? [Y]es / [n]o / number to edit: ")
		line, err := readAnswerLine(deps)
		if err != nil {
			return agentcore.AskResponse{}, err
		}
		switch {
		case line == "" || strings.EqualFold(line, "y") || strings.EqualFold(line, "yes"):
			return agentcore.AskResponse{Answers: answers}, nil
		case strings.EqualFold(line, "n") || strings.EqualFold(line, "no"):
			return agentcore.AskResponse{}, errDeclined
		}
		n, convErr := strconv.Atoi(line)
		if convErr != nil || n < 1 || n > len(questions) {
			fmt.Fprintf(out, "  enter y, n, or a question number between 1 and %d\n", len(questions))
			continue
		}
		updated, editErr := askOneREPL(out, deps, n-1, len(questions), questions[n-1], answers[n-1])
		if editErr != nil {
			return agentcore.AskResponse{}, editErr
		}
		answers[n-1] = updated
		out0()
	}
}

// writeQuestionText prints the question verbatim, line by line: the first line
// continues the "?" lead-in and the rest are indented under it. The text is never
// truncated — a question that carries context must stay readable — and blank lines
// are preserved so paragraphs do not run together.
func writeQuestionText(out io.Writer, first, indent, text string, color bool) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lead := indent
		if i == 0 {
			lead = first
		}
		fmt.Fprintf(out, "%s%s\n", lead, ui.Colorize(color, ui.Bold, line))
	}
}

// printOptionsPage prints one page of the option menu, marking the current
// selections and pointing at the page bounds when there are more than fit.
func printOptionsPage(out io.Writer, q agentcore.AskQuestion, answer agentcore.AskAnswer, page int, color bool) {
	total := len(q.Options)
	start, end := optionPageBounds(page, total)
	selected := make(map[string]bool, len(answer.Selected))
	for _, s := range answer.Selected {
		selected[s] = true
	}
	for i := start; i < end; i++ {
		label := fmt.Sprintf("%d) %s", i+1, q.Options[i])
		prefix := "  "
		if q.Multi {
			mark := "[ ]"
			if selected[q.Options[i]] {
				mark = "[x]"
			}
			label = mark + " " + label
		} else if selected[q.Options[i]] {
			prefix = ui.Colorize(color, ui.Green, "• ")
		}
		fmt.Fprintf(out, "%s%s\n", prefix, ui.Colorize(color, ui.Bold, label))
	}
	if total > replOptionsPerPage {
		fmt.Fprintf(out, "  (options %d-%d of %d)\n", start+1, end, total)
	}
	if q.Multi && len(answer.Selected) > 0 {
		fmt.Fprintf(out, "  selected: %s\n", strings.Join(answer.Selected, ", "))
	}
}

// readAnswerLine reads one trimmed line from the shared input. EOF with nothing
// typed is an error so the tool reports it instead of waiting for input that will
// never arrive.
func readAnswerLine(deps replDeps) (string, error) {
	line, err := deps.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("input closed")
	}
	return strings.TrimSpace(line), nil
}

// questionHint describes the answering mode next to the question mark, so the
// operator knows whether several picks are allowed before they start typing.
func questionHint(q agentcore.AskQuestion) string {
	if q.Multi {
		return "(select all that apply)"
	}
	return ""
}

// answerLine renders one answer for the review block, spacing the free-form text
// away from the selected options.
func answerLine(a agentcore.AskAnswer) string {
	if a.IsEmpty() {
		return "(no answer)"
	}
	return a.Display()
}

// isPageCommand reports whether line asks to move to another page of options.
func isPageCommand(line string) bool {
	switch strings.ToLower(line) {
	case "n", "next", ">", "p", "prev", "previous", "<":
		return true
	}
	return false
}

// nextOptionPage applies a page command, clamping to the available pages.
func nextOptionPage(page, total int, cmd string) int {
	if total <= 0 {
		return 0
	}
	pages := (total + replOptionsPerPage - 1) / replOptionsPerPage
	switch strings.ToLower(cmd) {
	case "n", "next", ">":
		page++
	case "p", "prev", "previous", "<":
		page--
	}
	if page < 0 {
		page = 0
	}
	if page > pages-1 {
		page = pages - 1
	}
	return page
}

// optionPageBounds returns the half-open range of options visible on page.
func optionPageBounds(page, total int) (int, int) {
	start := page * replOptionsPerPage
	if start >= total {
		// Clamp to the last page so an out-of-range page shows the final options
		// rather than an empty menu.
		start = (total - 1) / replOptionsPerPage * replOptionsPerPage
	}
	if start < 0 {
		start = 0
	}
	end := start + replOptionsPerPage
	if end > total {
		end = total
	}
	return start, end
}

// parseOptionSelection parses "1", "1,3", "1 3" or "2-4" into 1-based option
// numbers, rejecting anything that is not entirely such a list (so free text that
// happens to contain a digit still becomes a text answer). Out-of-range numbers are
// dropped; the caller treats an empty result as "not a selection".
func parseOptionSelection(line string, total int) ([]int, bool) {
	fields := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		return nil, false
	}
	var out []int
	for _, f := range fields {
		lo, hi, isRange := strings.Cut(f, "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, false
		}
		end := start
		if isRange {
			end, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, false
			}
		}
		if start < 1 || end < start || end > total {
			return nil, false
		}
		for n := start; n <= end; n++ {
			out = append(out, n)
		}
	}
	return out, true
}

// toggleOptions flips each 1-based option number in the answer's selection,
// keeping the selection in option order so the review reads consistently.
func toggleOptions(answer *agentcore.AskAnswer, options []string, picked []int) {
	selected := make(map[string]bool, len(answer.Selected))
	for _, s := range answer.Selected {
		selected[s] = true
	}
	for _, n := range picked {
		opt := options[n-1]
		selected[opt] = !selected[opt]
	}
	answer.Selected = answer.Selected[:0]
	for _, opt := range options {
		if selected[opt] {
			answer.Selected = append(answer.Selected, opt)
		}
	}
}

// firstLineOfText keeps a question readable in the review block, where a whole
// paragraph per row would bury the answers.
func firstLineOfText(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// withAsker installs the REPL asker on the turn's context, so the question tool
// can reach the operator. It is a tiny wrapper that keeps streamRun readable.
func withAsker(ctx context.Context, out io.Writer, deps replDeps) context.Context {
	return agentcore.WithAskUser(ctx, func(_ context.Context, req agentcore.AskRequest) (agentcore.AskResponse, error) {
		return askUserREPL(out, deps, req)
	})
}
