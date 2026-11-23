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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/run"
	"github.com/roberChen/pigo/internal/diag"
	"github.com/roberChen/pigo/internal/hooks"
	"github.com/roberChen/pigo/internal/provider"
	"github.com/roberChen/pigo/internal/runtime"
	"github.com/roberChen/pigo/internal/session"
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

// ModelSwitch reports what a session's model change resolved to: the canonical
// model id, the provider that serves it, and the compaction budget the host
// resolved for the pair.
//
// The budget is reported rather than left for the client to recompute because
// only the host knows the layered configuration ([checkpoint]/[compaction], a
// per-model compact_at, a policy command) that decides where auto-compaction
// fires. A client that re-derived it from the window alone would disagree with
// the loop it is driving whenever the input ceiling, the ladder or a configured
// cap is the tighter rule — which is the common case.
type ModelSwitch struct {
	Model    string
	Provider string
	// ContextWindow is the model's combined budget (input + output).
	ContextWindow int
	// InputBudget is what one request may carry.
	InputBudget int
	// TriggerPoint is where automatic compaction fires, in tokens, measured
	// against InputBudget. 0 means it is off for this model.
	TriggerPoint int
	// TriggerSource names the rule that decided TriggerPoint (ladder, reserve,
	// configured, off), so a UI can explain the number.
	TriggerSource string
}

// ModelSwitcher is the optional capability a session agent implements when its
// model can change mid-session. The server asserts it rather than making it part
// of Agent: an agent that cannot switch is a legitimate implementation (a
// scripted test double, an embedded runner with a fixed endpoint), and refusing
// the request honestly beats a method every implementation must stub out.
type ModelSwitcher interface {
	// SetModel switches to model (a concrete id or a bare provider name) and
	// re-resolves the compaction plan for it. It must not be called while a turn
	// is running; the server serializes that.
	SetModel(model string) (ModelSwitch, error)
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
		baseURL:  f.BaseURL,
		protocol: f.Protocol,

		explicitProvider: f.Provider,
		model:            model,
		provider:         env.ProviderName,
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
	env    run.Env
	apiKey string
	// baseURL and protocol are the factory's endpoint settings, kept so an
	// in-session model switch resolves the new provider exactly as assembly did
	// instead of falling back to the environment-only heuristic chain.
	baseURL  string
	protocol string
	// explicitProvider is the factory's --provider override (empty when the
	// launch did not name one). A switch resolves through it when set, so a
	// session started against a named or custom gateway keeps using that gateway
	// instead of being re-routed by the model-id heuristic.
	explicitProvider string

	// mu guards the switchable pieces below (model, provider, cfg). A turn reads
	// them, a model switch writes them, and the server serializes the two — but
	// the lock makes that invariant local to this type rather than dependent on a
	// caller two packages away.
	mu       sync.Mutex
	model    string
	provider string
	agentCtx *agentcore.AgentContext
	cfg      runtime.RunConfig
	// dispatcher is the hook dispatcher for this session, or nil when no hooks
	// are configured. It is kept for UserPromptSubmit, which must run before the
	// prompt enters the loop.
	dispatcher *hooks.Dispatcher
	// hookEvent is the observer seam InstallDriverHooks returns — SessionEnd and
	// PreCompact fan out through it (#424). Dropping it (as passing a nil event
	// handler would) silently disables those two hook points for every host
	// session, which is exactly the per-driver drift InstallDriverHooks exists to
	// prevent. Nil when no hooks are configured.
	hookEvent func(agentcore.AgentEvent)
}

func (a *envAgent) buildRunConfig(ctx context.Context, spec AgentSpec, thinking agentcore.ThinkingLevel) error {
	creds := provider.NewCredentialStore(nil)
	// An explicit key (config.toml or --api-key) must reach the wire, since the env
	// var may be absent; empty means "resolve from the environment as usual".
	creds.SetOverride(a.provider, a.apiKey)
	// The image gate follows the live model rather than a snapshot: a model switch
	// to a text-only model must stop read/view_image handing it a picture, which
	// the wire would reject, without rebuilding the tool set.
	run.AttachImageCapability(a.env.Tools, func() bool {
		model, providerName := a.currentModel()
		return provider.ModelSupportsImages(providerName, model)
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
	d, hookEvent := run.InstallDriverHooks(ctx, &cfg, set, run.HookDeps{
		SessionID:  spec.SessionID,
		ProjectDir: a.env.Cwd,
		WarnLog:    diag.Writer(),
	}, "startup", nil)
	a.dispatcher = d
	a.hookEvent = hookEvent
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
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	stream := runtime.StartRun(ctx, a.agentCtx, cfg)
	return runtime.DrainStream(ctx, stream, chainEventObserver(h, a.hookEvent))
}

// chainEventObserver appends obs to h's OnEvent seam, keeping whatever handler the
// caller installed first.
//
// It is a separate function so the ordering contract is testable without a
// provider: the caller's handler owns the protocol event stream and must run
// before any hook, and a nil observer must leave the handler untouched (so a
// session with no hooks pays nothing and behaves exactly as before).
func chainEventObserver(h runtime.StreamHandler, obs func(agentcore.AgentEvent)) runtime.StreamHandler {
	if obs == nil {
		return h
	}
	prev := h.OnEvent
	h.OnEvent = func(ev agentcore.AgentEvent) {
		if prev != nil {
			prev(ev)
		}
		obs(ev)
	}
	return h
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

func (a *envAgent) Model() string    { m, _ := a.currentModel(); return m }
func (a *envAgent) Provider() string { _, p := a.currentModel(); return p }

// currentModel reads the model/provider pair under the switch lock.
func (a *envAgent) currentModel() (model, providerName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model, a.provider
}

// SetModel switches the session to another model (or, when the resolution
// demands it, another provider) without rebuilding the assembled environment.
//
// That is the whole point of doing it here rather than through the factory: the
// tool set, memory database, plugins, MCP connections, hook dispatcher and the
// conversation all survive, so a switch is not observable as a new session to a
// SessionStart hook and does not reopen a single resource.
//
// Only the per-turn streaming pieces are replaced (model, provider, stream,
// credential lookup) plus the compaction plan, which is re-resolved for the new
// model in the session's own working directory. Everything else on the config —
// the hook seams, the host's permission gate, the follow-up/reminder wiring —
// stays exactly as assembly installed it, so a switch can neither drop a hook
// nor widen the tool boundary.
func (a *envAgent) SetModel(model string) (ModelSwitch, error) {
	id := strings.TrimSpace(model)
	if id == "" {
		return ModelSwitch{}, fmt.Errorf("model is required")
	}
	// Match /model exactly: a bare provider name canonicalizes to that provider's
	// default model id, so the wire request and the header carry a real id.
	resolved := provider.CanonicalizeModel(id)
	prov, providerName, err := provider.ResolveProvider(resolved, a.baseURL, a.protocol, a.explicitProvider, os.Getenv)
	if err != nil {
		return ModelSwitch{}, err
	}
	plan := run.PlanCompactionIn(a.env.Cwd, providerName, resolved)

	// The credential lookup is rebuilt for the new provider: an explicit key from
	// the factory reaches the wire under whichever provider now serves the session,
	// and an empty one keeps resolving from the environment as before.
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride(providerName, a.apiKey)

	a.mu.Lock()
	a.model = resolved
	a.provider = providerName
	a.env.Provider = prov
	a.env.ProviderName = providerName
	a.cfg.Model = resolved
	a.cfg.Provider = providerName
	a.cfg.Stream = provider.StreamFnFromProvider(prov)
	a.cfg.GetAPIKey = creds.GetAPIKey
	run.AttachCompaction(&a.cfg, plan)
	a.mu.Unlock()

	return ModelSwitch{
		Model:         resolved,
		Provider:      providerName,
		ContextWindow: plan.ContextWindow,
		InputBudget:   plan.InputBudget,
		TriggerPoint:  plan.TriggerPoint,
		TriggerSource: string(plan.TriggerInfo.Source),
	}, nil
}

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
