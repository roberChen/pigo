package tui

import (
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli/ui"
)

// newTestStatusBar builds a status bar with a known cwd (already ~-abbreviated
// by the caller's intent) so tests do not depend on the real $HOME.
func newTestStatusBar() statusBar {
	opts := Options{Model: "claude-opus", ThinkingLevel: agentcore.ThinkingHigh}
	s := newStatusBar(DefaultTheme(), opts, "/tmp/project")
	s.cwd = "~/project"
	return s
}

func TestStatusBarRendersAllFields(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{branch: "master", ahead: 4, ok: true, files: []gitFile{
		{path: "a.go", code: " M", added: 30, removed: 5, counts: true},
		{path: "b.go", code: "M ", added: 12, removed: 2, counts: true},
		{path: "c.go", code: "??"},
	}})
	s.SetSkills(4)
	s.SetMCP(2, 5)
	s.SetContext(telemetryEventView{util: 0.42, window: 200000, tokens: 84000, known: true})
	s.SetTask("running: Read")

	const width = 240
	out := s.Render(width, sectionNone)

	for _, want := range []string{
		"pigo",           // app badge
		"claude-opus",    // model
		"high",           // thinking level
		"~/project",      // cwd
		"master",         // git branch
		glyphAhead + "4", // ahead marker
		glyphFiles + " 3 " + glyphAdded + "42 " + glyphRemoved + "7", // file count + line totals
		glyphSkill + " 4", // skill count
		glyphMCP + " 2/5", // active/configured MCP servers
		"42%",             // context usage
		"running: Read",   // task
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q; got %q", want, out)
		}
	}

	if w := ui.Width(out); w > width {
		t.Errorf("render width %d exceeds terminal width %d", w, width)
	}
}

func TestStatusBarHidesGitWhenNotRepo(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{ok: false})

	out := s.Render(120, sectionNone)
	if strings.Contains(out, "master") || strings.Contains(out, "*") || strings.Contains(out, "+") {
		t.Errorf("git segment should be hidden when ok=false; got %q", out)
	}
}

// TestStatusBarContextSegmentIsPermanent checks the context readout is always
// present, even before any measurement: the block renders a "--" placeholder
// instead of disappearing (the segment must be a fixed landmark).
func TestStatusBarContextSegmentIsPermanent(t *testing.T) {
	s := newTestStatusBar()
	// Nothing measured yet: no window and no known count.
	s.SetContext(telemetryEventView{})

	out := s.Render(120, sectionNone)
	if !strings.Contains(out, glyphCtx) {
		t.Fatalf("context segment must always render, got %q", out)
	}
	if !strings.Contains(out, glyphCtx+" --") {
		t.Errorf("unmeasured context should render the -- placeholder; got %q", out)
	}
}

func TestStatusBarTruncationKeepsPriorityFields(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{branch: "master", ahead: 4, ok: true, files: []gitFile{
		{path: "a.go", code: " M", added: 4, removed: 1, counts: true},
		{path: "b.go", code: "??"},
		{path: "c.go", code: "??"},
	}})
	s.SetContext(telemetryEventView{util: 0.42, window: 200000, tokens: 84000, known: true})
	s.SetTask("TASK")

	// Narrow width: only the highest-priority fields (task > model > token)
	// should survive; cwd and git should drop first.
	const width = 24
	out := s.Render(width, sectionNone)

	if w := ui.Width(out); w > width {
		t.Fatalf("truncated render width %d exceeds %d: %q", w, width, out)
	}
	if !strings.Contains(out, "TASK") {
		t.Errorf("highest-priority task field dropped under truncation: %q", out)
	}
	// cwd (lowest priority) must be gone before task.
	if strings.Contains(out, "~/project") {
		t.Errorf("lowest-priority cwd should drop first under truncation: %q", out)
	}
}

func TestStatusBarVeryNarrowNeverOverflows(t *testing.T) {
	s := newTestStatusBar()
	s.SetTask("a-fairly-long-task-description-that-cannot-fit")

	for _, width := range []int{1, 2, 3, 5, 8} {
		out := s.Render(width, sectionNone)
		if w := ui.Width(out); w > width {
			t.Errorf("width %d: render width %d overflows: %q", width, w, out)
		}
	}
}

func TestStatusBarZeroWidthEmpty(t *testing.T) {
	s := newTestStatusBar()
	if out := s.Render(0, sectionNone); out != "" {
		t.Errorf("zero width should render empty, got %q", out)
	}
}

// TestStatusBarContextTokenCount checks the context segment shows a
// comma-grouped token count with the percentage once telemetry reports tokens.
func TestStatusBarContextTokenCount(t *testing.T) {
	s := newTestStatusBar()
	s.SetContext(telemetryEventView{util: 0.46, window: 200000, tokens: 90866, known: true})

	out := s.Render(200, sectionNone)
	for _, want := range []string{glyphCtx, "90,866", "46%"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q; got %q", want, out)
		}
	}
}

func TestHumanizeInt(t *testing.T) {
	cases := map[int]string{0: "0", 90866: "90,866", 1000: "1,000", 999: "999", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := humanizeInt(in); got != want {
			t.Errorf("humanizeInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAbbreviateHome(t *testing.T) {
	home := homeDir()
	if home == "" {
		t.Skip("no home dir available")
	}
	if got := abbreviateHome(home); got != "~" {
		t.Errorf("abbreviateHome(home) = %q, want ~", got)
	}
	if got := abbreviateHome(home + "/foo/bar"); got != "~/foo/bar" {
		t.Errorf("abbreviateHome(home/foo/bar) = %q, want ~/foo/bar", got)
	}
	if got := abbreviateHome("/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("abbreviateHome(/etc/passwd) = %q, want unchanged", got)
	}
}

// TestStatusBarGitTextFormatting checks the branch block carries only the branch
// and the ahead marker: the working tree's own state lives in the files block.
func TestStatusBarGitTextFormatting(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{branch: "main", ok: true})
	out := s.Render(160, sectionNone)
	if strings.Contains(out, glyphAdded) || strings.Contains(out, glyphAhead) {
		t.Errorf("clean tree should show no file/ahead markers: %q", out)
	}
	if !strings.Contains(out, "main") {
		t.Errorf("branch name missing: %q", out)
	}
	if strings.Contains(out, glyphFiles) {
		t.Errorf("clean tree should show no files block: %q", out)
	}
}

// TestStatusBarFilesSegmentLineTotals checks the files block totals the counted
// files only and keeps the plain path count when no file has a delta.
func TestStatusBarFilesSegmentLineTotals(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{ok: true, files: []gitFile{
		{path: "a.go", code: " M", added: 4, removed: 1, counts: true},
		{path: "b.bin", code: " M", binary: true}, // no line delta
		{path: "c.go", code: "??"},                // untracked, no delta
		{path: "d.go", code: "MM", added: 10, removed: 2, counts: true},
	}})
	out := s.Render(200, sectionNone)
	if want := glyphFiles + " 4 " + glyphAdded + "14 " + glyphRemoved + "3"; !strings.Contains(out, want) {
		t.Errorf("files block missing totals %q; got %q", want, out)
	}

	untracked := newTestStatusBar()
	untracked.SetGit(gitInfoMsg{ok: true, files: []gitFile{{path: "new.go", code: "??"}}})
	out = untracked.Render(120, sectionNone)
	if want := glyphFiles + " 1"; !strings.Contains(out, want) {
		t.Errorf("untracked-only files block missing %q; got %q", want, out)
	}
	if strings.Contains(out, glyphAdded) {
		t.Errorf("untracked-only files block should show no line delta: %q", out)
	}
}

// TestStatusBarClickZones checks that the blocks which open drawers report a hit
// span, and that the display-only blocks (app badge, cwd) do not.
func TestStatusBarClickZones(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{branch: "master", ok: true, files: []gitFile{{path: "a.go", code: "??"}}})
	s.SetSkills(2)
	s.SetMCP(1, 3)

	const width = 240
	_, zones := s.layout(width, sectionNone)
	got := make(map[statusSection]bool, len(zones))
	for _, z := range zones {
		got[z.section] = true
	}
	for _, want := range []statusSection{sectionFiles, sectionModel, sectionThinking, sectionSkills, sectionMCP} {
		if !got[want] {
			t.Errorf("section %v has no clickable span; zones = %+v", want, zones)
		}
	}
	if got[sectionNone] {
		t.Errorf("a display-only block must not be clickable: %+v", zones)
	}

	// Every column reports either the section drawn there or no section at all,
	// and the app badge at column 0 is display-only.
	if sec, ok := s.sectionAt(0, width, sectionNone); ok {
		t.Errorf("column 0 (app badge) should not open a drawer, got %v", sec)
	}
	found := map[statusSection]bool{}
	for x := 0; x < width; x++ {
		if sec, ok := s.sectionAt(x, width, sectionNone); ok {
			found[sec] = true
		}
	}
	if !found[sectionModel] || !found[sectionFiles] {
		t.Errorf("hit test did not find the model/files blocks: %v", found)
	}
}

// TestStatusBarDropsClickZonesWithBlocks checks a dropped block leaves no stale
// click target behind: the hit map is recomputed at the same width.
func TestStatusBarDropsClickZonesWithBlocks(t *testing.T) {
	s := newTestStatusBar()
	s.SetSkills(3)

	const narrow = 20
	out := s.Render(narrow, sectionNone)
	if strings.Contains(out, glyphSkill) {
		t.Fatalf("skills block should be dropped at width %d: %q", narrow, out)
	}
	for x := 0; x < narrow; x++ {
		if sec, ok := s.sectionAt(x, narrow, sectionNone); ok && sec == sectionSkills {
			t.Fatalf("dropped skills block still clickable at column %d", x)
		}
	}
}

// TestStatusBarClickZonesMatchRenderedRow pins the hit map to the painted row: on
// every clickable span, the characters actually drawn there must be that block's
// text. Without this, a layout change could leave a block clickable in the wrong
// columns while both the view and the hit test still look plausible on their own.
func TestStatusBarClickZonesMatchRenderedRow(t *testing.T) {
	s := newTestStatusBar()
	s.SetGit(gitInfoMsg{branch: "master", ok: true, files: []gitFile{{path: "a.go", code: "??"}}})
	s.SetSkills(2)
	s.SetMCP(1, 3)

	const width = 240
	out := stripANSI(s.Render(width, sectionNone))
	if got := ui.Width(out); got != width {
		t.Fatalf("row width = %d, want %d (all blocks are single-column)", got, width)
	}
	runes := []rune(out)
	if len(runes) != width {
		t.Fatalf("row runes = %d, want %d", len(runes), width)
	}
	glyphs := map[statusSection]string{
		sectionModel: glyphModel, sectionThinking: glyphThink, sectionSkills: glyphSkill,
		sectionMCP: glyphMCP, sectionFiles: glyphFiles, sectionContext: glyphCtx,
	}
	_, zones := s.layout(width, sectionNone)
	if len(zones) != len(glyphs) {
		t.Fatalf("zones = %d, want one per clickable block (%d)", len(zones), len(glyphs))
	}
	for _, z := range zones {
		got := string(runes[z.x0:z.x1])
		if want, ok := glyphs[z.section]; ok && !strings.Contains(got, want) {
			t.Errorf("section %v spans columns %d-%d = %q, want it to hold %q", z.section, z.x0, z.x1, got, want)
		}
	}
}
