// This file implements the diagnostic collector: a diag sink a front-end can
// install when it owns the terminal.
//
// The TUI paints into the alt screen, so a diagnostic written straight to
// stderr from any goroutine (a reload rebuilding hooks, an MCP refresh, a
// compaction checkpoint, a sub-agent's background job) lands in the middle of a
// frame and corrupts it. Installing a Collector turns those writes into queued
// lines the model drains on the UI goroutine and folds into the transcript.
//
// It keeps pigo's quiet-by-default contract: a collector built with keep=false
// swallows every write without retaining anything, which is exactly what
// "silent unless PIGO_DEBUG" means when the terminal cannot be used for it.
package diag

import (
	"strings"
	"sync"
)

// collectorMaxLines bounds the backlog so a chatty background failure (a
// reconnect loop, say) cannot grow memory without limit while the UI is busy.
const collectorMaxLines = 200

// collectorMaxLine bounds one line; a diagnostic is a message, not a dump.
const collectorMaxLine = 4096

// Collector is a diag sink that queues written lines for a front-end to render.
// A zero-value Collector is not usable; build one with Collect.
type Collector struct {
	keep bool

	mu      sync.Mutex
	partial string // unterminated tail of the previous Write
	lines   []string
	dropped int
	notify  chan struct{}
}

// Collect returns a collector to install with Install. keep controls whether the
// lines are retained for Drain: false makes it a discarding sink that still
// keeps every diagnostic off the terminal.
func Collect(keep bool) *Collector {
	return &Collector{keep: keep, notify: make(chan struct{}, 1)}
}

// Keeping reports whether the collector retains lines. A front-end uses it to
// skip arming a drain command it would never be woken by.
func (c *Collector) Keeping() bool { return c != nil && c.keep }

// Write implements io.Writer so a diagnostic is never counted as a write error.
// It splits on newlines (a Write may carry several lines, or half of one), caps
// each line, and wakes the waiting front-end at most once per burst.
func (c *Collector) Write(p []byte) (int, error) {
	n := len(p)
	if c == nil || !c.keep {
		return n, nil
	}
	c.mu.Lock()
	text := c.partial + string(p)
	c.partial = ""
	lines := strings.Split(text, "\n")
	// The last element is either "" (text ended with a newline) or the start of
	// a line whose remainder arrives in the next Write.
	if last := lines[len(lines)-1]; last != "" {
		c.partial = last
	}
	lines = lines[:len(lines)-1]
	queued := 0
	for _, line := range lines {
		if c.appendLocked(strings.TrimRight(line, "\r")) {
			queued++
		}
	}
	// Wake the reader only when something is actually waiting for it: a write of
	// just a newline (or of capped-away blank lines) must not produce a spurious
	// wakeup, or a front-end that re-arms on every drain would spin.
	if queued > 0 {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()
	return n, nil
}

// appendLocked queues one line, dropping the oldest when the backlog is full so
// the newest diagnostics (usually the ones that matter) survive. It reports
// whether a line was queued (blank lines are skipped).
func (c *Collector) appendLocked(line string) bool {
	if line == "" {
		return false
	}
	if len(line) > collectorMaxLine {
		line = line[:collectorMaxLine] + "…"
	}
	if len(c.lines) >= collectorMaxLines {
		c.lines = append(c.lines[:0], c.lines[1:]...)
		c.dropped++
	}
	c.lines = append(c.lines, line)
	return true
}

// Drain returns and clears the queued lines. A dropped-line marker is prepended
// when the backlog overflowed, so the reader knows the list is partial.
func (c *Collector) Drain() []string {
	if c == nil || !c.keep {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.lines
	c.lines = nil
	if c.dropped > 0 {
		out = append([]string{"(" + itoa(c.dropped) + " earlier diagnostics dropped)"}, out...)
		c.dropped = 0
	}
	return out
}

// Wait returns the channel the front-end blocks on between drains. A send
// happens once per write burst, and Drain consumes the queued lines, so a token
// still buffered when the reader wakes makes the next drain immediate.
func (c *Collector) Wait() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.notify
}

// itoa is strconv.Itoa without pulling strconv into this leaf package's import
// set for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
