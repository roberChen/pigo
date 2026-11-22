package tui

import (
	"os/exec"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// gitInfoMsg is the result of an async git probe (fetchGitCmd). It is defined
// here rather than in msgs.go to avoid conflicting with the event-bridge message
// set (#387). The status bar consumes it to render the branch + working-tree
// state segment and the changed-files drawer.
//
//   - branch: the current branch name (empty when detached / unknown).
//   - ahead:  commits the branch is ahead of its upstream (0 when unknown or no
//     upstream). Derived cheaply from `git status --porcelain -b`.
//   - files:  one entry per changed/untracked path (staged, unstaged and
//     untracked all count), in porcelain order, carrying the status code and
//     (when git can supply it) the added/removed line counts. It is the single
//     source for both what the files block counts and what its drawer lists.
//   - ok:     false when the cwd is not a git repository or any git command
//     failed; the status bar hides the git segment in that case.
type gitInfoMsg struct {
	branch string
	ahead  int
	files  []gitFile
	ok     bool
}

// gitFile is one changed path in the working tree. code is git's two-column
// porcelain status (" M" unstaged modification, "M " staged, "??" untracked, …)
// and is kept verbatim so the drawer can show exactly what git reported.
//
// counts reports whether added/removed are meaningful: untracked files and
// binaries have no line delta (git prints "-" for them), so the drawer says
// "new"/"binary" instead of inventing a "+0 −0".
type gitFile struct {
	path    string
	code    string
	added   int
	removed int
	counts  bool
	binary  bool
}

// lineTotals sums the line deltas of the changed files git could count, and
// reports whether any carried one — untracked and binary files do not, so their
// absence is never rendered as a "+0 −0".
func (g gitInfoMsg) lineTotals() (added, removed int, counted bool) {
	for _, f := range g.files {
		if !f.counts {
			continue
		}
		added += f.added
		removed += f.removed
		counted = true
	}
	return added, removed, counted
}

// fetchGitCmd returns a tea.Cmd that probes the git working tree rooted at cwd
// and reports a gitInfoMsg. It runs read-only git plumbing with fixed arguments
// (no user interpolation, so no command-injection surface) off the tea
// goroutine. Any error — not a repo, git missing, detached parse failure —
// collapses to gitInfoMsg{ok:false}, which the status bar renders as "no git".
func fetchGitCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		// One porcelain call with the branch header gives us branch name, ahead
		// count, and every dirty/untracked entry in a single stable, parseable
		// format (-z would drop the header line, so we keep the newline form).
		out, err := runGit(cwd, "status", "--porcelain", "-b")
		if err != nil {
			return gitInfoMsg{ok: false}
		}
		info := parseGitStatus(out)
		// A second call supplies the per-file line deltas. It is best-effort: a
		// repository with no HEAD (fresh `git init`) errors here, and the drawer
		// then shows the paths without counts instead of losing the whole segment.
		if numstat, err := runGit(cwd, "diff", "--numstat", "HEAD"); err == nil {
			info.files = attachNumstat(info.files, numstat)
		}
		return info
	}
}

// runGit executes a read-only git subcommand in dir and returns its stdout. The
// argument list is always caller-controlled constants (see fetchGitCmd), never
// user input.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseGitStatus parses the output of `git status --porcelain -b` into a
// gitInfoMsg. The first line is the branch header ("## branch...upstream [ahead N,
// behind M]"); every subsequent non-empty line is one changed or untracked entry.
// A parsed result always has ok=true, since reaching this point means git ran.
func parseGitStatus(out string) gitInfoMsg {
	info := gitInfoMsg{ok: true}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if i == 0 {
			info.branch, info.ahead = parseBranchHeader(line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		info.files = append(info.files, parseStatusEntry(line))
	}
	return info
}

// parseStatusEntry splits one `git status --porcelain` line into its two-column
// status code and its path. Renames are reported as "R  old -> new"; the path
// kept is the new one, which is what both the drawer and `git diff --numstat`
// name the file by.
func parseStatusEntry(line string) gitFile {
	f := gitFile{path: strings.TrimSpace(line)}
	if len(line) >= 4 {
		f.code = line[:2]
		f.path = porcelainPath(line[3:])
	}
	// An untracked file has no HEAD version, so there is no line delta to show.
	f.counts = f.code != "??"
	return f
}

// porcelainPath extracts the path from the field git prints after the status
// columns, resolving a rename ("old -> new" → "new") and unquoting a path that
// git had to escape (a leading `"` marks git's C-style quoting).
func porcelainPath(field string) string {
	p := field
	if i := strings.LastIndex(p, " -> "); i >= 0 {
		p = p[i+4:]
	}
	return unquoteGitPath(p)
}

// unquoteGitPath undoes git's C-style path quoting (`"\346\226\207"` → 中文). git
// quotes a path whenever it holds a space, a quote or a non-ASCII byte, and does
// so inconsistently across commands — `status` quotes "a b.go" while `--numstat`
// does not — so both parsers normalise through here before paths are compared.
func unquoteGitPath(p string) string {
	if !strings.HasPrefix(p, `"`) {
		return p
	}
	if unquoted, err := strconv.Unquote(p); err == nil {
		return unquoted
	}
	return p
}

// attachNumstat merges the output of `git diff --numstat HEAD` into the status
// entries, matching by path. A path with no matching entry is ignored (git can
// report a delta the working tree no longer has) and an entry with no delta keeps
// counts=false, so the drawer degrades to a count-free row rather than a lie.
func attachNumstat(files []gitFile, out string) []gitFile {
	if len(files) == 0 {
		return files
	}
	counts := make(map[string][2]int, len(files))
	binaries := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		added, removed, path, binary, ok := parseNumstatLine(line)
		if !ok {
			continue
		}
		counts[path] = [2]int{added, removed}
		if binary {
			binaries[path] = true
		}
	}
	for i := range files {
		c, ok := counts[files[i].path]
		if !ok {
			continue
		}
		files[i].added, files[i].removed = c[0], c[1]
		files[i].binary = binaries[files[i].path]
		files[i].counts = !files[i].binary
	}
	return files
}

// parseNumstatLine parses one `--numstat` line: "added\tremoved\tpath", where
// either count is "-" for a binary file and the path may carry git's rename or
// brace-compacted rename notation.
func parseNumstatLine(line string) (added, removed int, path string, binary, ok bool) {
	if strings.TrimSpace(line) == "" {
		return 0, 0, "", false, false
	}
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) != 3 {
		return 0, 0, "", false, false
	}
	a, aerr := strconv.Atoi(parts[0])
	d, derr := strconv.Atoi(parts[1])
	path = numstatPath(parts[2])
	if aerr != nil || derr != nil {
		// "-" counts: a binary file, which has no line delta.
		return 0, 0, path, true, path != ""
	}
	return a, d, path, false, path != ""
}

// numstatPath resolves the path column of a --numstat line, which for a rename
// is written as "old => new" or, when the two share a directory, as
// "dir/{old => new}". Both forms resolve to the new path, matching what
// porcelain reports so the two sources can be joined. Unlike status, numstat
// quotes only the non-ASCII paths, so the result is unquoted at the end.
func numstatPath(raw string) string {
	p := unquoteGitPath(raw)
	if open := strings.IndexByte(p, '{'); open >= 0 {
		if close := strings.IndexByte(p[open:], '}'); close > 0 {
			inner := p[open+1 : open+close]
			if i := strings.Index(inner, " => "); i >= 0 {
				p = p[:open] + inner[i+4:] + p[open+close+1:]
			}
		}
	} else if i := strings.LastIndex(p, " => "); i >= 0 {
		p = p[i+4:]
	}
	return unquoteGitPath(p)
}

// parseBranchHeader extracts the branch name and ahead count from a porcelain
// branch header line, e.g.:
//
//	## master...origin/master [ahead 4, behind 1]
//	## feature-x
//	## HEAD (no branch)
//
// It returns the branch name and ahead count (0 when absent). A line that is not
// a branch header yields ("", 0).
func parseBranchHeader(line string) (string, int) {
	const prefix = "## "
	if !strings.HasPrefix(line, prefix) {
		return "", 0
	}
	rest := strings.TrimPrefix(line, prefix)

	// Split off the optional " [ahead N, behind M]" tracking suffix.
	branchPart := rest
	ahead := 0
	if idx := strings.Index(rest, " ["); idx >= 0 {
		branchPart = rest[:idx]
		ahead = parseAhead(rest[idx:])
	}

	// "## HEAD (no branch)" — detached; keep the raw token as the branch label.
	branchPart = strings.TrimSpace(branchPart)

	// Trim the "...upstream" tracking-branch tail if present.
	if idx := strings.Index(branchPart, "..."); idx >= 0 {
		branchPart = branchPart[:idx]
	}
	return branchPart, ahead
}

// parseAhead pulls the integer following "ahead " out of a tracking suffix such
// as "[ahead 4, behind 1]". It returns 0 when no ahead count is present.
func parseAhead(suffix string) int {
	const marker = "ahead "
	idx := strings.Index(suffix, marker)
	if idx < 0 {
		return 0
	}
	digits := suffix[idx+len(marker):]
	n := 0
	found := false
	for _, r := range digits {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		found = true
	}
	if !found {
		return 0
	}
	return n
}
