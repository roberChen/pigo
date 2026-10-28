// This file defines custom (user-defined) sub-agents: named agents the main agent
// can dispatch through a normal tool call, each with its own prompt, tools, model,
// reasoning effort, hooks and isolation.
//
// A definition is a complete agent configuration, not a prompt fragment: the agent
// runs as a full agent loop (its own context and its own persisted session,
// parented to the calling conversation), and the main agent continues that session
// by passing the id its result reported. Sub-agents never ask the user themselves;
// when a decision is needed, the main agent asks and then continues the child.
//
// Custom agents are defined in the pigorc only — the shell config is their single
// source of truth (there is no config.toml agent table). The DSL is:
//
//	agent add <name> [--description T] [--system-prompt T | --prompt-file F]
//	          [--tools a,b] [--model M] [--thinking L] [--isolation I]
//	          [--timeout N] [--inherit-hooks]
//	agent hook <name> <event> [--matcher RE] [--timeout N] --command CMD
//	agent remove <name>
package config

// AgentConfig defines one custom sub-agent, as written in the pigorc:
//
//	agent add researcher \
//	  --description "Read-only codebase researcher; returns a cited report." \
//	  --prompt-file ~/.pigo/prompts/researcher.md \
//	  --tools read,grep,find,ls \
//	  --model claude-haiku-4 --thinking off --timeout 300
//	agent hook researcher PreToolUse --matcher '^bash$' --command './.pigo/hooks/no-bash.sh'
type AgentConfig struct {
	// Name is the tool name the main agent calls and the name recorded on the
	// agent's sessions. Lowercase letters, digits, '.', '_' and '-' are allowed.
	Name string
	// Description tells the main agent when to dispatch this agent. It is injected
	// into the tool list, so it should be action-oriented.
	Description string
	// SystemPrompt is the child's system prompt (the agent's instructions).
	SystemPrompt string
	// Tools restricts the child to these built-in tools (allow list). Empty means
	// the default child set (the parent's builtins minus the delegation and
	// question tools).
	Tools []string
	// Model, when non-empty, runs the child against this model instead of the
	// parent's (e.g. a cheap model for research). The provider is resolved the same
	// way the CLI resolves it for that id.
	Model string
	// ThinkingLevel overrides the reasoning effort for this agent (off|minimal|low|
	// medium|high|xhigh|max). Empty inherits the parent's level.
	ThinkingLevel string
	// Isolation selects how the child runs: "goroutine" (default, in-process) or
	// "process" (a `pigo --subagent-rpc` subprocess; no session continuation).
	Isolation string
	// TimeoutSeconds bounds one child run. Zero means no per-agent cap.
	TimeoutSeconds int
	// InheritHooks, when true, also runs the global/project hook set inside this
	// agent. The default is false: an agent's hooks are the ones declared here,
	// defined independently of the main agent's.
	InheritHooks bool
	// Hooks are this agent's own hooks, keyed per event.
	Hooks []AgentHookConfig
}

// AgentHookConfig is one hook an agent runs (same wire contract as any other
// hook: stdin JSON, exit code decides).
type AgentHookConfig struct {
	Event   string
	Matcher string
	Command string
	Timeout int
}
