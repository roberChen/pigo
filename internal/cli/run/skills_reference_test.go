package run

// Tests for the generated config reference that ships with the built-in pigorc
// skill. The reference is published into the skill's read-only `pigo://` tree on
// every load (nothing is written to the skills directory), and a user skill of the
// same name shadows the built-in one without receiving generated content.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/builtinskills"
	"github.com/roberChen/pigo/internal/cli/config"
	"github.com/roberChen/pigo/internal/runtime"
)

// TestLoadSkillsPublishesConfigReference verifies the built-in pigorc skill is
// discovered from the binary and its reference is rendered from the current
// binary's registries into the virtual tree on every load.
func TestLoadSkillsPublishesConfigReference(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	skillsDir := t.TempDir()
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
			if !builtinskills.IsVirtual(s.Path) {
				t.Errorf("built-in pigorc location = %q, want a pigo:// address", s.Path)
			}
		}
	}
	if !found {
		t.Fatalf("the built-in pigorc skill was not discovered: %d skills loaded", len(skills))
	}

	// The reference is readable through the virtual tree...
	loc := config.ReferenceVirtualPath()
	data, err := builtinskills.ReadVirtual(loc)
	if err != nil {
		t.Fatalf("reference not published to %s: %v", loc, err)
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

	// ...and nothing was installed into the skills directory.
	if _, err := os.Stat(filepath.Join(skillsDir, "pigorc")); !os.IsNotExist(err) {
		t.Errorf("the built-in skill must not be installed into the skills dir (err=%v)", err)
	}

	// A second load regenerates identically from the same inputs.
	if _, err := LoadSkills(false); err != nil {
		t.Fatalf("reload: %v", err)
	}
	again, err := builtinskills.ReadVirtual(loc)
	if err != nil {
		t.Fatalf("read after reload: %v", err)
	}
	if string(again) != doc {
		t.Error("the reference should be regenerated identically from the same inputs")
	}
}

// TestUserSkillShadowsBuiltinOnlyWhenConfigured verifies the two halves of the
// override rule: a hand-written skill of the same name on disk is IGNORED by
// default (so an upgrade's built-in fix is not pinned to a stale copy left behind by
// an older pigo), and it wins once its name is listed under [skills]
// override_builtin.
func TestUserSkillShadowsBuiltinOnlyWhenConfigured(t *testing.T) {
	skillsDir := t.TempDir()
	t.Setenv("PIGO_HOME", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", skillsDir)
	t.Cleanup(func() { config.SetCurrentSkillOverrides(nil) })

	skillDir := filepath.Join(skillsDir, "pigorc")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	own := "---\nname: pigorc\ndescription: my own pigorc notes\n---\n\n# mine\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(own), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	// Default: the built-in wins, and the unlisted copy is reported as ignored.
	config.SetCurrentSkillOverrides(nil)
	skills, ignored, err := LoadSkillsDetail(false)
	if err != nil {
		t.Fatalf("LoadSkillsDetail: %v", err)
	}
	assertSinglePigorc(t, skills, true, false)
	if len(ignored) != 1 || ignored[0].Frontmatter.Name != "pigorc" {
		t.Fatalf("ignored = %v, want the on-disk pigorc copy", ignored)
	}
	if builtinskills.IsVirtual(ignored[0].Path) {
		t.Errorf("the ignored entry should be the disk copy, got %q", ignored[0].Path)
	}

	// Opted in: the user's copy shadows the built-in, and nothing is ignored.
	config.SetCurrentSkillOverrides([]string{"pigorc"})
	skills, ignored, err = LoadSkillsDetail(false)
	if err != nil {
		t.Fatalf("LoadSkillsDetail: %v", err)
	}
	assertSinglePigorc(t, skills, false, true)
	if len(ignored) != 0 {
		t.Fatalf("ignored = %v, want none once the override is configured", ignored)
	}
}

// assertSinglePigorc asserts the loaded set holds exactly one pigorc skill, with a
// built-in (or disk) source, and — for the disk case — the user's body.
func assertSinglePigorc(t *testing.T, skills []*runtime.Skill, wantBuiltin, wantUserBody bool) {
	t.Helper()
	var matches int
	for _, s := range skills {
		if s.Frontmatter.Name != "pigorc" {
			continue
		}
		matches++
		if got := builtinskills.IsVirtual(s.Path); got != wantBuiltin {
			t.Errorf("pigorc built-in = %v, want %v (path %q)", got, wantBuiltin, s.Path)
		}
		if wantUserBody && !strings.Contains(s.Body, "# mine") {
			t.Errorf("the user's body should win, got %q", s.Body)
		}
	}
	if matches != 1 {
		t.Fatalf("pigorc appears %d times, want exactly 1", matches)
	}
}
