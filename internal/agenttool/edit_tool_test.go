package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
)

func runEdit(t *testing.T, tool *EditTool, args map[string]any) agentcore.AgentToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, gerr := tool.Execute(context.Background(), "call-1", raw, nil)
	if gerr != nil {
		t.Fatalf("execute returned go error: %v", gerr)
	}
	return res
}

func seedFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return p
}

func TestEditToolUniqueMatch(t *testing.T) {
	dir := t.TempDir()
	p := seedFile(t, dir, "f.txt", "alpha\nbeta\ngamma\n")
	tool := &EditTool{Root: dir}
	res := runEdit(t, tool, map[string]any{"path": "f.txt", "old_string": "beta", "new_string": "BETA"})
	if strings.Contains(resultText(res), "not found") || strings.Contains(resultText(res), "not unique") {
		t.Fatalf("unexpected error: %q", resultText(res))
	}
	got, _ := os.ReadFile(p)
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Errorf("content = %q", got)
	}
	// Diff present.
	if !strings.Contains(resultText(res), "-beta") || !strings.Contains(resultText(res), "+BETA") {
		t.Errorf("diff missing markers: %q", resultText(res))
	}
}

func TestEditToolNonUniqueErrors(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, "f.txt", "x\nx\nx\n")
	tool := &EditTool{Root: dir}
	res := runEdit(t, tool, map[string]any{"path": "f.txt", "old_string": "x", "new_string": "y"})
	if !strings.Contains(resultText(res), "not unique") {
		t.Errorf("expected non-unique error, got %q", resultText(res))
	}
	// File unchanged.
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != "x\nx\nx\n" {
		t.Errorf("file should be unchanged, got %q", got)
	}
}

func TestEditToolReplaceAll(t *testing.T) {
	dir := t.TempDir()
	p := seedFile(t, dir, "f.txt", "x\nx\nx\n")
	tool := &EditTool{Root: dir}
	res := runEdit(t, tool, map[string]any{"path": "f.txt", "old_string": "x", "new_string": "y", "replace_all": true})
	got, _ := os.ReadFile(p)
	if string(got) != "y\ny\ny\n" {
		t.Errorf("content = %q, want all replaced", got)
	}
	details, ok := res.Details.(map[string]any)
	if !ok || details["replacements"] != 3 {
		t.Errorf("expected 3 replacements, details = %+v", res.Details)
	}
}

func TestEditToolNotFound(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, "f.txt", "hello\n")
	tool := &EditTool{Root: dir}
	res := runEdit(t, tool, map[string]any{"path": "f.txt", "old_string": "missing", "new_string": "x"})
	if !strings.Contains(resultText(res), "not found") {
		t.Errorf("expected not-found error, got %q", resultText(res))
	}
}

func TestEditToolMissingFile(t *testing.T) {
	tool := &EditTool{Root: t.TempDir()}
	res := runEdit(t, tool, map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"})
	if !strings.Contains(resultText(res), "does not exist") {
		t.Errorf("expected does-not-exist, got %q", resultText(res))
	}
}

func TestEditToolIdenticalStrings(t *testing.T) {
	dir := t.TempDir()
	seedFile(t, dir, "f.txt", "a\n")
	tool := &EditTool{Root: dir}
	res := runEdit(t, tool, map[string]any{"path": "f.txt", "old_string": "a", "new_string": "a"})
	if !strings.Contains(resultText(res), "identical") {
		t.Errorf("expected identical error, got %q", resultText(res))
	}
}

func TestEditToolPathTraversal(t *testing.T) {
	dir := t.TempDir()
	tool := &EditTool{Root: dir}
	res := runEdit(t, tool, map[string]any{"path": "../x.txt", "old_string": "a", "new_string": "b"})
	if !strings.Contains(resultText(res), "outside the workspace root") {
		t.Errorf("expected boundary error, got %q", resultText(res))
	}
}

func TestEditToolExtraRootsAllowsSkillModification(t *testing.T) {
	work := t.TempDir()
	skills := t.TempDir()
	skillFile := filepath.Join(skills, "weather", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(skillFile, []byte("old body\n"), 0o644); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	// Without ExtraRoots the out-of-workspace skill edit is rejected.
	bounded := &EditTool{Root: work}
	res := runEdit(t, bounded, map[string]any{"path": skillFile, "old_string": "old body", "new_string": "new body"})
	if !strings.Contains(resultText(res), "outside the workspace root") {
		t.Fatalf("expected boundary rejection without ExtraRoots, got %q", resultText(res))
	}

	// With the skills dir as an extra root the edit applies.
	tool := &EditTool{Root: work, ExtraRoots: []string{skills}}
	res = runEdit(t, tool, map[string]any{"path": skillFile, "old_string": "old body", "new_string": "new body"})
	if strings.Contains(resultText(res), "outside the workspace root") {
		t.Fatalf("edit still blocked with ExtraRoots: %q", resultText(res))
	}
	got, _ := os.ReadFile(skillFile)
	if !strings.Contains(string(got), "new body") {
		t.Fatalf("skill not modified, content = %q", got)
	}
}

func TestEditToolMode(t *testing.T) {
	tool := &EditTool{}
	if tool.Name() != "edit" {
		t.Errorf("name = %q", tool.Name())
	}
	if tool.ExecutionMode() != agentcore.ToolExecutionSequential {
		t.Error("edit should be sequential")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Errorf("schema not valid JSON: %v", err)
	}
}

func TestUnifiedDiff(t *testing.T) {
	diff := unifiedDiff("f.txt", "a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(diff, "--- a/f.txt") || !strings.Contains(diff, "+++ b/f.txt") {
		t.Errorf("missing header: %q", diff)
	}
	if !strings.Contains(diff, "-b") || !strings.Contains(diff, "+B") {
		t.Errorf("missing change lines: %q", diff)
	}
	// Unchanged context lines carry a leading space.
	if !strings.Contains(diff, " a") || !strings.Contains(diff, " c") {
		t.Errorf("missing context lines: %q", diff)
	}
	// Small files fall entirely inside the context window, so one hunk covers it.
	if got := strings.Count(diff, "@@ -"); got != 1 {
		t.Errorf("hunk header count = %d, want 1: %q", got, diff)
	}
	if !strings.Contains(diff, "@@ -1,3 +1,3 @@") {
		t.Errorf("missing hunk header: %q", diff)
	}
}

func TestUnifiedDiffEqualContentIsEmpty(t *testing.T) {
	if diff := unifiedDiff("f.txt", "a\nb\n", "a\nb\n"); diff != "" {
		t.Errorf("equal contents should produce an empty diff, got %q", diff)
	}
}

func TestUnifiedDiffHunksAndContext(t *testing.T) {
	// A 12-line file with one change at line 6: the diff must show 3 context
	// lines on each side (lines 3..9) and elide the rest.
	var oldB, newB strings.Builder
	for i := 1; i <= 12; i++ {
		oldB.WriteString(fmt.Sprintf("line %d\n", i))
		if i == 6 {
			newB.WriteString("LINE SIX\n")
		} else {
			newB.WriteString(fmt.Sprintf("line %d\n", i))
		}
	}
	diff := unifiedDiff("f.txt", oldB.String(), newB.String())

	if !strings.Contains(diff, "@@ -3,7 +3,7 @@") {
		t.Errorf("missing hunk header for change at line 6: %q", diff)
	}
	for _, want := range []string{" line 3", " line 5", "-line 6", "+LINE SIX", " line 7", " line 9"} {
		if !strings.Contains(diff, want) {
			t.Errorf("missing %q: %q", want, diff)
		}
	}
	// Lines 1, 2 and 10..12 are beyond the context window and must be elided.
	for _, hidden := range []string{"line 1\n", "line 2\n", " line 10", " line 11", " line 12"} {
		if strings.Contains(diff, hidden) {
			t.Errorf("elided context %q leaked into diff: %q", hidden, diff)
		}
	}
}

func TestUnifiedDiffTwoHunks(t *testing.T) {
	// Changes at lines 2 and 11 are far apart: two hunks, each with its own
	// correct @@ header.
	var oldB, newB strings.Builder
	for i := 1; i <= 12; i++ {
		oldB.WriteString(fmt.Sprintf("line %d\n", i))
		switch i {
		case 2:
			newB.WriteString("two changed\n")
		case 11:
			newB.WriteString("eleven changed\n")
		default:
			newB.WriteString(fmt.Sprintf("line %d\n", i))
		}
	}
	diff := unifiedDiff("f.txt", oldB.String(), newB.String())

	if got := strings.Count(diff, "@@ -"); got != 2 {
		t.Fatalf("hunk count = %d, want 2: %q", got, diff)
	}
	if !strings.Contains(diff, "@@ -1,5 +1,5 @@") {
		t.Errorf("missing first hunk header: %q", diff)
	}
	if !strings.Contains(diff, "@@ -8,5 +8,5 @@") {
		t.Errorf("missing second hunk header: %q", diff)
	}
	// The gap between the hunks (lines 6, 7) is elided.
	for _, hidden := range []string{" line 6", " line 7"} {
		if strings.Contains(diff, hidden) {
			t.Errorf("gap line %q leaked between hunks: %q", hidden, diff)
		}
	}
}

func TestUnifiedDiffEdgePositions(t *testing.T) {
	cases := []struct {
		name, oldC, newC, want string
	}{
		{"append at end", "a\nb\n", "a\nb\nc\n", "@@ -1,2 +1,3 @@"},
		{"insert at start", "b\n", "a\nb\n", "@@ -1,1 +1,2 @@"},
		{"create from empty", "", "a\nb\n", "@@ -0,0 +1,2 @@"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := unifiedDiff("f.txt", tc.oldC, tc.newC)
			if !strings.Contains(diff, tc.want) {
				t.Errorf("missing %q: %q", tc.want, diff)
			}
		})
	}
}
