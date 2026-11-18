// This file implements the edit tool (US-017): exact string replacement within
// a file. old_string must match exactly; if it is not unique (and replace_all
// is false) the edit is rejected. A unified-style diff of the change is returned
// for the UI to render. Paths resolve against a Root with the same traversal
// guard as the read/write tools.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/builtinskills"
)

// EditTool performs exact string replacements in files under Root.
type EditTool struct {
	// Root bounds all edits; a path resolving outside Root is rejected. Empty
	// Root defaults to the current working directory.
	Root string
	// ExtraRoots are additional trusted directories an edit may target even though
	// they lie outside Root. It exists for the skills directory so the model can
	// modify existing skills that live outside the workspace.
	ExtraRoots []string
	// Snap, when non-nil, records the file's prior content before it is edited so
	// the /rewind command can roll the change back. It is shared with the write tool.
	Snap *FileSnapshotRecorder
}

// editToolArgs is the decoded argument shape for EditTool.
type editToolArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// Name implements AgentTool.
func (t *EditTool) Name() string { return "edit" }

// Description implements AgentTool.
func (t *EditTool) Description() string {
	return "Replace an exact string in a file. old_string must be unique unless " +
		"replace_all is set. Returns a diff of the change."
}

// Schema implements AgentTool.
func (t *EditTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":        {"type": "string", "description": "File path to edit, relative to the workspace root."},
    "old_string":  {"type": "string", "description": "Exact text to replace."},
    "new_string":  {"type": "string", "description": "Replacement text."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match."}
  },
  "required": ["path", "old_string", "new_string"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Edits mutate the filesystem → sequential.
func (t *EditTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

// resolvePath resolves p against Root (or any ExtraRoots) via the shared
// resolveWithin boundary policy, so every file tool enforces the same
// workspace-escape guard while edits can also reach trusted extra roots.
func (t *EditTool) resolvePath(p string) (string, error) {
	if len(t.ExtraRoots) == 0 {
		return resolveWithin(t.Root, p)
	}
	return resolveWithinAny(append([]string{t.Root}, t.ExtraRoots...), p)
}

// Execute implements AgentTool. Edit failures (no match, non-unique match,
// missing file, out-of-root) are encoded as error results.
func (t *EditTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[editToolArgs](args, "edit")
	if bad != nil {
		return *bad, nil
	}
	if a.Path == "" {
		return errorResult("edit: path is required"), nil
	}
	// Built-in skills live in the binary and are strictly read-only: a write or
	// edit must fail as a rule, not fall through to a confusing path error.
	if builtinskills.IsVirtual(a.Path) {
		return errorResult("edit: " + a.Path + ": " + builtinskills.ErrReadOnly.Error()), nil
	}
	if a.OldString == a.NewString {
		return errorResult("edit: old_string and new_string are identical; nothing to change"), nil
	}
	full, err := t.resolvePath(a.Path)
	if err != nil {
		return errorResult("edit: " + err.Error()), nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult(fmt.Sprintf("edit: file %q does not exist", a.Path)), nil
		}
		return errorResult(fmt.Sprintf("edit: cannot read %q: %v", a.Path, err)), nil
	}
	original := string(data)

	count := strings.Count(original, a.OldString)
	if count == 0 {
		return errorResult(fmt.Sprintf("edit: old_string not found in %q", a.Path)), nil
	}
	if count > 1 && !a.ReplaceAll {
		return errorResult(fmt.Sprintf("edit: old_string is not unique in %q (%d matches); provide more context or set replace_all", a.Path, count)), nil
	}

	var updated string
	if a.ReplaceAll {
		updated = strings.ReplaceAll(original, a.OldString, a.NewString)
	} else {
		updated = strings.Replace(original, a.OldString, a.NewString, 1)
	}

	// Snapshot the prior state before mutating so /rewind can restore it.
	t.Snap.Record(full)
	if err := os.WriteFile(full, []byte(updated), filePerm); err != nil {
		return errorResult(fmt.Sprintf("edit: cannot write %q: %v", a.Path, err)), nil
	}

	diff := unifiedDiff(a.Path, original, updated)
	replaced := 1
	if a.ReplaceAll {
		replaced = count
	}
	msg := fmt.Sprintf("Edited %s (%d replacement(s))\n%s", a.Path, replaced, diff)
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(msg)},
		Details: map[string]any{"path": a.Path, "replacements": replaced, "diff": diff},
	}, nil
}

// diffContextLines is how many unchanged lines of context surround each change
// in the emitted hunks, matching git's default. Unchanged regions farther than
// this from any change are elided, so a one-line edit in a large file still
// produces a small diff.
const diffContextLines = 3

// unifiedDiff produces a unified diff between old and new content in the shape
// git emits: a ---/+++ header, then one "@@ -oldStart,oldCount +newStart,newCount @@"
// hunk per changed region with diffContextLines unchanged lines around it.
// Unchanged lines beyond the context are elided. It returns "" when the
// contents are equal.
func unifiedDiff(path, oldContent, newContent string) string {
	oldLines := splitLinesKeep(oldContent)
	newLines := splitLinesKeep(newContent)

	// Longest common subsequence over lines drives the -/+ markers.
	ops := numberDiffLines(diffLines(oldLines, newLines))
	hunks := groupHunks(ops, diffContextLines)
	if len(hunks) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		for _, op := range h.ops {
			switch op.kind {
			case diffEqual:
				fmt.Fprintf(&b, " %s\n", op.text)
			case diffDelete:
				fmt.Fprintf(&b, "-%s\n", op.text)
			case diffInsert:
				fmt.Fprintf(&b, "+%s\n", op.text)
			}
		}
	}
	return b.String()
}

// splitLinesKeep splits s into lines, dropping a single trailing newline so an
// empty final element is not produced for the common "ends with \n" case.
func splitLinesKeep(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffKind int

const (
	diffEqual diffKind = iota
	diffDelete
	diffInsert
)

type diffOp struct {
	kind diffKind
	text string
	// oldNo / newNo are the 1-based line numbers the op occupies in the old /
	// new file (0 when it has no line on that side). An insert also records the
	// old-file line it precedes and a delete the new-file line it precedes -
	// the values a zero-count hunk header needs.
	oldNo, newNo int
}

// numberDiffLines stamps each op with its old/new line numbers by walking the
// ops with a counter per side: equal consumes both, delete only the old
// counter, insert only the new one.
func numberDiffLines(ops []diffOp) []diffOp {
	oldNo, newNo := 1, 1
	for i := range ops {
		switch ops[i].kind {
		case diffEqual:
			ops[i].oldNo, ops[i].newNo = oldNo, newNo
			oldNo++
			newNo++
		case diffDelete:
			ops[i].oldNo, ops[i].newNo = oldNo, newNo
			oldNo++
		case diffInsert:
			ops[i].oldNo, ops[i].newNo = oldNo, newNo
			newNo++
		}
	}
	return ops
}

// diffHunk is one hunk: a contiguous slice of ops plus the start and line
// counts its @@ header reports.
type diffHunk struct {
	ops                []diffOp
	oldStart, oldCount int
	newStart, newCount int
}

// groupHunks splits numbered ops into hunks: every op within ctx positions of
// a change belongs to that change's hunk, so nearby changes merge into one
// hunk while everything else is elided. It returns nil when nothing changed.
func groupHunks(ops []diffOp, ctx int) []diffHunk {
	n := len(ops)
	if n == 0 {
		return nil
	}

	// dist[i] is the distance in ops from i to the nearest change (0 for a
	// change itself), computed by one sweep from each side. far doubles as an
	// out-of-range index sentinel on both sides, so it must be far enough past
	// the array that the derived distance always exceeds ctx.
	far := n + ctx + 1
	dist := make([]int, n)
	for i := range dist {
		dist[i] = far
	}
	lastChange := -far
	for i := 0; i < n; i++ {
		if ops[i].kind != diffEqual {
			lastChange = i
		}
		if d := i - lastChange; d < dist[i] {
			dist[i] = d
		}
	}
	nextChange := far
	for i := n - 1; i >= 0; i-- {
		if ops[i].kind != diffEqual {
			nextChange = i
		}
		if d := nextChange - i; d < dist[i] {
			dist[i] = d
		}
	}

	var hunks []diffHunk
	for i := 0; i < n; {
		if dist[i] > ctx {
			i++
			continue
		}
		j := i
		for j < n && dist[j] <= ctx {
			j++
		}
		hunks = append(hunks, newHunk(ops[i:j]))
		i = j
	}
	return hunks
}

// newHunk computes a hunk's @@ header from its ops: the starts are the line
// numbers of the first old/new line in the hunk. A hunk with no line on one
// side is a pure insertion/deletion, and the header then reports the line
// *after which* it applies - one before the op's recorded number.
func newHunk(ops []diffOp) diffHunk {
	h := diffHunk{ops: ops}
	for _, op := range ops {
		switch op.kind {
		case diffEqual:
			h.oldCount++
			h.newCount++
			if h.oldStart == 0 {
				h.oldStart = op.oldNo
			}
			if h.newStart == 0 {
				h.newStart = op.newNo
			}
		case diffDelete:
			h.oldCount++
			if h.oldStart == 0 {
				h.oldStart = op.oldNo
			}
		case diffInsert:
			h.newCount++
			if h.newStart == 0 {
				h.newStart = op.newNo
			}
		}
	}
	if h.oldStart == 0 {
		h.oldStart = ops[0].oldNo - 1
	}
	if h.newStart == 0 {
		h.newStart = ops[0].newNo - 1
	}
	return h
}

// diffLines computes a line diff via a standard LCS dynamic-programming table,
// then backtracks to emit equal/delete/insert ops in order.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// lcs[i][j] = length of LCS of a[i:] and b[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, diffOp{kind: diffEqual, text: a[i]})
			i++
			j++
		} else if lcs[i+1][j] >= lcs[i][j+1] {
			ops = append(ops, diffOp{kind: diffDelete, text: a[i]})
			i++
		} else {
			ops = append(ops, diffOp{kind: diffInsert, text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: diffDelete, text: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: diffInsert, text: b[j]})
	}
	return ops
}
