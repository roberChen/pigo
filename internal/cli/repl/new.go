// This file implements the REPL's /new command: start a fresh, empty session
// without leaving pigo.
//
// /new is the counterpart of /resume: /resume loads a stored conversation, /new
// starts a new one. It is not a reset of the current session (that would keep the
// same id and history alive) — it writes the outgoing session's unsaved tail,
// mints a new session id, drops the conversation, and re-fires SessionStart so
// context-injecting hooks run for the new session. Everything that belongs to the
// session machinery rather than to one conversation (tools, hooks, MCP
// connections, memory store, trust, slash registry) is left in place.
package repl

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
)

// runNew handles /new. An argument is rejected rather than silently ignored: the
// next thing to do after /new is type the first message, and a stray "/new hello"
// should not look like it started a conversation.
func runNew(out io.Writer, deps *replDeps, line string) {
	if arg := strings.TrimSpace(strings.TrimPrefix(line, "/new")); arg != "" {
		fmt.Fprintln(out, "usage: /new (takes no arguments; send your first message afterwards)")
		return
	}
	// Flush the outgoing session first, so a turn that has not been persisted yet is
	// never dropped by the switch (the same guard /resume, /fork and /clone use).
	cli.PersistTurn(out, deps)
	resetSession(deps)
	fmt.Fprintf(out, "new session %s (empty)\n", deps.header.ID)
}

// resetSession starts a fresh conversation in place: a new session id, an empty
// message list, a reset branch cursor and the session-scoped bookkeeping (side
// thread, telemetry) cleared. The hook session id is re-stamped and SessionStart is
// dispatched for it, mirroring what a launch does, so a hook that injects context
// sees the new session and its context lands in the first turn.
func resetSession(deps *replDeps) {
	now := time.Now().UTC()
	systemPrompt := ""
	if deps.agentCtx != nil {
		systemPrompt = deps.agentCtx.SystemPrompt
	}
	deps.header = session.SessionHeader{
		ID:           session.NewID(now),
		CreatedAt:    now,
		UpdatedAt:    now,
		Model:        deps.live.Model,
		Provider:     deps.live.ProviderName,
		SystemPrompt: systemPrompt,
		Cwd:          deps.cwd,
	}
	if deps.agentCtx == nil {
		deps.agentCtx = &agentcore.AgentContext{SystemPrompt: systemPrompt}
	} else {
		deps.agentCtx.Messages = nil
	}
	deps.persisted = 0
	deps.curLeaf = ""
	deps.lastBtw = nil
	deps.lastBtwBase = 0
	if deps.telemetry != nil {
		deps.telemetry.Reset()
	}

	deps.hookDeps.SessionID = deps.header.ID
	if deps.dispatcher == nil {
		return
	}
	if deps.reminders == nil {
		deps.reminders = runtime.NewReminderRegistry()
	}
	cfg := runtime.RunConfig{Reminders: deps.reminders}
	run.DispatchSessionStart(context.Background(), deps.dispatcher, &cfg, deps.hookDeps, "startup")
	deps.reminders = cfg.Reminders
}
