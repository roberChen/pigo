// This file implements the /resume session picker for the full-screen TUI
// (FR-16): an overlay listing the stored sessions that loads the chosen one in
// place. It is the TUI counterpart to `pigo --resume <id>` and the REPL's
// /resume, and it reuses the same session descriptions (cli.DescribeSession) and
// argument resolution (cli.ResolveSessionArg) so both surfaces behave alike.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/session"
)

// resumeListLimit caps how many recent sessions the picker offers, so a long
// session history stays a short, scannable list.
const resumeListLimit = 20

// maxPickerRows caps how many session rows the overlay shows at once; a longer
// list scrolls a window around the selection so the shell layout stays bounded.
const maxPickerRows = 10

// sessionListMsg carries the result of loading the resumable sessions. The load
// runs as a tea.Cmd (loadSessionsCmd) because it reads a file per session, and
// blocking the Update loop on that would stall the UI.
type sessionListMsg struct {
	sums []session.SessionSummary
	err  error
}

// loadSessionsCmd reads the recent sessions — headers plus first-prompt
// previews — off the tea loop.
func loadSessionsCmd(store *session.Store) tea.Cmd {
	return func() tea.Msg {
		sums, err := store.Summaries(resumeListLimit)
		return sessionListMsg{sums: sums, err: err}
	}
}

// sessionPicker is the /resume overlay state: the candidate sessions, the
// highlighted row, and whether the overlay is up. It renders nothing while
// inactive, so the shell layout is unchanged when it is closed.
type sessionPicker struct {
	theme    Theme
	active   bool
	sums     []session.SessionSummary
	selected int
	// currentID is the session the TUI is on now, so its row can be tagged and
	// the user can tell where they already are.
	currentID string
}

// newSessionPicker builds an inactive picker bound to the theme used for its
// rows.
func newSessionPicker(theme Theme) sessionPicker { return sessionPicker{theme: theme} }

// open shows the picker over the given sessions with the newest one
// highlighted. currentID is the session currently loaded ("" when none).
func (p *sessionPicker) open(sums []session.SessionSummary, currentID string) {
	p.sums = sums
	p.currentID = currentID
	p.selected = 0
	p.active = len(sums) > 0
}

// close hides the picker and drops its candidates.
func (p *sessionPicker) close() {
	p.active = false
	p.sums = nil
	p.selected = 0
	p.currentID = ""
}

// moveUp / moveDown cycle the highlighted row, wrapping at the ends so arrow
// navigation is continuous.
func (p *sessionPicker) moveUp() {
	if len(p.sums) == 0 {
		return
	}
	p.selected--
	if p.selected < 0 {
		p.selected = len(p.sums) - 1
	}
}

func (p *sessionPicker) moveDown() {
	if len(p.sums) == 0 {
		return
	}
	p.selected++
	if p.selected >= len(p.sums) {
		p.selected = 0
	}
}

// current returns the highlighted session, or ok=false when the picker is
// inactive or empty.
func (p sessionPicker) current() (session.SessionSummary, bool) {
	if !p.active || p.selected < 0 || p.selected >= len(p.sums) {
		return session.SessionSummary{}, false
	}
	return p.sums[p.selected], true
}

// rows reports how many terminal rows the overlay occupies, so the model can
// reserve that space above the input line during relayout.
func (p sessionPicker) rows() int {
	if !p.active {
		return 0
	}
	if len(p.sums) > maxPickerRows {
		return maxPickerRows
	}
	return len(p.sums)
}

// view renders the overlay as a block of session rows, the highlighted one
// marked with a "›" caret. Each row is the shared one-line session description,
// truncated to the width so it never wraps. It returns "" when inactive.
func (p sessionPicker) view(width int) string {
	if !p.active || len(p.sums) == 0 {
		return ""
	}
	start, end := p.window()
	rowWidth := width - 2 // reserve the caret / indent column
	if rowWidth < 1 {
		rowWidth = width
	}
	now := time.Now()
	var b strings.Builder
	for i := start; i < end; i++ {
		label := cli.DescribeSession(p.sums[i], now)
		if p.currentID != "" && p.sums[i].Header.ID == p.currentID {
			label += " (current)"
		}
		line := TruncateToWidth(label, rowWidth)
		if i == p.selected {
			b.WriteString(p.theme.Accent.Render("› " + line))
		} else {
			b.WriteString(p.theme.System.Render("  " + line))
		}
		if i < end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// window returns the [start,end) slice of candidates to display, scrolled to
// keep the selection visible when the list is taller than maxPickerRows.
func (p sessionPicker) window() (int, int) {
	n := len(p.sums)
	if n <= maxPickerRows {
		return 0, n
	}
	start := p.selected - maxPickerRows + 1
	if start < 0 {
		start = 0
	}
	if start > n-maxPickerRows {
		start = n - maxPickerRows
	}
	return start, start + maxPickerRows
}

// runResume implements /resume: bare "/resume" loads the stored sessions and
// opens the picker, while "/resume <n|id>" switches straight to that session.
// Both paths are gated on a session being bound (the pure constructor used by
// tests has none) and echoed to the transcript like every other slash command.
func (m Model) runResume(line string) (tea.Model, tea.Cmd) {
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	m.picker.close()
	m.relayout()
	if m.session == nil {
		m.transcript.addSystem("(resume unavailable: no active session)")
		m.relayout()
		return m, nil
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
	if arg != "" {
		return m.resumeByArg(arg)
	}
	return m, loadSessionsCmd(m.session.store)
}

// applySessionList folds the loaded session list into the model: it either opens
// the picker or reports why nothing can be resumed.
func (m Model) applySessionList(msg sessionListMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.transcript.addSystem("resume failed: " + msg.err.Error())
		m.relayout()
		return m, nil
	}
	if len(msg.sums) == 0 {
		m.transcript.addSystem("no stored sessions to resume")
		m.relayout()
		return m, nil
	}
	currentID := ""
	if m.session != nil {
		currentID = m.session.header.ID
	}
	m.picker.open(msg.sums, currentID)
	m.transcript.addSystem("select a session to resume (↑/↓ to move, Enter to load, Esc to cancel)")
	m.relayout()
	return m, nil
}

// resumeByArg resolves a /resume argument (a 1-based index into the recent list,
// a session id, or a unique id prefix) and switches to it.
func (m Model) resumeByArg(arg string) (tea.Model, tea.Cmd) {
	sums, err := m.session.store.Summaries(resumeListLimit)
	if err != nil {
		m.transcript.addSystem("resume failed: " + err.Error())
		m.relayout()
		return m, nil
	}
	id, ok := cli.ResolveSessionArg(arg, sums)
	if !ok {
		m.transcript.addSystem(fmt.Sprintf("no session %q — run /resume to choose from %d stored sessions", arg, len(sums)))
		m.relayout()
		return m, nil
	}
	return m.resumeSession(id)
}

// submitSessionPick loads the highlighted session and closes the overlay.
func (m Model) submitSessionPick() (tea.Model, tea.Cmd) {
	choice, ok := m.picker.current()
	m.picker.close()
	if !ok {
		m.relayout()
		return m, nil
	}
	return m.resumeSession(choice.Header.ID)
}

// resumeSession swaps the live session to id. It rebuilds the run session from
// the model's options with ResumeID set — the exact path a `pigo --resume`
// launch takes, so hooks, trust and the slash registry are wired identically —
// then resets the transcript and replays the resumed history. The previous
// conversation's tool cards, sub-agent rows, mouse selection and LAN mirror all
// belong to the session being left, so they are dropped.
func (m Model) resumeSession(id string) (tea.Model, tea.Cmd) {
	// Persist the outgoing session's unsaved tail first (a no-op when everything
	// was already written) so switching never drops a turn.
	saveErr := m.session.persist()
	opts := m.opts
	opts.ResumeID = id
	s, history, err := newRunSessionWithStore(m.session.store, opts)
	if err != nil {
		m.transcript.addSystem("resume failed: " + err.Error())
		m.relayout()
		return m, nil
	}
	m.shutdownRemote()
	m.transcript = newTranscript(m.theme)
	m.transcript.addBanner(renderBanner(m.theme, m.opts, m.cwd))
	m.toolCards = make(map[string]*toolCard)
	m.lastToolCard = nil
	m.subagents = subagentPanel{}
	m.sel = selection{}
	m.picker.close()
	m.menu.close()
	m.running = false
	m.runCh = nil
	m.spinner.stop()
	m.session = s
	m.startRunFn = s.startRun
	m.interruptFn = s.interrupt
	m.live = s.live
	m.slash = s.slash
	m.statusBar.SetModel(s.live.Model)
	m.statusBar.SetThinking(string(s.live.ThinkingLevel))
	// The context-usage readout belongs to the conversation being left, so hide
	// it until the resumed session reports its own telemetry.
	m.statusBar.SetTelemetry(telemetryEventView{})
	// Replay the conversation, tool cards included, and leave Ctrl+O pointing at
	// the last replayed call as a live run would.
	m.lastToolCard = seedTranscript(&m.transcript, history)
	m.relayout()
	if saveErr != nil {
		m.transcript.addSystem("warning: could not save the previous session: " + saveErr.Error())
	}
	note := fmt.Sprintf("resumed session %s (%d messages)", s.header.ID, len(history))
	if s.header.Cwd != "" && m.cwd != "" && s.header.Cwd != m.cwd {
		// The session was recorded in another project: say so, since the tools
		// keep operating on the launch directory (pigo does not cd).
		note += fmt.Sprintf(" (recorded in %s; tools still run in %s)", s.header.Cwd, m.cwd)
	}
	m.transcript.addSystem(note)
	m.relayout()
	return m, m.input.Focus()
}
