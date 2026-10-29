// This file implements reading and applying pigo's configuration layers, split out
// of main() so a live reload can run the exact same code. Before this, startup was
// the only reader: evaluate the pigorc, overlay config.toml, publish providers and
// agents and MCP servers — all inline in main, where nothing else could reach it.
//
// The two entry points are loadConfigLayers (read the files and publish what the
// process needs) and resolveConfig (overlay them onto the pristine CLI flags).
// Both are pure with respect to the options struct: they return a new value instead
// of mutating the caller's, which is what lets startup and reload start from the
// same CLI-only baseline.
package main

import (
	"fmt"
	"os"
	"strings"

	flag "github.com/spf13/pflag"

	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/provider"
)

// configLayers is one read of the configuration files, with every non-fatal
// problem collected so the caller decides where to show them (stderr at startup, a
// transcript line on a reload).
type configLayers struct {
	pigorc   *config.Pigorc
	file     config.FileConfig
	messages []string
}

// flagCommandLineChanged reports whether the user passed name on the command line.
// applyFileConfig uses it to decide whether a config-file value may override the
// flag, so the precedence CLI > file > default holds on a reload as well.
func flagCommandLineChanged(name string) bool { return flag.CommandLine.Changed(name) }

// configuredProviderNames remembers the custom providers the last load registered,
// so a reload can drop the definitions a deleted config entry used to provide.
var configuredProviderNames []string

// loadConfigLayers reads the config files and publishes the process-level state
// they define: the evaluated pigorc (plus the environment variables it exported),
// the decoded config.toml, the third-party provider definitions, the custom
// sub-agents and the MCP servers. Everything it publishes is process state that
// later resolution reads (provider registry, run.ResolveHookSet, run.SetupEnv), so
// a reload publishes exactly the same things a launch does.
//
// trusted gates the project files (./.pigorc, ./.pigo/config.json): a project
// script and project hooks execute code, so they only apply in a trusted directory.
func loadConfigLayers(cwd, buildVersion string, trusted bool) configLayers {
	pigorc := config.LoadPigorc(cwd, trusted, buildVersion)
	config.SetCurrentPigorc(pigorc)

	var messages []string
	if out := strings.TrimSpace(pigorc.Output); out != "" {
		messages = append(messages, out)
	}
	messages = append(messages, pigorc.Warnings...)
	// The script's exports join this process's environment so provider keys, base
	// URLs and PIGO_* knobs behave as if they had been exported in the shell.
	for k, v := range pigorc.Env {
		_ = os.Setenv(k, v)
	}
	for _, k := range pigorc.Unset {
		_ = os.Unsetenv(k)
	}

	file, err := config.LoadFileConfig(config.FileConfigPath())
	if err != nil {
		messages = append(messages, err.Error())
	}
	// The pigorc's options are folded into the decoded file first, so a script
	// option beats the file's value while an explicit CLI flag still beats both.
	messages = append(messages, pigorc.ApplyToFileConfig(&file)...)

	// Providers must exist before anything resolves a model: this is what makes
	// `--provider <name>`, model-id inference and the /model picker treat a
	// configured provider like a built-in. A bad definition is reported, never fatal.
	messages = append(messages, registerConfiguredProviders(file.Providers)...)
	// Custom sub-agents come only from the pigorc scripts; the tool assembly
	// (run.SetupEnv) registers them. MCP servers come from config.toml `[[mcp]]`
	// and pigorc `mcp add ...`, and are connected there too.
	config.SetCurrentAgents(pigorc.Agents)
	config.SetCurrentMCP(file.MCP)
	return configLayers{pigorc: pigorc, file: file, messages: messages}
}

// resolveConfig overlays a fresh read of the configuration layers onto the pristine
// CLI flags and finishes the resolution startup and reload share: model-id
// canonicalization and the named-credential lookup. It returns the effective
// options plus the layer messages to display.
func resolveConfig(flags cliOptions, cwd string) (cliOptions, []string) {
	layers := loadConfigLayers(cwd, version, flags.approve || run.Trusted(cwd))
	opts := flags
	// file values replace built-in defaults, but a flag the user set on the command
	// line always wins (CLI > file > default).
	applyFileConfig(&opts, layers.file, flagCommandLineChanged)

	// A bare provider name ("zai", "deepseek") means that provider's default model,
	// so canonicalize once here: every downstream consumer (SetupEnv, the REPL/TUI
	// live seeds, sub-agent children) then carries a real model id.
	opts.model = provider.CanonicalizeModel(opts.model)

	// Resolve a credential reference into an API key: the config carries only a
	// NAME; the literal secret lives in $PIGO_HOME/.credentials.yaml (0600). A
	// missing reference warns rather than aborts — the provider may authenticate
	// from the ambient environment.
	if opts.credentialRef != "" && opts.apiKey == "" {
		path := provider.CredentialFilePath()
		if provider.CredentialFilePermissionsWarn(path) {
			layers.messages = append(layers.messages, fmt.Sprintf("warning: %s is readable by group/other; chmod 600 recommended", path))
		}
		key, resolveErr := provider.ResolveCredentialReference(path, opts.credentialRef)
		if resolveErr != nil {
			layers.messages = append(layers.messages, fmt.Sprintf("credential %q: %v", opts.credentialRef, resolveErr))
		} else {
			opts.apiKey = key
		}
	}
	return opts, layers.messages
}

// newReloader builds the live config reloader for an interactive session. The
// loader re-derives the effective options from the pristine CLI flags, so a reload
// follows the same precedence chain (CLI > config file > default) as the launch and
// publishes the same process state (environment, providers, agents, MCP servers).
func newReloader(flags cliOptions, cwd string) *run.Reloader {
	return run.NewReloader(func() (run.ReloadSettings, error) {
		resolved, messages := resolveConfig(flags, cwd)
		return reloadSettings(resolved, messages), nil
	}, run.WatchedConfigFiles(cwd))
}

// reloadSettings projects resolved options into the settings a live reload hands to
// the run assembly. The warning list travels with it so the front-end can show what
// the newly read layers complained about.
func reloadSettings(opts cliOptions, messages []string) run.ReloadSettings {
	settings := run.ReloadSettings{
		Model:              opts.model,
		BaseURL:            opts.baseURL,
		Protocol:           opts.protocol,
		ProviderName:       opts.provider,
		APIKey:             opts.apiKey,
		NoTools:            opts.noTools,
		NoSkills:           opts.noSkills,
		MemoryEnabled:      opts.memory.Memory.Enabled,
		Policy:             run.NewToolPolicy(opts.allowedTools, opts.disallowedTools),
		SystemPrompt:       opts.systemPrompt,
		AppendSystemPrompt: opts.appendSystemPrompt,
		OutputFormat:       opts.outputFmt,
		Approve:            opts.approve,
		MemoryConfig:       opts.memory,
		Messages:           messages,
	}
	// A thinking-level failure is reported by the driver that needs the level; the
	// report simply omits the line rather than failing the whole reload.
	if level, err := run.ResolveThinkingLevel(opts.thinkingLevel); err == nil {
		settings.Thinking = string(level)
	}
	return settings
}
