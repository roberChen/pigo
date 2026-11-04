// Tests for the diagnostic sink: silent by default, PIGO_DEBUG opts into stderr,
// Install redirects, and the collector queues lines for a terminal-owning
// front-end instead of writing them.
package diag

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestWriterSilentByDefault verifies a launch with no PIGO_DEBUG and no installed
// sink discards diagnostics: pigo must not narrate itself, and machine modes
// need a clean stderr.
func TestWriterSilentByDefault(t *testing.T) {
	t.Setenv("PIGO_DEBUG", "")
	Install(nil)
	t.Cleanup(func() { Install(nil) })

	if got := Writer(); got != io.Discard {
		t.Fatalf("default writer = %T, want io.Discard", got)
	}
	if n, err := Writer().Write([]byte("boom\n")); n != 5 || err != nil {
		t.Errorf("discarding write = (%d, %v), want (5, nil)", n, err)
	}
}

// TestWriterDebugGoesToStderr verifies PIGO_DEBUG makes the default sink stderr,
// so a debug run still shows diagnostics even without a front-end redirect.
func TestWriterDebugGoesToStderr(t *testing.T) {
	t.Setenv("PIGO_DEBUG", "1")
	Install(nil)
	t.Cleanup(func() { Install(nil) })

	if got := Writer(); got != os.Stderr {
		t.Fatalf("debug writer = %T, want os.Stderr", got)
	}
}

// TestInstallOverridesDefault verifies an installed sink wins over PIGO_DEBUG and
// that Diagf prefixes and terminates the line exactly once.
func TestInstallOverridesDefault(t *testing.T) {
	t.Setenv("PIGO_DEBUG", "1")
	var buf bytes.Buffer
	Install(&buf)
	t.Cleanup(func() { Install(nil) })

	Diagf("could not load %s: %d", "hooks", 3)
	if got := buf.String(); got != "pigo: could not load hooks: 3\n" {
		t.Errorf("output = %q", got)
	}
}

// TestCollectorQueuesLines verifies the collector splits a write into lines,
// carries an unterminated tail into the next write, and hands every line to the
// front-end in order.
func TestCollectorQueuesLines(t *testing.T) {
	c := Collect(true)
	if !c.Keeping() {
		t.Fatal("a keeping collector should report Keeping")
	}
	if _, err := c.Write([]byte("first\nsec")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := c.Drain(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("drain after partial write = %q", got)
	}
	if _, err := c.Write([]byte("ond\n\nthird\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := c.Drain()
	if len(got) != 2 || got[0] != "second" || got[1] != "third" {
		t.Errorf("drain = %q, want [second third] (blank lines dropped)", got)
	}
	if rest := c.Drain(); len(rest) != 0 {
		t.Errorf("second drain = %q, want empty", rest)
	}
}

// TestCollectorNotifiesOncePerBurst verifies Wait is signalled for a batch of
// lines but stays quiet for an empty write, so the front-end neither spins nor
// misses work.
func TestCollectorNotifiesOncePerBurst(t *testing.T) {
	c := Collect(true)
	select {
	case <-c.Wait():
		t.Fatal("a fresh collector must not be ready")
	default:
	}
	c.Write([]byte("one\ntwo\n"))
	select {
	case <-c.Wait():
	default:
		t.Fatal("a write with lines should wake the reader")
	}
	// One token for the whole burst: the reader drains both lines at once.
	select {
	case <-c.Wait():
		t.Fatal("a burst should only notify once")
	default:
	}
	c.Write([]byte("\n"))
	select {
	case <-c.Wait():
		t.Fatal("a blank write should not wake the reader")
	default:
	}
}

// TestCollectorDiscardsWhenNotKeeping verifies keep=false is a sink that keeps
// nothing: this is how the TUI stays quiet without PIGO_DEBUG while still
// intercepting stderr.
func TestCollectorDiscardsWhenNotKeeping(t *testing.T) {
	c := Collect(false)
	if c.Keeping() {
		t.Error("keep=false should not report Keeping")
	}
	if n, err := c.Write([]byte("invisible\n")); err != nil || n != len("invisible\n") {
		t.Errorf("Write = (%d, %v), want the full length and no error", n, err)
	}
	if got := c.Drain(); len(got) != 0 {
		t.Errorf("Drain = %q, want empty", got)
	}
}

// TestCollectorBoundsBacklog verifies a chatty source cannot grow the queue
// without limit and that the reader is told lines were dropped.
func TestCollectorBoundsBacklog(t *testing.T) {
	c := Collect(true)
	var many strings.Builder
	for i := 0; i < collectorMaxLines+10; i++ {
		many.WriteString("line\n")
	}
	c.Write([]byte(many.String()))
	got := c.Drain()
	if len(got) != collectorMaxLines+1 {
		t.Fatalf("drained %d lines, want %d (cap + dropped marker)", len(got), collectorMaxLines+1)
	}
	if !strings.Contains(got[0], "earlier diagnostics dropped") {
		t.Errorf("first line = %q, want the dropped-lines marker", got[0])
	}
}

// TestNilCollectorIsSafe verifies the nil receiver works: a front-end that wired
// no collector must not panic on a diagnostic.
func TestNilCollectorIsSafe(t *testing.T) {
	var c *Collector
	if c.Keeping() || c.Drain() != nil || c.Wait() != nil {
		t.Error("nil collector should report nothing and stay inert")
	}
	if n, err := c.Write([]byte("x\n")); n != 2 || err != nil {
		t.Errorf("nil Write = (%d, %v)", n, err)
	}
}
