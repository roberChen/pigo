// Package diag is the single sink for pigo's non-fatal diagnostics: a bad
// config option, a plugin that failed to start, a hook layer that was disabled,
// a memory store that could not be opened, a checkpoint that could not be
// written.
//
// These used to be written straight to os.Stderr from wherever they happened
// (assembly, reload, background goroutines, the runtime loop). That is wrong for
// two reasons:
//
//   - The TUI owns the terminal while it runs (alt screen). A write from any
//     goroutine — a reload rebuilding hooks, an MCP refresh, a compaction
//     checkpoint — lands in the middle of the frame and corrupts the display.
//   - Even outside the TUI, pigo starts silently: a launch should not narrate
//     itself, and machine modes must keep stdout/stderr clean.
//
// So every such message goes through this package, which is silent by default
// and emits only when PIGO_DEBUG is set. A front-end may install its own sink
// instead (the TUI collects the lines into its transcript, so a debug session
// never has to smear the screen to show them).
package diag

import (
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	mu sync.RWMutex
	// sink is the explicitly installed writer. nil means "no front-end sink":
	// fall back to PIGO_DEBUG (stderr when set, discard otherwise).
	sink io.Writer
)

// DebugEnabled reports whether PIGO_DEBUG is set. It is evaluated on every call
// because a pigorc may export it while the config layers load.
func DebugEnabled() bool { return os.Getenv("PIGO_DEBUG") != "" }

// Writer returns the writer every diagnostic should go to. It is never nil, so
// it can be handed to code that takes an io.Writer (a hook dispatcher, a plugin
// manager, an MCP client) without nil checks.
func Writer() io.Writer {
	mu.RLock()
	w := sink
	mu.RUnlock()
	if w != nil {
		return w
	}
	if DebugEnabled() {
		return os.Stderr
	}
	return io.Discard
}

// Install routes every subsequent diagnostic to w for the rest of the process.
// A front-end calls it once, before the work that can emit diagnostics (the TUI
// installs a collector, machine modes leave it unset). Passing nil clears the
// sink, restoring the PIGO_DEBUG default.
func Install(w io.Writer) {
	mu.Lock()
	sink = w
	mu.Unlock()
}

// Diagf writes one "pigo: "-prefixed diagnostic line to the sink. The line is
// emitted with a single Write so concurrent goroutines cannot interleave inside
// a message. Multi-line messages (a pigorc's captured output) are written as
// they are, matching the previous stderr behavior.
func Diagf(format string, a ...any) {
	fmt.Fprintf(Writer(), "pigo: "+format+"\n", a...)
}
