// This file implements live configuration reload: re-reading the config layers
// and applying them to a session that is already running, so editing a pigorc or
// config.toml does not require quitting pigo.
//
// Two rules shape it. A reload must go through the same assembly a launch uses
// (setupEnv), or the reloaded state would slowly drift from a fresh launch — hence
// the loader callback is the driver's own config-reading code, not a second
// implementation. And a reload must not disturb anything a config file does not
// own: background shell jobs, the todo list, the reminder schedule, running
// sub-agents, the memory database and the plugin processes are carried over from
// the previous assembly (see ToolState).
//
// Detection is deliberately cheap and boring: the mtime/size of the config files
// that can be read by a running session. A turn boundary is the only place a
// reload is applied, so a half-written file is never picked up mid-request.
package run

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/runtime"
)

// ReloadSettings is the effective configuration a reload produced. It is the
// assembly input for setupEnv plus the driver-level settings the config report
// shows, so a reloaded run reports exactly what it is running with.
type ReloadSettings struct {
	Model        string
	BaseURL      string
	Protocol     string
	ProviderName string
	APIKey       string

	NoTools       bool
	NoSkills      bool
	MemoryEnabled bool
	Policy        ToolPolicy

	SystemPrompt       string
	AppendSystemPrompt []string

	// Thinking, OutputFormat, Approve and MemoryConfig are driver-level values
	// (flags plus config.toml); they feed the config report, not the assembly.
	Thinking     string
	OutputFormat string
	Approve      bool
	MemoryConfig config.MemorySettings

	// Messages are the non-fatal things reading the layers produced: script output,
	// a bad pigorc option, a malformed config.toml, a rejected definition, a provider
	// that was removed. They are shown to the user; a reload never fails over them.
	Messages []string
}

// ReloadLoader re-reads the configuration layers and returns the effective
// settings. It is supplied by the driver because reading them is a driver
// concern (flag precedence, process environment, provider registration).
type ReloadLoader func() (ReloadSettings, error)

// ReloadResult is what a successful reload produced: the rebuilt environment, the
// settings it used, the freshly resolved hook set, and a human-readable summary of
// what changed.
type ReloadResult struct {
	Env      Env
	Settings ReloadSettings
	Hooks    hooks.HookSet
	Summary  []string
}

// Reloader applies a fresh configuration to a live run. It is safe to call from
// the front-end goroutine only (it mutates the reloader's stamp bookkeeping).
type Reloader struct {
	load    ReloadLoader
	watched []string
	stamps  map[string]fileStamp
	mu      sync.Mutex
	// current is the assembly the run is using now: the baseline a reload diffs
	// against, and the state source for the stores a reload must carry over.
	current *Env
}

// fileStamp is the identity of a config file for change detection: its size and
// modification time. A missing file is a stamp of its own so creating it counts as
// a change.
type fileStamp struct {
	exists  bool
	size    int64
	modNano int64
}

// NewReloader builds a reloader over load that watches the given config files. A
// nil loader or an empty watch list still produces a usable reloader: a reload
// then returns an error and Changed is always false.
func NewReloader(load ReloadLoader, watched []string) *Reloader {
	r := &Reloader{load: load, watched: dedupePaths(watched), stamps: map[string]fileStamp{}}
	for _, path := range r.watched {
		r.stamps[path] = stampOf(path)
	}
	return r
}

// WatchedConfigFiles lists the files a running session re-reads on reload: the
// two pigorc candidates, config.toml, and the global/project JSON config layers
// (whose hooks and thinking level a reload picks up). Paths that cannot be
// resolved are omitted.
func WatchedConfigFiles(cwd string) []string {
	return dedupePaths([]string{
		config.FileConfigPath(),
		config.DefaultPigorcPath(),
		config.ProjectPigorcPath(cwd),
		filepath.Join(ConfigDir(), "config.json"),
		projectFilePath(cwd, ".pigo", "config.json"),
	})
}

// Seed records the assembly the run started with, which a reload uses as its
// baseline: the tools it keeps, and the state it carries over. Without a seed a
// reload has nothing to apply to and returns an error.
func (r *Reloader) Seed(env Env) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = &env
}

// Changed reports whether any watched config file changed since the last reload
// (or since construction). It only stats files, so a front-end can call it before
// every turn.
func (r *Reloader) Changed() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, path := range r.watched {
		if stampOf(path) != r.stamps[path] {
			return true
		}
	}
	return false
}

// Reload re-reads the configuration and rebuilds env's assembly, carrying the
// session state over. It returns an error when the new configuration cannot be
// assembled (an unresolvable provider, an invalid tool boundary); in that case the
// caller keeps running with the old environment and nothing has been closed.
func (r *Reloader) Reload() (*ReloadResult, error) {
	if r == nil || r.load == nil {
		return nil, fmt.Errorf("config reload is not wired in this run")
	}
	r.mu.Lock()
	seeded := r.current
	r.mu.Unlock()
	if seeded == nil {
		return nil, fmt.Errorf("config reload has no running assembly to apply to")
	}
	env := *seeded
	settings, err := r.load()
	if err != nil {
		return nil, err
	}
	next, err := setupEnv(
		settings.Model, settings.BaseURL, settings.Protocol, settings.ProviderName, settings.APIKey,
		settings.NoTools, settings.NoSkills, settings.SystemPrompt, settings.AppendSystemPrompt,
		settings.MemoryEnabled, settings.Policy, ToolStateFromEnv(env),
	)
	if err != nil {
		return nil, err
	}
	// The report must describe the run that now exists, including the settings only
	// the driver resolved.
	next.Report.SetSnapshot(config.RuntimeSnapshot{
		Model:        settings.Model,
		Provider:     settings.ProviderName,
		Protocol:     settings.Protocol,
		BaseURL:      settings.BaseURL,
		Thinking:     settings.Thinking,
		OutputFormat: settings.OutputFormat,
		Approve:      settings.Approve,
		NoTools:      settings.NoTools,
		NoSkills:     settings.NoSkills,
		Memory:       settings.MemoryConfig,
	})

	// Hooks are re-resolved from the same layers the reload just published, so a
	// hook added or removed in a pigorc/config.json takes effect now. A malformed
	// layer is reported in the summary but does not undo the reload.
	var set hooks.HookSet
	if resolved, err := ResolveHookSet(next.Cwd, Trusted(next.Cwd)); err != nil {
		settings.Messages = append(settings.Messages, "hooks disabled: "+err.Error())
	} else {
		set = resolved
	}

	// Hand the session's own state from the old assembly to the new one before
	// anything is closed: MCP connections are the one thing a reload replaces
	// outright, because their definitions come from the config.
	if next.MCP != nil && next.MCP != env.MCP {
		if env.MCP != nil {
			_ = env.MCP.Close()
		}
	}
	if env.Memory != nil && env.Memory != next.Memory {
		_ = env.Memory.Close()
	}
	if env.Plugins != nil && env.Plugins != next.Plugins {
		_ = env.Plugins.Close()
	}

	r.recordStamps()
	summary := describeReload(env, next, settings, set)
	r.mu.Lock()
	r.current = &next
	r.mu.Unlock()
	return &ReloadResult{Env: next, Settings: settings, Hooks: set, Summary: summary}, nil
}

// RecordStamps refreshes the change-detection baseline without reloading, so a
// driver that applied config changes another way does not reload them again.
func (r *Reloader) RecordStamps() {
	if r == nil {
		return
	}
	r.recordStamps()
}

// recordStamps refreshes the watched-file baseline after a reload.
func (r *Reloader) recordStamps() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, path := range r.watched {
		r.stamps[path] = stampOf(path)
	}
}

// stampOf returns the change-detection stamp for path.
func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, size: info.Size(), modNano: info.ModTime().UnixNano()}
}

// projectFilePath joins cwd with elems, returning "" when cwd is unknown.
func projectFilePath(cwd string, elems ...string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(append([]string{cwd}, elems...)...)
}

// dedupePaths drops empty paths and duplicates, preserving order.
func dedupePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// describeReload renders what the reload changed, comparing the tool set, skills,
// MCP servers and hooks. An unchanged reload still says so, so a /reload after
// editing the wrong file is not mistaken for success.
func describeReload(prev, next Env, settings ReloadSettings, set hooks.HookSet) []string {
	var lines []string
	lines = append(lines, "config reloaded")

	if model, provider := prev.ProviderName, next.ProviderName; model != provider {
		lines = append(lines, fmt.Sprintf("  provider    %s -> %s", orDash(model), orDash(provider)))
	}

	added, removed := diffToolNames(prev.Tools, next.Tools)
	if len(added) == 0 && len(removed) == 0 {
		lines = append(lines, fmt.Sprintf("  tools       %d (unchanged)", len(next.Tools)))
	} else {
		lines = append(lines, fmt.Sprintf("  tools       %d  added: %s  removed: %s",
			len(next.Tools), joinOrNone(added), joinOrNone(removed)))
	}

	if changed, line := diffSkills(prev.Skills, next.Skills); changed {
		lines = append(lines, "  "+line)
	} else {
		lines = append(lines, fmt.Sprintf("  skills      %d (unchanged)", len(next.Skills)))
	}

	if line, changed := diffMCP(prev.MCP, next.MCP); changed {
		lines = append(lines, "  "+line)
	}
	lines = append(lines, "  hooks       "+describeHookSet(set))
	for _, msg := range settings.Messages {
		lines = append(lines, "  "+msg)
	}
	return lines
}

// diffToolNames returns the tool names present only in next (added) and only in
// prev (removed), both sorted.
func diffToolNames(prev, next []agentcore.AgentTool) (added, removed []string) {
	prevNames := make(map[string]bool, len(prev))
	for _, t := range prev {
		prevNames[t.Name()] = true
	}
	nextNames := make(map[string]bool, len(next))
	for _, t := range next {
		nextNames[t.Name()] = true
		if !prevNames[t.Name()] {
			added = append(added, t.Name())
		}
	}
	for _, t := range prev {
		if !nextNames[t.Name()] {
			removed = append(removed, t.Name())
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// diffSkills compares two skill sets by name, reporting counts when they differ.
func diffSkills(prev, next []*runtime.Skill) (bool, string) {
	if len(prev) == len(next) {
		prevNames := make(map[string]bool, len(prev))
		for _, s := range prev {
			prevNames[s.Frontmatter.Name] = true
		}
		for _, s := range next {
			if !prevNames[s.Frontmatter.Name] {
				return true, fmt.Sprintf("skills      %d  added: %s", len(next), s.Frontmatter.Name)
			}
		}
		return false, ""
	}
	return true, fmt.Sprintf("skills      %d -> %d", len(prev), len(next))
}

// diffMCP reports the connected server set when it changed.
func diffMCP(prev, next *mcp.Manager) (string, bool) {
	prevServers := serverSet(prev)
	nextServers := serverSet(next)
	if equalStringSets(prevServers, nextServers) {
		return "", false
	}
	return fmt.Sprintf("mcp         %s", joinOrNone(sortedKeys(nextServers))), true
}

// serverSet returns the connected servers of mgr as a set.
func serverSet(mgr *mcp.Manager) map[string]bool {
	out := map[string]bool{}
	if mgr == nil {
		return out
	}
	for _, name := range mgr.Servers() {
		out[name] = true
	}
	return out
}

// equalStringSets reports whether two sets hold the same names.
func equalStringSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name := range a {
		if !b[name] {
			return false
		}
	}
	return true
}

// sortedKeys returns a set's keys, sorted.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// describeHookSet renders the resolved hook set compactly: the number of events and
// commands, then the events themselves.
func describeHookSet(set hooks.HookSet) string {
	events := 0
	commands := 0
	var names []string
	for event, matchers := range set {
		count := 0
		for _, m := range matchers {
			count += len(m.Hooks)
		}
		if count == 0 {
			continue
		}
		events++
		commands += count
		names = append(names, event)
	}
	if commands == 0 {
		return "none"
	}
	sort.Strings(names)
	return fmt.Sprintf("%d commands (%s)", commands, strings.Join(names, ", "))
}

// joinOrNone joins names, rendering "(none)" for an empty list.
func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// orDash renders an empty value as "-".
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
