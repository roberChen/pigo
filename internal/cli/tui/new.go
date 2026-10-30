// This file implements the TUI's /new command: start a fresh, empty session
// without leaving pigo.
//
// It is the counterpart of /resume: a new session is assembled exactly as a launch
// would (new id, empty context, SessionStart dispatched for the new id, blank
// transcript), while the outgoing session's unsaved tail is persisted first. The
// configuration that is live now — which may have been reloaded since launch — is
// what the new session runs with, so /new never silently resurrects the launch-time
// tool set.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// sessionOptions derives the Options for a session switch from the running
// session, so /new and /resume keep the configuration that is in effect now
// (possibly reloaded) instead of the launch-time copy Model.opts still holds.
// Everything the session does not own (prompt-template sources, approval, version,
// dream settings) is inherited unchanged.
func (m Model) sessionOptions() Options {
	opts := m.opts
	s := m.session
	if s == nil {
		return opts
	}
	opts.Model = s.live.Model
	opts.ProviderName = s.live.ProviderName
	opts.Provider = s.live.Provider
	opts.BaseURL = s.live.BaseURL
	opts.Protocol = s.live.Protocol
	opts.ThinkingLevel = s.live.ThinkingLevel
	opts.Tools = s.agentCtx.Tools
	opts.SysPrompt = s.agentCtx.SystemPrompt
	opts.Skills = s.skills
	opts.Plugins = s.plugins
	opts.MCP = s.mcp
	opts.Reloader = s.reloader
	return opts
}

// newSession handles /new: persist the outgoing conversation, then start a fresh
// empty session and reset the screen to it. An argument is rejected rather than
// ignored, so "/new hello" does not look like it started a conversation.
func (m Model) newSession(line string) (tea.Model, tea.Cmd) {
	m.transcript.addUser(line)
	m.input.Clear()
	m.menu.close()
	if m.session == nil {
		m.transcript.addSystem("(new session unavailable: no active session)")
		m.relayout()
		return m, nil
	}
	if arg := trimSlashArg(line, "/new"); arg != "" {
		m.transcript.addSystem("usage: /new (takes no arguments; send your first message afterwards)")
		m.relayout()
		return m, nil
	}
	saveErr := m.session.persist()
	opts := m.sessionOptions()
	opts.ResumeID = ""
	s, history, err := newRunSessionWithStore(m.session.store, opts)
	if err != nil {
		m.transcript.addSystem("new session failed: " + err.Error())
		m.relayout()
		return m, nil
	}
	m = m.swapToSession(s, history, saveErr)
	m.transcript.addSystem(fmt.Sprintf("new session %s (empty)", s.header.ID))
	m.relayout()
	return m, m.input.Focus()
}

// trimSlashArg returns the argument written after the command, without the leading
// "/name" and surrounding whitespace.
func trimSlashArg(line, command string) string {
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), command))
	return arg
}
