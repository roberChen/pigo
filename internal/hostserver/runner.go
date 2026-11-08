// This file defines the runner seam: the host owns sessions, persistence,
// permissions and the wire protocol, but the *agent* behind a session is an
// interface. DefaultFactory is the production implementation (run.SetupEnvIn +
// runtime.StartRun); tests inject a scripted factory so host behaviour — turn
// serialization, isolation between connections, resume, cancellation — is
// testable without a provider, a network, or credentials.
package hostserver

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/diag"
	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
)

// Agent is one session's live execution environment. Implementations own the
// assembled run environment (provider, tools rooted at the session's cwd, system
// prompt) and the agent context the loop mutates.
//
// Run is called with at most one turn in flight per session (the server
// serializes prompts), and must return after the loop settles — a long-running
// turn is cancelled through ctx, not by abandoning Run.
type Agent interface {
	// Run appends prompt as a user turn (an empty prompt runs follow-ups only)
	// and drives the loop to completion, streaming through h.
	Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error)
	// Messages returns the live context's messages. Compaction may replace the
	// slice, so the server re-reads it after every turn instead of caching.
	Messages() agentcore.MessageList
	// SystemPrompt is the prompt the session runs under (persisted on the header).
	SystemPrompt() string
	// Model and Provider label the session for listing and for the header.
	Model() string
	Provider() string
	// Close releases everything the agent owns (memory DB, plugins, MCP clients).
	// It must be safe to call after a failed or cancelled run.
	Close()
}

// AgentSpec is everything the factory needs to build one session's agent. It
// deliberately carries no protocol types: the factory is the boundary between
// the host's transport-agnostic session model and pigo's run assembly.
type AgentSpec struct {
	SessionID string
	// Cwd roots the built-in tools and the prompt; empty means the host's own
	// working directory.
	Cwd string
	// Model overrides the factory default for this session (session.create may
	// name one); empty means the factory default.
	Model string
	// SystemPrompt, when non-empty, is the prompt a resumed session ran under, so
	// the resumed context stays faithful instead of silently changing instructions.
	SystemPrompt string
	// Messages seeds the context on resume (already truncated at the last
	// compaction checkpoint by the session store).
	Messages agentcore.MessageList
	// BeforeToolCall is the host's permission gate. The factory must install it
	// so it is consulted before user hooks, exactly as the REPL installs the
	// trust gate.
	BeforeToolCall agentcore.BeforeToolCallFunc
}

// Factory builds an Agent for a session.
type Factory interface {
	NewAgent(ctx context.Context, spec AgentSpec) (Agent, error)
}

// DefaultFactory assembles real pigo runs. Every field maps to a setting the CLI
// already resolves for its own run modes; the host fixes them for its lifetime
// (per-session config reload is not part of the split).
type DefaultFactory struct {
	Model    string
	BaseURL  string
	Protocol string
	Provider string
	APIKey   string
	// Thinking is the reasoning-effort level every session starts at. Empty means
	// "resolve from the layered config chain", matching a launch with no
	// --thinking-level flag.
	Thinking agentcore.ThinkingLevel
	NoTools  bool
	NoSkills bool
	Memory   bool
	Policy   run.ToolPolicy
	// SystemPrompt / AppendSystemPrompt mirror --system-prompt and
	// --append-system-prompt. A resumed session keeps its own header prompt.
	SystemPrompt       string
	AppendSystemPrompt []string
}

// NewAgent assembles the session's environment and seeds its context.
func (f *DefaultFactory) NewAgent(ctx context.Context, spec AgentSpec) (Agent, error) {
	model := f.Model
	if spec.Model != "" {
		model = spec.Model
	}
	env, err := run.SetupEnvIn(spec.Cwd, model, f.BaseURL, f.Protocol, f.Provider, f.APIKey,
		f.NoTools, f.NoSkills, f.SystemPrompt, f.AppendSystemPrompt, f.Memory, f.Policy)
	if err != nil {
		return nil, err
	}
	sysPrompt := spec.SystemPrompt
	if sysPrompt == "" {
		sysPrompt = env.SysPrompt
	}
	a := &envAgent{
		env:      env,
		apiKey:   f.APIKey,
		model:    model,
		provider: env.ProviderName,
		agentCtx: &agentcore.AgentContext{
			SystemPrompt: sysPrompt,
			Messages:     spec.Messages,
			Tools:        env.Tools,
		},
	}
	if err := a.buildRunConfig(ctx, spec, f.Thinking); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// envAgent is the production Agent: an assembled run.Env plus the loop config
// rebuilt from it.
type envAgent struct {
	env      run.Env
	apiKey   string
	model    string
	provider string
	agentCtx *agentcore.AgentContext
	cfg      runtime.RunConfig
	// dispatcher is the hook dispatcher for this session, or nil when no hooks
	// are configured. It is kept for UserPromptSubmit, which must run before the
	// prompt enters the loop.
	dispatcher *hooks.Dispatcher
}

func (a *envAgent) buildRunConfig(ctx context.Context, spec AgentSpec, thinking agentcore.ThinkingLevel) error {
	creds := provider.NewCredentialStore(nil)
	// An explicit key (config.toml or --api-key) must reach the wire, since the env
	// var may be absent; empty means "resolve from the environment as usual".
	creds.SetOverride(a.provider, a.apiKey)
	// A host session has one model for its lifetime (session.create fixes it), so a
	// snapshot of the image capability is enough. It keeps read/view_image from
	// returning an image block a text-only model's wire would reject.
	run.AttachImageCapability(a.env.Tools, func() bool {
		return provider.ModelSupportsImages(a.provider, a.model)
	})
	if thinking == "" {
		level, err := run.ResolveThinkingLevel("")
		if err != nil {
			// A malformed optional config layer must not make the host unable to
			// start a session; the default level is a safe fallback.
			diag.Diagf("host: thinking level: %v", err)
		}
		thinking = level
	}
	cfg := run.NewConfig(a.model, a.provider, thinking, a.env.Provider,
		creds, run.ToolRegistry(a.env.Tools), run.TodoReminders(a.env.Tools), a.env.Schedule,
		a.env.SubAgentJobs, a.env.MCP)
	cfg.SessionID = spec.SessionID
	cfg.MemoryRoot = run.MemoryRootFromTools(a.env.Tools)
	// The permission gate goes on before hooks so hooks chain *after* it: the gate
	// is authoritative, exactly like the REPL's trust seam (a denied call never
	// reaches a user hook).
	if spec.BeforeToolCall != nil {
		cfg.Batch.ToolExecutorConfig.BeforeToolCall = spec.BeforeToolCall
	}
	set, err := run.ResolveHookSet(a.env.Cwd, run.Trusted(a.env.Cwd))
	if err != nil {
		return err
	}
	d, _ := run.InstallDriverHooks(ctx, &cfg, set, run.HookDeps{
		SessionID:  spec.SessionID,
		ProjectDir: a.env.Cwd,
		WarnLog:    diag.Writer(),
	}, "startup", nil)
	a.dispatcher = d
	a.cfg = cfg
	return nil
}

func (a *envAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	if prompt != "" {
		a.agentCtx.Messages = append(a.agentCtx.Messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	if a.dispatcher != nil {
		if block, reason := run.DispatchUserPromptSubmit(ctx, a.dispatcher, &a.cfg, run.HookDeps{
			SessionID:  a.cfg.SessionID,
			ProjectDir: a.env.Cwd,
			WarnLog:    diag.Writer(),
		}, prompt); block {
			return nil, fmt.Errorf("prompt blocked by hook: %s", reason)
		}
	}
	stream := runtime.StartRun(ctx, a.agentCtx, a.cfg)
	return runtime.DrainStream(ctx, stream, h)
}

func (a *envAgent) Messages() agentcore.MessageList {
	if a.agentCtx == nil {
		return nil
	}
	return a.agentCtx.Messages
}

func (a *envAgent) SystemPrompt() string {
	if a.agentCtx == nil {
		return ""
	}
	return a.agentCtx.SystemPrompt
}

func (a *envAgent) Model() string    { return a.model }
func (a *envAgent) Provider() string { return a.provider }

// Close releases the resources setupEnv opened: plugins (processes), MCP
// connections and the memory database. The tools hold references to the same
// objects, so closing here is enough, and every field is nil-checked because a
// session may have been assembled with --no-tools.
func (a *envAgent) Close() {
	if a.env.Plugins != nil {
		a.env.Plugins.Close()
	}
	if a.env.MCP != nil {
		a.env.MCP.Close()
	}
	if a.env.Memory != nil {
		a.env.Memory.Close()
	}
}

// newSessionHeader builds the on-disk header for a new session in cwd.
func newSessionHeader(id string, now time.Time, cwd, model, providerName, sysPrompt string) session.SessionHeader {
	return session.SessionHeader{
		ID:           id,
		CreatedAt:    now,
		UpdatedAt:    now,
		Model:        model,
		Provider:     providerName,
		SystemPrompt: sysPrompt,
		Cwd:          cwd,
	}
}

// resolveCwd normalizes a session's cwd. An empty request means the host's own
// working directory; a relative value is joined onto it so a UI can send "." and
// stay consistent with the process it is talking to.
func resolveCwd(requested, fallback string) string {
	if requested == "" {
		return fallback
	}
	if filepath.IsAbs(requested) || fallback == "" {
		return requested
	}
	return filepath.Join(fallback, requested)
}
