// This file implements the detach side of `pigo serve --daemon`: re-exec the
// binary with a marker environment variable, put it in its own session so the
// controlling terminal closing does not kill it, and redirect its output to a
// log file.
//
// The child is the same binary running the same code path, only without the
// --daemon flag being honoured (the marker env var stops the recursion). That is
// deliberate: a daemon that is the same `pigo serve` has exactly one
// implementation to test, and the log contains the same startup line a
// foreground run prints.
package hostcmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// daemonChildEnv marks a re-exec'd host so runStart does not detach again.
const daemonChildEnv = "PIGO_HOST_DAEMON_CHILD"

// logPath is where a detached host writes its stdout/stderr. It lives beside the
// socket and token so one directory holds everything about the host.
func logPath(dir string) string { return filepath.Join(dir, "server.log") }

// startDaemon spawns the detached host and waits until it is listening.
func startDaemon(opts ServeOptions, dir string, out, errOut io.Writer) int {
	if os.Getenv(daemonChildEnv) == "1" {
		// Should be unreachable: main clears --daemon for a child. Treating it as
		// foreground is the safe fallback (it serves instead of forking forever).
		opts.Daemon = false
		return runStart(opts, out, errOut)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(errOut, "pigo serve: locate executable: %v\n", err)
		return 1
	}
	ready := opts.ReadyFile
	if ready == "" {
		// A private file in the state dir: the child writes the socket path once
		// it listens, the parent waits for it, then removes it.
		ready = filepath.Join(dir, ".daemon-ready")
	}
	_ = os.Remove(ready)

	logf, err := os.OpenFile(logPath(dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(errOut, "pigo serve: open log: %v\n", err)
		return 1
	}
	defer logf.Close()

	// Rebuild the child's argv from the parent's: ["serve"] plus the flags this
	// process was given, minus --daemon (the child serves in the foreground), and
	// with an explicit --dir so the child writes its socket/token where the parent
	// and every later client will look for them.
	childArgs := append([]string{"serve"}, stripFlag(opts.ChildArgs, "--daemon")...)
	if opts.Dir != "" && !hasFlag(childArgs, "--dir") {
		childArgs = append(childArgs, "--dir", opts.Dir)
	}
	cmd := exec.Command(exe, childArgs...)
	cmd.Env = append(os.Environ(), daemonChildEnv+"=1", "PIGO_HOST_READY="+ready)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	// The host must outlive the terminal that started it; how that is achieved
	// differs per platform (see detachProcess).
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(errOut, "pigo serve: start daemon: %v\n", err)
		return 1
	}
	// The child is detached: never Wait (it outlives us), but do reap it if it
	// exits immediately so the failure is reported instead of silently ignored.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if data, err := os.ReadFile(ready); err == nil && len(data) > 0 {
				_ = os.Remove(ready)
				fmt.Fprintf(out, "pigo host listening on %s (pid %d, log %s)\n", string(data), cmd.Process.Pid, logPath(dir))
				return 0
			}
		case err := <-exited:
			_ = os.Remove(ready)
			fmt.Fprintf(errOut, "pigo serve: daemon exited during startup: %v (see %s)\n", err, logPath(dir))
			return 1
		case <-deadline:
			_ = os.Remove(ready)
			// Leave the child running: a slow start is not a failure, and killing it
			// here could destroy a host that just became ready.
			fmt.Fprintf(errOut, "pigo serve: daemon did not report readiness in 10s (see %s)\n", logPath(dir))
			return 1
		}
	}
}

// stripFlag removes every spelling of name (--x and --x=value) from args.
func stripFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// hasFlag reports whether args already sets name (in either spelling).
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// readyPathFromEnv returns the readiness file the parent asked for.
func readyPathFromEnv() string { return os.Getenv("PIGO_HOST_READY") }
