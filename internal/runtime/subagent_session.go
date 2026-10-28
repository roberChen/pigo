// This file implements the persisted session of a sub-agent conversation.
//
// A sub-agent is a complete agent: its run is persisted exactly like a top-level
// conversation — same store, same JSONL schema, same entry tree — except that its
// header is parented to the conversation that dispatched it and carries the agent
// name. That makes the child's transcript inspectable after the fact, and it is
// what lets the parent agent continue the same sub-agent conversation later
// ("session_id" on the next call) instead of starting from a fresh context.
//
// The one thing a sub-agent session cannot do is stand alone: it is hidden from
// the resume listings and the drivers refuse to resume it directly
// (session.ErrNotResumableSubAgent). Only its parent continues it.
package runtime

import (
	"fmt"
	"os"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// subAgentSession is the persistence state of one sub-agent conversation: the
// store, the child session's header, and the branch cursor so each run appends
// only its new messages.
type subAgentSession struct {
	store     *session.Store
	header    session.SessionHeader
	curLeaf   string
	persisted int
}

// openSubAgentSession resolves the session backing a sub-agent run. parentSession
// is the dispatching conversation's id; when it is empty (a run without a backing
// session, e.g. an SDK session with persistence disabled) nothing is persisted
// and a nil session is returned.
//
// continueID, when non-empty, selects an earlier run of the same agent to
// continue: it must be a sub-agent session whose parent is this very conversation
// and whose agent matches, so a child cannot be pointed at an unrelated session.
// A fresh run creates a child header parented to parentSession.
func openSubAgentSession(parentSession, continueID, agentName, model, providerName, systemPrompt string) (*subAgentSession, agentcore.MessageList, error) {
	if parentSession == "" {
		return nil, nil, nil
	}
	store, err := session.OpenDefaultStore()
	if err != nil {
		return nil, nil, fmt.Errorf("open session store: %w", err)
	}
	now := time.Now().UTC()
	cwd, _ := os.Getwd()

	if continueID != "" {
		header, entries, err := store.LoadEntries(continueID)
		if err != nil {
			return nil, nil, fmt.Errorf("load sub-agent session %s: %w", continueID, err)
		}
		if !header.IsSubAgent() {
			return nil, nil, fmt.Errorf("session %s is not a sub-agent session and cannot be continued by an agent", continueID)
		}
		if header.ParentSession != parentSession {
			return nil, nil, fmt.Errorf("session %s belongs to another conversation and cannot be continued here", continueID)
		}
		if agentName != "" && header.SubAgent != agentName {
			return nil, nil, fmt.Errorf("session %s belongs to sub-agent %q, not %q", continueID, header.SubAgent, agentName)
		}
		msgs := make(agentcore.MessageList, len(entries))
		for i, e := range entries {
			msgs[i] = e.Message
		}
		leaf := ""
		if len(entries) > 0 {
			leaf = entries[len(entries)-1].ID
		}
		return &subAgentSession{store: store, header: header, curLeaf: leaf, persisted: len(msgs)}, msgs, nil
	}

	header := session.SessionHeader{
		ID:            session.NewID(now),
		CreatedAt:     now,
		UpdatedAt:     now,
		Model:         model,
		Provider:      providerName,
		SystemPrompt:  systemPrompt,
		Cwd:           cwd,
		ParentSession: parentSession,
		SubAgent:      agentName,
	}
	return &subAgentSession{store: store, header: header}, nil, nil
}

// save appends the messages produced since the last save as a branch of the
// child session. A failed save is returned to the caller, which surfaces it on
// the tool result text: the child's work must not be silently lost.
func (s *subAgentSession) save(msgs agentcore.MessageList) error {
	if s == nil || s.persisted >= len(msgs) {
		return nil
	}
	tail := msgs[s.persisted:]
	s.header.UpdatedAt = time.Now().UTC()
	leaf, err := s.store.AppendBranch(s.header, s.curLeaf, tail)
	if err != nil {
		return err
	}
	s.curLeaf = leaf
	s.persisted = len(msgs)
	return nil
}
