// This file implements the exit hint both interactive front-ends print when a
// session ends: the command that reopens it. Quitting is not the end of the
// conversation — every turn is already on disk — so the last line tells the
// operator how to get back to it without going through /resume or
// --list-sessions.
//
// The hint is printed on the way out and nowhere else: the REPL after its prompt
// loop returns, the TUI after the alt screen is gone, so the line lands in the
// terminal's scrollback next to the conversation it refers to. Machine modes
// (-p, stream-json) never print it; their stdout is a contract.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/roberChen/pigo/internal/cli/ui"
	"github.com/roberChen/pigo/internal/session"
)

// PrintResumeHint writes the command that reopens header's session to w. It is
// silent when there is nothing to reopen: no session, a sub-agent session
// (resuming one directly is refused — the parent agent continues it), or a
// session that never persisted a turn, where the printed command would only
// fail because no file exists.
func PrintResumeHint(w io.Writer, store *session.Store, header session.SessionHeader) {
	if w == nil || store == nil || header.ID == "" || header.IsSubAgent() {
		return
	}
	if !store.Exists(header.ID) {
		return
	}
	color := ui.Enabled()
	fmt.Fprintf(w, "\n%s %s\n",
		ui.Colorize(color, ui.Dim, "to resume this session:"),
		ui.Colorize(color, ui.Cyan, ResumeCommand(header.ID)))
}

// ResumeCommand is the copy-pasteable command that reopens session id.
func ResumeCommand(id string) string {
	return programName() + " --resume " + shellArg(id)
}

// programName repeats how pigo was invoked, so the hint works in the shell the
// operator is sitting in: a path invocation (./pigo, /usr/local/bin/pigo) is
// kept verbatim even when pigo is not on PATH, while a temporary build — `go
// run` compiles into …/b001/exe/pigo, `go test` into <pkg>.test — collapses to
// the plain name, since that path is gone by the time the line is pasted.
func programName() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return "pigo"
	}
	arg := os.Args[0]
	if strings.HasSuffix(arg, ".test") || strings.Contains(arg, string(os.PathSeparator)+"exe"+string(os.PathSeparator)) {
		return "pigo"
	}
	return arg
}

// shellArg leaves s alone when it is already safe to paste into a POSIX shell
// and single-quotes it otherwise. Session ids are built from digits, letters and
// dashes, but an imported session keeps whatever id its file carried, and the
// hint must never splice an arbitrary string into a shell command unquoted.
func shellArg(s string) string {
	for _, r := range s {
		safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ':' || r == '+'
		if !safe {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	if s == "" {
		return "''"
	}
	return s
}
