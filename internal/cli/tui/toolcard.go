package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/smallnest/pigo/internal/cli/ui"
)

// This file implements the rich tool-call card component (US-006, SPEC 3.2,
// FR-6/7/8). A toolCard is a bordered inline block in the transcript that shows
// a single tool invocation: a header with the tool name and a status icon
// (running / success / warn), the decoded call arguments, the tool's response
// rendered as an indented tree, and - for tools that report a diff (edit,
// #560) - a colored Diff section. Cards are created on toolStartMsg,
// completed on toolEndMsg, and toggled between a capped and a full response view
// with Ctrl+O (see model.go). All width math goes through ui.Width /
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

// toolCard is a single tool invocation rendered as a bordered card. input holds
// the decoded call arguments (nil when the args were not a JSON object);
// response is the parsed result tree, populated on completion; diff holds a
// unified diff the tool reported in its result metadata (edit today), rendered
// as its own colored section instead of plain response text. expanded flips
// the response between a capped preview and the full tree.
type toolCard struct {
	id       string
	name     string
	input    map[string]any
	response []respNode
	diff     string
	state    cardState
	expanded bool

	// rawArgs holds the argument payload verbatim when it did not decode to a
	// JSON object — a model can emit malformed tool-call JSON (a truncated or
	// duplicated key). The call then fails schema validation with an error result
	// the model can retry from, and the card shows what was actually sent instead
	// of an empty arguments section.
	rawArgs string

	// rev counts the mutations that change the card's rendering (result
	// arrival, expand toggle). The transcript caches each block's rendered text
	// and re-renders only when the card's rev differs from the one the cache was
	// built from, so a streamed delta or keystroke elsewhere does not re-draw
	// every finished card.
	rev int
}

// cardRawArgsLimit caps how much unparsed argument text a card shows, so a
// pathological blob cannot flood the transcript.
const cardRawArgsLimit = 400

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

// toggleExpanded flips the response between the capped preview and the full
// tree, bumping the render revision so the transcript re-renders the card.
func (c *toolCard) toggleExpanded() {
	c.expanded = !c.expanded
	c.rev++
}

// collapsedResponseLines is how many response lines a card shows before it is
// expanded; past this the preview is truncated and a Ctrl+O hint is appended.
const collapsedResponseLines = 5

// collapsedDiffLines is the same cap for the Diff section. It is larger than
// the response cap because a hunk spends lines on file headers, the @@ marker
// and context around the actual change.
const collapsedDiffLines = 12

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

// render draws the card at the given content width: a rounded border wrapping a
// header (status icon + tool name), an "Input arguments" section listing the input map,
// a "Response" section with the tree lines, and - when the tool reported a diff
// (edit) - a colored "Diff" section. When not expanded the response is capped
// to collapsedResponseLines (the diff to collapsedDiffLines) with a
// "(Ctrl+O for more)" hint; when expanded every line is shown.
func (c toolCard) render(theme Theme, width int) string {
	if width < 4 {
		width = 4
	}
	// The rounded border consumes one column on each side; wrap everything to the
	// inner width so nothing overflows the frame.
	inner := width - 2

	var lines []string

	icon := c.styledIcon(theme)
	nameBudget := inner - ui.Width(icon) - 1
	if nameBudget < 1 {
		nameBudget = 1
	}
	header := c.name
	if arg := c.primaryArg(); arg != "" {
		header = c.name + "(" + arg + ")"
	}
	header = TruncateToWidth(header, nameBudget)
	lines = append(lines, icon+" "+theme.ToolHeader.Render(header))

	if len(c.input) > 0 {
		lines = append(lines, theme.ToolBody.Render("Input arguments"))
		for _, k := range sortedKeys(c.input) {
			kv := "  " + k + ": " + fmt.Sprintf("%v", c.input[k])
			lines = append(lines, theme.ToolBody.Render(WrapToWidth(kv, inner)))
		}
	} else if c.rawArgs != "" {
		// The arguments did not decode to an object: show the text the model
		// actually produced, labeled, so a malformed call is visible rather than
		// looking like a call with no arguments.
		lines = append(lines, theme.ToolBody.Render("Input arguments (unparsed)"))
		lines = append(lines, theme.ToolBody.Render(WrapToWidth("  "+c.rawArgs, inner)))
	}

	if len(c.response) > 0 {
		lines = append(lines, theme.ToolBody.Render("Response"))
		resp := c.response
		truncated := false
		if !c.expanded && len(resp) > collapsedResponseLines {
			resp = resp[:collapsedResponseLines]
			truncated = true
		}
		for _, n := range resp {
			indent := strings.Repeat("  ", n.depth)
			lines = append(lines, theme.ToolBody.Render(WrapToWidth(indent+n.text, inner)))
		}
		if truncated {
			lines = append(lines, theme.System.Render("(Ctrl+O for more)"))
		}
	}

	if c.diff != "" {
		lines = append(lines, theme.ToolBody.Render("Diff"))
		dl := strings.Split(strings.TrimRight(c.diff, "\n"), "\n")
		truncated := false
		if !c.expanded && len(dl) > collapsedDiffLines {
			dl = dl[:collapsedDiffLines]
			truncated = true
		}
		for _, ln := range dl {
			lines = append(lines, diffLineStyle(theme, ln).Render(WrapToWidth("  "+ln, inner)))
		}
		if truncated {
			lines = append(lines, theme.System.Render("(Ctrl+O for more)"))
		}
	}

	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colorGray)).
		Width(inner)
	return border.Render(strings.Join(lines, "\n"))
}

// primaryArg returns the most salient call argument to inline in the card header
// so the user can see what the tool is operating on at a glance (FR-6), e.g.
// Bash(cd /x && git add -A). It picks the command for bash and the file path for
// the file tools, otherwise the first argument in sorted-key order. Returns ""
// when the call carried no arguments.
func (c toolCard) primaryArg() string {
	if len(c.input) == 0 {
		return ""
	}
	var keyPrefs []string
	switch strings.ToLower(c.name) {
	case "bash":
		keyPrefs = []string{"command"}
	case "read", "write", "edit", "multiedit":
		// The file tools emit "path"; accept "file_path" as a fallback for
		// callers that use the Claude-style key.
		keyPrefs = []string{"path", "file_path"}
	}
	for _, key := range keyPrefs {
		if v, ok := c.input[key]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	keys := sortedKeys(c.input)
	return fmt.Sprintf("%v", c.input[keys[0]])
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

// diffLineStyle picks the theme style for one unified-diff line, mirroring
// ui.RenderDiffLine's compact-REPL coloring: dim file headers and context,
// cyan @@ hunk markers, red removals, green additions.
func diffLineStyle(theme Theme, line string) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
		return theme.DiffCtx
	case strings.HasPrefix(line, "@@"):
		return theme.DiffHunk
	case strings.HasPrefix(line, "-"):
		return theme.DiffDel
	case strings.HasPrefix(line, "+"):
		return theme.DiffAdd
	default:
		return theme.DiffCtx
	}
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
