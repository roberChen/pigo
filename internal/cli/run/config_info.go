// This file implements the report behind the pigo_config tool: the effective
// configuration of the running process, rendered on demand. It lives here rather
// than in the tool because the assembly layer (this package) is the one place that
// holds every ingredient at once — the resolved tool set, the discovered skills,
// the connected MCP servers and plugins, the layered hook set and the config
// files that were actually read.
//
// Two rules shape the output. It reports what the run ACTUALLY uses, not what a
// file says it should (a project hook layer under an untrusted directory is
// reported as skipped, a configured MCP server that failed to start is reported as
// not connected). And it is read at call time, so a hook set that grew after an
// interactive trust prompt, or an MCP server that announced a new tool list
// mid-run, is reflected without restarting anything.
package run

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/plugin"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/trust"
)

// reportDescriptionLimit bounds a skill or agent description in the report so one
// verbose definition cannot crowd out the rest of the answer.
const reportDescriptionLimit = 110

// ConfigReport is the snapshot the pigo_config tool renders. Its fields are filled
// by SetupEnv once the run is assembled, so a report cannot describe a tool set
// that was never advertised.
type ConfigReport struct {
	// Cwd is the working directory every path in the report is resolved against.
	Cwd string
	// Model and ProviderName are the resolved model id and provider.
	Model        string
	ProviderName string
	// Tools is the final advertised tool set (after plugins, MCP and the tool
	// boundary), so the report and the model see the same list.
	Tools []agentcore.AgentTool
	// MCP is the connected MCP manager, or nil when no server is connected.
	MCP *mcp.Manager
	// Skills is the discovered skill set (nil under --no-skills).
	Skills []*runtime.Skill
	// Plugins is the loaded plugin manager, or nil when none were discovered.
	Plugins *plugin.Manager
	// Policy is the tool boundary in force (--allowed-tools/--disallowed-tools).
	Policy ToolPolicy
	// NoTools and NoSkills record the switches that disabled whole subsystems.
	NoTools  bool
	NoSkills bool
	// MemoryOn reports whether the persistent memory store is wired in, and
	// MemoryNote explains why it is not when it failed to open.
	MemoryOn   bool
	MemoryNote string
	// ScheduleOn reports whether the schedule_* tools are registered.
	ScheduleOn bool
	// Snapshot holds the driver-resolved settings (endpoint, reasoning effort,
	// output format, approval, memory tables). It is empty for a driver that does
	// not provide one, in which case the report shows only what assembly knows.
	Snapshot config.RuntimeSnapshot
}

// SetSnapshot records the driver-resolved settings. It is a method rather than an
// exported field write so a driver can enrich the report in one call after
// SetupEnv, and so a nil report (library use without the tool) is a no-op.
func (r *ConfigReport) SetSnapshot(s config.RuntimeSnapshot) {
	if r == nil {
		return
	}
	r.Snapshot = s
	if s.NoTools {
		r.NoTools = true
	}
	if s.NoSkills {
		r.NoSkills = true
	}
}

// newConfigTool builds the tool over report. The tool holds the report pointer, so
// fields filled in after construction (the final tool list, the discovered skills)
// are visible when the model calls it.
func newConfigTool(report *ConfigReport) agentcore.AgentTool {
	return &agenttool.ConfigTool{
		Report: func(section string) string {
			return report.Render(section)
		},
	}
}

// Render renders the requested section, or the whole report when section is empty
// or "all". Sections are separated by a blank line so a filtered report and the
// full one read consistently.
func (r *ConfigReport) Render(section string) string {
	if r == nil {
		return ""
	}
	section = strings.ToLower(strings.TrimSpace(section))
	want := func(name string) bool {
		return section == "" || section == string(agenttool.ConfigSectionAll) || section == name
	}
	var blocks []string
	if want(string(agenttool.ConfigSectionRuntime)) {
		blocks = append(blocks, r.renderRuntime())
	}
	if want(string(agenttool.ConfigSectionFiles)) {
		blocks = append(blocks, r.renderFiles())
	}
	if want(string(agenttool.ConfigSectionTools)) {
		blocks = append(blocks, r.renderTools())
	}
	if want(string(agenttool.ConfigSectionSkills)) {
		blocks = append(blocks, r.renderSkills())
	}
	if want(string(agenttool.ConfigSectionMCP)) {
		blocks = append(blocks, r.renderMCP())
	}
	if want(string(agenttool.ConfigSectionHooks)) {
		blocks = append(blocks, r.renderHooks())
	}
	if want(string(agenttool.ConfigSectionAgents)) {
		blocks = append(blocks, r.renderAgents())
	}
	if want(string(agenttool.ConfigSectionMemory)) {
		blocks = append(blocks, r.renderMemory())
	}
	// Drop empty blocks (a section with nothing to report still renders a header,
	// but be defensive) and join with one blank line.
	kept := blocks[:0]
	for _, b := range blocks {
		if strings.TrimSpace(b) != "" {
			kept = append(kept, b)
		}
	}
	return strings.Join(kept, "\n\n")
}

// renderRuntime renders the identity of the run: version, working directory and
// trust, model and endpoint, and the reasoning/output/approval settings.
func (r *ConfigReport) renderRuntime() string {
	lines := []string{"pigo " + config.CurrentVersion() + " runtime"}
	target := r.Cwd
	if target == "" {
		target = "(unknown)"
	}
	lines = append(lines, "  working dir  "+target+"  ("+trustWord(Trusted(r.Cwd))+")")

	snap := r.Snapshot
	model := snap.Model
	if model == "" {
		model = r.Model
	}
	if model != "" {
		lines = append(lines, "  model        "+model)
	}
	provider := snap.Provider
	if provider == "" {
		provider = r.ProviderName
	}
	if provider != "" {
		line := "  provider     " + provider
		if snap.Protocol != "" {
			line += "  protocol=" + snap.Protocol
		}
		if snap.BaseURL != "" {
			line += "  endpoint=" + snap.BaseURL
		}
		lines = append(lines, line)
	}
	if snap.Thinking != "" {
		lines = append(lines, "  thinking     "+snap.Thinking)
	}
	if snap.OutputFormat != "" {
		lines = append(lines, "  output       "+snap.OutputFormat)
	}
	lines = append(lines, "  approve      "+boolOnOff(snap.Approve))
	lines = append(lines, "  flags        no-tools="+boolOnOff(r.NoTools)+"  no-skills="+boolOnOff(r.NoSkills))
	return strings.Join(lines, "\n")
}

// renderFiles renders the configuration files that were read, with the ones that
// are absent or deliberately skipped marked as such — the usual reason a setting
// "does not work" is that the file it lives in was never loaded.
func (r *ConfigReport) renderFiles() string {
	lines := []string{"config files"}
	configFile := config.FileConfigPath()
	lines = append(lines, kvPath("config.toml", configFile))

	trusted := Trusted(r.Cwd)
	globalJSON := filepath.Join(ConfigDir(), "config.json")
	lines = append(lines, kvPath("global config.json", globalJSON)+"   thinking + hooks layer")
	projectJSON := projectPath(r.Cwd, ".pigo", "config.json")
	lines = append(lines, gatedPath("project config.json", projectJSON, exists(projectJSON), trusted, "the directory is not trusted"))

	pigorc := config.CurrentPigorc()
	loaded := make(map[string]bool, len(pigorcSources(pigorc)))
	for _, src := range pigorcSources(pigorc) {
		loaded[src] = true
	}
	globalPigorc := config.DefaultPigorcPath()
	lines = append(lines, loadedPath("pigorc", globalPigorc, loaded[globalPigorc], true, ""))
	projectPigorc := config.ProjectPigorcPath(r.Cwd)
	lines = append(lines, loadedPath("project pigorc", projectPigorc, loaded[projectPigorc], trusted, "the directory is not trusted"))

	skillsLabel := "skills"
	if r.NoSkills {
		skillsLabel += " (--no-skills)"
	}
	lines = append(lines, kvPath(skillsLabel, SkillsDir()))
	pluginsLabel := "plugins"
	if r.NoTools {
		pluginsLabel += " (--no-tools)"
	}
	lines = append(lines, kvPath(pluginsLabel, PluginsDir()))
	lines = append(lines, kvPath("memory", filepath.Join(MemoryDir(), "index.db")))
	lines = append(lines, kvPath("credentials", providerCredentialPath()))
	lines = append(lines, kvPath("trust store", trust.DefaultPath()))
	return strings.Join(lines, "\n")
}

// renderTools renders the advertised tool set, grouped by where each tool came
// from, plus the boundary and the tool-backed subsystems.
func (r *ConfigReport) renderTools() string {
	if r.NoTools {
		return "tools  disabled (--no-tools)"
	}
	names := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		names = append(names, t.Name())
	}
	sort.Strings(names)

	groups := map[string][]string{}
	agentNames := agentToolNames()
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, mcp.ToolNamePrefix):
			groups["mcp"] = append(groups["mcp"], name)
		case agentNames[name]:
			groups["sub-agent"] = append(groups["sub-agent"], name)
		case pluginToolNames(r.Plugins)[name]:
			groups["plugin"] = append(groups["plugin"], name)
		default:
			groups["builtin"] = append(groups["builtin"], name)
		}
	}
	lines := []string{fmt.Sprintf("tools (%d)", len(names))}
	for _, group := range []string{"builtin", "sub-agent", "plugin", "mcp"} {
		if len(groups[group]) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-10s %d: %s", group, len(groups[group]), strings.Join(groups[group], ", ")))
	}
	lines = append(lines, "  boundary   allowed="+listOrNone(r.Policy.Allow)+"  disallowed="+listOrNone(r.Policy.Deny))
	lines = append(lines, "  memory_search "+boolOnOff(r.MemoryOn)+"  schedule_* "+boolOnOff(r.ScheduleOn))
	return strings.Join(lines, "\n")
}

// renderSkills renders the discovered skills and whether the model may invoke them
// on its own or only through the /name command.
func (r *ConfigReport) renderSkills() string {
	if r.NoSkills {
		return "skills  disabled (--no-skills)"
	}
	if len(r.Skills) == 0 {
		return "skills (0)  from " + SkillsDir()
	}
	lines := []string{fmt.Sprintf("skills (%d)  from %s", len(r.Skills), SkillsDir())}
	for _, s := range r.Skills {
		mark := "model"
		if s.Frontmatter.DisableModelInvocation {
			mark = "slash-only"
		}
		lines = append(lines, fmt.Sprintf("  %-14s [%s] %s", s.Frontmatter.Name, mark, clip(s.Frontmatter.Description, reportDescriptionLimit)))
	}
	return strings.Join(lines, "\n")
}

// renderMCP renders every configured MCP server with its transport and whether it
// actually connected, plus the tool count each connected server contributed.
func (r *ConfigReport) renderMCP() string {
	configured := config.CurrentMCP()
	connected := map[string]bool{}
	var tools []agentcore.AgentTool
	if r.MCP != nil {
		for _, name := range r.MCP.Servers() {
			connected[name] = true
		}
		tools = r.MCP.Tools()
	}
	if len(configured) == 0 && len(connected) == 0 {
		return "mcp servers  none configured"
	}
	lines := []string{fmt.Sprintf("mcp servers (%d connected, %d configured)", len(connected), len(configured))}
	seen := make(map[string]bool, len(configured))
	for _, sc := range configured {
		seen[sc.Name] = true
		lines = append(lines, "  "+mcpServerLine(sc, connected[sc.Name], countToolsWithPrefix(tools, mcp.ServerToolPrefix(sc.Name))))
	}
	// A server that connected without a config entry should be impossible, but
	// reporting it beats silently hiding a live connection.
	for name := range connected {
		if seen[name] {
			continue
		}
		count := countToolsWithPrefix(tools, mcp.ServerToolPrefix(name))
		lines = append(lines, fmt.Sprintf("  %-12s connected tools=%d", name, count))
	}
	return strings.Join(lines, "\n")
}

// renderHooks renders the resolved hook set: every event, matcher and command that
// will actually run.
func (r *ConfigReport) renderHooks() string {
	set, err := ResolveHookSet(r.Cwd, Trusted(r.Cwd))
	if err != nil {
		return "hooks  failed to resolve: " + err.Error()
	}
	total := 0
	for _, matchers := range set {
		for _, m := range matchers {
			total += len(m.Hooks)
		}
	}
	if total == 0 {
		return "hooks (0)  none configured (global config.json, project config.json, pigorc)"
	}
	lines := []string{fmt.Sprintf("hooks (%d events, %d commands)", len(set), total)}
	for _, event := range hooks.AllEvents {
		matchers := set[event]
		for _, m := range matchers {
			matcher := m.Matcher
			if matcher == "" {
				matcher = "*"
			}
			for _, h := range m.Hooks {
				lines = append(lines, fmt.Sprintf("  %-18s matcher=%-12s %s", event, matcher, clip(h.Command, reportDescriptionLimit)))
			}
		}
	}
	// An event outside AllEvents cannot be dispatched by the runtime; surface it so
	// a typo in a hook definition is visible instead of silent.
	known := make(map[string]bool, len(hooks.AllEvents))
	for _, event := range hooks.AllEvents {
		known[event] = true
	}
	var unknown []string
	for event := range set {
		if !known[event] {
			unknown = append(unknown, event)
		}
	}
	sort.Strings(unknown)
	for _, event := range unknown {
		lines = append(lines, fmt.Sprintf("  %-18s (unknown event: never dispatched)", event))
	}
	return strings.Join(lines, "\n")
}

// renderAgents renders the configured custom sub-agents and whether each one made
// it into the tool set (a definition can be skipped on a name collision).
func (r *ConfigReport) renderAgents() string {
	agents := config.CurrentAgents()
	if len(agents) == 0 {
		return "sub-agents (0)  none configured"
	}
	registered := agentToolNames()
	lines := []string{fmt.Sprintf("sub-agents (%d)", len(agents))}
	for _, a := range agents {
		state := "registered"
		if !registered[strings.ToLower(a.Name)] {
			state = "NOT registered"
		}
		bits := []string{state, "isolation=" + orDefault(a.Isolation, "goroutine")}
		bits = append(bits, "model="+orDefault(a.Model, "(inherit)"))
		if a.ThinkingLevel != "" {
			bits = append(bits, "thinking="+a.ThinkingLevel)
		}
		if len(a.Tools) > 0 {
			bits = append(bits, "tools="+strings.Join(a.Tools, ","))
		} else {
			bits = append(bits, "tools=(default child set)")
		}
		hooksWord := "own"
		if a.InheritHooks {
			hooksWord = "own+inherited"
		}
		bits = append(bits, fmt.Sprintf("hooks=%s(%d)", hooksWord, len(a.Hooks)))
		if a.TimeoutSeconds > 0 {
			bits = append(bits, fmt.Sprintf("timeout=%ds", a.TimeoutSeconds))
		}
		lines = append(lines, fmt.Sprintf("  %-14s %s", a.Name, strings.Join(bits, "  ")))
	}
	return strings.Join(lines, "\n")
}

// renderMemory renders the persistent-memory, checkpoint and compaction settings
// that are in force, plus where the store lives.
func (r *ConfigReport) renderMemory() string {
	lines := []string{"memory settings"}
	state := "on"
	if !r.MemoryOn {
		state = "off"
		if r.NoTools {
			state += " (--no-tools)"
		} else if r.MemoryNote != "" {
			state += " (" + r.MemoryNote + ")"
		}
	}
	lines = append(lines, "  store       "+state+"  "+filepath.Join(MemoryDir(), "index.db"))
	lines = append(lines, prefixedLines("  ", r.Snapshot.Memory.MemorySummary())...)
	return strings.Join(lines, "\n")
}

// countToolsWithPrefix counts the tools whose name starts with prefix.
func countToolsWithPrefix(tools []agentcore.AgentTool, prefix string) int {
	n := 0
	for _, t := range tools {
		if strings.HasPrefix(t.Name(), prefix) {
			n++
		}
	}
	return n
}

// mcpServerLine renders one configured MCP server.
func mcpServerLine(sc mcp.ServerConfig, connected bool, tools int) string {
	transport := sc.Transport
	if transport == "" {
		transport = "http"
	}
	if strings.TrimSpace(sc.Command) != "" {
		transport = "stdio"
	}
	endpoint := strings.TrimSpace(sc.URL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(strings.Join(append([]string{sc.Command}, sc.Args...), " "))
	}
	// The default is "not connected": a definition that failed to start is the
	// interesting case, and claiming a live connection that does not exist would
	// send the reader looking in the wrong place.
	state := "not connected"
	switch {
	case sc.Disabled:
		state = "disabled"
	case connected:
		state = fmt.Sprintf("connected tools=%d", tools)
	}
	line := fmt.Sprintf("%-12s %-18s %-5s %s", sc.Name, state, transport, endpoint)
	if sc.OAuth {
		line += "  oauth=on"
	}
	if sc.TimeoutSeconds > 0 {
		line += fmt.Sprintf("  timeout=%ds", sc.TimeoutSeconds)
	}
	return line
}

// pigorcSources lists the pigorc files the evaluated script actually came from.
func pigorcSources(p *config.Pigorc) []string {
	if p == nil {
		return nil
	}
	return p.Sources
}

// loadedPath renders a config file that is only read when it exists and the layer
// is allowed to run. note explains the case where the file is present but was not
// read, which is the interesting one: a missing file is obvious, a skipped file is
// not.
func loadedPath(label, path string, loaded, permitted bool, note string) string {
	if path == "" {
		return fmt.Sprintf("  %-22s (unresolved)", label)
	}
	switch {
	case loaded:
		return fmt.Sprintf("  %-22s %s (evaluated)", label, path)
	case !exists(path):
		return fmt.Sprintf("  %-22s %s (missing)", label, path)
	case !permitted:
		return fmt.Sprintf("  %-22s %s (skipped: %s)", label, path, note)
	default:
		return fmt.Sprintf("  %-22s %s (present, not evaluated)", label, path)
	}
}

// gatedPath renders a config file that is read only when its layer is permitted,
// so a present-but-skipped project file is visible rather than silently ignored.
func gatedPath(label, path string, present, permitted bool, note string) string {
	if path == "" {
		return fmt.Sprintf("  %-22s (unresolved)", label)
	}
	if !present {
		return fmt.Sprintf("  %-22s %s (missing)", label, path)
	}
	if !permitted {
		return fmt.Sprintf("  %-22s %s (skipped: %s)", label, path, note)
	}
	return fmt.Sprintf("  %-22s %s (found)", label, path)
}

// projectPath joins the cwd-anchored config path, returning "" when the cwd is
// unknown so a report built without one does not claim a bogus path.
func projectPath(cwd string, elems ...string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(append([]string{cwd}, elems...)...)
}

// kvPath renders "label  path (state)" with the label left-padded to a fixed
// column so the section stays scannable.
func kvPath(label, path string) string {
	if path == "" {
		return fmt.Sprintf("  %-22s (unresolved)", label)
	}
	return fmt.Sprintf("  %-22s %s (%s)", label, path, presenceWord(path))
}

// presenceWord reports whether path exists on disk.
func presenceWord(path string) string {
	if exists(path) {
		return "found"
	}
	return "missing"
}

// exists reports whether path names an existing file or directory.
func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// trustWord renders the trust state of the working directory.
func trustWord(trusted bool) string {
	if trusted {
		return "trusted"
	}
	return "not trusted"
}

// boolOnOff renders a boolean as on/off.
func boolOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// orDefault returns s, or fallback when s is empty.
func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// listOrNone joins names, or renders "(none)" for an empty list.
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ",")
}

// clip shortens s to limit runes, marking the cut with an ellipsis.
func clip(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len([]rune(s)) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "..."
}

// agentToolNames returns the set of registered custom sub-agent tool names, keyed
// by lowercase name.
func agentToolNames() map[string]bool {
	agents := config.CurrentAgents()
	out := make(map[string]bool, len(agents))
	for _, a := range agents {
		out[strings.ToLower(a.Name)] = true
	}
	return out
}

// pluginToolNames returns the set of tool names the loaded plugins declared.
func pluginToolNames(mgr *plugin.Manager) map[string]bool {
	if mgr == nil {
		return nil
	}
	out := map[string]bool{}
	for _, p := range mgr.Plugins() {
		for _, spec := range p.Manifest.Tools {
			out[spec.Name] = true
		}
	}
	return out
}

// providerCredentialPath is the named-credential file (issue #568) the report
// shows so a missing reference is obvious.
func providerCredentialPath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".credentials.yaml")
}

// prefixedLines prefixes every line with prefix, so a multi-line summary nests
// under its section.
func prefixedLines(prefix string, lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, prefix+line)
	}
	return out
}
