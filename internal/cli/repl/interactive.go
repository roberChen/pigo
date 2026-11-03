// This file wires the line-based REPL (US-003) and session persistence
// (US-024, #43) into the pigo command. When invoked without a prompt on a
// terminal, pigo starts the REPL loop (see repl.go); each run's messages are
// persisted to a local JSONL session so the conversation can be listed, resumed
// and replayed later.
package repl

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/cli/headless"
	"github.com/smallnest/pigo/internal/cli/prompts"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/dream"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/plugin"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// sessionStore returns the session store for the interactive REPL. It is a thin
// alias for headless.SessionStore so the REPL and headless runs share one store
// rooted at ~/.pigo/sessions (or PIGO_HOME).
func sessionStore() (*session.Store, error) {
	return headless.SessionStore()
}

// Options carries the resolved run configuration plus optional
// resume state into Run.
type Options struct {
	Model        string
	ProviderName string
	Provider     provider.Provider
	BaseURL      string
	APIKey       string
	Protocol     string
	// ThinkingLevel is the resolved reasoning-effort level (US-023): it seeds the
	// live run config so every REPL turn requests it, until a control command
	// changes it.
	ThinkingLevel agentcore.ThinkingLevel
	Tools         []agentcore.AgentTool
	SysPrompt     string

	// ResumeID, when non-empty, resumes an existing session: its messages seed
	// the context and replayed transcript. Otherwise a fresh session is created.
	ResumeID string

	// Approve, when true, grants the launch directory session trust before the
	// run so the first-launch trust prompt is skipped and side-effect tools run
	// without per-call confirmation (mirrors pi's --approve/-a).
	Approve bool
	// Skills is the pre-loaded skill set (loaded once by setupAgentEnv, shared
	// with prompt injection). /skill resolves names against it; skills are not
	// registered as one slash command each. Empty under --no-skills.
	Skills []*runtime.Skill

	// Plugins holds the loaded plugin manager so the REPL can deliver lifecycle
	// events to subscribed plugins (US-017, #133). It may be nil (no plugins).
	Plugins *plugin.Manager

	// MCP holds the connected MCP server manager so each turn can pick up a
	// server's live tool-list changes. It may be nil (no MCP servers).
	MCP *mcp.Manager

	// Reloader applies a live configuration reload (/reload, plus automatic
	// pickup when a config file changes between turns). It is seeded with the
	// initial assembly by the caller; nil disables reloading for this run.
	Reloader *run.Reloader

	// ConfigPrompts holds prompt-template paths from the config.toml `prompts`
	// array (settings tier); each is a file or dir loaded non-recursively.
	ConfigPrompts []string
	// CliPrompts holds --prompt-template paths (CLI tier, repeatable).
	CliPrompts []string
	// NoPromptTemplates disables all prompt-template discovery (global, project,
	// settings, CLI); built-in slash commands are unaffected. Independent of
	// --no-skills.
	NoPromptTemplates bool

	// Dream is the resolved [dream] configuration (US-008). Run uses it to decide
	// whether to launch the startup background consolidation; a zero value
	// (Enabled false) disables the auto-trigger entirely.
	Dream dream.Config

	// InitialInput, when non-empty, pre-fills the very first prompt line instead
	// of submitting it. cmd/pigo uses it for --image: the references land in the
	// composer so the user adds the question, and later prompts start empty.
	InitialInput string
}

// Run starts the line-based REPL over a persisted session. It keeps
// a single growing AgentContext across prompts (so turns share history) and
// saves the session's messages after each run completes (see runREPL/streamRun
// in repl.go).
func Run(opts Options) error {
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride(opts.ProviderName, opts.APIKey)
	reg := run.ToolRegistry(opts.Tools)

	store, err := sessionStore()
	if err != nil {
		return err
	}

	// Resolve the launch directory once (pigo does not cd during a session). It is
	// the trust key, the directory side-effect tools are gated against, and — new
	// for #526 — the value stamped onto a fresh SessionHeader.Cwd so the session is
	// attributed to a project and a later /dream pass can distill it under the
	// right scope (mirrors headless.headlessCwd). An unresolvable cwd yields ""
	// (the session stays unattributed) rather than aborting the session.
	cwd, cwdErr := os.Getwd()

	// Establish the session: resume an existing one or create a fresh header.
	now := time.Now().UTC()
	var (
		agentCtx *agentcore.AgentContext
		header   session.SessionHeader
		history  []agentcore.AgentMessage
		curLeaf  string // active leaf id on resume; "" for a fresh session
	)
	if opts.ResumeID != "" {
		// Interactive resume always appends a fresh user message before running,
		// so a session that ended normally (trailing assistant reply) is resumable
		// here. Load the conversation (the active branch, not every abandoned branch
		// in the file) and rebuild the context from the last compaction checkpoint
		// on: the replay shows the full history, the agent continues from the point
		// where the summary left off.
		h, path, err := store.LoadPath(opts.ResumeID)
		if err != nil {
			return err
		}
		if h.IsSubAgent() {
			return fmt.Errorf("session %s: %w", opts.ResumeID, session.ErrNotResumableSubAgent)
		}
		history = session.Messages(path)
		curLeaf = session.TipLeaf(path)
		header = h
		agentCtx = &agentcore.AgentContext{
			SystemPrompt: h.SystemPrompt,
			Messages:     session.MessagesSinceLastCompaction(history),
			Tools:        opts.Tools,
		}
		if agentCtx.SystemPrompt == "" {
			agentCtx.SystemPrompt = opts.SysPrompt
		}
	} else {
		agentCtx = &agentcore.AgentContext{SystemPrompt: opts.SysPrompt, Tools: opts.Tools}
		header = session.SessionHeader{
			ID:           session.NewID(now),
			CreatedAt:    now,
			UpdatedAt:    now,
			Model:        opts.Model,
			Provider:     opts.ProviderName,
			SystemPrompt: opts.SysPrompt,
			Cwd:          cwd,
		}
	}

	// live holds the run configuration that a control command (e.g. /model) may
	// mutate mid-session. streamRun reads it on each prompt so a model switch
	// takes effect on the next turn; header is updated so the switch is persisted
	// with the session.
	live := &cli.LiveConfig{
		Model:         opts.Model,
		ProviderName:  opts.ProviderName,
		Provider:      opts.Provider,
		BaseURL:       opts.BaseURL,
		Protocol:      opts.Protocol,
		ThinkingLevel: opts.ThinkingLevel,
		ContextWindow: cli.DefaultContextWindow,
	}

	// Project trust (US-018, #134): load the persisted trust store for the
	// launch directory. A load failure (e.g. a corrupted trust.json) is
	// non-fatal: trust is disabled (mgr stays nil) and the REPL still runs -
	// the store is surfaced rather than silently overwritten. cwd is captured
	// once above since pigo does not cd during a session; if it cannot be resolved
	// trust is disabled too, since an empty cwd would silently never match.
	mgr, mgrErr := trust.NewManager(trust.DefaultPath())
	if mgrErr != nil {
		fmt.Fprintf(os.Stderr, "pigo: trust store unavailable, trust disabled: %v\n", mgrErr)
		mgr = nil
	}
	if cwdErr != nil && mgr != nil {
		fmt.Fprintf(os.Stderr, "pigo: cannot resolve working directory, trust disabled: %v\n", cwdErr)
		mgr = nil
	}
	// in is the shared input reader for the main loop and the tool-call
	// confirmation prompt (see repl.go). Wrapping os.Stdin once here means both
	// read from the same buffer.
	reader := bufio.NewReaderSize(os.Stdin, replScanBufInit)

	// Wire slash-commands: built-ins (compile-time) plus any user templates under
	// ~/.pigo/commands (mirrors the commands/*.md convention) plus skills under
	// ~/.agents/skills. A load error is non-fatal — the REPL still runs with the
	// built-ins. Instance built-ins that need live state (/model, /help) are
	// registered against `live`.
	slash, err := prompts.BuildSlashRegistry(live, creds, opts.Skills, opts.Plugins, prompts.PromptTemplateSources{
		Settings:       opts.ConfigPrompts,
		CLI:            opts.CliPrompts,
		Disable:        opts.NoPromptTemplates,
		ProjectDir:     filepath.Join(cwd, ".pigo", "prompts"),
		ProjectTrusted: mgr != nil && mgr.IsTrusted(cwd),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pigo: slash-commands: %v\n", err)
	}
	// /mcp is installed here (not by BuildSlashRegistry) because it needs the
	// connected manager the assembly just built.
	prompts.RegisterMCPCommand(slash, opts.MCP)
	trust.RegisterCommand(slash, mgr, cwd)

	// --approve grants the launch directory session trust up front (mirrors pi's
	// --approve/-a), so the first-launch prompt is skipped and side-effect tools
	// run without per-call confirmation. Otherwise, on the first launch in an
	// undecided directory, ask the user how much to trust it before any tool
	// runs. This happens before replay so the trust question is the first thing
	// the user sees, not their prior history.
	trust.EstablishTrust(os.Stdout, reader, mgr, cwd, opts.Approve)

	// Replay the resumed conversation so the user sees history before re-prompting.
	if len(history) > 0 {
		replayTranscript(os.Stdout, history)
	}

	// Startup background consolidation (US-008, FR-4/FR-17): if dream is enabled
	// and due, spawn `pigo --dream` in a goroutine now so it runs while the user
	// works — it never blocks the first prompt, and prints a one-line notice on
	// completion. The dream state/lock live under dream.ResolveMemoryRoot (the
	// same root the subprocess consolidates), independent of whether the memory
	// tool is wired into this session. Not-due / disabled is a cheap no-op.
	maybeStartBackgroundDream(os.Stdout, dream.ResolveMemoryRoot(), cwd, opts.Dream)

	return runREPL(os.Stdin, os.Stdout, replDeps{
		store:        store,
		header:       header,
		agentCtx:     agentCtx,
		live:         live,
		reg:          reg,
		reminders:    run.TodoReminders(opts.Tools),
		schedule:     agenttool.ScheduleFromTools(opts.Tools),
		subagentJobs: runtime.SubAgentJobsFromTools(opts.Tools),
		mcp:          opts.MCP,
		slash:        slash,
		creds:        creds,
		trust:        mgr,
		cwd:          cwd,
		in:           reader,
		confirmMu:    &sync.Mutex{},
		curLeaf:      curLeaf,
		// The persisted cursor indexes the agent's context, which after a resumed
		// compaction starts at the checkpoint (history is the full replay).
		persisted:  len(agentCtx.Messages),
		memoryRoot: run.MemoryRootFromTools(opts.Tools),
		memstore:   run.MemoryStoreFromTools(opts.Tools),
		snap:       run.SnapshotRecorderFromTools(opts.Tools),
		jobs:       run.BashJobStoreFromTools(opts.Tools),
		notifier:   plugin.NewEventNotifier(opts.Plugins, os.Stderr),
		goal:       agenttool.NewGoalState(),
		telemetry:  cli.NewTelemetryHolder(),
		reloader:   opts.Reloader,
		// A reload re-reads skills, plugins and MCP servers, so the slash
		// registry is rebuilt from the sources resolved at launch (same tiers,
		// same precedence).
		skills: opts.Skills,
		// InitialInput is pre-filled, not submitted: --image lands in the prompt
		// so the user asks the question around it.
		initialInput: opts.InitialInput,
		rebuildSlash: func(skills []*runtime.Skill, plugins *plugin.Manager, mcpMgr *mcp.Manager) *runtime.SlashRegistry {
			reg, err := prompts.BuildSlashRegistry(live, creds, skills, plugins, prompts.PromptTemplateSources{
				Settings:       opts.ConfigPrompts,
				CLI:            opts.CliPrompts,
				Disable:        opts.NoPromptTemplates,
				ProjectDir:     filepath.Join(cwd, ".pigo", "prompts"),
				ProjectTrusted: mgr != nil && mgr.IsTrusted(cwd),
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "pigo: slash-commands: %v\n", err)
			}
			prompts.RegisterMCPCommand(reg, mcpMgr)
			return reg
		},
	})
}

// formatHelpLine renders one slash-command line for /help as
// "/name <argument-hint> - description (source: <tier>)", omitting the hint
// segment when absent. It is the plain, testable form of the /help line; the
// /help Action applies color on top of the same structure.
func formatHelpLine(c runtime.SlashCommand) string {
	s := "/" + c.Name
	if c.ArgumentHint != "" {
		s += " " + c.ArgumentHint
	}
	if c.Description != "" {
		s += " - " + c.Description
	}
	s += " (source: " + c.Tier.String() + ")"
	return s
}
