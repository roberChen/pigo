// This file implements the TUI entry point. It also owns the terminal's
// diagnostic contract: while the alt screen is up, nothing may write to
// stdout/stderr behind Bubble Tea's back.
package tui

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/diag"
)

// Run starts the full-screen TUI and blocks until the user quits (Ctrl+C /
// Ctrl+D) or the program errors. It is the alt-screen counterpart to repl.Run:
// cmd/pigo's dispatch calls it on the (no prompt + TTY + no --no-tui) path and
// maps its error to the process exit code. The alt-screen is entered/left via
// the View returned by the root Model, so a clean return here restores the
// terminal to the user's prior scrollback.
//
// Diagnostics are redirected into the transcript before the session is
// assembled, so everything the interactive session does from here on — trust
// store, hooks, slash registry, /reload, MCP refreshes, checkpoints, background
// sub-agents — renders in the transcript instead of being written to stderr
// behind Bubble Tea's back. (run.SetupEnv runs earlier, in cmd/pigo's dispatch,
// while the terminal is still in cooked mode; its diagnostics therefore go to
// the default sink, which is discarded unless PIGO_DEBUG is set.) Lines are
// collected only when PIGO_DEBUG is set — a normal session stays completely
// quiet — and they never touch the terminal.
func Run(opts Options) error {
	collector := diag.Collect(diag.DebugEnabled())
	diag.Install(collector)
	defer diag.Install(nil)

	// Assemble the session (store, resume-or-fresh context, live config) before
	// entering the alt-screen, mirroring repl.Run: a store/resume failure is a
	// clean pre-launch error rather than a broken interactive session.
	s, history, err := newRunSession(opts)
	if err != nil {
		// No program was started, so nothing will drain the collector. With
		// PIGO_DEBUG on, the user asked for diagnostics: hand them to stderr, where
		// the exit error is about to be printed anyway (the terminal is in cooked
		// mode and no frame has been painted).
		if lines := collector.Drain(); len(lines) > 0 {
			for _, line := range lines {
				fmt.Fprintln(os.Stderr, line)
			}
		}
		return err
	}
	p := tea.NewProgram(NewModel(opts).withSession(s, history).withDiag(collector))
	_, err = p.Run()
	return err
}
