// This file implements the read-only virtual filesystem that exposes the
// built-in skills under the `pigo://` scheme (US: built-in skills are virtual).
//
// Built-in skills used to be copied into the user's skills directory on first
// run, which meant pigo wrote into `~/.agents/skills` — a directory it does not
// own — and could not tell its own installed copy from a hand-written skill of
// the same name. They are now read straight out of the binary: the embedded
// trees are addressable as `pigo://skills/<name>/SKILL.md` and are strictly
// read-only, so a built-in skill can be read but never edited, executed or
// deleted. A user skill of the same name lives on disk and simply takes
// precedence during discovery.
package builtinskills

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
)

// Scheme prefixes every virtual path pigo owns. It is deliberately not a real
// filesystem path, so a `pigo://` string can never collide with (or be opened
// as) a file on disk.
const Scheme = "pigo://"

// VirtualRoot is the top-level directory of the virtual tree. Everything a
// built-in collection contributes lives under it, so the addresses read like
// `pigo://skills/pigorc/SKILL.md`.
const VirtualRoot = "skills"

// ErrReadOnly is returned for any attempt to modify the virtual tree. The file
// tools surface it as a plain message instead of a filesystem error, because
// "built-in skills cannot be written" is a rule, not a failure.
var ErrReadOnly = errors.New("built-in skills are read-only")

// virtualTree is the process-wide view of `pigo://`. It layers two sources:
//
//   - the embedded skill trees (skills/<name>/...), compiled into the binary;
//   - files generated at runtime by the assembly layer, currently the `pigorc`
//     skill's configuration reference, which is rendered from the running
//     binary's own registries rather than committed by hand.
//
// The generated layer wins, so the reference always describes this build.
type virtualTree struct {
	embed fs.FS

	mu        sync.RWMutex
	generated map[string][]byte
}

var tree = &virtualTree{embed: skillsFS, generated: map[string][]byte{}}

// IsVirtual reports whether p addresses the virtual tree.
func IsVirtual(p string) bool { return strings.HasPrefix(p, Scheme) }

// VirtualPath returns the address of a file inside a built-in skill, e.g.
// VirtualPath("pigorc", "SKILL.md") == "pigo://skills/pigorc/SKILL.md".
func VirtualPath(skill, file string) string {
	return Scheme + path.Join(VirtualRoot, skill, file)
}

// SkillPath returns the directory address of a built-in skill, with a trailing
// slash so it reads as a directory rather than a file.
func SkillPath(skill string) string { return Scheme + path.Join(VirtualRoot, skill) + "/" }

// canonical converts a `pigo://` address into a path inside the virtual tree.
// It rejects anything that escapes the root: path.Clean runs on a rooted path,
// so ".." segments collapse instead of climbing out.
func canonical(p string) (string, error) {
	rest, ok := strings.CutPrefix(p, Scheme)
	if !ok {
		return "", fmt.Errorf("%q is not a %s path", p, Scheme)
	}
	clean := strings.TrimPrefix(path.Clean("/"+rest), "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%s is a directory, not a file", Scheme)
	}
	return clean, nil
}

// Read returns the contents of a virtual file. Generated files win over the
// embedded ones. Errors are wrapped with the address the caller used so a
// typo's message names the `pigo://` form, not the internal layout.
func (v *virtualTree) Read(p string) ([]byte, error) {
	name, err := canonical(p)
	if err != nil {
		return nil, err
	}
	v.mu.RLock()
	data, ok := v.generated[name]
	v.mu.RUnlock()
	if ok {
		return data, nil
	}
	data, err = fs.ReadFile(v.embed, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: no such built-in file", p)
		}
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return data, nil
}

// Stat reports a virtual file's metadata, or fs.ErrNotExist when the address
// names nothing. The read tool uses it to tell a file from a directory before
// reading, mirroring what os.Stat does for a disk path.
func (v *virtualTree) Stat(p string) (fs.FileInfo, error) {
	name, err := canonical(p)
	if err != nil {
		return nil, err
	}
	v.mu.RLock()
	_, generated := v.generated[name]
	v.mu.RUnlock()
	if generated {
		return virtualInfo{name: path.Base(name), size: int64(len(v.generated[name]))}, nil
	}
	info, err := fs.Stat(v.embed, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: no such built-in file", p)
		}
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return info, nil
}

// Write always fails: the virtual tree is compiled in and must not be altered
// at runtime. It exists so the write/edit tools can reject a `pigo://` path
// through the same seam they use for disk writes.
func (v *virtualTree) Write(string, []byte) error { return ErrReadOnly }

// SetGenerated publishes a file generated by the running binary, replacing any
// previously generated version. Only the assembly layer calls it, once per
// launch; passing empty data removes the entry.
func (v *virtualTree) SetGenerated(p string, data []byte) error {
	name, err := canonical(p)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(data) == 0 {
		delete(v.generated, name)
		return nil
	}
	v.generated[name] = data
	return nil
}

// SkillNames returns every built-in skill in the embedded tree, sorted by name.
// It is derived from the embedded files rather than from Manifest so a skill
// added to the tree is discoverable even if a collection forgets to list it.
func (v *virtualTree) SkillNames() []string {
	entries, err := fs.ReadDir(v.embed, VirtualRoot)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, statErr := fs.Stat(v.embed, path.Join(VirtualRoot, e.Name(), "SKILL.md")); statErr != nil {
			continue // a directory without SKILL.md is not a skill
		}
		names = append(names, e.Name())
	}
	return names
}

// ReadVirtual returns the contents of a `pigo://` address.
func ReadVirtual(p string) ([]byte, error) { return tree.Read(p) }

// StatVirtual reports metadata for a `pigo://` address.
func StatVirtual(p string) (fs.FileInfo, error) { return tree.Stat(p) }

// WriteVirtual always returns ErrReadOnly.
func WriteVirtual(p string, data []byte) error { return tree.Write(p, data) }

// SetGenerated publishes a runtime-generated virtual file (see
// virtualTree.SetGenerated).
func SetGenerated(p string, data []byte) error { return tree.SetGenerated(p, data) }

// BuiltinSkillNames returns the names of the built-in skills.
func BuiltinSkillNames() []string { return tree.SkillNames() }

// virtualInfo describes a generated file, which has no on-disk counterpart.
type virtualInfo struct {
	name string
	size int64
}

func (i virtualInfo) Name() string       { return i.name }
func (i virtualInfo) Size() int64        { return i.size }
func (i virtualInfo) Mode() fs.FileMode  { return 0o444 }
func (i virtualInfo) ModTime() time.Time { return time.Time{} }
func (i virtualInfo) IsDir() bool        { return false }
func (i virtualInfo) Sys() any           { return nil }
