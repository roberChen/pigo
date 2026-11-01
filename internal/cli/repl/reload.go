// This file implements /reload and the automatic config reload for the REPL.
//
// Editing a pigorc or config.toml used to require quitting and restarting pigo:
// hooks, tools, skills and MCP connections were resolved once at launch. The REPL
// now re-reads them on demand (/reload) and, when a config file changed since the
// last turn, before running the next prompt — a turn boundary is the only place a
// reload is applied, so a half-written file is never used mid-request.
package repl

import (
	"fmt"
	"io"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/run"
)

// reloadNow applies a fresh configuration to the running session and prints what
// changed. It reports whether the session was updated; on failure the old
// configuration is left untouched and the error is shown.
func reloadNow(out io.Writer, deps *replDeps) bool {
	if deps.reloader == nil {
		fmt.Fprintln(out, "config reload is not available in this run")
		return false
	}
	res, err := deps.reloader.Reload()
	if err != nil {
		fmt.Fprintf(out, "config reload failed: %v (still using the previous configuration)\n", err)
		return false
	}
	applyReload(deps, res)
	for _, line := range res.Summary {
		fmt.Fprintln(out, line)
	}
	return true
}

// autoReload applies pending configuration changes before a turn. It runs on the
// prompt path only: a slash command that inspects state (e.g. /status) should show
// the state the user asked about, and a reload only needs to land before a request
// can observe it.
func autoReload(out io.Writer, deps *replDeps) {
	if deps.reloader == nil || !deps.reloader.Changed() {
		return
	}
	if reloadNow(out, deps) {
		fmt.Fprintln(out, "(configuration changed on disk and was reloaded)")
	}
}

// applyReload swaps the session over to the newly assembled environment: the tool
// set and registry, the system prompt, the MCP connections, the hook dispatcher,
// the live model/provider, and the slash registry (skills may have changed). The
// stores a reload carries over (bash jobs, todo list, schedule, sub-agents, memory)
// are already the same objects, so nothing else has to move.
func applyReload(deps *replDeps, res *run.ReloadResult) {
	env := res.Env
	deps.agentCtx.Tools = env.Tools
	if env.SysPrompt != "" {
		deps.agentCtx.SystemPrompt = env.SysPrompt
	}
	deps.reg = run.ToolRegistry(env.Tools)
	// Keep an existing reminder registry: it also holds context injected by hooks,
	// which a config reload must not discard. A session that had none gets one so
	// the todo tool's progress reminders keep working.
	if deps.reminders == nil {
		deps.reminders = run.TodoReminders(env.Tools)
	}
	deps.schedule = env.Schedule
	deps.subagentJobs = env.SubAgentJobs
	deps.mcp = env.MCP
	deps.memstore = env.Memory
	deps.memoryRoot = run.MemoryRootFromTools(env.Tools)
	deps.snap = run.SnapshotRecorderFromTools(env.Tools)
	deps.jobs = run.BashJobStoreFromTools(env.Tools)

	// A reload is not a new session, so SessionStart is not re-dispatched; the hook
	// set itself may still have changed (added, removed, or newly trusted). A nil
	// dispatcher is the "no hooks" state and keeps every hook path off.
	deps.dispatcher = run.BuildDispatcher(res.Hooks, deps.hookDeps)

	settings := res.Settings
	if settings.Model != "" {
		deps.live.Model = settings.Model
	}
	if env.ProviderName != "" {
		deps.live.ProviderName = env.ProviderName
	}
	if env.Provider != nil {
		deps.live.Provider = env.Provider
	}
	deps.live.BaseURL = settings.BaseURL
	deps.live.Protocol = settings.Protocol
	if level := strings.TrimSpace(settings.Thinking); level != "" {
		deps.live.ThinkingLevel = agentcore.ThinkingLevel(level)
	}
	deps.header.Model = deps.live.Model
	deps.header.Provider = deps.live.ProviderName

	deps.skills = env.Skills
	if deps.rebuildSlash != nil {
		if reg := deps.rebuildSlash(env.Skills, env.Plugins, env.MCP); reg != nil {
			deps.slash = reg
		}
	}
}
