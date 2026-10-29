// Tests for live configuration reload: change detection, the state a reload must
// preserve (background shell jobs, todo list, schedule, sub-agents, memory, plugins),
// the failure path that leaves the old configuration running, and the summary shown
// to the user.
package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/config"
)

// baseSettings is the assembly input the reload tests start from: a valid provider
// with no tools-side surprises.
func baseSettings() ReloadSettings {
	return ReloadSettings{
		Model:        "openrouter/free",
		NoSkills:     true,
		MemoryConfig: config.MemorySettings{},
	}
}

// newTestEnv assembles an environment the way a driver would at startup.
func newTestEnv(t *testing.T, systemPrompt string) Env {
	t.Helper()
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	config.SetCurrentMCP(nil)
	config.SetCurrentAgents(nil)
	env, err := SetupEnv("openrouter/free", "", "", "", "", false, true, systemPrompt, nil, false, ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	t.Cleanup(func() {
		if env.Plugins != nil {
			_ = env.Plugins.Close()
		}
		if env.Memory != nil {
			_ = env.Memory.Close()
		}
		if env.MCP != nil {
			_ = env.MCP.Close()
		}
	})
	return env
}

// TestReloaderRequiresSeed verifies a reloader with nothing to apply to fails
// instead of assembling an environment nobody asked for.
func TestReloaderRequiresSeed(t *testing.T) {
	r := NewReloader(func() (ReloadSettings, error) { return baseSettings(), nil }, nil)
	if _, err := r.Reload(); err == nil {
		t.Fatal("Reload without Seed should fail")
	}
	if r.Changed() {
		t.Error("a reloader with no watched files should never report a change")
	}
}

// TestReloaderPreservesSessionState verifies the promise that makes a reload safe:
// the tool set is rebuilt from the new configuration, but the state the session
// accumulated (the todo list here, and by identity the bash/schedule/sub-agent
// stores) survives.
func TestReloaderPreservesSessionState(t *testing.T) {
	env := newTestEnv(t, "system prompt one")
	todo := TodoStoreFromTools(env.Tools)
	todo.Set([]agenttool.TodoItem{{Content: "keep me", Status: agenttool.TodoPending}})
	bash := BashJobStoreFromTools(env.Tools)
	if bash == nil {
		t.Fatal("the bash tool should share a job store")
	}

	settings := baseSettings()
	settings.SystemPrompt = "system prompt two"
	r := NewReloader(func() (ReloadSettings, error) { return settings, nil }, nil)
	r.Seed(env)

	res, err := r.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !strings.Contains(res.Env.SysPrompt, "system prompt two") {
		t.Error("the rebuilt environment should use the new system prompt")
	}
	if got := TodoStoreFromTools(res.Env.Tools); got != todo {
		t.Error("the reload should keep the session's todo store")
	}
	if items := todo.Snapshot(); len(items) != 1 || items[0].Content != "keep me" {
		t.Errorf("todo list lost across reload: %+v", items)
	}
	if got := BashJobStoreFromTools(res.Env.Tools); got != bash {
		t.Error("the reload should keep the session's bash job store")
	}
	if res.Env.Schedule == nil || res.Env.SubAgentJobs == nil {
		t.Error("the rebuilt environment should carry the schedule and sub-agent registry")
	}
	// The report must describe the new run, not the old one.
	if res.Env.Report == nil {
		t.Fatal("the rebuilt environment should expose a config report")
	}
	if got := res.Env.Report.Snapshot.Thinking; got != settings.Thinking {
		t.Errorf("report thinking = %q, want the reloaded setting", got)
	}
}

// TestReloaderDiffSummary verifies the summary names a tool that appeared, so a
// user can tell a reload actually took effect.
func TestReloaderDiffSummary(t *testing.T) {
	env := newTestEnv(t, "prompt")
	settings := baseSettings()
	settings.Policy = ToolPolicy{Allow: []string{"read", "grep"}}
	r := NewReloader(func() (ReloadSettings, error) { return settings, nil }, nil)
	r.Seed(env)

	res, err := r.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	summary := strings.Join(res.Summary, "\n")
	if !strings.Contains(summary, "config reloaded") {
		t.Errorf("summary should open with the reload line:\n%s", summary)
	}
	if !strings.Contains(summary, "removed:") || !strings.Contains(summary, "bash") {
		t.Errorf("summary should name the tools the boundary removed:\n%s", summary)
	}
	if len(res.Env.Tools) >= len(env.Tools) {
		t.Errorf("the tool boundary should have narrowed the set (%d -> %d)", len(env.Tools), len(res.Env.Tools))
	}
}

// TestReloaderFailureKeepsOldConfiguration verifies an unassemblable configuration
// (here: a tool boundary naming a tool that does not exist) is rejected without
// touching the running one.
func TestReloaderFailureKeepsOldConfiguration(t *testing.T) {
	env := newTestEnv(t, "prompt")
	settings := baseSettings()
	settings.Policy = ToolPolicy{Allow: []string{"no-such-tool"}}
	r := NewReloader(func() (ReloadSettings, error) { return settings, nil }, nil)
	r.Seed(env)

	if _, err := r.Reload(); err == nil {
		t.Fatal("an unknown tool name in the boundary should fail the reload")
	}
	// The seeded assembly is still the baseline, so a corrected reload works.
	settings.Policy = ToolPolicy{}
	if _, err := r.Reload(); err != nil {
		t.Fatalf("reload after a failure: %v", err)
	}
}

// TestReloaderChangedDetectsEdits verifies change detection: an untouched file is
// quiet, an edited one is reported once, and a reload resets the baseline.
func TestReloaderChangedDetectsEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pigorc")
	if err := os.WriteFile(path, []byte("# v1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := newTestEnv(t, "prompt")

	r := NewReloader(func() (ReloadSettings, error) { return baseSettings(), nil }, []string{path})
	if r.Changed() {
		t.Fatal("nothing changed yet")
	}
	if err := os.WriteFile(path, []byte("# v2 longer\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !r.Changed() {
		t.Fatal("an edited config file should be reported as changed")
	}
	r.Seed(env)
	if _, err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if r.Changed() {
		t.Error("a completed reload should reset the baseline")
	}
}

// TestWatchedConfigFiles verifies the watch list names the files a session can
// really pick up (config.toml and both pigorc candidates at minimum).
func TestWatchedConfigFiles(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	cwd := t.TempDir()
	files := WatchedConfigFiles(cwd)
	want := map[string]bool{
		config.FileConfigPath():       false,
		config.DefaultPigorcPath():    false,
		config.ProjectPigorcPath(cwd): false,
	}
	for _, f := range files {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for path, seen := range want {
		if path != "" && !seen {
			t.Errorf("WatchedConfigFiles is missing %s (got %v)", path, files)
		}
	}
	// Creating a pigorc that did not exist must count as a change.
	r := NewReloader(func() (ReloadSettings, error) { return baseSettings(), nil }, []string{config.ProjectPigorcPath(cwd)})
	if r.Changed() {
		t.Fatal("no file yet")
	}
	if err := os.WriteFile(config.ProjectPigorcPath(cwd), []byte("option model x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !r.Changed() {
		t.Error("a newly created pigorc should be detected")
	}
}
