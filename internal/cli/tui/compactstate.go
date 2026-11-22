// This file implements the TUI's sticky compacting state: once a compaction
// starts, the session stays in it until one attempt succeeds. A failed or
// aborted attempt is not an exit — the context is still over budget, so letting
// the operator carry on as if nothing happened would leave them talking to a
// model that cannot fit the conversation, and the next turn would only cross the
// threshold again.
//
// Two very different producers feed this state and both must be sticky:
//
//   - /compact (manual). The TUI owns the retry loop: each attempt runs as a
//     compactDoneMsg, and only a successful one leaves the state. While waiting,
//     the operator can retry with Enter or run a read-only command.
//   - the run loop's auto-compaction. The loop retries on its own (see
//     runtime/compaction_retry.go), so the TUI only has to keep the indicator up
//     and suppress the per-attempt noise until the loop settles.
//
// The escape hatch is deliberately intact: this state may refuse a prompt, but
// it must never trap the operator. Ctrl+D and /exit //quit always work, and
// Ctrl+C (or Esc) aborts the in-flight attempt first and quits on the next
// press, so a summarizer that is down for good cannot pin the program.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/roberChen/pigo/internal/cli/ui"
)

// compactState tracks the sticky compacting state of one session.
//
// It is separate from Model.running on purpose: running means "something is in
// flight and the composer is blurred", while active means "this session may not
// do anything but compact". Between two attempts nothing is in flight (the
// spinner stops, the composer wakes up for commands) yet the state is still
// active, which is exactly the window in which the operator retries.
type compactState struct {
	// active is true from the moment a compaction starts until one succeeds.
	active bool
	// attempt is the 1-based number of the attempt currently in flight, or of
	// the last one that failed while waiting for a retry.
	attempt int
	// lastErr is the failure of the last attempt, empty before the first one
	// fails. It is what the waiting screen reports.
	lastErr string
}

// beginCompact enters the sticky state and resets the attempt counter. Every
// entry point starts at attempt 1: a fresh /compact is a fresh compaction.
func (m *Model) beginCompact() {
	m.compacting = compactState{active: true, attempt: 1}
}

// endCompact leaves the sticky state, whichever way it ended.
func (m *Model) endCompact() {
	m.compacting = compactState{}
}

// compactingStatusLine is the row shown in the spinner's slot while the state is
// active but no attempt is in flight — the waiting-for-retry screen. It must say
// three things the operator needs: what failed, that nothing was lost, and how
// to get out of it.
//
// The hint is laid out first and the error gets the remaining columns: a
// summarizer error is arbitrary text and can be arbitrarily long, while "Enter
// retries · Esc quits" is the part that must never be clipped. Truncating the
// assembled line instead would let a verbose failure push the exit instructions
// off a narrow terminal — the exact case where the operator needs them.
func (m Model) compactingStatusLine(width int) string {
	hint := m.theme.System.Render("(context left unchanged · Enter retries · Esc quits)")
	note := fmt.Sprintf("compaction failed (attempt %d): %s", m.compacting.attempt, m.compacting.lastErr)
	// One column for the space between the note and the hint.
	room := width - ui.Width(hint) - 1
	if room <= 0 {
		// Too narrow for both: the hint alone still explains the state and the way
		// out, which is worth more than the message.
		return TruncateToWidth(hint, width)
	}
	return m.theme.System.Render(TruncateToWidth(note, room)) + " " + hint
}

// compactAttemptInFlight reports whether a compaction attempt is running right
// now. While it is, the composer must not run anything that reads the message
// list: the attempt goroutine owns agentCtx.Messages until it settles.
func (m Model) compactAttemptInFlight() bool {
	return m.compacting.active && m.running
}

// compactingCommandAllows reports whether line may run while the session is
// stuck compacting, given whether an attempt is currently in flight.
//
// The split is about the context, not about politeness: a command that reads
// agentCtx.Messages while an attempt owns it is a data race, so those are only
// accepted between attempts. Commands that touch nothing but the registry or the
// program lifecycle are always fine — refusing /exit here would be a trap.
func compactingCommandAllows(line string, attemptInFlight bool) bool {
	name := strings.TrimSpace(line)
	name = strings.TrimPrefix(name, "/")
	if i := strings.IndexAny(name, " \t"); i >= 0 {
		name = name[:i]
	}
	name = strings.ToLower(name)
	switch name {
	case "exit", "quit", "help":
		// Lifecycle and documentation: nothing to race with.
		return true
	case "status", "session", "memory", "perf":
		// Read the message list (counts, tokens, telemetry), so only safely
		// runnable when no attempt holds it.
		return !attemptInFlight
	}
	return false
}

// handleCompactingKey owns the keyboard while the session is compacting. It is
// the reason the composer cannot accept a prompt here: everything that is not an
// allowlisted command, a retry, or an exit is swallowed with an explanation
// rather than forwarded to the editor.
func (m Model) handleCompactingKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+d":
		// The hard exit stays hard: a stuck compaction must never pin the program.
		m.shutdownRemote()
		m.quitting = true
		return m, tea.Quit
	case "ctrl+c", "esc":
		// Esc closes the drawer first, so the "innermost thing on top" rule still
		// holds inside the compacting state.
		if msg.String() == "esc" && m.statusPanel.open() {
			m.statusPanel.close()
			m.relayout()
			return m, nil
		}
		return m.compactingInterrupt()
	case "enter":
		return m.compactingEnter()
	}
	// Editing: a command may be composed, a prompt may not. The buffer must start
	// with "/" for any editing to be accepted, so a stray character is refused
	// instead of silently becoming a message that then has to be rejected.
	if m.compactAttemptInFlight() {
		// Nothing edits the composer mid-attempt: the attempt goroutine owns the
		// context a command would read, and the spinner already says it is working.
		return m, nil
	}
	if !m.compactingBufferIsCommand() && !isSlashStartKey(msg) {
		// A prompt cannot be composed here at all. The waiting row directly above
		// the composer already says why and what is allowed, so this is silent
		// rather than one system line per keystroke.
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.refreshMenu()
	m.relayout()
	return m, cmd
}

// compactingBufferIsCommand reports whether the composer already holds a command
// in progress, which is what makes further editing (appending a name, backspace
// correcting it) legal while compacting.
func (m Model) compactingBufferIsCommand() bool {
	return strings.HasPrefix(strings.TrimSpace(m.input.Value()), "/")
}

// isSlashStartKey reports whether msg is the keystroke that opens a command: "/"
// itself. Only that one character may enter an empty composer here, which keeps
// the "message box does not take input" rule enforceable at the keystroke level
// instead of after the fact.
func isSlashStartKey(msg tea.KeyPressMsg) bool {
	return msg.Text == "/"
}

// compactingEnter is Enter while compacting: retry when an attempt has failed,
// run an allowlisted command when one is typed, and do nothing while an attempt
// is in flight.
func (m Model) compactingEnter() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	if line == "" {
		if m.compactAttemptInFlight() {
			return m, nil
		}
		// Enter on an empty composer is the retry key — the one action that is
		// always meaningful in this state, and the reason the composer stays
		// focused at all.
		return m.retryCompaction()
	}
	if !strings.HasPrefix(line, "/") {
		m.transcript.addSystem("(compacting: a message cannot be sent until the context is compacted · Enter retries)")
		m.relayout()
		return m, nil
	}
	if line == "/compact" {
		// The command spelling of the retry key: reaching for /compact again while
		// stuck is the obvious thing to do, and it means exactly what Enter means.
		m.input.Clear()
		m.menu.close()
		return m.retryCompaction()
	}
	if !compactingCommandAllows(line, m.compactAttemptInFlight()) {
		if m.compactAttemptInFlight() {
			m.transcript.addSystem("(compacting: a compaction attempt is in flight; Esc aborts it)")
		} else {
			m.transcript.addSystem("(compacting: only /status /session /memory /perf /help /exit can be run · Enter retries)")
		}
		m.relayout()
		return m, nil
	}
	m.input.Clear()
	m.menu.close()
	return m.runSlash(line)
}

// compactingInterrupt is Ctrl+C / Esc while compacting. With an attempt in
// flight it aborts that attempt (dropping the incomplete summary, so the retry
// starts over rather than continuing it) and stays in the compacting state — the
// context is still over budget, so aborting is not the same as being done. With
// nothing in flight the press is the escape hatch and quits, which is what makes
// this a two-stage exit rather than a trap.
func (m Model) compactingInterrupt() (tea.Model, tea.Cmd) {
	if m.compactAttemptInFlight() {
		if m.interruptFn != nil {
			m.interruptFn()
		}
		m.transcript.addSystem("(aborting this compaction attempt · the context is unchanged)")
		m.relayout()
		return m, nil
	}
	m.shutdownRemote()
	m.quitting = true
	return m, tea.Quit
}

// retryCompaction starts another manual compaction attempt from the sticky
// state. It is a no-op while an attempt is already in flight, so Enter cannot
// stack attempts on top of each other.
func (m Model) retryCompaction() (tea.Model, tea.Cmd) {
	if m.session == nil || m.compactAttemptInFlight() {
		return m, nil
	}
	m.compacting.active = true
	m.compacting.attempt++
	m.compacting.lastErr = ""
	m.running = true
	m.spinner.begin(time.Now(), m.thinkingLabel())
	m.spinner.pin(fmt.Sprintf("Compacting conversation (attempt %d)", m.compacting.attempt))
	m.relayout()
	return m, tea.Batch(m.session.compactCmd(), m.tickSpinner())
}
