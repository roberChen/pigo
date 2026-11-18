// Package builtinskills exposes the skills that ship inside the pigo binary.
//
// The skill trees are compiled in with //go:embed and are read-only: they are
// addressable through the `pigo://` scheme (see vfs.go) instead of being copied
// into the user's skills directory. That keeps pigo from writing into
// `~/.agents/skills` — a directory it does not own — and makes it unambiguous
// which skills are built in: a built-in can be read but never edited, executed
// or deleted, and a user skill of the same name on disk simply wins.
//
// The package is deliberately dependency-free (standard library only). The file
// tools resolve `pigo://` through it, and the file tools sit below the packages
// that could otherwise own this, so importing them here would create a cycle.
package builtinskills

import (
	"embed"
)

// skillsFS holds the embedded skill trees under skills/<name>/. The `all:`
// prefix is required so files whose names begin with '_' or '.' (e.g.
// weather/_meta.json) are embedded too — the default pattern skips them.
//
//go:embed all:skills
var skillsFS embed.FS

// Set is one collection of built-in skills. Sets exist for provenance and
// diagnostics only: discovery walks the embedded tree, so a skill added to
// skills/<name>/ is available without touching this file.
type Set struct {
	// Name identifies the collection (e.g. "goal-workflow").
	Name string
	// Skills lists the skill directory names the collection contributes.
	Skills []string
}

// goalWorkflowSkills is the goal-workflow set (https://goal.rpcx.io) minus
// humanize-it (intentionally excluded), plus the two standalone skills
// architecture-diagram and weather.
var goalWorkflowSkills = []string{
	// goal-workflow core
	"prd", "prd-to-spec", "to-issues", "review-it", "ship-it",
	// goal-workflow bonus (humanize-it intentionally excluded)
	"insight-diagram", "refactor", "modern-go", "note-it",
	"code-to-spec", "smell", "loop-it", "to-design", "graph",
	// standalone additions
	"architecture-diagram", "weather",
}

// pigoConfigSkills is the configuration collection: the `pigorc` skill documents
// how to configure pigo and carries a machine-generated reference that pigo
// renders into the virtual tree on every launch (see internal/cli/run).
var pigoConfigSkills = []string{"pigorc"}

// Manifest lists the built-in collections. It is purely descriptive: nothing is
// installed from it and discovery does not consult it, because the embedded tree
// is authoritative. Its job is to be the declared inventory the tests check the
// tree against, in both directions — a listed skill must be addressable, and a
// skill in the tree must be listed — so the two cannot drift apart silently.
func Manifest() []Set {
	return []Set{
		{Name: "goal-workflow", Skills: goalWorkflowSkills},
		{Name: "pigo-config", Skills: pigoConfigSkills},
	}
}
