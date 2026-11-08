// Package hostcmd implements the `pigo serve` and `pigo attach` subcommands:
// the host daemon and its line-based client (docs/architecture.md §6).
//
//	pigo serve                         # run the host in the foreground
//	pigo serve --daemon                # detach it and return immediately
//	pigo serve status                  # pid, socket, live session count
//	pigo serve stop                    # graceful shutdown
//	pigo attach [session-id]           # talk to a running host
//
// The split exists so a UI never assembles providers, tools or sessions itself:
// `serve` owns execution and persistence, `attach` is a thin client over
// internal/hostclient.
package hostcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/diag"
	"github.com/smallnest/pigo/internal/hostclient"
	"github.com/smallnest/pigo/internal/hostpath"
	"github.com/smallnest/pigo/internal/hostserver"
	"github.com/smallnest/pigo/internal/trust"
)

// ServeOptions is the resolved configuration for `pigo serve`.
type ServeOptions struct {
	// Dir is the host state directory; empty means hostpath.DefaultDir().
	Dir string
	// Daemon asks the parent process to detach the host and exit.
	Daemon bool
	// Approve pre-grants session trust for the launch directory, mirroring
	// --approve: side-effect tools then skip the permission prompt.
	Approve bool
	// Build is reported to clients.
	Build string
	// Factory assembles the agent behind each session. Required.
	Factory hostserver.Factory
	// LogPath is where a daemon's stdout/stderr go. Empty means the default
	// under Dir.
	LogPath string
	// ReadyFile, when set, is created once the daemon is listening. It is the
	// parent's signal that the child is up (tests use it too).
	ReadyFile string
	// ChildArgs is the parent's own argv after "serve", replayed for the daemon
	// child minus --daemon. The parent fills it from os.Args.
	ChildArgs []string
}

// Run dispatches a `serve` invocation. args are the positional arguments that
// followed the subcommand (empty, "start", "status", "stop").
func Run(args []string, opts ServeOptions, out, errOut io.Writer) int {
	action := "start"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "start":
		return runStart(opts, out, errOut)
	case "status":
		return runStatus(opts, out, errOut)
	case "stop":
		return runStop(opts, out, errOut)
	default:
		fmt.Fprintf(errOut, "pigo serve: unknown action %q (want start, status or stop)\n", action)
		return 2
	}
}

// runStart starts the host, in the foreground or detached.
func runStart(opts ServeOptions, out, errOut io.Writer) int {
	dir := opts.Dir
	if dir == "" {
		dir = hostpath.DefaultDir()
	}
	if dir == "" {
		fmt.Fprintln(errOut, "pigo serve: cannot resolve the state directory (set PIGO_HOME)")
		return 1
	}
	if opts.Daemon {
		return startDaemon(opts, dir, out, errOut)
	}

	// The trust manager is built here (rather than left to NewServer) whenever
	// --approve needs it, because session trust lives in the manager instance:
	// granting it on a throwaway copy would leave the server's own gate untouched.
	var mgr *trust.Manager
	if opts.Approve {
		m, terr := trust.NewManager(trust.DefaultPath())
		if terr != nil {
			// Trust is a safety gate, not a config: a failure here means side-effect
			// tools will prompt (and, with no client attached, be denied), so it is
			// a diagnostic rather than a startup failure.
			diag.Diagf("pigo serve: trust: %v", terr)
		} else if cwd, err := os.Getwd(); err == nil {
			m.SetSessionTrust(cwd)
			mgr = m
		}
	}

	srv, err := hostserver.NewServer(hostserver.Config{
		Dir:     dir,
		Factory: opts.Factory,
		Trust:   mgr,
		Daemon:  false,
		Build:   opts.Build,
	})
	if err != nil {
		fmt.Fprintf(errOut, "pigo serve: %v\n", err)
		return 1
	}
	if err := srv.Listen(); err != nil {
		fmt.Fprintf(errOut, "pigo serve: %v\n", err)
		return 1
	}
	if opts.ReadyFile == "" {
		opts.ReadyFile = readyPathFromEnv()
	}
	if opts.ReadyFile != "" {
		_ = os.WriteFile(opts.ReadyFile, []byte(srv.Socket()), 0o600)
	}
	fmt.Fprintf(out, "pigo host listening on %s (pid %d)\n", srv.Socket(), os.Getpid())

	// SIGTERM/SIGINT shut down gracefully so attached clients get a reason and
	// the socket/token files are removed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintf(errOut, "pigo serve: %v\n", err)
		return 1
	}
	return 0
}

// runStatus reports the host without needing the token for the common case: a
// live connection when one is possible, otherwise the state file.
func runStatus(opts ServeOptions, out, errOut io.Writer) int {
	dir := opts.Dir
	if dir == "" {
		dir = hostpath.DefaultDir()
	}
	c, err := hostclient.Connect(hostclient.Options{Dir: dir, Client: "serve status"})
	if err == nil {
		defer c.Close()
		st, serr := c.ServerStatus()
		if serr == nil {
			fmt.Fprintf(out, "pid %d, up since %s, %d live session(s), socket %s\n",
				st.PID, st.Started.Local().Format(time.RFC3339), st.Sessions, st.Socket)
			return 0
		}
	}
	st, rerr := hostpath.ReadState(dir)
	if rerr != nil {
		fmt.Fprintln(out, "no pigo host is running")
		return 1
	}
	// A state file without a live socket is what a crashed host leaves behind.
	fmt.Fprintf(out, "pigo host pid %d is not responding (socket %s); run `pigo serve stop` to clear it\n", st.PID, st.Socket)
	return 1
}

// runStop asks the running host to shut down.
func runStop(opts ServeOptions, out, errOut io.Writer) int {
	dir := opts.Dir
	if dir == "" {
		dir = hostpath.DefaultDir()
	}
	c, err := hostclient.Connect(hostclient.Options{Dir: dir, Client: "serve stop"})
	if err != nil {
		fmt.Fprintln(errOut, "pigo serve stop: no running host")
		return 1
	}
	defer c.Close()
	if err := c.Shutdown(); err != nil {
		fmt.Fprintf(errOut, "pigo serve stop: %v\n", err)
		return 1
	}
	// Wait for the files to go away so `stop` returning means the host is gone.
	// Probing the socket is not enough: Shutdown closes the listener first (which
	// makes a dial fail immediately) and only removes the socket, state and token
	// afterwards, so a caller could otherwise observe a half-removed host.
	deadline := time.Now().Add(5 * time.Second)
	for hostpath.Probe(hostpath.SocketPath(dir), 100*time.Millisecond) || !hostFilesGone(dir) {
		if time.Now().After(deadline) {
			fmt.Fprintln(errOut, "pigo serve stop: host did not exit in time")
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Fprintln(out, "pigo host stopped")
	return 0
}

// hostFilesGone reports whether the host's socket, state and token files are all
// removed. It is what makes `serve stop` mean "fully shut down" rather than
// "stopped accepting".
func hostFilesGone(dir string) bool {
	for _, path := range []string{hostpath.SocketPath(dir), hostpath.StatePath(dir), hostpath.TokenPath(dir)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// FactoryOptions carries everything the CLI resolved that the daemon needs to
// assemble sessions. It exists so the CLI keeps all provider/config knowledge
// and the host package never parses a flag.
type FactoryOptions struct {
	Model              string
	BaseURL            string
	Protocol           string
	Provider           string
	APIKey             string
	ThinkingLevel      string
	NoTools            bool
	NoSkills           bool
	Memory             bool
	Policy             run.ToolPolicy
	SystemPrompt       string
	AppendSystemPrompt []string
}

// NewFactory builds the production agent factory from the CLI's resolved
// settings.
func NewFactory(o FactoryOptions) (*hostserver.DefaultFactory, error) {
	level, err := run.ResolveThinkingLevel(o.ThinkingLevel)
	if err != nil {
		return nil, err
	}
	return &hostserver.DefaultFactory{
		Model:              o.Model,
		BaseURL:            o.BaseURL,
		Protocol:           o.Protocol,
		Provider:           o.Provider,
		APIKey:             o.APIKey,
		Thinking:           level,
		NoTools:            o.NoTools,
		NoSkills:           o.NoSkills,
		Memory:             o.Memory,
		Policy:             o.Policy,
		SystemPrompt:       o.SystemPrompt,
		AppendSystemPrompt: o.AppendSystemPrompt,
	}, nil
}
