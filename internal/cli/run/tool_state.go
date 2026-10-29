// This file implements the session-scoped state a config reload must preserve.
// Rebuilding the tool set is the point of a reload, but the built-in tools also
// own live state that has nothing to do with configuration: a background bash job
// that is still running, the todo list the model is working through, the reminder
// schedule, the registry of background sub-agents, the open memory database, and
// the plugin processes. Throwing those away because a config file changed would be
// a hostile surprise (and would orphan running processes), so the old assembly
// hands them to the new one.
package run

import (
	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/memory"
	"github.com/smallnest/pigo/internal/plugin"
	"github.com/smallnest/pigo/internal/runtime"
)

// ToolState is the mutable state the built-in tools share for the lifetime of a
// session. Every field is optional: a nil field tells the assembly to create a
// fresh instance, which is what a first launch does.
type ToolState struct {
	// Snap records file mutations so /rewind can roll back a turn.
	Snap *agenttool.FileSnapshotRecorder
	// BashJobs tracks background shell commands.
	BashJobs *agenttool.BashJobStore
	// Todo holds the current task list.
	Todo *agenttool.TodoStore
	// Schedule is the session-local reminder scheduler.
	Schedule *agenttool.Schedule
	// SubAgentJobs holds the background sub-agent registry.
	SubAgentJobs *runtime.SubAgentJobs
	// Memory is the open persistent-memory store.
	Memory *memory.Store
	// Plugins are the running plugin processes.
	Plugins *plugin.Manager
}

// ToolStateFromEnv recovers the session state from an assembled environment. It is
// how a reload builds the state to carry over: every collaborator is already
// reachable through the tools or the Env fields, so nothing new has to be tracked.
func ToolStateFromEnv(env Env) *ToolState {
	return &ToolState{
		Snap:         SnapshotRecorderFromTools(env.Tools),
		BashJobs:     BashJobStoreFromTools(env.Tools),
		Todo:         TodoStoreFromTools(env.Tools),
		Schedule:     env.Schedule,
		SubAgentJobs: env.SubAgentJobs,
		Memory:       env.Memory,
		Plugins:      env.Plugins,
	}
}

// rebindStatefulTools points freshly built built-in tools at the previous run's
// stores, so a rebuilt tool set continues the session instead of starting a new
// one. A nil field keeps the fresh instance created by BuiltinTools.
func rebindStatefulTools(tools []agentcore.AgentTool, state *ToolState) {
	for _, tool := range tools {
		switch t := tool.(type) {
		case *agenttool.WriteTool:
			if state.Snap != nil {
				t.Snap = state.Snap
			}
		case *agenttool.EditTool:
			if state.Snap != nil {
				t.Snap = state.Snap
			}
		case *agenttool.BashTool:
			if state.BashJobs != nil {
				t.Jobs = state.BashJobs
			}
		case *agenttool.BashOutputTool:
			if state.BashJobs != nil {
				t.Jobs = state.BashJobs
			}
		case *agenttool.BashKillTool:
			if state.BashJobs != nil {
				t.Jobs = state.BashJobs
			}
		case *agenttool.TodoTool:
			if state.Todo != nil {
				t.Store = state.Todo
			}
		}
	}
}

// TodoStoreFromTools returns the TodoStore backing the run's todo tool, or nil when
// the tool is not wired in. A reload uses it to keep the model's task list.
func TodoStoreFromTools(tools []agentcore.AgentTool) *agenttool.TodoStore {
	for _, t := range tools {
		if tt, ok := t.(*agenttool.TodoTool); ok && tt.Store != nil {
			return tt.Store
		}
	}
	return nil
}
