// Tests for the TUI's reload path: applying a rebuilt environment to a live
// session, the change detection that triggers it before a turn, and the model's
// transcript/registry handling of a finished reload.
package tui

import (
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/cli/run"
)

// newReloadSession assembles a TUI session over the given settings plus a seeded
// reloader, isolating every on-disk location under temp dirs.
func newReloadSession(t *testing.T, settings run.ReloadSettings, watched ...string) (*runSession, *run.Reloader) {
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

	s, _, err := newRunSessionWithStore(newTestStore(t), Options{
		Model:        "openrouter/free",
		ProviderName: env.ProviderName,
		Provider:     env.Provider,
		Tools:        env.Tools,
		SysPrompt:    env.SysPrompt,
		Reloader:     reloader,
	})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	return s, reloader
}

// TestApplyReloadSwapsSessionState verifies a reload swaps the tool set, registry,
// prompt, live model and slash registry, while keeping the session's own state.
func TestApplyReloadSwapsSessionState(t *testing.T) {
	s, _ := newReloadSession(t, run.ReloadSettings{
		Model:        "openrouter/free",
		NoSkills:     true,
		SystemPrompt: "prompt two",
		Thinking:     "high",
	})
	if !s.configChanged() {
		// The loader always yields the same settings, so nothing changed on disk.
		t.Log("configChanged is false, as expected for a stub loader")
	}
	res, err := s.reloader.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s.applyReload(res)

	if !strings.Contains(s.agentCtx.SystemPrompt, "prompt two") {
		t.Error("the session should adopt the reloaded system prompt")
	}
	if s.reg == nil || len(s.agentCtx.Tools) == 0 {
		t.Error("the session should adopt the reloaded tool set")
	}
	if s.live.Model != "openrouter/free" {
		t.Errorf("live model = %q, want the reloaded value", s.live.Model)
	}
	if s.live.ThinkingLevel != "high" {
		t.Errorf("live thinking = %q, want high", s.live.ThinkingLevel)
	}
	if s.slash == nil {
		t.Error("the slash registry should be rebuilt after a reload")
	}
	// buildConfig reads the swapped collaborators, so the next turn is assembled
	// from the new configuration.
	cfg := s.buildConfig()
	if cfg.Model != "openrouter/free" {
		t.Errorf("cfg.Model = %q, want the reloaded model", cfg.Model)
	}
}

// TestReloadCmdWithoutReloader verifies a session without a reloader reports the
// failure through the message channel instead of panicking.
func TestReloadCmdWithoutReloader(t *testing.T) {
	s := &runSession{}
	msg, ok := s.reloadCmd("hello")().(reloadDoneMsg)
	if !ok {
		t.Fatal("reloadCmd should yield a reloadDoneMsg")
	}
	if msg.err == nil {
		t.Error("a session without a reloader should report an error")
	}
	if msg.startPrompt != "hello" {
		t.Errorf("startPrompt = %q, want the pending turn", msg.startPrompt)
	}
}

// TestReloadDoneMsgAppliesResult verifies the model folds a successful reload into
// the transcript and its shared state (live config + slash registry).
func TestReloadDoneMsgAppliesResult(t *testing.T) {
	s, _ := newReloadSession(t, run.ReloadSettings{
		Model:        "openrouter/free",
		NoSkills:     true,
		SystemPrompt: "prompt two",
	})
	res, err := s.reloader.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	m := NewModel(Options{Model: "openrouter/free"}).withSession(s, nil)
	updated, _ := m.Update(reloadDoneMsg{res: res})
	got := updated.(Model)
	if !strings.Contains(got.session.agentCtx.SystemPrompt, "prompt two") {
		t.Error("the model should apply the reload to the session")
	}
	if got.live != got.session.live || got.slash != got.session.slash {
		t.Error("the model should rebind the live config and slash registry after a reload")
	}
}
