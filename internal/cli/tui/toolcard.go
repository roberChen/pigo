package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"

	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/ui"
	"github.com/smallnest/pigo/internal/mcp"
)

// This file implements the rich tool-call card component (US-006, SPEC 3.2,
// FR-6/7/8). A toolCard is a bordered inline block in the transcript that shows a
// single tool invocation: a header naming the tool and what it acted on, the call
// arguments worth showing, the tool's response, and - for tools that report a diff
// (edit, #560) - a colored Diff section.
//
// Rendering is per tool: bash shows its command and an output preview, read shows
// the file it opened, grep/find their pattern, todo a checklist, and an MCP tool a
// compact "server/tool" plus a one-line argument summary instead of the full JSON
// object it used to dump. Every section is capped so one enormous result cannot
// flood the transcript; a capped card says how much is hidden, and Ctrl+O (or a
// click on the card) reveals the rest — the response in full, and the call itself
// argument by argument, values untruncated and multi-line ones broken out, because
// the header only ever carries a clipped fragment of the salient argument. All width math goes through ui.Width /
// WrapToWidth / TruncateToWidth so CJK and emoji (two columns) never split.

// cardState is the lifecycle of a tool card: running while the tool executes,
// then success or warn once it finishes (warn covers a reported tool error).
type cardState int

const (
	cardRunning cardState = iota
	cardSuccess
	cardWarn
)

// respNode is one line of a tool's response, with depth giving the tree indent
// level (each level is rendered as two leading spaces).
type respNode struct {
	text  string
	depth int
}

// cardLineKind selects the style of one rendered body line. Cards mix styles (a
// todo list marks completed items differently from pending ones; a failed tool's
// output is red), so lines carry their own kind instead of every line sharing the
// body style.
type cardLineKind int

const (
	lineBody cardLineKind = iota
	lineDim
	lineGood
	lineWarn
	lineBad
	lineAccent
	// Diff line kinds keep the unified-diff palette (red removals, green additions,
	// cyan hunk markers, dim headers/context) inside the Diff section.
	lineDiffAdd
	lineDiffDel
	lineDiffHunk
	lineDiffCtx
)

// cardLine is one line of a card body.
type cardLine struct {
	text string
	kind cardLineKind
}

// Preview budgets. Each is the number of content lines a section shows while the
// card is collapsed; expanding lifts it to cardExpandedLines. cardArgValueLimit is
// the column budget of one collapsed argument value — expanding drops it, so the
// whole value (multi-line included) is shown.
const (
	cardPreviewLines   = 6   // generic / MCP / file / fetch output
	cardBashLines      = 8   // command output (head + tail)
	cardTaskLines      = 10  // sub-agent report
	cardInputLines     = 3   // argument lines before "+N more"
	cardExpandedLines  = 400 // hard ceiling once expanded
	collapsedDiffLines = 12
	cardRawArgsLimit   = 400
	cardArgValueLimit  = 120
)

// toolCard is a single tool invocation rendered as a bordered card. input holds
// the decoded call arguments (nil when the args were not a JSON object);
// response is the parsed result tree, populated on completion; diff holds a
// unified diff the tool reported in its result metadata (edit today), rendered
// as its own colored section instead of plain response text. expanded flips the
// card between its capped preview and the full content; selected marks the card the
// user picked (a click or card navigation) so its border stands out.
type toolCard struct {
	id       string
	name     string
	input    map[string]any
	response []respNode
	diff     string
	state    cardState
	expanded bool
	selected bool

	// rawArgs holds the argument payload verbatim when it did not decode to a
	// JSON object — a model can emit malformed tool-call JSON (a truncated or
	// duplicated key). The call then fails schema validation with an error result
	// the model can retry from, and the card shows what was actually sent instead
	// of an empty arguments section.
	rawArgs string

	// rev counts the mutations that change the card's rendering (result
	// arrival, expand/select toggle). The transcript caches each block's rendered
	// text and re-renders only when the card's rev differs from the one the cache
	// was built from, so a streamed delta or keystroke elsewhere does not re-draw
	// every finished card.
	rev int
}

// newToolCard builds a card for a tool call from its untyped arguments. When the
// arguments are not a JSON object (malformed JSON as a model can stream), the raw
// text is kept so the card can show it.
func newToolCard(id, name string, args any) *toolCard {
	input := argsToMap(args)
	return &toolCard{id: id, name: name, input: input, rawArgs: rawToolArgs(args, input), state: cardRunning}
}

// rawToolArgs returns the raw argument text to display when input did not decode
// (input == nil), clipped to cardRawArgsLimit. Payloads with no textual form, and
// the empty object a call may legitimately carry, yield "".
func rawToolArgs(args any, input map[string]any) string {
	if input != nil {
		return ""
	}
	var s string
	switch v := args.(type) {
	case json.RawMessage:
		s = string(v)
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" || s == "null" {
		return ""
	}
	if runes := []rune(s); len(runes) > cardRawArgsLimit {
		s = string(runes[:cardRawArgsLimit]) + "…"
	}
	return s
}

// complete attaches the finished call's outcome (status, parsed response tree
// and any diff) and bumps the render revision so the transcript re-renders the
// card on its next reflow.
func (c *toolCard) complete(state cardState, response []respNode, diff string) {
	c.state = state
	c.response = response
	c.diff = diff
	c.rev++
}

// toggleExpanded flips the card between its capped preview and the full content,
// bumping the render revision so the transcript re-renders the card.
func (c *toolCard) toggleExpanded() {
	c.expanded = !c.expanded
	c.rev++
}

// setSelected marks the card as the user's current pick (or clears it). It bumps
// the render revision only on a change, so the cached render stays valid when the
// selection moves between cards.
func (c *toolCard) setSelected(selected bool) {
	if c.selected == selected {
		return
	}
	c.selected = selected
	c.rev++
}

// statusIcon returns the header status glyph for the card's state. Running is a
// spinner-like ellipsis, success a check, warn a bang.
func (c toolCard) statusIcon() string {
	switch c.state {
	case cardSuccess:
		return "✓"
	case cardWarn:
		return "!"
	default:
		return "…"
	}
}

// styledIcon renders the status glyph with the state's theme color: gray while
// running, green on success, yellow/red on warn.
func (c toolCard) styledIcon(theme Theme) string {
	icon := c.statusIcon()
	switch c.state {
	case cardSuccess:
		return theme.Success.Render(icon)
	case cardWarn:
		return theme.Warn.Render(icon)
	default:
		return theme.System.Render(icon)
	}
}

// toolFamily is the rendering family of the tool: the MCP namespace collapses to
// "mcp" (every server's tools render alike), everything else keeps its own name.
func (c toolCard) toolFamily() string {
	name := strings.ToLower(c.name)
	if strings.HasPrefix(name, mcp.ToolNamePrefix) {
		return "mcp"
	}
	return name
}

// displayName is the name shown in the header: an MCP tool becomes
// "mcp server/tool", everything else keeps the tool's own name.
func (c toolCard) displayName() string {
	if c.toolFamily() != "mcp" {
		return c.name
	}
	rest := strings.TrimPrefix(strings.ToLower(c.name), mcp.ToolNamePrefix)
	if i := strings.Index(rest, "__"); i > 0 {
		return "mcp " + rest[:i] + "/" + rest[i+2:]
	}
	return "mcp " + rest
}

// render draws the card at the given content width: a rounded border wrapping the
// header, the extra arguments, the todo checklist, the response preview and - when
// the tool reported a diff (edit) - a colored Diff section. A selected card gets an
// accent-colored border; a capped section says how many lines are hidden.
func (c toolCard) render(theme Theme, width int) string {
	if width < 4 {
		width = 4
	}
	// The rounded border consumes one column on each side; wrap everything to the
	// inner width so nothing overflows the frame.
	inner := width - 2

	// The header mixes three styles (expand marker, status glyph, tool name), so it
	// is composed per row after wrapping: wrapping styled text would count the escape
	// sequences as columns and break the line early.
	rendered := c.headerLines(theme, inner)
	for _, ln := range c.bodyLines(inner) {
		rendered = append(rendered, cardLineStyle(theme, ln.kind).Render(WrapToWidth(ln.text, inner)))
	}

	borderColor := colorGray
	if c.selected {
		borderColor = colorAccent
	}
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Width(inner)
	return border.Render(strings.Join(rendered, "\n"))
}

// headerLines renders the card's title rows: the expand marker, the status glyph and
// the tool summary, wrapped to the inner width and styled row by row.
func (c toolCard) headerLines(theme Theme, inner int) []string {
	marker := "▸"
	if c.expanded {
		marker = "▾"
	}
	glyph := c.statusIcon()
	name := c.headerText()
	plain := marker + " " + glyph + " " + name
	rows := strings.Split(WrapToWidth(plain, inner), "\n")

	out := make([]string, 0, len(rows))
	for i, row := range rows {
		if i > 0 {
			out = append(out, theme.ToolHeader.Render(row))
			continue
		}
		// Split the plain prefix back off so each part keeps its own color.
		rest := strings.TrimPrefix(row, marker+" "+glyph)
		line := theme.System.Render(marker) + " " + c.styledIcon(theme)
		if rest != "" {
			line += theme.ToolHeader.Render(rest)
		}
		out = append(out, line)
	}
	return out
}

// cardLineStyle maps a line kind to its theme style.
func cardLineStyle(theme Theme, kind cardLineKind) lipgloss.Style {
	switch kind {
	case lineDim:
		return theme.System
	case lineGood:
		return theme.Success
	case lineWarn:
		return theme.Warn
	case lineBad:
		return theme.Error
	case lineAccent:
		return theme.Accent
	case lineDiffAdd:
		return theme.DiffAdd
	case lineDiffDel:
		return theme.DiffDel
	case lineDiffHunk:
		return theme.DiffHunk
	case lineDiffCtx:
		return theme.DiffCtx
	default:
		return theme.ToolBody
	}
}

// headerText renders the card's title: the tool (with its server, for MCP) plus the
// one thing it acted on, so a long transcript is scannable without expanding
// anything.
func (c toolCard) headerText() string {
	name := c.displayName()
	if summary := c.summaryArg(); summary != "" {
		return name + "(" + summary + ")"
	}
	return name
}

// summaryArg picks the most salient argument per tool for the header: the command
// for bash, the path for the file tools, the pattern for search, a todo's progress,
// and for an MCP tool its first argument (a server tool usually has one obvious
// input rather than a form).
func (c toolCard) summaryArg() string {
	if len(c.input) == 0 {
		return ""
	}
	switch c.toolFamily() {
	case "bash":
		return clipLine(argText(c.input, "command"), 80)
	case "bash_output", "kill_bash":
		return clipLine(argText(c.input, "job_id", "id"), 40)
	case "read", "view_image", "write", "edit", "multiedit":
		return clipLine(argText(c.input, "path", "file_path"), 90)
	case "grep", "find":
		if p := argText(c.input, "pattern"); p != "" {
			return clipLine(p, 70)
		}
		return clipLine(argText(c.input, "query"), 70)
	case "webfetch":
		return clipLine(argText(c.input, "url"), 90)
	case "websearch":
		return clipLine(argText(c.input, "query"), 80)
	case "todo":
		items := parseTodoItems(c.input)
		if len(items) == 0 {
			return ""
		}
		return fmt.Sprintf("%d/%d done", countTodoDone(items), len(items))
	case "task", "task_output":
		return clipLine(firstLine(argText(c.input, "description", "prompt", "job_id")), 80)
	case "question":
		return clipLine(argText(c.input, "question"), 80)
	case "mcp":
		return ""
	case "memory_search":
		return clipLine(argText(c.input, "query"), 80)
	}
	keys := sortedKeys(c.input)
	if len(keys) == 0 {
		return ""
	}
	return clipLine(fmt.Sprintf("%v", c.input[keys[0]]), 80)
}

// bodyLines builds the card body: extra arguments, a todo checklist, the response
// preview, and the diff. Each section is capped, and a capped section ends with a
// line saying how much is hidden.
func (c toolCard) bodyLines(inner int) []cardLine {
	var out []cardLine
	out = append(out, c.argumentLines(inner)...)
	out = append(out, c.todoLines()...)
	out = append(out, c.responseLines()...)
	out = append(out, c.diffLines()...)
	return out
}

// argumentLines renders the call arguments the header does not already show. A
// generic tool lists its first few keys; an MCP tool collapses all of them into one
// compact line instead of one line per key (a server's schema is often many
// fields, and the full dump crowded out the result). An expanded card shows the
// call in full — see expandedArgLines.
func (c toolCard) argumentLines(inner int) []cardLine {
	if c.rawArgs != "" {
		return c.rawArgLines(inner)
	}
	if len(c.input) == 0 {
		return nil
	}
	if c.toolFamily() == "todo" {
		// The checklist below is the arguments; a raw key dump would repeat it.
		return nil
	}
	if c.expanded {
		return c.expandedArgLines(inner)
	}
	if c.toolFamily() == "mcp" {
		// One line regardless of how many fields the server's schema has: the card is
		// a log entry, not a form. Expanding it breaks the call out per key.
		return []cardLine{{text: "args " + clipLine(c.mcpArgSummary(), maxInt(inner-6, 20)), kind: lineDim}}
	}
	hiddenKeys := c.headerOnlyArgs()
	lines := make([]string, 0, len(c.input))
	for _, k := range sortedKeys(c.input) {
		if hiddenKeys[k] {
			continue
		}
		lines = append(lines, "  "+k+": "+clipLine(fmt.Sprintf("%v", c.input[k]), cardArgValueLimit))
	}
	if len(lines) == 0 {
		return nil
	}
	clamped, hidden := clampStrings(lines, cardInputLines, false)
	out := make([]cardLine, 0, len(clamped)+1)
	for _, ln := range clamped {
		out = append(out, cardLine{text: ln, kind: lineDim})
	}
	if hidden > 0 {
		out = append(out, cardLine{text: fmt.Sprintf("  … %d more args · Ctrl+O", hidden), kind: lineDim})
	}
	return out
}

// expandedArgLines renders the whole call for an expanded card: every argument, in
// full. Expanding is how the user checks what the tool was actually passed, so the
// column budget of the collapsed card no longer applies — a value is neither cut at
// cardArgValueLimit nor flattened onto one line, and a multi-line script or file body
// keeps its line breaks. The header's own argument is included rather than repeated
// as a clipped fragment: the header shows a bash script's first 80 columns, and the
// point of expanding is to see the rest of it. Only the row count stays bounded, by
// cardExpandedLines, because a giant write body would otherwise stall the frame; the
// note on a bounded section says where the rest is.
func (c toolCard) expandedArgLines(inner int) []cardLine {
	rows := make([]string, 0, len(c.input)*2)
	for _, k := range c.expandedKeys() {
		prefix := "  " + k + ": "
		values := argValueRows(c.input[k], maxInt(inner-ui.Width(prefix), 20))
		if len(values) <= 1 {
			value := ""
			if len(values) == 1 {
				value = values[0]
			}
			rows = append(rows, prefix+value)
			continue
		}
		rows = append(rows, "  "+k+":")
		for _, v := range values {
			rows = append(rows, "    "+v)
		}
	}
	clamped, hidden := clampStrings(rows, cardExpandedLines, false)
	out := make([]cardLine, 0, len(clamped)+1)
	for _, row := range clamped {
		out = append(out, cardLine{text: row, kind: lineDim})
	}
	if hidden > 0 {
		out = append(out, cardLine{text: fmt.Sprintf("  … %d more argument lines (see /export)", hidden), kind: lineDim})
	}
	return out
}

// expandedKeys orders the arguments of an expanded card: the ones the header
// summarised first (the path of a write, the command of a bash), then the rest. The
// identifier is what makes a long payload readable, and with the row cap applied at
// the end it must also never be the part that falls off a huge `content`.
func (c toolCard) expandedKeys() []string {
	hidden := c.headerOnlyArgs()
	lead := make([]string, 0, len(hidden))
	rest := make([]string, 0, len(c.input))
	for _, k := range sortedKeys(c.input) {
		if hidden[k] {
			lead = append(lead, k)
			continue
		}
		rest = append(rest, k)
	}
	return append(lead, rest...)
}

// argValueRows formats one argument value as wrapped rows, untruncated. A string is
// shown verbatim so multi-line content keeps its shape; anything else (number, bool,
// list, object) is rendered as JSON, which is exact and still reads as the value the
// model sent — fmt's %v would print a map as `map[a:1]` and lose the braces. width is
// the room left after the key, so a long single-line value wraps instead of running
// under the card border.
func argValueRows(v any, width int) []string {
	var text string
	if s, ok := v.(string); ok {
		text = s
	} else if raw, err := json.Marshal(v); err == nil {
		text = string(raw)
	} else {
		text = fmt.Sprintf("%v", v)
	}
	if text == "" {
		return []string{`""`}
	}
	return strings.Split(WrapToWidth(strings.TrimRight(text, "\n"), width), "\n")
}

// rawArgLines renders a call whose arguments did not decode to a JSON object. The
// payload is all there is, so an expanded card shows all of it; collapsed it keeps
// the first cardPreviewLines rows with a count of the rest.
func (c toolCard) rawArgLines(inner int) []cardLine {
	out := []cardLine{{text: "args (unparsed)", kind: lineDim}}
	rows := strings.Split(strings.TrimRight(c.rawArgs, "\n"), "\n")
	limit := cardPreviewLines
	if c.expanded {
		limit = cardExpandedLines
		for i, row := range rows {
			rows[i] = WrapToWidth(row, maxInt(inner-2, 20))
		}
		rows = strings.Split(strings.Join(rows, "\n"), "\n")
	}
	clamped, hidden := clampStrings(rows, limit, false)
	for _, row := range clamped {
		out = append(out, cardLine{text: "  " + row, kind: lineBad})
	}
	if hidden > 0 {
		hint := "  … %d more lines · Ctrl+O"
		if c.expanded {
			hint = "  … %d more lines (see /export)"
		}
		out = append(out, cardLine{text: fmt.Sprintf(hint, hidden), kind: lineDim})
	}
	return out
}

// headerOnlyArgs returns the argument keys already represented in the header, so
// the card body does not repeat them.
func (c toolCard) headerOnlyArgs() map[string]bool {
	switch c.toolFamily() {
	case "bash":
		return map[string]bool{"command": true, "description": true}
	case "bash_output", "kill_bash":
		return map[string]bool{"job_id": true, "id": true}
	case "read", "view_image", "write", "edit", "multiedit":
		return map[string]bool{"path": true, "file_path": true}
	case "grep", "find":
		return map[string]bool{"pattern": true, "path": true}
	case "webfetch":
		return map[string]bool{"url": true}
	case "websearch":
		return map[string]bool{"query": true}
	case "task", "task_output":
		return map[string]bool{"description": true, "prompt": true, "job_id": true}
	case "question":
		return map[string]bool{"question": true}
	}
	return nil
}

// mcpArgSummary renders an MCP call's arguments as one compact JSON object. It is
// clipped to the card width by the caller, so the card shows every field of the
// call (a server's schema can be many keys) without spending a line per key, and
// without picking one "most important" argument the way a hand-written summary
// would have to guess.
func (c toolCard) mcpArgSummary() string {
	if len(c.input) == 0 {
		return ""
	}
	raw, err := json.Marshal(c.input)
	if err != nil {
		return fmt.Sprintf("%v", c.input)
	}
	return string(raw)
}

// todoLines renders a todo call as a checklist with per-status styling and a
// progress footer. The raw arguments are an array of objects and the result is the
// same list rendered as text, so neither is readable as-is.
func (c toolCard) todoLines() []cardLine {
	if c.toolFamily() != "todo" {
		return nil
	}
	items := parseTodoItems(c.input)
	if len(items) == 0 {
		return nil
	}
	limit := cardPreviewLines
	if c.expanded {
		limit = cardExpandedLines
	}
	out := make([]cardLine, 0, len(items)+2)
	for i, item := range items {
		if i >= limit {
			out = append(out, cardLine{text: fmt.Sprintf("… %d more items · Ctrl+O", len(items)-i), kind: lineDim})
			break
		}
		mark, kind := "[ ]", lineBody
		switch item.Status {
		case agenttool.TodoCompleted:
			mark, kind = "[x]", lineDim
		case agenttool.TodoInProgress:
			mark, kind = "[~]", lineAccent
		}
		out = append(out, cardLine{text: mark + " " + item.Content, kind: kind})
	}
	out = append(out, cardLine{text: fmt.Sprintf("%d/%d completed", countTodoDone(items), len(items)), kind: lineDim})
	return out
}

// responseLines renders the tool's response, capped to the tool's preview budget.
// Command output keeps its head and tail (the interesting parts of a long log);
// file and MCP content keep the head.
func (c toolCard) responseLines() []cardLine {
	if c.toolFamily() == "todo" {
		// The checklist above is the result; the textual echo adds nothing.
		return nil
	}
	if len(c.response) == 0 {
		return nil
	}
	raw := make([]string, 0, len(c.response))
	for _, n := range c.response {
		raw = append(raw, strings.Repeat("  ", n.depth)+n.text)
	}
	limit := c.previewBudget()
	keepTail := c.toolFamily() == "bash"
	if c.expanded {
		limit = cardExpandedLines
	}
	clamped, hidden := clampStrings(raw, limit, keepTail)
	// A failed tool's message is red, so the reason stands out. Command output is not:
	// a long failing build log would turn the whole card red and be harder to read, and
	// the "!" status glyph already says the command failed.
	kind := lineBody
	if c.state == cardWarn && c.toolFamily() != "bash" && c.toolFamily() != "bash_output" {
		kind = lineBad
	}
	out := make([]cardLine, 0, len(clamped)+1)
	for _, ln := range clamped {
		out = append(out, cardLine{text: ln, kind: kind})
	}
	if hidden > 0 {
		out = append(out, cardLine{text: c.hiddenHint(hidden), kind: lineDim})
	}
	return out
}

// hiddenHint renders the "N lines hidden" note; expanded cards point at the export
// path instead of the (already used) expand key.
func (c toolCard) hiddenHint(hidden int) string {
	if c.expanded {
		return fmt.Sprintf("… %d lines omitted (use /export for the full transcript)", hidden)
	}
	return fmt.Sprintf("… %d more lines · Ctrl+O", hidden)
}

// previewBudget is how many response lines the card shows while collapsed, per
// tool: command output and sub-agent reports get more room than a file or MCP
// result, because that is where the answer usually is.
func (c toolCard) previewBudget() int {
	switch c.toolFamily() {
	case "bash", "bash_output":
		return cardBashLines
	case "task", "task_output":
		return cardTaskLines
	case "edit", "write":
		return 2 // the summary line; the change itself is the Diff section
	}
	return cardPreviewLines
}

// diffLines renders the reported unified diff, capped while collapsed.
func (c toolCard) diffLines() []cardLine {
	if c.diff == "" {
		return nil
	}
	out := []cardLine{{text: "diff", kind: lineDim}}
	dl := strings.Split(strings.TrimRight(c.diff, "\n"), "\n")
	limit := collapsedDiffLines
	if c.expanded {
		limit = cardExpandedLines
	}
	hidden := 0
	if len(dl) > limit {
		hidden = len(dl) - limit
		dl = dl[:limit]
	}
	for _, ln := range dl {
		out = append(out, cardLine{text: "  " + ln, kind: diffLineKind(ln)})
	}
	if hidden > 0 {
		out = append(out, cardLine{text: c.hiddenHint(hidden), kind: lineDim})
	}
	return out
}

// diffLineKind picks the diff palette kind for one unified-diff line, mirroring
// ui.RenderDiffLine's compact-REPL coloring.
func diffLineKind(line string) cardLineKind {
	switch {
	case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
		return lineDiffCtx
	case strings.HasPrefix(line, "@@"):
		return lineDiffHunk
	case strings.HasPrefix(line, "-"):
		return lineDiffDel
	case strings.HasPrefix(line, "+"):
		return lineDiffAdd
	default:
		return lineDiffCtx
	}
}

// clampStrings keeps at most limit lines and reports how many were dropped. With
// keepTail it keeps the head and the tail, eliding the middle with a marker line —
// which is what makes a long command log readable: the start and the failure at the
// end both stay visible.
func clampStrings(lines []string, limit int, keepTail bool) ([]string, int) {
	if limit <= 0 || len(lines) <= limit {
		return lines, 0
	}
	if !keepTail || limit < 3 {
		return lines[:limit], len(lines) - limit
	}
	available := limit - 1 // one line for the elision marker
	head := available / 2
	tail := available - head
	kept := make([]string, 0, limit)
	kept = append(kept, lines[:head]...)
	kept = append(kept, "…")
	kept = append(kept, lines[len(lines)-tail:]...)
	return kept, len(lines) - head - tail
}

// parseTodoItems decodes a todo call's "todos" argument, tolerating the shapes a
// model emits (a decoded array, or the same array re-encoded).
func parseTodoItems(input map[string]any) []agenttool.TodoItem {
	raw, ok := input["todos"]
	if !ok {
		return nil
	}
	if items, ok := raw.([]agenttool.TodoItem); ok {
		return items
	}
	var items []agenttool.TodoItem
	if list, ok := raw.([]any); ok {
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				content, _ := m["content"].(string)
				status, _ := m["status"].(string)
				items = append(items, agenttool.TodoItem{Content: content, Status: agenttool.TodoStatus(status)})
			}
		}
		if len(items) > 0 {
			return items
		}
	}
	// Anything else (e.g. a JSON-encoded string) is re-decoded through the wire shape.
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var decoded []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil
	}
	for _, d := range decoded {
		items = append(items, agenttool.TodoItem{Content: d.Content, Status: agenttool.TodoStatus(d.Status)})
	}
	return items
}

// countTodoDone counts completed items.
func countTodoDone(items []agenttool.TodoItem) int {
	done := 0
	for _, item := range items {
		if item.Status == agenttool.TodoCompleted {
			done++
		}
	}
	return done
}

// argText returns the first non-empty string form of the named arguments, so a tool
// that accepts both "path" and "file_path" (or "job_id" and "id") shows one.
func argText(input map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := input[k]; ok {
			if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

// firstLine returns the first line of s, so a multi-line command or prompt becomes
// a single-line header summary.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// maxInt returns the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// clipLine collapses s onto one line and truncates it to limit runes, backing up to a
// word boundary when there is one near the cut so a header or argument summary does
// not end mid-word.
//
// The search for that boundary walks the rune slice. Using a string offset here
// would mix units: a byte index into a CJK (or emoji) string is several times the
// rune count, so slicing the rune slice at it runs past the end and panics.
func clipLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if limit <= 0 || len(runes) <= limit {
		return s
	}
	cut := limit
	for i := cut - 1; i >= cut/2; i-- {
		if unicode.IsSpace(runes[i]) {
			cut = i
			break
		}
	}
	return string(runes[:cut]) + "…"
}

// primaryArg returns the header summary without the display name, kept for callers
// that need the raw value.
func (c toolCard) primaryArg() string {
	return c.summaryArg()
}

// sortedKeys returns the map keys in a stable (sorted) order so the input
// section renders deterministically instead of in Go's random map order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stripDiffTail removes the trailing unified diff from a tool result text so
// the card's Response section keeps only the summary line; the diff itself
// renders in the colored Diff section. It cuts at the diff's "--- a/" header
// rather than matching the exact suffix, so a result clipped mid-diff still
// splits cleanly. The cut is only applied when the tool reported a diff
// (card.diff != ""), so e.g. bash output of git diff is never mangled.
func stripDiffTail(text string) string {
	if i := strings.Index(text, "\n--- a/"); i >= 0 {
		return text[:i+1]
	}
	return text
}

// parseToolResult splits a tool's textual result into response tree nodes,
// inferring depth from leading whitespace (every two leading spaces is one
// level). Trailing empty lines are trimmed so the card does not render blank
// tail rows.
func parseToolResult(result string) []respNode {
	lines := strings.Split(result, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	nodes := make([]respNode, 0, len(lines))
	for _, ln := range lines {
		leading := len(ln) - len(strings.TrimLeft(ln, " "))
		nodes = append(nodes, respNode{text: ln[leading:], depth: leading / 2})
	}
	return nodes
}
