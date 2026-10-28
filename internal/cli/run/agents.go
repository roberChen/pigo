// This file builds custom sub-agent tools from their configuration (config.toml
// `[[agents]]` tables and pigorc `agent add ...`).
//
// A defined agent is a complete agent configuration, not a prompt fragment: it
// runs its own agent loop with its own tool set, model, reasoning effort, hooks
// and timeout, and persists its own session (parented to the calling
// conversation) exactly like a top-level run. It is registered as an ordinary
// tool, so the main agent dispatches it by name and reads the child's report from
// the tool result — and can continue the same child session by passing the
// session id that result reported.
//
// Hooks are the agent's own by default (defined independently of the parent);
// inherit_hooks additionally runs the resolved global/project hook set inside the
// child. Sub-agents deliberately never get the `question` tool: they cannot ask
// the user themselves. When a child needs a decision it says so in its report, and
// the main agent asks the user and continues that child session with the answer.
package run

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/compaction"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
)

// defaultAgentContextWindow is the fallback context-token budget for a sub-agent
// run, mirroring internal/cli.DefaultContextWindow (which run cannot import
// without an import cycle). Children therefore compact like a top-level run
// instead of growing until the provider rejects the request.
const defaultAgentContextWindow = 128000

// agentBuildContext is the parent-side state an agent definition needs to build
// its tool: the working directory and tool policy for the child set, the parent's
// provider as the inheritance default, and the names already taken in the parent's
// tool set (so a definition cannot shadow a builtin).
type agentBuildContext struct {
	cwd          string
	policy       ToolPolicy
	model        string
	baseURL      string
	protocol     string
	provider     provider.Provider
	providerName string
	apiKey       string
	takenNames   map[string]bool
	jobs         *runtime.SubAgentJobs
}

// buildAgentTools turns the configured agent definitions into sub-agent tools.
// A definition that cannot be built (unknown tool, unknown isolation, duplicate
// name, provider resolution failure) is reported and skipped: one bad definition
// must not take the whole session down.
func buildAgentTools(defs []config.AgentConfig, bc agentBuildContext) []agentcore.AgentTool {
	if len(defs) == 0 {
		return nil
	}
	taken := make(map[string]bool, len(bc.takenNames)+len(defs))
	for name := range bc.takenNames {
		taken[name] = true
	}
	var out []agentcore.AgentTool
	for _, def := range defs {
		name := strings.ToLower(strings.TrimSpace(def.Name))
		if name == "" {
			fmt.Fprintln(os.Stderr, "pigo: agent definition skipped: name is required")
			continue
		}
		if taken[name] {
			fmt.Fprintf(os.Stderr, "pigo: agent %q skipped: the name is already used by another tool\n", name)
			continue
		}
		tool, err := buildAgentTool(def, name, bc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pigo: agent %q skipped: %v\n", name, err)
			continue
		}
		taken[name] = true
		out = append(out, tool)
	}
	return out
}

// buildAgentTool builds one sub-agent tool from its definition.
func buildAgentTool(def config.AgentConfig, name string, bc agentBuildContext) (*runtime.SubAgentTool, error) {
	// Model: the definition may pin its own, otherwise the parent's is inherited.
	childModel, childProv, childProviderName := bc.model, bc.provider, bc.providerName
	if m := strings.TrimSpace(def.Model); m != "" {
		m = provider.CanonicalizeModel(m)
		p, n, err := provider.ResolveProvider(m, bc.baseURL, bc.protocol, "", os.Getenv)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", m, err)
		}
		childModel, childProv, childProviderName = m, p, n
	}

	// Credentials: the parent's explicit key only applies when the agent runs
	// against the same provider it was resolved for.
	creds := provider.NewCredentialStore(nil)
	if childProviderName == bc.providerName {
		creds.SetOverride(childProviderName, bc.apiKey)
	}

	// Tools: the default child set (builtins minus delegation and question),
	// narrowed by the definition's allow list.
	childTools := ChildToolSet(bc.cwd, bc.policy)
	if len(def.Tools) > 0 {
		var err error
		childTools, err = filterAgentTools(childTools, def.Tools, name)
		if err != nil {
			return nil, err
		}
	}

	hookSet := agentHookSet(def, bc.cwd)
	thinking := agentcore.ThinkingLevel(strings.TrimSpace(def.ThinkingLevel))
	if thinking != "" && !validAgentThinking(thinking) {
		return nil, fmt.Errorf("unknown thinking_level %q (want off|minimal|low|medium|high|xhigh|max)", def.ThinkingLevel)
	}

	factory := func() runtime.RunConfig {
		cfg := runtime.RunConfig{
			LoopConfig: runtime.LoopConfig{
				Model:         childModel,
				Provider:      childProviderName,
				ThinkingLevel: thinking,
				Stream:        provider.StreamFnFromProvider(childProv),
				GetAPIKey:     creds.GetAPIKey,
				// Children compact like a top-level run: without a window and
				// thresholds a long delegation would silently overflow.
				ContextWindow: defaultAgentContextWindow,
				Compaction:    compaction.DefaultCompactionSettings,
			},
			Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: ToolRegistry(childTools)}},
		}
		if d := hooks.NewDispatcher(hookSet, bc.cwd, os.Stderr); d != nil {
			deps := HookDeps{ProjectDir: bc.cwd, WarnLog: os.Stderr}
			InstallToolSeams(&cfg, d, deps)
			// A child ends on SubagentStop, not the parent's Stop event.
			InstallSubagentStop(&cfg, d, deps)
		}
		return cfg
	}

	spec := runtime.SubAgentSpec{
		Name:          name,
		Description:   strings.TrimSpace(def.Description),
		SystemPrompt:  def.SystemPrompt,
		Tools:         childTools,
		NewRunConfig:  factory,
		ThinkingLevel: thinking,
		Timeout:       time.Duration(def.TimeoutSeconds) * time.Second,
		Jobs:          bc.jobs,
	}
	if spec.Description == "" {
		spec.Description = "Delegate work to the " + name + " sub-agent."
	}
	switch strings.ToLower(strings.TrimSpace(def.Isolation)) {
	case "", "goroutine":
	case "process":
		spec.Isolation = runtime.SubAgentIsolationProcess
		spec.Process = runtime.SubAgentProcessConfig{
			Model:    childModel,
			BaseURL:  bc.baseURL,
			Protocol: bc.protocol,
		}
	default:
		return nil, fmt.Errorf("unknown isolation %q (want goroutine or process)", def.Isolation)
	}
	return runtime.NewSubAgentTool(spec), nil
}

// filterAgentTools narrows the child tool set to the definition's allow list.
// An unknown name is reported and dropped: a typo should not silently widen or
// empty the agent's tool set without saying so.
func filterAgentTools(tools []agentcore.AgentTool, allow []string, agentName string) ([]agentcore.AgentTool, error) {
	want := make(map[string]bool, len(allow))
	for _, name := range allow {
		if n := strings.ToLower(strings.TrimSpace(name)); n != "" {
			want[n] = true
		}
	}
	out := make([]agentcore.AgentTool, 0, len(tools))
	for _, tool := range tools {
		if want[strings.ToLower(tool.Name())] {
			out = append(out, tool)
			delete(want, strings.ToLower(tool.Name()))
		}
	}
	for name := range want {
		fmt.Fprintf(os.Stderr, "pigo: agent %q: tool %q is not available to sub-agents and was ignored\n", agentName, name)
	}
	return out, nil
}

// agentHookSet resolves an agent's hooks: its own definitions plus, when
// inherit_hooks is set, the resolved global/project hook set (which already
// includes any pigorc hooks). The agent's own hooks come first so they run before
// inherited ones, matching the usual precedence.
func agentHookSet(def config.AgentConfig, cwd string) hooks.HookSet {
	var set hooks.HookSet
	for _, h := range def.Hooks {
		canonical, ok := hooks.CanonicalEvent(h.Event)
		if !ok {
			fmt.Fprintf(os.Stderr, "pigo: agent %q: unknown hook event %q ignored\n", def.Name, h.Event)
			continue
		}
		if strings.TrimSpace(h.Command) == "" {
			fmt.Fprintf(os.Stderr, "pigo: agent %q: hook %s has no command, ignored\n", def.Name, canonical)
			continue
		}
		var timeout *int
		if h.Timeout > 0 {
			n := h.Timeout
			timeout = &n
		}
		if set == nil {
			set = make(hooks.HookSet)
		}
		set[canonical] = append(set[canonical], hooks.HookMatcherConfig{
			Matcher: h.Matcher,
			Hooks:   []hooks.HookConfig{{Type: hooks.CommandType, Command: h.Command, Timeout: timeout}},
		})
	}
	if !def.InheritHooks {
		return set
	}
	inherited, err := ResolveHookSet(cwd, Trusted(cwd))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pigo: agent %q: inherited hooks disabled: %v\n", def.Name, err)
		return set
	}
	for event, matchers := range inherited {
		if len(matchers) == 0 {
			continue
		}
		if set == nil {
			set = make(hooks.HookSet)
		}
		set[event] = append(set[event], matchers...)
	}
	return set
}

// validAgentThinking reports whether level is one of the reasoning-effort values
// the loop accepts. It mirrors the enum in agentcore without importing the prompts
// package (which sits above this one).
func validAgentThinking(level agentcore.ThinkingLevel) bool {
	switch level {
	case agentcore.ThinkingOff, agentcore.ThinkingMinimal, agentcore.ThinkingLow,
		agentcore.ThinkingMedium, agentcore.ThinkingHigh, agentcore.ThinkingXHigh,
		agentcore.ThinkingMax:
		return true
	}
	return false
}
