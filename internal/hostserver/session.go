// This file holds the host's session state machine: one conversation's header,
// live agent, run cancellation, persistence cursor and permission wait table.
//
// Persistence reuses internal/session's branch model exactly as the REPL and
// headless drivers do: after each turn the messages produced since the last
// persist are appended as a branch descending from the current leaf, and a
// compaction (which rewrites agentCtx.Messages to fewer entries) rewrites the
// session through PersistCompactedContext instead of silently dropping the turn.
package hostserver

import (
	"context"
	"sync"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli"
	"github.com/roberChen/pigo/internal/hostapi"
	"github.com/roberChen/pigo/internal/session"
)

// sessionState is one live session on the host. The mutable fields are guarded
// by mu; the immutable ones (id, srv, cwd, title, perms) are set before the
// session is published to the server's map. agent is only driven while holding a
// turn, and a prompt request is the only writer, serialized by mu.
type sessionState struct {
	id    string
	cwd   string
	title string

	// srv is the host that owns this session. It is fixed at creation so a
	// detached session (its client is gone) can still persist its turn: the
	// session outlives the connection that created it.
	srv *Server

	mu sync.Mutex
	// owner is the connection currently allowed to drive this session. It is nil
	// while a turn runs on after its client disconnected; a reconnecting client
	// may adopt such a session instead of loading a second copy from disk.
	owner     *conn
	agent     Agent
	header    session.SessionHeader
	curLeaf   string
	persisted int
	// alwaysApproved is scoped to this session, not to the host's directory trust
	// store. A client may only lift its own permission gate; another session on
	// the same host must still answer for itself.
	alwaysApproved bool
	// running is true while a prompt request is in flight; a second prompt is
	// refused with CodeBusy rather than queued (a UI that wants to wait can watch
	// the turn's events).
	running bool
	// cancel aborts the in-flight turn, or nil when idle.
	cancel context.CancelFunc
	// perms is the wait table for permission requests issued by this session's
	// tool gate.
	perms *permissionTable
	// turnID identifies the in-flight turn, and eventSeq is its last event's
	// sequence number. events is the bounded replay log used by a read-only
	// reconnect; observers are watchers, never writers.
	turnID    string
	eventSeq  uint64
	events    []hostapi.SessionEvent
	observers map[*conn]struct{}
	truncated bool
	// turnBase is the context at the turn's start. The live agent's message list
	// is not safe to snapshot while the turn is running.
	turnBase agentcore.MessageList
	// closeOnFinish tells the prompt handler to release the agent after it has
	// persisted the cancelled turn. Closing it earlier races Run's cleanup.
	closeOnFinish bool
	// closed makes a second close a no-op.
	closed bool
}

// maxReplayEvents bounds a running turn's replay log. Overflow is reported in
// SessionResumeResult.EventsTruncated, so a UI knows it must re-read the final
// transcript rather than treating the replay as complete.
const maxReplayEvents = 4096

// startTurn resets the replay log for a new turn. The caller must have taken
// s.observers first (takeObservers) so the previous turn's watchers are told the
// session is theirs to resume again instead of being silently forgotten.
func (s *sessionState) startTurn(turnID string, base agentcore.MessageList) {
	s.turnID = turnID
	s.eventSeq = 0
	s.events = nil
	s.truncated = false
	s.turnBase = append(agentcore.MessageList(nil), base...)
	s.observers = make(map[*conn]struct{})
}

// takeObservers removes and returns the read-only watchers of this session. The
// caller must hold s.mu and notify them after unlocking: a watcher whose
// subscription was dropped without a word would sit on a dead channel forever,
// because no event can reach it again.
func (s *sessionState) takeObservers() []*conn {
	observers := make([]*conn, 0, len(s.observers))
	for c := range s.observers {
		observers = append(observers, c)
	}
	s.observers = nil
	return observers
}

// releaseObservers notifies every dropped watcher and detaches it from its
// connection. reason explains why the subscription ended.
func (s *sessionState) releaseObservers(observers []*conn, reason string) {
	for _, c := range observers {
		_ = c.notify(eventSessionClosed, hostapi.SessionClosedEvent{SessionID: s.id, Reason: reason})
		c.unwatch(s.id)
	}
}

// emit appends an event to the turn log and fans it out to the owner and any
// read-only observers. Sequence numbers are assigned before the first send, so
// a reconnect never sees the same event under two ids.
func (s *sessionState) emit(ev hostapi.SessionEvent) {
	s.mu.Lock()
	s.eventSeq++
	ev.TurnID = s.turnID
	ev.Seq = s.eventSeq
	s.events = append(s.events, ev)
	if len(s.events) > maxReplayEvents {
		s.events = append([]hostapi.SessionEvent(nil), s.events[len(s.events)-maxReplayEvents:]...)
		s.truncated = true
	}
	owner := s.owner
	observers := make([]*conn, 0, len(s.observers))
	for c := range s.observers {
		observers = append(observers, c)
	}
	s.mu.Unlock()

	if owner != nil {
		_ = owner.notify(eventSession, ev)
	}
	for _, c := range observers {
		_ = c.notifyObserver(ev)
	}
}

// replay returns the current running-turn snapshot without changing observer
// registration. Repeated read-only resumes are therefore safe.
func (s *sessionState) replay() (hostapi.SessionResumeResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayLocked()
}

// replayLocked is replay for a caller already holding s.mu. Keeping observe's
// snapshot and registration in one critical section is what makes the replay
// complete: an event emitted between two lock acquisitions would otherwise be in
// neither the snapshot nor the queue and vanish from the observer's stream.
func (s *sessionState) replayLocked() (hostapi.SessionResumeResult, bool) {
	if s.closed || !s.running || s.owner != nil || s.agent == nil {
		return hostapi.SessionResumeResult{}, false
	}
	return hostapi.SessionResumeResult{
		Session:         s.info(),
		Messages:        append(agentcore.MessageList(nil), s.turnBase...),
		TurnID:          s.turnID,
		EventSeq:        s.eventSeq,
		Events:          append([]hostapi.SessionEvent(nil), s.events...),
		EventsTruncated: s.truncated,
	}, true
}

// observe registers c as a read-only watcher of a running, ownerless turn. The
// replay snapshot, the server-side observer registration and the connection-side
// watch queue all happen in one lock section, so new events are either in the
// replay or queued behind the resume response — never lost, never duplicated.
//
// Lock order: s.mu (the session's own lock) is held while taking the
// connection's lock. That direction is fixed (see conn.close, which snapshots
// under the connection lock and only then touches session locks).
func (s *sessionState) observe(c *conn) (hostapi.SessionResumeResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rr, ok := s.replayLocked()
	if !ok {
		return hostapi.SessionResumeResult{}, false
	}
	if s.observers == nil {
		s.observers = make(map[*conn]struct{})
	}
	s.observers[c] = struct{}{}
	// Register the delivery-side state while the session lock is still held: an
	// event emitted between observe returning and c.watch would find no queue and
	// be dropped.
	c.watch(s)
	return rr, true
}

// closeObservers ends every read-only subscription and detaches it from its
// connection. Observers are not owners and cannot keep a session alive.
func (s *sessionState) closeObservers(reason string) {
	s.mu.Lock()
	observers := s.takeObservers()
	s.mu.Unlock()
	s.releaseObservers(observers, reason)
}

// info renders the session's public metadata. A caller must hold s.mu.
func (s *sessionState) info() hostapi.SessionInfo {
	sysPrompt := ""
	if s.agent != nil {
		sysPrompt = s.agent.SystemPrompt()
	}
	_ = sysPrompt
	return hostapi.SessionInfo{
		ID:        s.id,
		Cwd:       s.cwd,
		Model:     s.header.Model,
		Provider:  s.header.Provider,
		Title:     s.title,
		CreatedAt: s.header.CreatedAt,
		UpdatedAt: s.header.UpdatedAt,
		Running:   s.running,
		TurnID:    s.turnID,
	}
}

// permissionTable maps a request id to the channel its answer arrives on. The
// table lives on the session, so a permission.decide naming another session's
// request is rejected by ownership before the lookup even happens.
type permissionTable struct {
	mu      sync.Mutex
	pending map[string]chan permissionAnswer
	seq     int
}

type permissionAnswer struct {
	approve bool
	always  bool
}

func newPermissionTable() *permissionTable {
	return &permissionTable{pending: make(map[string]chan permissionAnswer)}
}

// open registers a new pending request and returns its id and answer channel.
func (t *permissionTable) open() (string, chan permissionAnswer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	id := "perm-" + itoa(t.seq)
	ch := make(chan permissionAnswer, 1)
	t.pending[id] = ch
	return id, ch
}

// resolve delivers an answer to a pending request. It reports false when the id
// is unknown (already answered, timed out, or cancelled), which the server turns
// into CodeNotFound instead of pretending the decision took effect.
func (t *permissionTable) resolve(id string, ans permissionAnswer) bool {
	t.mu.Lock()
	ch, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
	}
	t.mu.Unlock()
	if !ok {
		return false
	}
	ch <- ans
	return true
}

// abortAll fails every pending request with "denied", used when a turn is
// cancelled or the session closes so the tool gate does not wait forever.
func (t *permissionTable) abortAll() {
	t.mu.Lock()
	pending := t.pending
	t.pending = make(map[string]chan permissionAnswer)
	t.mu.Unlock()
	for _, ch := range pending {
		ch <- permissionAnswer{}
	}
}

// appendTail persists the messages produced since the last persist. It mirrors
// headlessSession.persist: a shrunken context means compaction rewrote the
// transcript, so the file is rewritten from the compacted context rather than
// appending an empty tail.
func (s *sessionState) appendTail(msgs agentcore.MessageList) error {
	store := s.srv.store
	// Compaction may replace agentCtx.Messages with fewer entries than are on disk.
	if s.persisted > len(msgs) {
		s.header.Model = s.agentModel()
		s.header.Provider = s.agentProvider()
		leaf, err := cli.PersistCompactedContext(store, s.header, s.curLeaf, msgs)
		if err != nil {
			return err
		}
		s.header.UpdatedAt = time.Now().UTC()
		s.curLeaf = leaf
		s.persisted = len(msgs)
		return nil
	}
	tail := msgs[s.persisted:]
	if len(tail) == 0 {
		return nil
	}
	s.header.Model = s.agentModel()
	s.header.Provider = s.agentProvider()
	s.header.UpdatedAt = time.Now().UTC()
	leaf, err := store.AppendBranch(s.header, s.curLeaf, tail)
	if err != nil {
		return err
	}
	s.curLeaf = leaf
	s.persisted = len(msgs)
	return nil
}

// agentModel/agentProvider read the labels off the live agent, falling back to
// the header so a session that never ran keeps its create-time metadata.
func (s *sessionState) agentModel() string {
	if s.agent != nil {
		if m := s.agent.Model(); m != "" {
			return m
		}
	}
	return s.header.Model
}

func (s *sessionState) agentProvider() string {
	if s.agent != nil {
		if p := s.agent.Provider(); p != "" {
			return p
		}
	}
	return s.header.Provider
}

// itoa avoids pulling strconv into this file for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
