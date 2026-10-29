// Tests for the REPL's reload path: applying a freshly assembled environment to a
// live session, the messages it prints, and the failure path that leaves the
// running configuration alone.
package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/session"
)

// newReloadDeps assembles a minimal REPL session plus a seeded reloader over the
// given settings, with the config layers stubbed so no real file is read.
func newReloadDeps(t *testing.T, settings run.ReloadSettings, watched ...string) (*replDeps, *run.Reloader) {
	t.Helper()
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	config.SetCurrentMCP(nil)
	config.SetCurrentAgents(nil)

	env, err := run.SetupEnv("openrouter/free", "", "", "", "", false, true, "prompt one", nil, false, run.ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	t.Cleanup(func() {
		if env.Plugins != nil {
			_ = env.Plugins.Close()
		}
		if env.MCP != nil {
			_ = env.MCP.Close()
		}
	})

	reloader := run.NewReloader(func() (run.ReloadSettings, error) { return settings, nil }, watched)
	reloader.Seed(env)

	deps := &replDeps{
		agentCtx: &agentcore.AgentContext{SystemPrompt: env.SysPrompt, Tools: env.Tools},
		live:     &cli.LiveConfig{Model: "openrouter/free", ProviderName: env.ProviderName, Provider: env.Provider},
		header:   session.SessionHeader{ID: "test-session"},
		reg:      run.ToolRegistry(env.Tools),
		hookDeps: run.HookDeps{SessionID: "test-session"},
		reloader: reloader,
	}
	return deps, reloader
}

// TestReloadNowAppliesNewEnvironment verifies /reload swaps the session onto the
// rebuilt environment (tools, registry, prompt, live model) and prints the summary.
func TestReloadNowAppliesNewEnvironment(t *testing.T) {
	deps, _ := newReloadDeps(t, run.ReloadSettings{
		Model:        "openrouter/free",
		NoSkills:     true,
		SystemPrompt: "prompt two",
	})

	var out bytes.Buffer
	if !reloadNow(&out, deps) {
		t.Fatalf("reloadNow reported failure:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "config reloaded") {
		t.Errorf("output should include the reload summary:\n%s", out.String())
	}
	if !strings.Contains(deps.agentCtx.SystemPrompt, "prompt two") {
		t.Error("the session should adopt the reloaded system prompt")
	}
	if deps.reg == nil || len(deps.agentCtx.Tools) == 0 {
		t.Error("the session should adopt the reloaded tool set")
	}
	if deps.live.Model != "openrouter/free" {
		t.Errorf("live model = %q, want the reloaded value", deps.live.Model)
	}
}

// TestReloadNowWithoutReloader verifies /reload says it is unavailable instead of
// failing silently when the driver wired no reloader (tests, library use).
func TestReloadNowWithoutReloader(t *testing.T) {
	deps := &replDeps{agentCtx: &agentcore.AgentContext{}, live: &cli.LiveConfig{}}
	var out bytes.Buffer
	if reloadNow(&out, deps) {
		t.Fatal("reloadNow should report failure without a reloader")
	}
	if !strings.Contains(out.String(), "not available") {
		t.Errorf("output = %q, want an explanatory message", out.String())
	}
}

// TestReloadNowFailureKeepsSession verifies a rejected configuration leaves the
// session untouched and says so.
func TestReloadNowFailureKeepsSession(t *testing.T) {
	deps, _ := newReloadDeps(t, run.ReloadSettings{
		Model:  "openrouter/free",
		Policy: run.ToolPolicy{Allow: []string{"no-such-tool"}},
	})
	before := deps.agentCtx.SystemPrompt
	var out bytes.Buffer
	if reloadNow(&out, deps) {
		t.Fatal("reloadNow should report failure for an invalid boundary")
	}
	if !strings.Contains(out.String(), "still using the previous configuration") {
		t.Errorf("output = %q, want the keep-running notice", out.String())
	}
	if deps.agentCtx.SystemPrompt != before {
		t.Error("a failed reload must not change the session")
	}
}

// TestAutoReloadPicksUpEdits verifies the automatic path: an untouched session does
// nothing, and an edited config file is applied before the turn with a notice.
func TestAutoReloadPicksUpEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pigorc")
	if err := os.WriteFile(path, []byte("# v1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	deps, _ := newReloadDeps(t, run.ReloadSettings{
		Model:        "openrouter/free",
		NoSkills:     true,
		SystemPrompt: "prompt two",
	}, path)
	deps.reloader.RecordStamps()

	var out bytes.Buffer
	autoReload(&out, deps)
	if out.Len() != 0 {
		t.Errorf("an unchanged configuration should be silent, got %q", out.String())
	}

	if err := os.WriteFile(path, []byte("# v2 edited\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	autoReload(&out, deps)
	if !strings.Contains(out.String(), "configuration changed on disk") {
		t.Errorf("output = %q, want the automatic-reload notice", out.String())
	}
	if !strings.Contains(deps.agentCtx.SystemPrompt, "prompt two") {
		t.Error("the automatic reload should have been applied")
	}
}
