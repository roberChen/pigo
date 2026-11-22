// This file implements tests for loading built-in skills out of the binary and
// merging them with on-disk skills (US: built-in skills are virtual). The
// invariants are: every built-in parses and carries a pigo:// location, and a
// disk skill of the same name shadows it exactly once.
package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/builtinskills"
)

// TestLoadBuiltinSkills verifies the shipped skills parse and that each one's
// location is a pigo:// address rather than an installed disk path — the address
// the system prompt hands the model to read.
func TestLoadBuiltinSkills(t *testing.T) {
	skills, err := LoadBuiltinSkills()
	if err != nil {
		t.Fatalf("LoadBuiltinSkills: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("no built-in skills were loaded")
	}
	for _, s := range skills {
		if !builtinskills.IsVirtual(s.Path) {
			t.Errorf("%s: location = %q, want a pigo:// address", s.Frontmatter.Name, s.Path)
		}
		if strings.TrimSpace(s.Body) == "" {
			t.Errorf("%s: empty body", s.Frontmatter.Name)
		}
	}
	// Sorted by name, so the prompt listing is stable across runs.
	for i := 1; i < len(skills); i++ {
		if skills[i-1].Frontmatter.Name > skills[i].Frontmatter.Name {
			t.Fatalf("skills are not sorted by name: %q before %q",
				skills[i-1].Frontmatter.Name, skills[i].Frontmatter.Name)
		}
	}
}

// TestLoadBuiltinSkillsIncludesPigorc verifies the skill whose generated
// reference documents pigo's configuration is among them: without it, the model
// cannot look up how to configure pigo.
func TestLoadBuiltinSkillsIncludesPigorc(t *testing.T) {
	skills, err := LoadBuiltinSkills()
	if err != nil {
		t.Fatalf("LoadBuiltinSkills: %v", err)
	}
	for _, s := range skills {
		if s.Frontmatter.Name == "pigorc" {
			return
		}
	}
	t.Fatal("the pigorc skill is missing from the built-in set")
}

// TestMergeSkillsDiskWins verifies a disk skill shadows a built-in one of the same
// name when the name is listed as an override, and that the result contains exactly
// one entry per name — a duplicate would show up twice in /skill and in the model's
// skill list.
func TestMergeSkillsDiskWins(t *testing.T) {
	builtin, err := LoadBuiltinSkills()
	if err != nil {
		t.Fatalf("LoadBuiltinSkills: %v", err)
	}

	dir := t.TempDir()
	own := "---\nname: pigorc\ndescription: user override\n---\n\n# user body\n"
	if err := os.WriteFile(filepath.Join(dir, "pigorc.md"), []byte(own), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	disk, err := LoadSkillsDir(dir)
	if err != nil {
		t.Fatalf("LoadSkillsDir: %v", err)
	}

	merged, ignored := MergeSkills(builtin, disk, []string{"pigorc"})
	if len(ignored) != 0 {
		t.Fatalf("an authorized override must not be reported as ignored: %v", ignored)
	}
	var pigorc int
	for _, s := range merged {
		if s.Frontmatter.Name != "pigorc" {
			continue
		}
		pigorc++
		if builtinskills.IsVirtual(s.Path) {
			t.Errorf("the disk skill should win, got %q", s.Path)
		}
		if !strings.Contains(s.Body, "# user body") {
			t.Errorf("the disk body should win, got %q", s.Body)
		}
	}
	if pigorc != 1 {
		t.Fatalf("pigorc appears %d times, want 1", pigorc)
	}
	// The built-in set is otherwise intact.
	if len(merged) != len(builtin) {
		t.Errorf("merged has %d skills, want %d (the override replaces, not adds)", len(merged), len(builtin))
	}
}

// TestMergeSkillsUnlistedDiskCopyIgnored verifies the default: a disk skill that
// collides with a built-in is NOT used unless its name is listed under
// [skills] override_builtin. Older pigo versions installed their built-ins into the
// skills directory, so this default is what lets a shipped skill be fixed by an
// upgrade instead of being pinned to the stale copy.
func TestMergeSkillsUnlistedDiskCopyIgnored(t *testing.T) {
	builtin, err := LoadBuiltinSkills()
	if err != nil {
		t.Fatalf("LoadBuiltinSkills: %v", err)
	}
	dir := t.TempDir()
	own := "---\nname: pigorc\ndescription: stale copy\n---\n\n# stale body\n"
	if err := os.WriteFile(filepath.Join(dir, "pigorc.md"), []byte(own), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	disk, err := LoadSkillsDir(dir)
	if err != nil {
		t.Fatalf("LoadSkillsDir: %v", err)
	}

	merged, ignored := MergeSkills(builtin, disk, nil)
	var found *Skill
	for _, s := range merged {
		if s.Frontmatter.Name == "pigorc" {
			found = s
		}
	}
	if found == nil {
		t.Fatal("pigorc is missing from the merged set")
	}
	if !builtinskills.IsVirtual(found.Path) {
		t.Errorf("the built-in copy should win by default, got %q", found.Path)
	}
	if len(ignored) != 1 || ignored[0].Frontmatter.Name != "pigorc" {
		t.Fatalf("ignored = %v, want the on-disk pigorc copy", ignored)
	}
	if builtinskills.IsVirtual(ignored[0].Path) {
		t.Errorf("the ignored entry should point at the disk copy, got %q", ignored[0].Path)
	}
}

// TestMergeSkillsOverrideNamesAreCaseAndSpaceInsensitive verifies the allow-list is
// matched the way every other user-facing name match is: forgivingly.
func TestMergeSkillsOverrideNamesAreCaseAndSpaceInsensitive(t *testing.T) {
	builtin := []*Skill{{Frontmatter: SkillFrontmatter{Name: "pigorc"}, Path: "pigo://skills/pigorc/SKILL.md"}}
	disk := []*Skill{{Frontmatter: SkillFrontmatter{Name: "pigorc"}, Path: "/tmp/pigorc.md"}}

	merged, ignored := MergeSkills(builtin, disk, []string{"  PIGORC "})
	if len(ignored) != 0 {
		t.Fatalf("ignored = %v, want none", ignored)
	}
	if len(merged) != 1 || merged[0].Path != "/tmp/pigorc.md" {
		t.Fatalf("merged = %v, want the disk copy", merged)
	}
}

// TestMergeSkillsDuplicateDiskNamesKeepLast verifies two disk skills with the same
// name (a stale foo.md next to foo/SKILL.md) do not get reported as built-in
// collisions: only a real built-in name may shadow, and the disk copy still loads.
func TestMergeSkillsDuplicateDiskNamesKeepLast(t *testing.T) {
	disk := []*Skill{
		{Frontmatter: SkillFrontmatter{Name: "foo"}, Path: "/tmp/foo.md"},
		{Frontmatter: SkillFrontmatter{Name: "foo"}, Path: "/tmp/foo/SKILL.md"},
	}
	merged, ignored := MergeSkills(nil, disk, nil)
	if len(ignored) != 0 {
		t.Fatalf("ignored = %v, want none (no built-in collision)", ignored)
	}
	if len(merged) != 1 || merged[0].Path != "/tmp/foo/SKILL.md" {
		t.Fatalf("merged = %v, want the later disk copy", merged)
	}
}

// TestMergeSkillsBuiltinOnly verifies merging with no disk skills yields the
// built-in set unchanged, so a fresh install (no skills dir) still works.
func TestMergeSkillsBuiltinOnly(t *testing.T) {
	builtin, err := LoadBuiltinSkills()
	if err != nil {
		t.Fatalf("LoadBuiltinSkills: %v", err)
	}
	merged, ignored := MergeSkills(builtin, nil, nil)
	if len(ignored) != 0 {
		t.Fatalf("ignored = %v, want none", ignored)
	}
	if len(merged) != len(builtin) {
		t.Fatalf("merged %d skills, want %d", len(merged), len(builtin))
	}
	for _, s := range merged {
		if !builtinskills.IsVirtual(s.Path) {
			t.Errorf("%s should still be the built-in copy, got %q", s.Frontmatter.Name, s.Path)
		}
	}
}

// TestFormatSkillsForPromptAdvertisesVirtualLocation verifies the prompt hands the
// model a pigo:// location for a built-in skill — the address the read tool
// resolves — instead of an absolute disk path that does not exist.
func TestFormatSkillsForPromptAdvertisesVirtualLocation(t *testing.T) {
	skills, err := LoadBuiltinSkills()
	if err != nil {
		t.Fatalf("LoadBuiltinSkills: %v", err)
	}
	block := FormatSkillsForPrompt(skills)
	if !strings.Contains(block, "<available_skills>") {
		t.Fatalf("no skills block: %q", block)
	}
	if !strings.Contains(block, "pigo://skills/pigorc/SKILL.md") {
		t.Errorf("prompt should advertise the virtual location, got %q", block)
	}
}
