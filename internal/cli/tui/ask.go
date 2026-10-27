// This file implements the TUI's side of the human-in-the-loop seam: the question
// tool's question is delivered as a questionAskedMsg on the run's bridge channel,
// the operator's answer (a typed line or a numbered option) is sent back on a
// reply channel, and the tool resumes with it. The run keeps draining while the
// prompt is up, so streaming, the spinner and the cancel path stay live.
package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
)

// questionAskedMsg carries one question from the question tool to the UI. The
// tool blocks on the answering goroutine until reply receives the operator's
// answer or the run is canceled, so the message must be handed to the UI without
// dropping it (the bridge channel provides the back-pressure).
type questionAskedMsg struct {
	question string
	options  []string
	reply    chan<- string
}

// askPrompt is the pending question shown just above the composer. While it is set
// the composer becomes the answer field (see model.handleKey).
type askPrompt struct {
	question string
	options  []string
	reply    chan<- string
}

// view renders the prompt as a block of one or more rows: the question in the
// accent color, then the suggested answers as a numbered row. Both wrap or clip
// to width so the shell layout stays fixed.
func (a *askPrompt) view(theme Theme, width int) string {
	if a == nil {
		return ""
	}
	if width <= 0 {
		width = 1
	}
	var b strings.Builder
	b.WriteString(theme.Accent.Render(WrapToWidth("? "+a.question, width)))
	if len(a.options) > 0 {
		parts := make([]string, 0, len(a.options))
		for i, opt := range a.options {
			parts = append(parts, fmt.Sprintf("%d) %s", i+1, opt))
		}
		b.WriteByte('\n')
		b.WriteString(theme.System.Render(TruncateToWidth("  "+strings.Join(parts, "   "), width)))
	}
	return b.String()
}

// rows reports how many terminal rows the prompt occupies, so relayout can
// reserve that space above the composer.
func (a *askPrompt) rows(theme Theme, width int) int {
	if a == nil {
		return 0
	}
	return strings.Count(a.view(theme, width), "\n") + 1
}

// matchOption maps a key press to the suggested answer it selects: a digit picks
// the option at that position, so a yes/no style question is answered with one
// keystroke. Keys are compared as strings, matching the rest of the key handling.
func (a *askPrompt) matchOption(key string) (string, bool) {
	if a == nil || len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return "", false
	}
	n := int(key[0] - '0')
	if n > len(a.options) {
		return "", false
	}
	return a.options[n-1], true
}

// askViaChannel builds the run's asker for the TUI: it hands the question to the
// UI on the bridge channel and waits for the answer. The reply channel is
// buffered so an answer to an already-canceled run cannot block the UI goroutine.
func askViaChannel(ch chan tea.Msg) agentcore.AskUserFunc {
	return func(ctx context.Context, req agentcore.AskRequest) (string, error) {
		reply := make(chan string, 1)
		select {
		case ch <- questionAskedMsg{question: req.Question, options: req.Options, reply: reply}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		select {
		case answer := <-reply:
			return answer, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// submitAnswer hands the operator's answer to the blocked question tool and
// returns the shell to its normal running state. The reply send is
// non-blocking: the run may be gone already (a canceled run), and the UI
// goroutine must never wait on it.
func (m Model) submitAnswer(answer string) (tea.Model, tea.Cmd) {
	if m.ask == nil {
		return m, nil
	}
	reply := m.ask.reply
	m.ask = nil
	m.spinner.unpin()
	m.input.Clear()
	m.input.Blur()
	m.relayout()
	select {
	case reply <- strings.TrimSpace(answer):
	default:
	}
	return m, nil
}

// cancelAsk abandons a pending question and interrupts the run: the asker
// unblocks on the canceled context and the question tool reports the abort to
// the model, which then ends the run as usual.
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
