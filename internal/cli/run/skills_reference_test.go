package run

// Tests for the generated config reference that ships with the built-in pigorc
// skill: loading skills must (re-)write it from the running binary, and a
// user-owned skill of the same name must never be touched.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/cli/config"
)

// TestLoadSkillsRefreshesConfigReference verifies the built-in pigorc skill is
// installed and its reference is rendered from the current binary's registries on
// every load.
func TestLoadSkillsRefreshesConfigReference(t *testing.T) {
	home := t.TempDir()
	skillsDir := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	t.Setenv("PIGO_SKILLS_DIR", skillsDir)
	config.SetFlagDocs([]config.FlagDoc{{Name: "model", Shorthand: "m", Default: "x", Usage: "model id"}})
	config.SetBuildVersion("test-build")
	t.Cleanup(func() {
		config.SetFlagDocs(nil)
		config.SetBuildVersion("")
	})

	skills, err := LoadSkills(false)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	var found bool
	for _, s := range skills {
		if s.Frontmatter.Name == "pigorc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the built-in pigorc skill was not installed: %d skills loaded", len(skills))
	}

	path := config.SkillsReferencePath(skillsDir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reference not written: %v", err)
	}
	doc := string(data)
	for _, want := range []string{
		config.ReferenceMarker,
		"pigo test-build",
		"## Options (`option <key> <value>`)",
		"| `--model, -m` |",
		"## Hook events",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("reference missing %q", want)
		}
	}

	// A second load regenerates (the file is not append-only) and stays identical
	// with the same inputs.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := LoadSkills(false); err != nil {
		t.Fatalf("reload: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after reload: %v", err)
	}
	if string(again) != doc {
		t.Error("the reference should be regenerated identically from the same inputs")
	}
}

// TestRefreshConfigReferenceLeavesUserSkillAlone verifies a hand-written skill
// named pigorc (installed before the built-in one, or edited by the user) is never
// overwritten with generated content.
func TestRefreshConfigReferenceLeavesUserSkillAlone(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "pigorc")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# my own pigorc notes\n"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	refreshConfigReference(dir)
	if _, err := os.Stat(filepath.Join(skillDir, config.ReferenceFileName)); !os.IsNotExist(err) {
		t.Fatalf("a user-owned skill must not receive a generated reference (err=%v)", err)
	}
}
