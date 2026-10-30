// Tests for the per-tool card rendering: what each kind of tool shows, the line
// budgets that keep a huge result out of the transcript, and the checklist an MCP
// or todo call renders instead of a raw argument dump.
package tui

import (
	"fmt"
	"strings"
	"testing"
)

// cardRender builds a card and renders it at a fixed width.
func cardRender(card *toolCard) string {
	return stripANSI(card.render(DefaultTheme(), 70))
}

// TestToolCardBashKeepsHeadAndTail verifies command output shows its beginning and
// its end: a long log's failure is at the bottom, its setup at the top, and the
// middle is what gets elided.
func TestToolCardBashKeepsHeadAndTail(t *testing.T) {
	card := newToolCard("b1", "bash", map[string]any{"command": "make test"})
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("out-%02d", i))
	}
	card.complete(cardSuccess, parseToolResult(strings.Join(lines, "\n")), "")

	out := cardRender(card)
	if !strings.Contains(out, "bash(make test)") {
		t.Errorf("header should show the command:\n%s", out)
	}
	if !strings.Contains(out, "out-01") || !strings.Contains(out, "out-30") {
		t.Errorf("preview should keep the head and the tail:\n%s", out)
	}
	if strings.Contains(out, "out-15") {
		t.Errorf("preview should elide the middle:\n%s", out)
	}
	if !strings.Contains(out, "more lines") {
		t.Errorf("preview should report the hidden lines:\n%s", out)
	}
	// The command is in the header, so it is not repeated as an argument.
	if strings.Contains(out, "command:") {
		t.Errorf("the command should not be repeated in the body:\n%s", out)
	}
}

// TestToolCardMCPIsCompact verifies an MCP call shows a short server/tool header, a
// one-line argument summary and a capped result, instead of the full JSON dump and
// full response the card used to print.
func TestToolCardMCPIsCompact(t *testing.T) {
	input := map[string]any{
		"owner": "smallnest", "repo": "pigo", "title": "a very long issue title",
		"body": strings.Repeat("detail ", 40),
	}
	card := newToolCard("m1", "mcp__github__create_issue", input)
	var result strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&result, "result line %d\n", i)
	}
	card.complete(cardSuccess, parseToolResult(result.String()), "")

	out := cardRender(card)
	if !strings.Contains(out, "mcp github/create_issue") {
		t.Errorf("header should name the server and tool:\n%s", out)
	}
	if !strings.Contains(out, "args ") {
		t.Errorf("MCP arguments should be summarized on one line:\n%s", out)
	}
	// One compact args line, not one line per key.
	if got := strings.Count(out, "owner:"); got > 1 {
		t.Errorf("arguments should not be dumped per key (found %d):\n%s", got, out)
	}
	// The whole argument list is one clipped row, however many fields the schema has.
	argRows := 0
	for _, row := range strings.Split(out, "\n") {
		if strings.Contains(row, "detail") {
			argRows++
		}
	}
	if argRows != 1 {
		t.Errorf("MCP arguments should render as a single row, got %d:\n%s", argRows, out)
	}
	if strings.Contains(out, "result line 39") && strings.Contains(out, "result line 30") {
		t.Errorf("the MCP result should be capped:\n%s", out)
	}
	if !strings.Contains(out, "result line 0") {
		t.Errorf("the MCP result preview should show its first line:\n%s", out)
	}
}

// TestToolCardTodoChecklist verifies a todo call renders as a status checklist with a
// progress footer, not as an array of objects.
func TestToolCardTodoChecklist(t *testing.T) {
	card := newToolCard("t1", "todo", map[string]any{"todos": []any{
		map[string]any{"content": "write the parser", "status": "completed"},
		map[string]any{"content": "add tests", "status": "in_progress"},
		map[string]any{"content": "update docs", "status": "pending"},
	}})
	card.complete(cardSuccess, parseToolResult("Todos:\n  [x] write the parser\n(1/3 completed)"), "")

	out := cardRender(card)
	if !strings.Contains(out, "todo(1/3 done)") {
		t.Errorf("header should summarize progress:\n%s", out)
	}
	for _, want := range []string{"[x] write the parser", "[~] add tests", "[ ] update docs", "1/3 completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q:\n%s", want, out)
		}
	}
	// The raw array and the tool's textual echo are not repeated.
	if strings.Contains(out, "todos:") || strings.Contains(out, "Todos:") {
		t.Errorf("the checklist should replace the raw arguments and echo:\n%s", out)
	}
}

// TestToolCardReadHidesPathArg verifies a file tool's path lives in the header and
// its content preview is capped.
func TestToolCardReadHidesPathArg(t *testing.T) {
	card := newToolCard("r1", "read", map[string]any{"path": "internal/cli/tui/model.go", "limit": 200})
	card.complete(cardSuccess, parseToolResult("package tui\n\nimport (\n\t\"strings\"\n)\n\nfunc x() {}\nfunc y() {}\nfunc z() {}"), "")

	out := cardRender(card)
	if !strings.Contains(out, "read(internal/cli/tui/model.go)") {
		t.Errorf("header should show the path:\n%s", out)
	}
	if strings.Contains(out, "path:") {
		t.Errorf("the path should not be repeated as an argument:\n%s", out)
	}
	if !strings.Contains(out, "limit: 200") {
		t.Errorf("other arguments should still be listed:\n%s", out)
	}
	if strings.Contains(out, "func z()") {
		t.Errorf("file content should be capped in the preview:\n%s", out)
	}
}

// TestToolCardExpandRevealsEverything verifies expansion lifts the per-tool budget.
func TestToolCardExpandRevealsEverything(t *testing.T) {
	card := newToolCard("r2", "read", map[string]any{"path": "big.txt"})
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "content-%02d\n", i)
	}
	card.complete(cardSuccess, parseToolResult(b.String()), "")

	if collapsed := cardRender(card); strings.Contains(collapsed, "content-19") {
		t.Errorf("collapsed preview should be capped:\n%s", collapsed)
	}
	card.expanded = true
	if expanded := cardRender(card); !strings.Contains(expanded, "content-19") {
		t.Errorf("expanded card should show every line:\n%s", expanded)
	}
}

// TestClampStrings verifies the two caps: a head cap drops the tail, a head+tail cap
// keeps both ends and counts what is gone.
func TestClampStrings(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	head, hidden := clampStrings(lines, 4, false)
	if len(head) != 4 || head[3] != "d" || hidden != 6 {
		t.Errorf("head cap = %v (hidden %d), want first 4 and 6 hidden", head, hidden)
	}

	mixed, hidden := clampStrings(lines, 5, true)
	if len(mixed) != 5 || mixed[0] != "a" || mixed[2] != "…" || mixed[4] != "j" {
		t.Errorf("head+tail cap = %v, want a,b,…,i,j", mixed)
	}
	if hidden != 6 {
		t.Errorf("hidden = %d, want 6", hidden)
	}

	if all, hidden := clampStrings(lines[:3], 5, true); len(all) != 3 || hidden != 0 {
		t.Errorf("content under the cap should pass through unchanged: %v (%d hidden)", all, hidden)
	}
}
