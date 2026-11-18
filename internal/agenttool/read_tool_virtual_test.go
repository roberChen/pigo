// This file implements tests for the file tools' handling of `pigo://` paths:
// the read tool serves built-in skill files out of the binary's embedded tree,
// and write/edit refuse them because built-in skills are read-only
// (US: built-in skills are virtual).
package agenttool

import (
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/builtinskills"
)

// TestReadToolReadsBuiltinSkill verifies a pigo:// address is served without
// touching the workspace root, so the location the system prompt advertises is
// actually readable even when it points far outside the cwd.
func TestReadToolReadsBuiltinSkill(t *testing.T) {
	tool := &ReadTool{Root: t.TempDir()}
	res, _ := runRead(t, tool, map[string]any{"path": "pigo://skills/pigorc/SKILL.md"})
	text := resultText(res)
	if strings.Contains(text, "does not exist") || strings.Contains(text, "outside the workspace") {
		t.Fatalf("a built-in skill must be readable regardless of the root: %q", text)
	}
	if !strings.Contains(text, "pigorc") {
		t.Errorf("expected the pigorc skill body, got %q", text[:min(len(text), 200)])
	}
	// Line numbering matches the disk path's shape.
	if !strings.Contains(text, "1\t") {
		t.Errorf("expected numbered lines, got %q", text[:min(len(text), 200)])
	}
}

// TestReadToolBuiltinOffsetLimit verifies paging works on a virtual file, so a
// large built-in skill can be read in slices like any other file.
func TestReadToolBuiltinOffsetLimit(t *testing.T) {
	tool := &ReadTool{Root: t.TempDir()}
	full, _ := runRead(t, tool, map[string]any{"path": "pigo://skills/pigorc/SKILL.md"})
	page, _ := runRead(t, tool, map[string]any{"path": "pigo://skills/pigorc/SKILL.md", "offset": 2, "limit": 3})

	fullText, pageText := resultText(full), resultText(page)
	if pageText == fullText {
		t.Fatal("offset/limit had no effect on a virtual file")
	}
	if !strings.Contains(pageText, "2\t") {
		t.Errorf("page should start at line 2: %q", pageText)
	}
	if strings.Contains(pageText, "1\t") {
		t.Errorf("page must not include line 1: %q", pageText)
	}
}

// TestReadToolBuiltinMissing verifies a bad address reports the pigo:// form and
// does not fall through to the workspace resolver (which would produce a
// confusing "outside the workspace" error for a path that never existed).
func TestReadToolBuiltinMissing(t *testing.T) {
	tool := &ReadTool{Root: t.TempDir()}
	res, _ := runRead(t, tool, map[string]any{"path": "pigo://skills/pigorc/nope.md"})
	text := resultText(res)
	if !strings.Contains(text, "pigo://skills/pigorc/nope.md") {
		t.Errorf("error should name the pigo:// address, got %q", text)
	}
	if strings.Contains(text, "outside the workspace") {
		t.Errorf("a pigo:// miss must not be reported as a workspace escape: %q", text)
	}
}

// TestReadToolBuiltinSubResource verifies a skill's supporting file is readable,
// since skills reference templates and scripts by relative path.
func TestReadToolBuiltinSubResource(t *testing.T) {
	tool := &ReadTool{Root: t.TempDir()}
	res, _ := runRead(t, tool, map[string]any{"path": "pigo://skills/architecture-diagram/assets/template.html"})
	text := resultText(res)
	if strings.Contains(text, "does not exist") {
		t.Fatalf("sub-resource not readable: %q", text)
	}
	if !strings.Contains(strings.ToLower(text), "html") {
		t.Errorf("expected HTML content: %q", text[:min(len(text), 200)])
	}
}

// TestWriteToolRejectsBuiltin verifies the read-only rule: a write to a pigo://
// address fails with the explicit reason rather than a path error, and the file
// is unchanged.
func TestWriteToolRejectsBuiltin(t *testing.T) {
	tool := &WriteTool{Root: t.TempDir()}
	res := runWrite(t, tool, map[string]any{
		"path":    "pigo://skills/pigorc/SKILL.md",
		"content": "hacked",
	})
	text := resultText(res)
	if !strings.Contains(text, builtinskills.ErrReadOnly.Error()) {
		t.Errorf("write should report the read-only rule, got %q", text)
	}

	data, err := builtinskills.ReadVirtual("pigo://skills/pigorc/SKILL.md")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) == "hacked" {
		t.Fatal("a rejected write must not have changed the built-in file")
	}
}

// TestEditToolRejectsBuiltin verifies edit is refused for the same reason.
func TestEditToolRejectsBuiltin(t *testing.T) {
	tool := &EditTool{Root: t.TempDir()}
	before, err := builtinskills.ReadVirtual("pigo://skills/pigorc/SKILL.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	res := runEdit(t, tool, map[string]any{
		"path":       "pigo://skills/pigorc/SKILL.md",
		"old_string": "pigorc",
		"new_string": "hacked",
	})
	text := resultText(res)
	if !strings.Contains(text, builtinskills.ErrReadOnly.Error()) {
		t.Errorf("edit should report the read-only rule, got %q", text)
	}

	after, err := builtinskills.ReadVirtual("pigo://skills/pigorc/SKILL.md")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("a rejected edit must not have changed the built-in file")
	}
}
