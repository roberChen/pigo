// This file implements `pigo attach`: the line-based client that proves the
// host split is real. It speaks only internal/hostclient, so it never links the
// run assembly — the same property the TUI and a future web UI need.
//
//	pigo attach                  # new session in the current directory
//	pigo attach <session-id>     # resume a persisted conversation
//	pigo attach --list           # list the sessions this connection owns
//
// Inside the prompt, `/exit` or Ctrl-D detaches (the session keeps its history)
// and Ctrl-C cancels the running turn.
//
// The concurrency is not incidental: a host turn may ask for permission while it
// is running, so the client cannot sit inside a blocking Prompt call. One
// goroutine pumps stdin into a channel and the loop multiplexes over input,
// permission requests, and turn completion. That also gives Ctrl-C a place to
// cancel without tearing the connection down.
package hostcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostclient"
)

// AttachOptions configures `pigo attach`.
type AttachOptions struct {
	// Dir is the host state directory; empty means hostpath.DefaultDir().
	Dir string
	// SessionID resumes a persisted session; empty starts a new one.
	SessionID string
	// Cwd roots a new session; empty means the host's own working directory.
	Cwd string
	// Model overrides the host's default model for a new session.
	Model string
	// List prints the owning connection's sessions and exits.
	List bool
}

// Attach runs the line client against a running host until the user detaches.
func Attach(opts AttachOptions, in io.Reader, out, errOut io.Writer) int {
	c, err := hostclient.Connect(hostclient.Options{Dir: opts.Dir, Client: "attach"})
	if err != nil {
		fmt.Fprintf(errOut, "pigo attach: %v\n", err)
		return 1
	}
	defer c.Close()

	info := c.ServerInfo()
	fmt.Fprintf(out, "attached to pigo host pid %d (protocol v%d)\n", info.PID, info.Version)

	if opts.List {
		sessions, err := c.ListSessions()
		if err != nil {
			fmt.Fprintf(errOut, "pigo attach: %v\n", err)
			return 1
		}
		if len(sessions) == 0 {
			fmt.Fprintln(out, "no sessions on this connection")
			return 0
		}
		for _, s := range sessions {
			fmt.Fprintf(out, "%s  %s  %s  %s\n", s.ID, s.UpdatedAt.Local().Format("2006-01-02 15:04"), s.Model, s.Title)
		}
		return 0
	}

	a := newAttacher(c, out, errOut)
	return a.run(opts, in)
}

// attacher holds the renderer and turn state for one attached client.
type attacher struct {
	client *hostclient.Client
	out    io.Writer
	errOut io.Writer

	// mu guards every field below. Events arrive on the client's dispatcher
	// goroutine while the loop reads stdin, so all shared state goes through it.
	mu sync.Mutex
	// atLineStart tracks whether the last byte written was a newline, so a delta
	// is not glued to a prompt marker and tool lines start cleanly.
	atLineStart bool
	// perm is the permission request awaiting an answer from the loop, or nil.
	perm *hostapi.PermissionRequest
	// permSig wakes the loop when perm is set; the question is asked from the
	// client's dispatcher goroutine, the answer taken from the loop.
	permSig chan struct{}
	// queued holds lines typed while a turn was running. They are the natural
	// answer to a permission question that arrives later (a user types ahead), and
	// otherwise get processed once the turn settles — never dropped.
	queued []string
	// turnCancel aborts the in-flight turn, or nil when idle.
	turnCancel context.CancelFunc
	// shutDown is set by a server_shutdown event so the loop can stop instead of
	// prompting a host that is going away.
	shutDown bool
}

func newAttacher(c *hostclient.Client, out, errOut io.Writer) *attacher {
	return &attacher{
		client:      c,
		out:         out,
		errOut:      errOut,
		atLineStart: true,
		permSig:     make(chan struct{}, 1),
	}
}

// printOut writes to the transcript under mu, which is what makes it safe to call
// from the input loop and the signal goroutine. The render and onPermission
// callbacks already run with mu held (they are invoked on the client's dispatcher
// goroutine), so they write to a.out directly instead: mu is not reentrant and
// calling printOut from there would deadlock the dispatcher.
func (a *attacher) printOut(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintf(a.out, format, args...)
}

// printErr is the same for diagnostics, which own a separate writer.
func (a *attacher) printErr(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintf(a.errOut, format, args...)
}

// input is one line read from stdin, or the reader's terminal error.
type input struct {
	line string
	err  error
}

// turnResult is the outcome of one prompt request.
type turnResult struct {
	res hostapi.PromptResult
	err error
}

// run wires the renderer and drives the loop until the user detaches.
func (a *attacher) run(opts AttachOptions, in io.Reader) int {
	sessionID := opts.SessionID
	if sessionID != "" {
		rr, err := a.client.Resume(sessionID)
		if err != nil {
			a.printErr("pigo attach: resume %s: %v\n", sessionID, err)
			return 1
		}
		a.printOut("resumed %s (%d messages, %s) in %s\n", rr.Session.ID, len(rr.Messages), rr.Session.Model, rr.Session.Cwd)
	} else {
		info, err := a.client.CreateSession(opts.Cwd, opts.Model, "")
		if err != nil {
			a.printErr("pigo attach: create session: %v\n", err)
			return 1
		}
		sessionID = info.ID
		a.printOut("session %s in %s\n", sessionID, info.Cwd)
	}
	a.installHandlers()

	lines := make(chan input)
	go pumpLines(in, lines)
	turnDone := make(chan turnResult, 1)

	// Ctrl-C cancels the running turn (or detaches when idle). Only installed for
	// a real terminal: tests drive Attach with a buffer, and a process-wide signal
	// handler there would be both useless and racy.
	if f, ok := in.(*os.File); ok && f == os.Stdin {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		defer signal.Stop(sigs)
		go func() {
			for range sigs {
				if a.cancelTurn() {
					a.printOut("\n[cancelling…]\n")
				} else {
					// Idle Ctrl-C with nothing to cancel: treat as detach so the user
					// is never stuck (the REPL's behaviour is the same).
					_ = a.client.Close()
				}
			}
		}()
	}

	// One select loop, three sources: user input, a permission request from the
	// host, and the running turn finishing. A single loop (rather than a nested
	// "wait for the turn" loop) is what lets a permission question arrive while
	// the turn is mid-flight without either side starving.
	exit := false
	running := false
	// exitPending is set when stdin closed while a turn was running: finish the
	// turn, then detach.
	exitPending := false
	for !exit {
		if a.isShutDown() {
			a.printOut("\nhost is gone; detaching\n")
			break
		}
		a.printOut("\n> ")
		select {
		case <-a.client.Done():
			// The connection is gone: no permission answer and no turn result can
			// arrive any more, and prompting again would block on stdin forever
			// (a crashed host sends nothing at all). The session is already
			// persisted on the host side, so this is a detach, not a loss.
			a.printOut("\nhost is gone; detaching\n")
			exit = true

		case <-a.permSig:
			// A question is on screen. Its answer is either already queued (the
			// user typed ahead) or the next line from stdin.
			req := a.takePermission()
			if req == nil {
				continue
			}
			if reply, ok := a.popQueued(); ok {
				a.answerPermission(req, reply)
				continue
			}
			if lines == nil {
				// stdin is already gone, so no line can ever answer this question.
				// Denying is the only safe answer, and waiting here would block on a
				// channel nobody will write to (the turn would never settle).
				a.answerPermission(req, "")
				continue
			}
			select {
			case in, ok := <-lines:
				if !ok {
					// Detaching with a question on screen denies it; silence must
					// never grant a tool.
					a.answerPermission(req, "")
					lines = nil
					continue
				}
				a.answerPermission(req, in.line)
			case <-a.client.Done():
				return 0
			}

		case in, ok := <-lines:
			if !ok {
				// stdin is gone. With a turn in flight, let it settle first rather
				// than dropping the connection under it.
				lines = nil
				if req := a.takePermission(); req != nil {
					a.answerPermission(req, "")
				}
				if running {
					exitPending = true
				} else {
					exit = true
				}
				continue
			}
			if in.err != nil {
				// The reader is unusable from here on: pumpLines closes the channel
				// next, so falling into the closed-channel case detaches (or lets a
				// running turn settle) exactly like EOF. Clearing `lines` instead
				// would leave the loop blocked on a channel nobody will write to.
				a.printErr("\npigo attach: read input: %v\n", in.err)
				continue
			}
			if running {
				// Typed ahead: hold it. It may be this turn's permission answer,
				// and if it is not it becomes the next input instead of vanishing.
				a.pushQueued(in.line)
				if req := a.takePermission(); req != nil {
					if reply, ok := a.popQueued(); ok {
						a.answerPermission(req, reply)
					}
				}
				continue
			}
			if keepGoing := a.handleLine(sessionID, in.line, turnDone, &running); !keepGoing {
				exit = true
			}

		case r := <-turnDone:
			running = false
			a.finishTurn(r)
			// Anything typed while the turn ran is processed now, in order. Stop as
			// soon as a line starts the next turn: startTurn refuses a second
			// concurrent turn, so continuing here would report "a turn is already
			// running" and drop the rest. The remainder stays queued for when that
			// turn completes.
			for !exit {
				line, ok := a.popQueued()
				if !ok {
					break
				}
				if !a.handleLine(sessionID, line, turnDone, &running) {
					exit = true
					break
				}
				if running {
					break
				}
			}
			// EOF was seen while a turn was running. If the queue started another
			// turn (typed-ahead prompts), let it finish too instead of detaching
			// under it; the next turnDone re-enters here with an empty queue.
			if exitPending && !running {
				exit = true
			}
		}
	}
	// A detached session is not deleted: its history is on disk and
	// `attach <id>` (or the REPL's --resume) picks it up.
	a.printOut("\ndetached from %s\n", sessionID)
	return 0
}

// handleLine applies one input line when no turn is running. It reports whether
// the session should continue (false ends the loop) and updates *running when it
// starts a turn.
func (a *attacher) handleLine(sessionID, raw string, turnDone chan<- turnResult, running *bool) bool {
	line := strings.TrimSpace(raw)
	switch {
	case line == "":
		return true
	case line == "/exit" || line == "/quit":
		return false
	case line == "/sessions":
		a.listSessions()
		return true
	}
	if a.startTurn(sessionID, line, turnDone) {
		*running = true
		return true
	}
	a.printOut("  (a turn is already running; Ctrl-C cancels it)\n")
	return true
}

// pushQueued records a line typed while a turn was running.
func (a *attacher) pushQueued(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queued = append(a.queued, line)
}

// popQueued returns the oldest queued line, if any.
func (a *attacher) popQueued() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queued) == 0 {
		return "", false
	}
	line := a.queued[0]
	a.queued = a.queued[1:]
	return line, true
}

// pumpLines turns a reader into a channel of lines. The goroutine owns the
// reader for the whole session, which is what makes the loop free to multiplex.
func pumpLines(in io.Reader, out chan<- input) {
	defer close(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		out <- input{line: sc.Text()}
	}
	if err := sc.Err(); err != nil {
		if !errors.Is(err, io.EOF) {
			out <- input{err: err}
		}
	}
}

// startTurn launches one prompt request in the background.
func (a *attacher) startTurn(sessionID, text string, done chan<- turnResult) bool {
	a.mu.Lock()
	if a.turnCancel != nil {
		a.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.turnCancel = cancel
	a.mu.Unlock()

	go func() {
		res, err := a.client.Prompt(ctx, sessionID, text)
		cancel()
		a.mu.Lock()
		a.turnCancel = nil
		a.mu.Unlock()
		done <- turnResult{res: res, err: err}
	}()
	return true
}

// cancelTurn aborts the in-flight turn, reporting whether one was running.
func (a *attacher) cancelTurn() bool {
	a.mu.Lock()
	cancel := a.turnCancel
	a.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// finishTurn reports the tail of a completed turn.
func (a *attacher) finishTurn(r turnResult) {
	if r.err != nil {
		if errors.Is(r.err, context.Canceled) {
			a.printOut("\n[cancelled]\n")
			return
		}
		a.printErr("\npigo attach: %v\n", r.err)
		return
	}
	a.mu.Lock()
	atStart := a.atLineStart
	a.mu.Unlock()
	if !atStart {
		a.printOut("\n")
	}
	if r.res.Error != "" {
		a.printOut("[error: %s]\n", r.res.Error)
	}
	if r.res.StopReason != "" && r.res.StopReason != agentcore.StopReasonEndTurn {
		a.printOut("[%s]\n", r.res.StopReason)
	}
}

// listSessions prints the sessions this connection owns.
func (a *attacher) listSessions() {
	sessions, err := a.client.ListSessions()
	if err != nil {
		a.printErr("\n%v\n", err)
		return
	}
	if len(sessions) == 0 {
		a.printOut("  no sessions on this connection\n")
		return
	}
	for _, s := range sessions {
		a.printOut("\n%s  %s  %s", s.ID, s.Model, s.Title)
	}
}

// takePermission returns the question awaiting an answer and clears it.
func (a *attacher) takePermission() *hostapi.PermissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	req := a.perm
	a.perm = nil
	return req
}

// isShutDown reports whether the host announced it is going away.
func (a *attacher) isShutDown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shutDown
}

// answerPermission interprets the user's reply to a permission question.
//
// Anything other than an explicit yes/always denies: a typo, an empty line, or a
// detach must never widen what a tool may do.
func (a *attacher) answerPermission(req *hostapi.PermissionRequest, reply string) {
	approve, always := false, false
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "y", "yes":
		approve = true
	case "a", "always":
		approve, always = true, true
	}
	if err := a.client.Decide(req.SessionID, req.RequestID, approve, always); err != nil {
		a.printErr("  (could not answer: %v)\n", err)
		return
	}
	switch {
	case always:
		a.printOut("  allowed for this session\n")
	case approve:
		a.printOut("  allowed once\n")
	default:
		a.printOut("  denied\n")
	}
}

// installHandlers subscribes the renderer to session and permission events.
func (a *attacher) installHandlers() {
	a.client.OnEvent(hostapi.EventSession, a.render)
	a.client.OnEvent(hostapi.EventPermission, a.onPermission)
	a.client.OnEvent(hostapi.EventServerShutdown, func(env hostapi.Envelope) {
		var ev hostapi.ServerShutdownEvent
		_ = env.DecodeParams(&ev)
		a.mu.Lock()
		a.shutDown = true
		fmt.Fprintf(a.out, "\nhost is shutting down: %s\n", ev.Reason)
		a.mu.Unlock()
	})
	a.client.OnEvent(hostapi.EventSessionClosed, func(env hostapi.Envelope) {
		var ev hostapi.SessionClosedEvent
		_ = env.DecodeParams(&ev)
		a.printOut("\nsession closed: %s\n", ev.Reason)
	})
}

// onPermission records a permission request and wakes the loop. It must not
// answer here: the loop owns stdin, and the answer has to come from the user.
func (a *attacher) onPermission(env hostapi.Envelope) {
	var req hostapi.PermissionRequest
	if err := env.DecodeParams(&req); err != nil {
		return
	}
	a.mu.Lock()
	a.perm = &req
	// Print while holding mu so the question cannot be interleaved with a text
	// delta from the same turn.
	fmt.Fprintf(a.out, "\n  \u26a0 %s wants to run outside a trusted directory\n    %s\n  allow? [y]es / [a]lways / [N]o: ",
		req.ToolName, strings.TrimSpace(req.Summary))
	a.atLineStart = false
	a.mu.Unlock()
	select {
	case a.permSig <- struct{}{}:
	default:
	}
}

// render draws one session event. Only the increments a terminal user needs are
// printed; the authoritative transcript is on disk.
func (a *attacher) render(env hostapi.Envelope) {
	var ev hostapi.SessionEvent
	if err := env.DecodeParams(&ev); err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Type {
	case hostapi.SessionEventTextDelta:
		fmt.Fprint(a.out, ev.Text)
		a.atLineStart = strings.HasSuffix(ev.Text, "\n")
	case hostapi.SessionEventThinking:
		// Thinking is dimmed in a full UI; a line client shows a marker so the
		// user knows the model is working rather than hung.
		if a.atLineStart {
			fmt.Fprint(a.out, "… ")
			a.atLineStart = false
		}
	case hostapi.SessionEventToolStart:
		if !a.atLineStart {
			fmt.Fprint(a.out, "\n")
		}
		fmt.Fprintf(a.out, "  ⚙ %s\n", ev.ToolName)
		a.atLineStart = true
	case hostapi.SessionEventToolEnd:
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		fmt.Fprintf(a.out, "  ⚙ %s → %s\n", ev.ToolName, status)
		a.atLineStart = true
	case hostapi.SessionEventCompaction:
		fmt.Fprintf(a.out, "  [compacted %d → %d tokens]\n", ev.TokensBefore, ev.TokensAfter)
		a.atLineStart = true
	case hostapi.SessionEventTurnEnd:
		// render already holds mu, so this writes directly: printOut would
		// self-deadlock on the non-reentrant mutex and wedge the dispatcher, which
		// is also the goroutine that would have to deliver every later event.
		if !a.atLineStart {
			fmt.Fprint(a.out, "\n")
			a.atLineStart = true
		}
	}
}
