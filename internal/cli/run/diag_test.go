// Tests that run assembly reports its non-fatal diagnostics through the diag
// sink instead of writing them to stderr. The TUI owns the terminal while it
// runs, so a stray fmt.Fprintln(os.Stderr, ...) from assembly, reload or a
// background goroutine paints over a frame; routing everything through diag is
// what lets the TUI render the message in its transcript and keeps a normal
// launch silent.
package run

import (
	"bytes"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/diag"
)

// TestSetupEnvWarningsGoToDiagSink runs the assembly path that emits the
// "--no-tools ignores the policy" warning and verifies the line reaches an
// installed sink with the "pigo: " prefix, i.e. that it would be captured by the
// TUI's collector.
func TestSetupEnvWarningsGoToDiagSink(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_DEBUG", "") // the installed sink, not the debug default, must win

	var buf bytes.Buffer
	diag.Install(&buf)
	t.Cleanup(func() { diag.Install(nil) })

	// --no-tools alongside a policy is contradictory: the boundary cannot apply,
	// and the user is told so. A bad policy name must not fail here either, since
	// validation is skipped when the set is empty.
	if _, err := SetupEnv("openrouter/free", "", "", "", "", true /*noTools*/, true /*noSkills*/, "", nil, false, NewToolPolicy([]string{"read"}, nil)); err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "--no-tools disables all tools") {
		t.Fatalf("warning did not reach the diag sink: %q", got)
	}
	if !strings.HasPrefix(got, "pigo: ") {
		t.Errorf("diagnostic = %q, want the pigo: prefix", got)
	}
}
