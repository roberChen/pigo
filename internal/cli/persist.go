// This file holds PersistTurn, the session-tree persistence step shared by the
// REPL loop and the /goal autonomous loop. It reads and advances a session's
// mutable cursor state through the Host contract, so a command need not import
// the concrete replDeps aggregate to persist the tail of a turn.
package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// PersistTurn appends the messages produced since the last persist as a new
// branch descending from the host's current leaf, advancing the leaf and the
// persisted-message cursor. Growing the tree with AppendBranch (rather than a
// full linear Save) is what lets a later /tree leaf-switch fork the on-disk
// history instead of clobbering it. If nothing new was produced it is a no-op:
// rewriting the file would regenerate entry ids and flatten the tree.
func PersistTurn(out io.Writer, h Host) {
	agentCtx := h.AgentCtx()
	// Automatic compaction during a turn rewrote Messages into a checkpoint + recent
	// tail, shrinking the slice below the persisted cursor, so the append-a-tail
	// branch model no longer holds (Messages[persisted:] would be out of range).
	// Persist the compacted context instead, which keeps the summarized history on
	// disk (see PersistCompactedContext).
	if h.Persisted() > len(agentCtx.Messages) {
		leaf, err := PersistCompactedContext(h.Store(), h.Header(), h.CurLeaf(), agentCtx.Messages)
		if err != nil {
			fmt.Fprintf(out, "pigo: session save failed: %v\n", err)
			return
		}
		h.SetPersisted(len(agentCtx.Messages))
		h.SetCurLeaf(leaf)
		return
	}
	tail := agentCtx.Messages[h.Persisted():]
	if len(tail) == 0 {
		return
	}
	header := h.Header()
	header.UpdatedAt = time.Now().UTC()
	leaf, err := h.Store().AppendBranch(header, h.CurLeaf(), tail)
	if err != nil {
		fmt.Fprintf(out, "pigo: session save failed: %v\n", err)
		return
	}
	h.SetCurLeaf(leaf)
	h.SetPersisted(len(agentCtx.Messages))
}

// PersistCompactedContext writes a context that begins with a compaction
// checkpoint, returning the session's new active leaf. It appends rather than
// rewrites whenever the checkpoint records how much it summarizes: the checkpoint
// is attached to the session tree at the last summarized entry and the rest of the
// context follows it, so the conversation the summary replaces stays on disk
// instead of being deleted (a resume then replays the full history while the agent
// continues from the checkpoint — see session.MessagesSinceLastCompaction).
//
// A linear Save — the collapse-everything fallback — is used only when the context
// cannot be attached that way: it does not start with a checkpoint recording its cut
// point (an older session, or a checkpoint built without one), or the session has
// no persisted entries yet. Callers should prefer this over calling Save directly
// so the two paths stay consistent.
func PersistCompactedContext(store *session.Store, header session.SessionHeader, curLeaf string, msgs agentcore.MessageList) (string, error) {
	header.UpdatedAt = time.Now().UTC()
	leaf, appended, err := store.AppendCompactedContext(header, curLeaf, msgs)
	switch {
	case err != nil:
		return "", err
	case appended:
		return leaf, nil
	}
	if err := store.Save(header, msgs); err != nil {
		return "", err
	}
	// The rewritten file is linear: its tip is the last entry, which is also the
	// leaf the next turn descends from. A read failure here leaves the cursor empty
	// rather than failing the save that already succeeded.
	if _, entries, err := store.LoadEntries(header.ID); err == nil {
		return session.TipLeaf(entries), nil
	}
	return "", nil
}
