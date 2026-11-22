package tui

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/roberChen/pigo/internal/cli/ui"
)

// statusBar renders the persistent bottom line described in the SPEC (US-003,
// Section 5.1): model name, thinking level, loaded-skill count, active MCP
// servers, cwd (with $HOME abbreviated to ~), git branch + ahead marker, the
// changed-file count with its line totals, context-usage %, and the current task
// text. It holds no styling of its own beyond the bar palette; all width fitting
// is done against ui.Width so CJK/emoji count as two columns.
//
// The bar is a row of flush filled blocks — no powerline arrows and no gaps, so
// the glyphs of the items themselves are the only shapes on the line and every
// column belongs to exactly one item. Every block that opens a drawer carries the
// statusSection that identifies it; layout() returns those blocks' column spans
// alongside the row, and the Model turns a click on the bar row into a section
// through that list (statuspanel.go). Hit-testing therefore uses exactly the
// columns that were painted instead of re-deriving them from the text.
//
// The component is a plain value: Update-side code copies it into the Model,
// mutates the exported-to-package snapshot fields via the setters, and calls
// Render(width) from View. It never performs I/O — the git probe lives in
// gitinfo.go and feeds it via SetGit.
type statusBar struct {
	theme Theme

	// Static-ish config sourced from Options.
	model    string
	thinking string

	// skills is how many skills the session loaded (0 hides the segment: there is
	// nothing to list, and the bar is not a place for empty counters). It is
	// refreshed by the Model from the session's skill set.
	skills int

	// mcpActive / mcpTotal are the connected and configured MCP server counts.
	// mcpTotal == 0 (no MCP configured) hides the segment.
	mcpActive int
	mcpTotal  int

	// cwd is the launch directory with $HOME already abbreviated to "~".
	cwd string

	// git is the latest probe result; rendered only when git.ok is true.
	git gitInfoMsg

	// contextPct is the latest context-window utilization in percent [0,100],
	// derived from the live context meter (telemetryEventView.util * 100). -1
	// means the window is not known yet; the segment is permanent, so it renders
	// a placeholder rather than disappearing.
	contextPct int

	// tokens is the most recently observed context-token count, shown alongside
	// the percentage. 0 means "not measured yet", rendered as "--".
	tokens int

	// task is the current activity text (e.g. the running tool or turn state).
	task string
}

// statusSection names a bar segment that opens a drawer above the bar when
// clicked. sectionNone marks a display-only segment (the app badge, the branch,
// the cwd and the activity text).
type statusSection int

const (
	sectionNone statusSection = iota
	sectionModel
	sectionThinking
	sectionSkills
	sectionMCP
	sectionFiles
	sectionContext
)

// statusZone is one clickable block's column span on the bar, in display columns
// from the left edge: the press column x is inside it when x0 <= x < x1.
type statusZone struct {
	section statusSection
	x0, x1  int
}

// newStatusBar builds a status bar from the theme, resolved Options, and the
// launch directory. contextPct starts at -1 (unknown) so the token segment stays
// hidden until telemetry arrives.
func newStatusBar(theme Theme, opts Options, cwd string) statusBar {
	return statusBar{
		theme:      theme,
		model:      opts.Model,
		thinking:   string(opts.ThinkingLevel),
		skills:     len(opts.Skills),
		cwd:        abbreviateHome(cwd),
		contextPct: -1,
	}
}

// SetGit stores the latest git probe result.
func (s *statusBar) SetGit(g gitInfoMsg) { s.git = g }

// SetModel updates the displayed model name after a /model switch.
func (s *statusBar) SetModel(model string) { s.model = model }

// SetThinking updates the displayed reasoning-effort level after a /think switch.
func (s *statusBar) SetThinking(level string) { s.thinking = level }

// SetSkills updates the loaded-skill count shown in the skills block.
func (s *statusBar) SetSkills(n int) { s.skills = n }

// SetMCP updates the active/configured MCP server counts. A total of 0 hides the
// segment, which is the "no MCP configured" case.
func (s *statusBar) SetMCP(active, total int) {
	s.mcpActive = active
	s.mcpTotal = total
}

// SetContext updates the context-usage readout from the live context meter.
// The segment is permanent: an unknown window or an unmeasured conversation
// renders a placeholder instead of hiding the block, so the readout has a fixed
// home the eye can return to.
func (s *statusBar) SetContext(ev telemetryEventView) {
	if ev.window <= 0 || !ev.known {
		s.contextPct = -1
		s.tokens = 0
		return
	}
	s.contextPct = ctxPercent(ev)
	s.tokens = ev.tokens
}

// SetTask records the current activity text shown at the far right / high
// priority slot of the bar.
func (s *statusBar) SetTask(task string) { s.task = task }

// telemetryEventView is the minimal projection of the context meter the status
// bar needs, so the status bar itself does not depend on agentcore for a
// three-field read.
type telemetryEventView struct {
	// util is the used/window ratio in [0,1]; only meaningful with window > 0.
	util float64
	// window is the model's context-token budget (0 when unknown).
	window int
	// tokens is the current context-token estimate.
	tokens int
	// known reports whether a measurement exists at all: false renders the
	// placeholder, distinguishing "nothing measured yet" from a real 0%.
	known bool
}

// appName is the badge shown at the far left of the bar.
const appName = "pigo"

// Glyphs prefixing each segment. They are common single-column Unicode, so the
// bar needs neither a powerline font nor a Nerd Font; ui.Width accounts for each
// during truncation and layout.
const (
	glyphGit     = "⎇" // git branch
	glyphAhead   = "⇡" // commits ahead of upstream
	glyphFiles   = "±" // changed files, with the added/removed line totals
	glyphModel   = "✱" // model name
	glyphThink   = "✽" // thinking level
	glyphSkill   = "✧" // loaded skills
	glyphMCP     = "⚙" // active MCP servers
	glyphCwd     = "▸" // working directory
	glyphCtx     = "◔" // context-window usage
	glyphTask    = "⏵" // current activity
	glyphAdded   = "+" // added lines
	glyphRemoved = "−"
	glyphAction  = "↵" // a drawer row that acts when clicked
)

// Powerline palette (ANSI 256-color cube, so it renders without true-color).
// Every segment is its own colored block on the bar background. The palette
// groups related state: git/working tree green-ish, conversation model/effort
// purple/slate, extensions (skills, MCP) teal/blue, environment steel, context
// amber, activity terracotta.
const (
	sbBarBg = "236" // bar background behind the gaps and the trailing pad

	sbAppFg = "233" // app badge text (dark, on light gray)
	sbAppBg = "252" // app badge block (light gray)

	sbGitFg = "231" // git text
	sbGitBg = "65"  // git block (muted green)

	sbFilesFg = "231" // changed-file count text
	sbFilesBg = "101" // changed-file block (olive)

	sbModelFg = "231" // model text
	sbModelBg = "97"  // model block (muted purple)

	sbThinkFg = "231" // thinking text
	sbThinkBg = "60"  // thinking block (slate)

	sbSkillFg = "231" // skills text
	sbSkillBg = "71"  // skills block (muted teal)

	sbMcpFg = "231" // mcp text
	sbMcpBg = "68"  // mcp block (steel blue)

	sbCwdFg = "231" // cwd text
	sbCwdBg = "67"  // cwd block (steel blue)

	sbCtxFg = "236" // context text (dark, on amber)
	sbCtxBg = "179" // context block (amber/gold)

	sbTaskFg = "231" // task text
	sbTaskBg = "131" // task block (muted terracotta)

	// sbActiveFg/sbActiveBg repaint the segment whose drawer is open, so the bar
	// itself says where the list above it came from.
	sbActiveFg = "232"
	sbActiveBg = "153"
)

// segment is one labelled field of the bar together with its colors and
// truncation priority. bg == "" means the segment sits on the bar background;
// a non-empty bg gives it a filled block. Higher priority survives longer when
// the terminal is too narrow. section != sectionNone makes the block a click
// target that opens the corresponding drawer.
type segment struct {
	text     string
	fg       string
	bg       string // "" => bar background
	priority int    // larger = kept longer under truncation
	section  statusSection
}

// Priority order (SPEC: task > model/app > token/files > git/skills/mcp > cwd).
// Higher is more important and dropped/truncated last. Within one tier the
// leftmost block goes first (dropLowest keeps the first minimum), so a narrow
// terminal loses the branch before the skill and MCP counts — both of which are a
// couple of columns and double as the entry points to their drawers.
const (
	prioCwd   = 0
	prioGit   = 1
	prioSkill = 1
	prioMCP   = 1
	prioToken = 2
	prioFiles = 2
	prioModel = 3
	prioApp   = 3 // the app badge rides at the model tier
	prioTask  = 4
)

// Render lays the bar out to exactly the configured width. Each segment is a
// filled block; blocks are flush and the tail is padded with the bar background,
// so the row is full but carries no separator glyphs. When the
// row would exceed the width it drops whole segments from lowest to highest
// priority; if even the single highest-priority segment still overflows it
// hard-truncates that segment's text. The rendered row's display width (ui.Width,
// which ignores ANSI) is always exactly width for width > 0; a non-positive width
// yields the empty string.
//
// active is the section whose drawer is open (sectionNone for none): that block is
// repainted in the active palette, so the bar itself says where the list above it
// came from. It is a parameter rather than bar state, so the highlight can never
// disagree with the drawer that is actually up.
func (s statusBar) Render(width int, active statusSection) string {
	row, _ := s.layout(width, active)
	return row
}

// layout renders the row and reports the clickable column spans of the blocks
// that survived truncation. It is Render plus the hit map, computed in the same
// pass so the two can never disagree.
func (s statusBar) layout(width int, active statusSection) (string, []statusZone) {
	if width <= 0 {
		return "", nil
	}

	segs := s.segments()
	for len(segs) > 0 {
		row, zones, w := s.renderFlat(segs, active)
		if w <= width {
			return row + barPad(width-w), zones
		}
		if len(segs) == 1 {
			break
		}
		segs = dropLowest(segs)
	}

	// Even a single highest-priority segment overflows: hard-truncate its text
	// onto the bar background (no separators, so width stays bounded).
	best := s.highest()
	txt := TruncateToWidth(best.text, width)
	base := lipgloss.NewStyle().Foreground(lipgloss.Color(sbAppFg)).Background(lipgloss.Color(sbBarBg))
	zones := []statusZone(nil)
	if best.section != sectionNone {
		zones = []statusZone{{section: best.section, x0: 0, x1: ui.Width(txt)}}
	}
	return base.Render(txt) + barPad(width-ui.Width(txt)), zones
}

// sectionAt reports which block covers column x on a bar rendered at width, and
// whether that block opens a drawer. It re-runs layout for that width, so the hit
// test matches the row actually painted — including after truncation dropped or
// shortened a block.
func (s statusBar) sectionAt(x, width int, active statusSection) (statusSection, bool) {
	_, zones := s.layout(width, active)
	for _, z := range zones {
		if x >= z.x0 && x < z.x1 {
			return z.section, true
		}
	}
	return sectionNone, false
}

// renderFlat builds the styled row for segs and returns it with its clickable
// zones and its visible width (excluding ANSI). Blocks sit flush against each
// other — no separator glyphs and no gaps, so the row reads as one continuous
// ribbon of colored fields and every column belongs to exactly one item. The
// block whose drawer is open is repainted in the active palette.
func (s statusBar) renderFlat(segs []segment, active statusSection) (string, []statusZone, int) {
	var b strings.Builder
	var zones []statusZone
	x := 0
	for _, seg := range segs {
		bg := seg.bg
		if bg == "" {
			bg = sbBarBg
		}
		fg := seg.fg
		if seg.section != sectionNone && seg.section == active {
			fg, bg = sbActiveFg, sbActiveBg
		}
		content := " " + seg.text + " "
		b.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color(fg)).
			Background(lipgloss.Color(bg)).
			Render(content))
		w := ui.Width(content)
		if seg.section != sectionNone {
			zones = append(zones, statusZone{section: seg.section, x0: x, x1: x + w})
		}
		x += w
	}
	return b.String(), zones, x
}

// barPad returns n spaces painted with the bar background so the row fills the
// full terminal width. n <= 0 yields the empty string.
func barPad(n int) string {
	if n <= 0 {
		return ""
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color(sbBarBg)).
		Render(strings.Repeat(" ", n))
}

// segments builds the ordered list of visible segments. Order in the slice is
// the left-to-right display order; priority governs truncation, not position.
func (s statusBar) segments() []segment {
	var segs []segment

	// App badge leads the bar as a filled block.
	segs = append(segs, segment{text: appName, fg: sbAppFg, bg: sbAppBg, priority: prioApp})

	if s.git.ok {
		segs = append(segs, segment{text: s.gitBranchText(), fg: sbGitFg, bg: sbGitBg, priority: prioGit})
	}
	// The changed-file block is the working tree's headline: how many paths, and
	// how many lines the edits add and remove. Clean trees show nothing.
	if len(s.git.files) > 0 {
		segs = append(segs, segment{text: s.filesText(), fg: sbFilesFg, bg: sbFilesBg, priority: prioFiles, section: sectionFiles})
	}
	if s.model != "" {
		segs = append(segs, segment{text: glyphModel + " " + s.model, fg: sbModelFg, bg: sbModelBg, priority: prioModel, section: sectionModel})
	}
	if s.thinking != "" {
		// Thinking rides with the model priority — it is cheap and contextual.
		segs = append(segs, segment{text: glyphThink + " " + s.thinking, fg: sbThinkFg, bg: sbThinkBg, priority: prioModel, section: sectionThinking})
	}
	if s.skills > 0 {
		segs = append(segs, segment{text: fmt.Sprintf("%s %d", glyphSkill, s.skills), fg: sbSkillFg, bg: sbSkillBg, priority: prioSkill, section: sectionSkills})
	}
	if s.mcpTotal > 0 {
		// "2/5" reads as active-of-configured, the same shape /mcp reports.
		segs = append(segs, segment{text: fmt.Sprintf("%s %d/%d", glyphMCP, s.mcpActive, s.mcpTotal), fg: sbMcpFg, bg: sbMcpBg, priority: prioMCP, section: sectionMCP})
	}
	if s.cwd != "" {
		segs = append(segs, segment{text: glyphCwd + " " + s.cwd, fg: sbCwdFg, bg: sbCwdBg, priority: prioCwd})
	}
	// The context readout is permanent (the highlighted amber block on the
	// right): before the first measurement it renders glyphCtx + "--" rather than
	// vanishing, so its position never shifts under the user's eye. Clicking it
	// opens the drawer that explains the number (window, reserve, threshold).
	segs = append(segs, segment{text: s.ctxText(), fg: sbCtxFg, bg: sbCtxBg, priority: prioToken, section: sectionContext})
	if s.task != "" {
		segs = append(segs, segment{text: glyphTask + " " + s.task, fg: sbTaskFg, bg: sbTaskBg, priority: prioTask})
	}
	return segs
}

// gitBranchText formats the git segment, e.g. "⎇ master ⇡4": the branch, plus
// "⇡N" when the branch is N commits ahead of its upstream. The working tree's
// own state moved to the files block, so this block stays about the branch.
func (s statusBar) gitBranchText() string {
	var b strings.Builder
	b.WriteString(glyphGit + " " + s.git.branch)
	if s.git.ahead > 0 {
		fmt.Fprintf(&b, " %s%d", glyphAhead, s.git.ahead)
	}
	return b.String()
}

// filesText formats the changed-file segment, e.g. "± 3 +42 −7": the number of
// changed paths followed by the total lines added and removed. A tree whose
// changes carry no line delta (untracked or binary only) shows the count alone
// rather than a misleading "+0 −0".
func (s statusBar) filesText() string {
	added, removed, counted := s.git.lineTotals()
	if !counted {
		return fmt.Sprintf("%s %d", glyphFiles, len(s.git.files))
	}
	return fmt.Sprintf("%s %d %s%d %s%d", glyphFiles, len(s.git.files), glyphAdded, added, glyphRemoved, removed)
}

// ctxText formats the context segment, e.g. "◔ 90,866 (46%)" when the token
// count is known, "◔ 46%" when only the window is known, and "◔ --" before the
// first measurement (the segment is permanent, so it never disappears).
func (s statusBar) ctxText() string {
	if s.contextPct < 0 {
		return fmt.Sprintf("%s --", glyphCtx)
	}
	if s.tokens > 0 {
		return fmt.Sprintf("%s %s (%d%%)", glyphCtx, humanizeInt(s.tokens), s.contextPct)
	}
	return fmt.Sprintf("%s %d%%", glyphCtx, s.contextPct)
}

// humanizeInt renders n with thousands separators, e.g. 90866 → "90,866".
func humanizeInt(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// highest returns the highest-priority segment, used as the last thing standing
// when the terminal cannot even fit one full segment (its section is what the
// truncated row remains clickable as).
func (s statusBar) highest() segment {
	segs := s.segments()
	if len(segs) == 0 {
		return segment{}
	}
	best := segs[0]
	for _, seg := range segs[1:] {
		if seg.priority > best.priority {
			best = seg
		}
	}
	return best
}

// dropLowest removes one occurrence of the lowest-priority segment, preserving
// display order among the rest. It returns the shortened slice.
func dropLowest(segs []segment) []segment {
	if len(segs) == 0 {
		return segs
	}
	lowIdx := 0
	for i, seg := range segs {
		if seg.priority < segs[lowIdx].priority {
			lowIdx = i
		}
	}
	out := make([]segment, 0, len(segs)-1)
	out = append(out, segs[:lowIdx]...)
	out = append(out, segs[lowIdx+1:]...)
	return out
}

// abbreviateHome replaces a leading $HOME in path with "~" so the status bar
// stays compact. It leaves paths outside $HOME untouched and never fails.
func abbreviateHome(path string) string {
	home := homeDir()
	if home == "" || path == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// homeDir returns the user's home directory, or "" when it cannot be
// determined. Kept as a tiny wrapper so abbreviateHome stays testable.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
