// Tests for the configuration-layer reader that startup and live reload share:
// the precedence between the pigorc, config.toml and the flags, and the fact that
// a second read sees edits (which is what makes a reload meaningful).
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/provider"
)

// writeConfigFile writes text to $XDG_CONFIG_HOME/pigo/config.toml.
func writeConfigFile(t *testing.T, text string) {
	t.Helper()
	path := config.FileConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

// writePigorc writes text to $PIGO_HOME/pigorc.
func writePigorc(t *testing.T, text string) {
	t.Helper()
	home := os.Getenv("PIGO_HOME")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "pigorc"), []byte(text), 0o600); err != nil {
		t.Fatalf("write pigorc: %v", err)
	}
}

// TestResolveConfigReadsEdits verifies a second read sees edited files, which is
// the whole point of the reload path: the loader is called again, not cached.
func TestResolveConfigReadsEdits(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PIGO_HOME", t.TempDir())
	cwd := t.TempDir()
	flags := cliOptions{model: "openrouter/free"}

	writeConfigFile(t, "model = \"openrouter/one\"\n")
	opts, _ := resolveConfig(flags, cwd)
	if opts.model != "openrouter/one" {
		t.Fatalf("model = %q, want the config.toml value", opts.model)
	}

	writeConfigFile(t, "model = \"openrouter/two\"\n")
	opts, _ = resolveConfig(flags, cwd)
	if opts.model != "openrouter/two" {
		t.Errorf("model = %q, want the edited config.toml value", opts.model)
	}
}

// TestResolveConfigPigorcBeatsFile verifies the precedence the launch uses also
// holds on a reload: a pigorc option outranks config.toml, and an environment
// variable outranks the file (the flag tier is exercised by applyFileConfig's own
// tests, which fake the "flag was set" predicate).
func TestResolveConfigPigorcBeatsFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PIGO_HOME", t.TempDir())
	cwd := t.TempDir()
	writeConfigFile(t, "model = \"openrouter/file\"\nthinking_level = \"low\"\n")
	writePigorc(t, "option model openrouter/pigorc\noption thinking_level high\n")

	opts, messages := resolveConfig(cliOptions{model: "openrouter/free"}, cwd)
	if opts.model != "openrouter/pigorc" {
		t.Errorf("model = %q, want the pigorc option to win over config.toml", opts.model)
	}
	if opts.thinkingLevel != "high" {
		t.Errorf("thinkingLevel = %q, want the pigorc option", opts.thinkingLevel)
	}
	for _, msg := range messages {
		if strings.Contains(msg, "unknown option") {
			t.Errorf("unexpected pigorc warning: %s", msg)
		}
	}
}

// TestResolveConfigReplacesAndDropsProviders verifies a provider edit takes effect
// and a deleted definition stops resolving — a reload must not leave a provider
// alive that the config no longer defines.
func TestResolveConfigReplacesAndDropsProviders(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PIGO_HOME", t.TempDir())
	cwd := t.TempDir()
	name := "reloadtest-provider"
	t.Cleanup(func() { provider.UnregisterCustomProvider(name) })

	writeConfigFile(t, `
[[providers]]
name = "`+name+`"
base_url = "https://one.example/v1"
models = ["reloadtest-model"]
`)
	resolveConfig(cliOptions{model: "openrouter/free"}, cwd)
	spec, ok := provider.LookupProviderSpec(name)
	if !ok || spec.DefaultBaseURL != "https://one.example/v1" {
		t.Fatalf("provider not registered from config: %+v (ok=%v)", spec, ok)
	}

	// Editing the endpoint replaces the definition.
	writeConfigFile(t, `
[[providers]]
name = "`+name+`"
base_url = "https://two.example/v1"
models = ["reloadtest-model"]
`)
	resolveConfig(cliOptions{model: "openrouter/free"}, cwd)
	if spec, _ := provider.LookupProviderSpec(name); spec.DefaultBaseURL != "https://two.example/v1" {
		t.Errorf("base URL = %q, want the edited definition", spec.DefaultBaseURL)
	}

	// Deleting the definition unregisters it.
	writeConfigFile(t, "model = \"openrouter/free\"\n")
	_, messages := resolveConfig(cliOptions{model: "openrouter/free"}, cwd)
	if _, ok := provider.LookupProviderSpec(name); ok {
		t.Error("a provider removed from the config should stop resolving")
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "removed") {
		t.Errorf("messages should report the removal, got:\n%s", joined)
	}
}

// TestReloaderAppliesPigorcEdits is the end-to-end shape of a live reload: it
// assembles a run from a pigorc, edits the script on disk, and verifies the reload
// applies the new definitions (a removed sub-agent disappears, an edited hook
// command is the one that will run) without restarting anything.
func TestReloaderAppliesPigorcEdits(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	cwd := t.TempDir()
	flags := cliOptions{model: "openrouter/free"}

	writePigorc(t, "hook add PreToolUse --matcher bash --command 'echo one'\n"+
		"agent add helper --description 'temporary helper' --system-prompt 'you help'\n")
	opts, _ := resolveConfig(flags, cwd)
	env, err := run.SetupEnv(opts.model, opts.baseURL, opts.protocol, opts.provider, opts.apiKey, opts.noTools, opts.noSkills, opts.systemPrompt, opts.appendSystemPrompt, opts.memory.Memory.Enabled, run.NewToolPolicy(opts.allowedTools, opts.disallowedTools))
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
	if !toolNamed(env.Tools, "helper") {
		t.Fatalf("the pigorc's agent should be a tool: %v", toolNamesOf(env.Tools))
	}

	reloader := newReloader(flags, cwd)
	reloader.Seed(env)

	// The user edits the pigorc: the agent is gone, the hook command changed.
	writePigorc(t, "hook add PreToolUse --matcher bash --command 'echo two'\n")
	res, err := reloader.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if toolNamed(res.Env.Tools, "helper") {
		t.Error("the removed sub-agent should not survive the reload")
	}
	summary := strings.Join(res.Summary, "\n")
	if !strings.Contains(summary, "removed:") || !strings.Contains(summary, "helper") {
		t.Errorf("summary should report the removed tool:\n%s", summary)
	}
	if !hookRunsCommand(res.Hooks, "echo two") {
		t.Errorf("the reloaded hook set should run the edited command, got %+v", res.Hooks)
	}
	if hookRunsCommand(res.Hooks, "echo one") {
		t.Error("the old hook command should be gone")
	}
}

// toolNamed reports whether tools holds a tool with the given name.
func toolNamed(tools []agentcore.AgentTool, name string) bool {
	for _, tool := range tools {
		if tool.Name() == name {
			return true
		}
	}
	return false
}

// toolNamesOf returns the tool names for failure messages.
func toolNamesOf(tools []agentcore.AgentTool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name())
	}
	return out
}

// hookRunsCommand reports whether any hook in set runs command.
func hookRunsCommand(set hooks.HookSet, command string) bool {
	for _, matchers := range set {
		for _, matcher := range matchers {
			for _, hook := range matcher.Hooks {
				if strings.TrimSpace(hook.Command) == command {
					return true
				}
			}
		}
	}
	return false
}
