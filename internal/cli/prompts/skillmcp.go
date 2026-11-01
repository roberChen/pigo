// This file implements the /skill and /mcp control commands shared by the REPL
// and the TUI. Skills used to be one slash command per skill
// (/<skill-name>); they are now selected through /skill (multi-select in the
// TUI, a listing plus "/skill <name> [args]" in the REPL), and MCP servers are
// switched on and off per session through /mcp instead of only at launch.
package prompts

import (
	"context"
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/cli/ui"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/runtime"
)

// RegisterSkillCommands installs /skill, the single entry point for skills now
// that skills are no longer registered as one command each. A bare "/skill"
// lists the available skills; "/skill <name> [args]" expands that skill's body
// as the next turn (what the old "/<name>" command did, with the full template
// syntax: $1, $@/$ARGUMENTS, ${1:-default}, ${@:N}). The TUI intercepts both
// forms: bare /skill opens the multi-select picker and a named /skill selects
// that skill and opens the argument form, so it only falls back here for the
// listing and the REPL.
func RegisterSkillCommands(reg *runtime.SlashRegistry, skills []*runtime.Skill) {
	reg.AddBuiltin(runtime.SlashCommand{
		Name:         "skill",
		ArgumentHint: "[name [args...]]",
		Description:  "list skills, or run one now: /skill <name> [args] (the TUI opens a picker and an argument form)",
		Run: func(args string) (message, prompt string) {
			fields := strings.Fields(args)
			if len(fields) == 0 {
				return SkillListing(skills), ""
			}
			if fields[0] == "clear" || fields[0] == "none" || fields[0] == "off" {
				// The TUI keeps a multi-select chip set; the REPL has no such
				// state, so "clear" there is a no-op with an explanation.
				return "(the skill selection is TUI-only; here each /skill <name> applies just that skill)", ""
			}
			skill := FindSkill(skills, fields[0])
			if skill == nil {
				return fmt.Sprintf("skill: unknown skill %q\n%s", fields[0], SkillListing(skills)), ""
			}
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
			return "", skill.SlashCommand().Expand(rest)
		},
	})
}

// FindSkill returns the skill with the given name (case-insensitive), or nil.
func FindSkill(skills []*runtime.Skill, name string) *runtime.Skill {
	for _, s := range skills {
		if s == nil {
			continue
		}
		if strings.EqualFold(s.Frontmatter.Name, name) {
			return s
		}
	}
	return nil
}

// SkillListing renders the skill picker's textual equivalent: one
// "name — description" line per skill, in load order. It is what "/skill" and
// an unknown "/skill <name>" print, so the REPL and the TUI's fallback text
// stay identical. An empty set explains where skills come from instead of
// printing an empty list.
func SkillListing(skills []*runtime.Skill) string {
	color := ui.Enabled()
	var b strings.Builder
	b.WriteString(ui.Colorize(color, ui.Bold, "skills:"))
	if len(skills) == 0 {
		b.WriteString("\n  (none loaded; add SKILL.md under ~/.agents/skills/ or .pigo/skills/)")
		return b.String()
	}
	for _, s := range skills {
		if s == nil {
			continue
		}
		name := s.Frontmatter.Name
		if s.Frontmatter.DisableModelInvocation {
			name += " (manual)"
		}
		b.WriteString("\n  ")
		b.WriteString(ui.Colorize(color, ui.Cyan, name))
		if desc := strings.TrimSpace(s.Frontmatter.Description); desc != "" {
			b.WriteString(ui.Colorize(color, ui.Dim, " - "+firstLine(desc)))
		}
		if hint := s.ParamHint(); hint != "" {
			b.WriteString(ui.Colorize(color, ui.Dim, "  "+hint))
		}
	}
	b.WriteString("\nrun one with /skill <name> [args]; in the TUI, bare /skill opens a picker whose argument form takes the skill's $1/$@ slots")
	return b.String()
}

// firstLine keeps a description on one line for the compact listing.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// RegisterMCPCommand installs /mcp: a bare invocation reports every configured
// server and whether it is active, and "/mcp <name> [on|off|toggle]" switches
// one at runtime. Enable dials the server and registers its tools; Disable
// closes it and unregisters them, and either way the next turn re-advertises
// the tool set (the manager marks itself dirty for the loop's Refresh). The TUI
// intercepts the bare form to open its multi-select picker.
func RegisterMCPCommand(reg *runtime.SlashRegistry, mgr *mcp.Manager) {
	reg.AddBuiltin(runtime.SlashCommand{
		Name:         "mcp",
		ArgumentHint: "[server [on|off]]",
		Description:  "list MCP servers, or switch one on/off for this session (the TUI opens a multi-select picker)",
		Run: func(args string) (message, prompt string) {
			return ApplyMCPCommand(mgr, args), ""
		},
	})
}

// ApplyMCPCommand implements /mcp's textual behavior for both front-ends: with
// no arguments it returns the status listing, otherwise it parses
// "<server> [on|off|toggle]" and switches that server. It is exported so the
// TUI can share the argument parsing for "/mcp <name> ..." even though it owns
// the interactive picker.
func ApplyMCPCommand(mgr *mcp.Manager, args string) string {
	if mgr == nil {
		return "mcp: no MCP servers configured (add [[mcp]] to config.toml or use `pigorc mcp add`)"
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return MCPListing(mgr)
	}
	name := fields[0]
	verb := "toggle"
	if len(fields) > 1 {
		verb = strings.ToLower(fields[1])
	}
	var active bool
	found := false
	for _, st := range mgr.Statuses() {
		if st.Name == name {
			active = st.Active
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("mcp: unknown server %q\n%s", name, MCPListing(mgr))
	}
	want := !active
	switch verb {
	case "on", "enable", "start":
		want = true
	case "off", "disable", "stop":
		want = false
	case "toggle":
	default:
		return fmt.Sprintf("mcp: unknown action %q (want on|off|toggle)", verb)
	}
	return SetMCPActive(mgr, name, want)
}

// SetMCPActive switches one configured server on or off and reports the result.
// It runs synchronously, so the TUI calls it off the tea loop (connecting a
// server can spawn a process or wait on HTTP).
func SetMCPActive(mgr *mcp.Manager, name string, active bool) string {
	if mgr == nil {
		return "mcp: no MCP servers configured"
	}
	if active {
		ctx, cancel := context.WithTimeout(context.Background(), mcp.DefaultTimeout)
		defer cancel()
		if err := mgr.Enable(ctx, name); err != nil {
			return fmt.Sprintf("mcp: %s failed to start: %v", name, err)
		}
		count := 0
		for _, st := range mgr.Statuses() {
			if st.Name == name {
				count = st.Tools
			}
		}
		return fmt.Sprintf("mcp: %s on (%d tools); the next turn advertises them", name, count)
	}
	if err := mgr.Disable(name); err != nil {
		return fmt.Sprintf("mcp: %s failed to stop: %v", name, err)
	}
	return fmt.Sprintf("mcp: %s off; its tools are no longer advertised", name)
}

// MCPListing renders the configured servers and their runtime state, the
// textual form of the TUI's /mcp picker.
func MCPListing(mgr *mcp.Manager) string {
	color := ui.Enabled()
	var b strings.Builder
	b.WriteString(ui.Colorize(color, ui.Bold, "mcp servers:"))
	statuses := mgr.Statuses()
	if len(statuses) == 0 {
		b.WriteString("\n  (none configured; add [[mcp]] to config.toml or use `pigorc mcp add`)")
		return b.String()
	}
	for _, st := range statuses {
		state := "off"
		if st.Active {
			state = fmt.Sprintf("on (%d tools)", st.Tools)
		}
		b.WriteString("\n  ")
		b.WriteString(ui.Colorize(color, ui.Cyan, st.Name))
		b.WriteString(ui.Colorize(color, ui.Dim, "  "+state))
		if st.Error != "" {
			b.WriteString(ui.Colorize(color, ui.Yellow, "  last error: "+firstLine(st.Error)))
		}
	}
	b.WriteString("\nswitch one with /mcp <server> [on|off]; in the TUI, bare /mcp opens a multi-select picker")
	return b.String()
}
