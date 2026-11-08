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

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/session"
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
	// running is true while a prompt request is in flight; a second prompt is
	// refused with CodeBusy rather than queued (a UI that wants to wait can watch
	// the turn's events).
	running bool
	// cancel aborts the in-flight turn, or nil when idle.
	cancel context.CancelFunc
	// perms is the wait table for permission requests issued by this session's
	// tool gate.
	perms *permissionTable
	// closed makes a second close a no-op.
	closed bool
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
