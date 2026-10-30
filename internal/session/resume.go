// This file implements reading a session back as the conversation it represents:
// the path from the root to the session's active leaf, and the slice of that path
// the agent should continue from after a compaction.
//
// Two things make this more than a copy of every entry in the file. A session is a
// tree, not a list: /tree leaf switches, /rewind and a continued fork all append
// sibling branches, so "every entry, in file order" is not a conversation — it is
// the union of several. And a compaction checkpoint stands in for the history it
// summarizes, so the checkpoint and everything after it is the context, while the
// summarized prefix is history to display rather than tokens to resend.
package session

import (
	"errors"
	"os"

	"github.com/smallnest/pigo/internal/agentcore"
)

// TipLeaf returns the id of a session's active leaf: the entry written last. A
// session is appended to in conversation order, so the last entry in the file is
// always the leaf the next turn should descend from — including after a leaf
// switch, which appends the new branch after the abandoned one.
func TipLeaf(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	return entries[len(entries)-1].ID
}

// LoadPath reads a session and returns its header plus the entries from the root
// down to the active leaf (see TipLeaf). Unlike LoadEntries, which returns the
// whole file including abandoned sibling branches, this is the linear conversation
// a resume continues. For a session that never branched the two are identical.
func (s *Store) LoadPath(id string) (SessionHeader, []Entry, error) {
	header, entries, err := s.LoadEntries(id)
	if err != nil {
		return SessionHeader{}, nil, err
	}
	path := PathToLeaf(entries, TipLeaf(entries))
	if len(path) == 0 {
		// A file with no entries (or an unrecoverable chain) still has a header;
		// report it as an empty conversation rather than an error.
		return header, nil, nil
	}
	return header, path, nil
}

// Messages maps entries to their messages, in order. It is the bridge between a
// loaded path and an agent context, so resume paths do not each re-implement it.
func Messages(entries []Entry) agentcore.MessageList {
	msgs := make(agentcore.MessageList, len(entries))
	for i, e := range entries {
		msgs[i] = e.Message
	}
	return msgs
}

// MessagesSinceLastCompaction returns the messages an agent should continue from:
// from the last compaction checkpoint onward, or the whole list when the
// conversation was never compacted. The checkpoint itself is included — it renders
// as a user message describing what came before — while the summarized prefix is
// dropped, so resuming a compacted session does not resend (and re-compact) the
// history the summary already covers.
func MessagesSinceLastCompaction(msgs agentcore.MessageList) agentcore.MessageList {
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := msgs[i].(agentcore.CompactionMessage); ok {
			return msgs[i:]
		}
	}
	return msgs
}

// AppendCompactedContext persists a context that begins with a compaction
// checkpoint, keeping the conversation the checkpoint summarizes instead of
// overwriting it.
//
// It attaches the checkpoint to the session tree at the last summarized entry and
// appends the rest of the context under it, so the file becomes:
//
//	... summarized entries ...          (kept, now a sibling branch)
//	        └── checkpoint -> retained tail -> later messages
//
// The retained tail was already on disk as part of the summarized branch; it is
// re-appended under the checkpoint so the active path still contains it. The
// resumed context therefore equals what the live run held (checkpoint + tail), and
// the earlier conversation remains recoverable (/tree, export) rather than being
// deleted by the rewrite.
//
// A cut point that reaches past the persisted history (a compaction taken mid-turn,
// over messages that never reached the disk) is clamped to the last persisted entry:
// those messages were never in the file, so nothing is lost by attaching there.
//
// It reports whether it could append. A false result means the caller must fall
// back to a linear Save: the context does not start with a checkpoint that records
// how much it summarizes (an older session, or a checkpoint built without a cut
// point), or the session has no persisted entries to attach to.
func (s *Store) AppendCompactedContext(header SessionHeader, leafID string, msgs agentcore.MessageList) (leaf string, appended bool, err error) {
	if len(msgs) == 0 {
		return "", false, nil
	}
	marker, ok := msgs[0].(agentcore.CompactionMessage)
	if !ok || marker.CoveredMessages <= 0 {
		return "", false, nil
	}
	_, entries, err := s.LoadEntries(header.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	path := PathToLeaf(entries, leafID)
	if len(path) == 0 {
		return "", false, nil
	}
	// The checkpoint's covered count indexes the CONTEXT it was taken over, which
	// starts at the previous checkpoint (a resumed compacted session has history on
	// disk that is not in context). Translate it into a position on the active path
	// before attaching, or a second compaction would branch off the wrong node.
	offset := 0
	pathMsgs := Messages(path)
	for i := len(pathMsgs) - 1; i >= 0; i-- {
		if _, ok := pathMsgs[i].(agentcore.CompactionMessage); ok {
			offset = i
			break
		}
	}
	covered := marker.CoveredMessages
	if offset+covered > len(path) {
		// The cut point reaches past what was persisted: the compaction summarized
		// messages of the turn in progress that never reached the disk. Attaching at
		// the last persisted entry loses nothing (those messages were never in the
		// file), and it keeps the context's checkpoint chain intact.
		covered = len(path) - offset
		if covered <= 0 {
			return "", false, nil
		}
	}
	leaf, err = s.AppendBranch(header, path[offset+covered-1].ID, msgs)
	if err != nil {
		return "", false, err
	}
	return leaf, true, nil
}
