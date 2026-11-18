// This file implements tests for the read-only `pigo://` virtual tree that
// replaced installing built-in skills into the user's skills directory
// (US: built-in skills are virtual). The invariants are: every shipped skill is
// addressable, sub-resources come along, nothing escapes the root, and nothing
// can be written.
package builtinskills

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestEveryManifestSkillIsAddressable verifies each skill the manifest claims is
// reachable at its virtual address — the address the system prompt advertises.
func TestEveryManifestSkillIsAddressable(t *testing.T) {
	for _, set := range Manifest() {
		for _, name := range set.Skills {
			loc := VirtualPath(name, "SKILL.md")
			data, err := ReadVirtual(loc)
			if err != nil {
				t.Errorf("%s/%s: %v", set.Name, name, err)
				continue
			}
			if !strings.HasPrefix(string(data), "---") {
				t.Errorf("%s: does not open with frontmatter", loc)
			}
		}
	}
}

// TestBuiltinSkillNamesMatchesManifest verifies discovery walks the embedded tree
// and finds exactly the skills the manifest lists, so a skill added to the tree is
// never silently invisible and the manifest never overstates the set.
func TestBuiltinSkillNamesMatchesManifest(t *testing.T) {
	var want []string
	for _, set := range Manifest() {
		want = append(want, set.Skills...)
	}
	slices.Sort(want)

	got := BuiltinSkillNames()
	if !slices.Equal(got, want) {
		t.Errorf("BuiltinSkillNames() = %v, want %v", got, want)
	}
}

// TestSubResourcesAreReadable verifies a skill's supporting files are embedded
// and readable, not just its SKILL.md: the skills reference templates and scripts
// by relative path, and a reader must be able to fetch them.
func TestSubResourcesAreReadable(t *testing.T) {
	for _, tc := range []struct{ skill, file string }{
		{"architecture-diagram", "assets/template.html"},
		{"graph", "scripts/render_graph_html.py"},
		{"weather", "_meta.json"},
		{"prd", "LICENSE"},
	} {
		loc := VirtualPath(tc.skill, tc.file)
		data, err := ReadVirtual(loc)
		if err != nil {
			t.Errorf("%s: %v", loc, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s: empty", loc)
		}
	}
}

// TestAddressesAreCanonical verifies the helpers produce the documented shape.
func TestAddressesAreCanonical(t *testing.T) {
	if got := VirtualPath("pigorc", "SKILL.md"); got != "pigo://skills/pigorc/SKILL.md" {
		t.Errorf("VirtualPath = %q", got)
	}
	if got := SkillPath("pigorc"); got != "pigo://skills/pigorc/" {
		t.Errorf("SkillPath = %q", got)
	}
	if !IsVirtual("pigo://skills/x/SKILL.md") {
		t.Error("IsVirtual should accept a pigo:// address")
	}
	for _, p := range []string{"/abs/path", "relative.md", "", "PIgo://skills/x"} {
		if IsVirtual(p) {
			t.Errorf("IsVirtual(%q) should be false", p)
		}
	}
}

// TestReadRejectsEscape verifies a `..` segment cannot climb out of the virtual
// root. path.Clean runs against a rooted path, so traversal collapses instead of
// escaping; the point is that it never reaches an on-disk file.
func TestReadRejectsEscape(t *testing.T) {
	for _, p := range []string{
		"pigo://../../etc/passwd",
		"pigo://skills/../../../etc/passwd",
	} {
		if data, err := ReadVirtual(p); err == nil {
			t.Errorf("ReadVirtual(%q) succeeded with %q; traversal must not resolve", p, data)
		}
	}
}

// TestReadMissingFile verifies a bad address yields a message naming the pigo://
// form the caller used, not the internal embedded layout.
func TestReadMissingFile(t *testing.T) {
	_, err := ReadVirtual("pigo://skills/pigorc/nope.md")
	if err == nil {
		t.Fatal("reading a missing built-in file should fail")
	}
	if !strings.Contains(err.Error(), "pigo://skills/pigorc/nope.md") {
		t.Errorf("error should name the pigo:// address, got %q", err)
	}
}

// TestTreeIsReadOnly verifies the tree cannot be modified at runtime.
func TestTreeIsReadOnly(t *testing.T) {
	if err := WriteVirtual("pigo://skills/pigorc/SKILL.md", []byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteVirtual = %v, want ErrReadOnly", err)
	}
	before, err := ReadVirtual("pigo://skills/pigorc/SKILL.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) == "x" {
		t.Fatal("a rejected write must not change the file")
	}
}

// TestGeneratedFilesWinAndCanBeRemoved verifies the generated layer: pigo
// publishes the pigorc reference into the tree at runtime, it overrides whatever
// the embedded tree holds for that address, and it can be cleared again (which
// restores the embedded copy). Tests must not leak a generated file into other
// tests, so each one restores the tree.
func TestGeneratedFilesWinAndCanBeRemoved(t *testing.T) {
	const loc = "pigo://skills/pigorc/generated-test.md"
	t.Cleanup(func() { _ = SetGenerated(loc, nil) })

	if _, err := ReadVirtual(loc); err == nil {
		t.Fatal("the address should not exist before it is generated")
	}
	if err := SetGenerated(loc, []byte("generated body\n")); err != nil {
		t.Fatalf("SetGenerated: %v", err)
	}
	data, err := ReadVirtual(loc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "generated body\n" {
		t.Errorf("read = %q", data)
	}
	info, err := StatVirtual(loc)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len("generated body\n")) {
		t.Errorf("size = %d", info.Size())
	}

	if err := SetGenerated(loc, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := ReadVirtual(loc); err == nil {
		t.Error("clearing a generated file should make the address disappear")
	}
}

// TestStatDistinguishesDirectory verifies a directory address is reported as a
// directory, so the read tool rejects it with a message instead of trying to
// read it.
func TestStatDistinguishesDirectory(t *testing.T) {
	info, err := StatVirtual("pigo://skills/pigorc")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("a skill directory address should stat as a directory")
	}
}

// TestNoBootstrapArtifactsRemain verifies the package no longer carries the
// installer: the state file it used to write must not be referenced, because a
// leftover record would make pigo look like it still manages installed skills.
func TestNoBootstrapArtifactsRemain(t *testing.T) {
	if _, err := ReadVirtual(VirtualPath("pigorc", "builtin-skills.json")); err == nil {
		t.Error("the bootstrap state file must not be part of the embedded tree")
	}
}
