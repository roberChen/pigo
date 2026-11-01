// This file implements /reload and the automatic config reload for the TUI.
//
// The TUI rebuilds its per-turn config (buildConfig) from the session's
// collaborators every turn, so a reload is just a matter of replacing those
// collaborators: tools, registry, prompt, hooks, skills, MCP connections and the
// live model. The reload itself runs off the tea loop (it may connect MCP servers)
// and yields a reloadDoneMsg that the model folds into the transcript.
package tui

import (
	"errors"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/plugin"
	"github.com/smallnest/pigo/internal/runtime"
)

// reloadDoneMsg reports the outcome of a config reload to the model. startPrompt,
// when non-empty, is the turn that was waiting for the reload (the automatic path)
// and is started once the new configuration is in place.
type reloadDoneMsg struct {
	res         *run.ReloadResult
	err         error
	startPrompt string
}

// reloadCmd re-reads the configuration off the tea loop. Applying the result is the
// model's job (on the UI goroutine), so the session is never mutated from this
// goroutine.
func (s *runSession) reloadCmd(startPrompt string) tea.Cmd {
	return func() tea.Msg {
		if s.reloader == nil {
			return reloadDoneMsg{err: errors.New("config reload is not available in this run"), startPrompt: startPrompt}
		}
		res, err := s.reloader.Reload()
		return reloadDoneMsg{res: res, err: err, startPrompt: startPrompt}
	}
}

// configChanged reports whether a watched config file changed since the last
// reload, so the model can reload before starting the next turn.
func (s *runSession) configChanged() bool {
	return s.reloader != nil && s.reloader.Changed()
}

// applyReload swaps the session onto the newly assembled environment: the tool set
// and registry (rebuilt each turn from s.reg), the system prompt, the MCP
// connections, the hook dispatcher, the live model/provider, and the slash registry
// (skills may have changed). Session state a reload carries over — background bash
// jobs, the todo list, the schedule, background sub-agents, the memory store — is
// already the same object, because the assembly rebinds it.
func (s *runSession) applyReload(res *run.ReloadResult) {
	env := res.Env
	s.agentCtx.Tools = env.Tools
	if env.SysPrompt != "" {
		s.agentCtx.SystemPrompt = env.SysPrompt
	}
	s.reg = run.ToolRegistry(env.Tools)
	// Keep an existing reminder registry (it also carries hook-injected context);
	// create one only if the session had none, so todo progress reminders keep
	// working after a reload that added the todo tool.
	if s.reminders == nil {
		s.reminders = run.TodoReminders(env.Tools)
	}
	s.schedule = env.Schedule
	s.jobs = env.SubAgentJobs
	s.mcp = env.MCP
	s.memstore = env.Memory
	s.memoryRoot = run.MemoryRootFromTools(env.Tools)

	// A reload is not a new session: SessionStart is not re-dispatched, but the hook
	// set itself may have changed. BuildDispatcher returns nil for an empty set,
	// which is the "no hooks" state every seam treats as off.
	s.plugins = env.Plugins
	base := baseObserver(s.plugins)
	if d := run.BuildDispatcher(res.Hooks, s.hookDeps); d != nil {
		s.dispatcher = d
		n := hooks.NewHookNotifier(d, s.hookDeps.SessionID, s.hookDeps.ProjectDir)
		s.onEvent = chainTUIEvent(base, n.Handle)
	} else {
		s.dispatcher = nil
		s.onEvent = base
	}

	settings := res.Settings
	if settings.Model != "" {
		s.live.Model = settings.Model
	}
	if env.ProviderName != "" {
		s.live.ProviderName = env.ProviderName
	}
	if env.Provider != nil {
		s.live.Provider = env.Provider
	}
	s.live.BaseURL = settings.BaseURL
	s.live.Protocol = settings.Protocol
	if level := settings.Thinking; level != "" {
		s.live.ThinkingLevel = agentcore.ThinkingLevel(level)
	}
	s.header.Model = s.live.Model
	s.header.Provider = s.live.ProviderName

	// Skills may have changed on disk: rebuild the slash registry from the same
	// sources the launch used so /skill sees the new skill set.
	s.skills = env.Skills
	// A reload replaces the MCP manager outright (its definitions come from the
	// config; the reloader closes the old one), so rebind the new manager to the
	// registry that was just rebuilt and rewrite agentCtx.Tools: the old manager's
	// tools are now dead and must not be advertised. /mcp then keeps switching
	// the new one.
	if s.mcp != nil {
		s.mcp.BindRegistry(s.reg)
	}
	s.syncMCPTools()
	if reg := newSlashRegistryFor(s.opts, s.live, s.skills, s.plugins, s.mcp); reg != nil {
		s.slash = reg
	}
}

// baseObserver returns the plugin event observer for a plugin manager, or nil when
// no plugin subscribed (the common case, where the observer seam stays unset).
func baseObserver(mgr *plugin.Manager) func(agentcore.AgentEvent) {
	if n := plugin.NewEventNotifier(mgr, os.Stderr); n != nil {
		return n.Handle
	}
	return nil
}

// ensure the runtime import stays used if the skill set type moves.
var _ = (*runtime.Skill)(nil)
