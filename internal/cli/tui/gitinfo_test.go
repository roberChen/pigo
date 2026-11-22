package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseGitStatusEntryCount(t *testing.T) {
	// Sample `git status --porcelain -b` output: header + 4 entries (staged,
	// modified, deleted, untracked).
	out := "## master...origin/master [ahead 4, behind 1]\n" +
		"M  cmd/pigo/main.go\n" +
		" M internal/foo.go\n" +
		"D  cmd/pigo/run.go\n" +
		"?? new_file.go\n"

	info := parseGitStatus(out)
	if !info.ok {
		t.Fatal("parseGitStatus should report ok=true")
	}
	if info.branch != "master" {
		t.Errorf("branch = %q, want master", info.branch)
	}
	if got := len(info.files); got != 4 {
		t.Errorf("files = %d, want 4", got)
	}
	if info.ahead != 4 {
		t.Errorf("ahead = %d, want 4", info.ahead)
	}
}

func TestParseGitStatusCleanTree(t *testing.T) {
	out := "## main...origin/main\n"
	info := parseGitStatus(out)
	if !info.ok {
		t.Fatal("ok should be true")
	}
	if info.branch != "main" {
		t.Errorf("branch = %q, want main", info.branch)
	}
	if len(info.files) != 0 {
		t.Errorf("files = %d, want 0 on a clean tree", len(info.files))
	}
	if info.ahead != 0 {
		t.Errorf("ahead = %d, want 0", info.ahead)
	}
}

func TestParseGitStatusNoUpstream(t *testing.T) {
	out := "## feature-x\n M a.go\n"
	info := parseGitStatus(out)
	if info.branch != "feature-x" {
		t.Errorf("branch = %q, want feature-x", info.branch)
	}
	if info.ahead != 0 {
		t.Errorf("ahead = %d, want 0", info.ahead)
	}
	if got := len(info.files); got != 1 {
		t.Errorf("files = %d, want 1", got)
	}
}

func TestParseGitStatusDetached(t *testing.T) {
	out := "## HEAD (no branch)\n"
	info := parseGitStatus(out)
	if info.branch != "HEAD (no branch)" {
		t.Errorf("branch = %q, want %q", info.branch, "HEAD (no branch)")
	}
}

func TestParseBranchHeaderAhead(t *testing.T) {
	branch, ahead := parseBranchHeader("## dev...origin/dev [ahead 12]")
	if branch != "dev" {
		t.Errorf("branch = %q, want dev", branch)
	}
	if ahead != 12 {
		t.Errorf("ahead = %d, want 12", ahead)
	}
}

func TestParseBranchHeaderBehindOnly(t *testing.T) {
	branch, ahead := parseBranchHeader("## dev...origin/dev [behind 3]")
	if branch != "dev" {
		t.Errorf("branch = %q, want dev", branch)
	}
	if ahead != 0 {
		t.Errorf("ahead = %d, want 0 (behind only)", ahead)
	}
}

func TestFetchGitCmdNonRepo(t *testing.T) {
	// A path that is not a git repository should collapse to ok=false. /tmp is
	// (almost) never a repo; if it somehow is on this host, skip.
	msg := fetchGitCmd("/")()
	git, ok := msg.(gitInfoMsg)
	if !ok {
		t.Fatalf("expected gitInfoMsg, got %T", msg)
	}
	if git.ok {
		t.Skip("host root unexpectedly reports a git repo; skipping")
	}
}

// TestParseGitStatusFiles checks each porcelain entry becomes a gitFile carrying
// its status code, with untracked files flagged as having no line delta.
func TestParseGitStatusFiles(t *testing.T) {
	out := "## master\n" +
		"M  staged.go\n" +
		" M unstaged.go\n" +
		"?? brand_new.go\n" +
		"R  old.go -> renamed.go\n"
	info := parseGitStatus(out)
	if len(info.files) != 4 {
		t.Fatalf("files = %d, want 4 (%+v)", len(info.files), info.files)
	}
	want := []struct {
		path   string
		code   string
		counts bool
	}{
		{"staged.go", "M ", true},
		{"unstaged.go", " M", true},
		{"brand_new.go", "??", false},
		{"renamed.go", "R ", true},
	}
	for i, w := range want {
		got := info.files[i]
		if got.path != w.path || got.code != w.code || got.counts != w.counts {
			t.Errorf("files[%d] = %+v, want path=%q code=%q counts=%v", i, got, w.path, w.code, w.counts)
		}
	}
	// dirty stays the entry count, unchanged by the per-file detail.
	if got := len(info.files); got != 4 {
		t.Errorf("files = %d, want 4", got)
	}
}

// TestParseGitStatusQuotedPath checks a path git had to C-quote is unquoted, so the
// drawer shows the real name.
func TestParseGitStatusQuotedPath(t *testing.T) {
	info := parseGitStatus("## main\n?? \"a b/c.txt\"\n")
	if len(info.files) != 1 {
		t.Fatalf("files = %d, want 1", len(info.files))
	}
	if got := info.files[0].path; got != "a b/c.txt" {
		t.Errorf("path = %q, want %q", got, "a b/c.txt")
	}
}

// TestAttachNumstat checks the numstat deltas are joined onto the porcelain entries
// by path, that a binary file is flagged instead of counted, and that a path with
// no delta keeps counts unset.
func TestAttachNumstat(t *testing.T) {
	files := []gitFile{
		{path: "a.go", code: " M", counts: true},
		{path: "img.png", code: " M", counts: true},
		{path: "gone.go", code: " M", counts: true},
		{path: "renamed.go", code: "R ", counts: false},
	}
	numstat := "12\t3\ta.go\n" +
		"-\t-\timg.png\n" +
		"4\t0\told.go => renamed.go\n"
	got := attachNumstat(files, numstat)

	if got[0].added != 12 || got[0].removed != 3 || !got[0].counts {
		t.Errorf("a.go = %+v, want +12 -3 counted", got[0])
	}
	if !got[1].binary || got[1].counts {
		t.Errorf("img.png = %+v, want binary without counts", got[1])
	}
	if !got[2].counts || got[2].added != 0 || got[2].removed != 0 {
		t.Errorf("gone.go = %+v, want counts still true (no delta reported)", got[2])
	}
	// A brace-compacted rename resolves to the new path and is counted.
	if !got[3].counts || got[3].added != 4 || got[3].removed != 0 {
		t.Errorf("renamed.go = %+v, want +4 -0 counted", got[3])
	}
}

func TestNumstatPath(t *testing.T) {
	cases := map[string]string{
		"a.go":                                    "a.go",
		"old.go => new.go":                        "new.go",
		"cmd/{old.go => new.go}":                  "cmd/new.go",
		"dir/{sub => other}/file.go":              "dir/other/file.go",
		"weird => name/with arrow.go":             "name/with arrow.go",
		`"\344\270\255.md" => "\344\270\2552.md"`: "中2.md",
		`"dir/{a.txt => b.txt}"`:                  "dir/b.txt",
	}
	for in, want := range cases {
		if got := numstatPath(in); got != want {
			t.Errorf("numstatPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchGitCmdCounts runs the whole probe against a real repository: a
// committed tree, one modified file whose name needs git's quoting and one
// non-ASCII rename, checking that both sources (porcelain paths, numstat deltas)
// join up into per-file counts. It skips when git is not installed.
func TestFetchGitCmdCounts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		base := []string{
			"-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "init.defaultBranch=main", "-c", "core.quotepath=true",
		}
		cmd := exec.Command("git", append(base, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("sp ace.go", "a\n")
	write("中文.md", "x\n")
	git("add", "-A")
	git("commit", "-qm", "init")
	write("sp ace.go", "a\nb\nc\n")
	git("mv", "中文.md", "中文2.md")
	write("中文2.md", "x\ny\n")
	git("add", "-A")

	msg, ok := fetchGitCmd(dir)().(gitInfoMsg)
	if !ok {
		t.Fatal("fetchGitCmd did not return a gitInfoMsg")
	}
	if !msg.ok || msg.branch != "main" {
		t.Fatalf("probe = %+v, want ok on branch main", msg)
	}
	byPath := make(map[string]gitFile, len(msg.files))
	for _, f := range msg.files {
		byPath[f.path] = f
	}
	sp, ok := byPath["sp ace.go"]
	if !ok || !sp.counts || sp.added != 2 || sp.removed != 0 {
		t.Errorf("sp ace.go = %+v, want +2 -0 (paths must survive git's quoting)", sp)
	}
	cn, ok := byPath["中文2.md"]
	if !ok || !cn.counts || cn.added != 1 || cn.removed != 0 {
		t.Errorf("中文2.md = %+v, want +1 -0 (rename must resolve to the new path)", cn)
	}
}
