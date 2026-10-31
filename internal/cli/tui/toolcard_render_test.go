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

// cardBody returns a rendered card's rows with the rounded border stripped, so a
// test can compare whole rows instead of matching across the frame characters.
func cardBody(out string) []string {
	lines := strings.Split(out, "\n")
	body := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimPrefix(ln, "│")
		ln = strings.TrimSuffix(ln, "│")
		body = append(body, strings.TrimRight(ln, " "))
	}
	return body
}

// hasRow reports whether the rows hold want verbatim.
func hasRow(rows []string, want string) bool {
	for _, row := range rows {
		if row == want {
			return true
		}
	}
	return false
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

// TestToolCardExpandedShowsFullArguments verifies an expanded card shows the call
// itself in full: the header's argument (which the header clips to one line) is
// repeated as written, a multi-line script keeps its line breaks, and a value longer
// than the collapsed column budget is no longer cut.
func TestToolCardExpandedShowsFullArguments(t *testing.T) {
	long := strings.Repeat("why this runs ", 20)
	card := newToolCard("b2", "bash", map[string]any{
		"command":     "set -e\ncd /tmp\necho one\necho two",
		"description": long,
		"timeout_ms":  300000,
	})

	collapsed := cardRender(card)
	if strings.Contains(collapsed, "command:") {
		t.Errorf("the collapsed body should leave the header's argument out:\n%s", collapsed)
	}
	if strings.Contains(collapsed, long) {
		t.Errorf("the collapsed body should clip a long value:\n%s", collapsed)
	}

	card.expanded = true
	expanded := cardRender(card)
	for _, want := range []string{"command:", "set -e", "cd /tmp", "echo one", "echo two", "description:", long[:40]} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded card missing %q:\n%s", want, expanded)
		}
	}
	// Every row of the script gets its own row rather than being flattened into the
	// header's one-liner.
	rows := cardBody(expanded)
	for _, want := range []string{"    set -e", "    cd /tmp", "    echo one", "    echo two"} {
		if !hasRow(rows, want) {
			t.Errorf("expanded arguments missing the row %q:\n%s", want, expanded)
		}
	}
}

// TestToolCardExpandedKeepsIdentifierFirst verifies the argument the header
// summarised (here the write path) leads the expanded list, so a body that hits the
// row ceiling cannot push the identifier out of the card.
func TestToolCardExpandedKeepsIdentifierFirst(t *testing.T) {
	var body strings.Builder
	for i := 0; i < cardExpandedLines+50; i++ {
		fmt.Fprintf(&body, "line-%03d\n", i)
	}
	card := newToolCard("w3", "write", map[string]any{"path": "big.txt", "content": body.String()})
	card.expanded = true

	rows := cardBody(cardRender(card))
	pathAt, contentAt := -1, -1
	for i, row := range rows {
		if row == "  path: big.txt" && pathAt < 0 {
			pathAt = i
		}
		if row == "  content:" && contentAt < 0 {
			contentAt = i
		}
	}
	if pathAt < 0 {
		t.Fatalf("expanded card lost the path row:\n%s", strings.Join(rows, "\n"))
	}
	if contentAt < 0 || pathAt > contentAt {
		t.Errorf("path should lead the payload (path row %d, content row %d):\n%s", pathAt, contentAt, strings.Join(rows, "\n"))
	}
}

// TestToolCardExpandedMCPListsEveryArgument verifies an expanded MCP card breaks the
// one-line argument summary back out: one row per key, values exact.
func TestToolCardExpandedMCPListsEveryArgument(t *testing.T) {
	input := map[string]any{"owner": "smallnest", "repo": "pigo", "labels": []any{"bug", "tui"}}
	card := newToolCard("m2", "mcp__github__list_issues", input)

	collapsed := cardRender(card)
	if !strings.Contains(collapsed, "args {") {
		t.Errorf("a collapsed MCP card should keep the compact one-line summary:\n%s", collapsed)
	}
	card.expanded = true
	expanded := cardRender(card)
	for _, want := range []string{"owner: smallnest", "repo: pigo", `labels: ["bug","tui"]`} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded MCP card missing %q:\n%s", want, expanded)
		}
	}
}

// TestToolCardExpandedArgumentsStayBounded verifies a giant argument value is still
// capped at cardExpandedLines and says where the rest is, so one write of a huge file
// cannot make the frame unbounded.
func TestToolCardExpandedArgumentsStayBounded(t *testing.T) {
	var body strings.Builder
	for i := 0; i < cardExpandedLines+50; i++ {
		fmt.Fprintf(&body, "line-%03d\n", i)
	}
	card := newToolCard("w1", "write", map[string]any{"path": "big.txt", "content": body.String()})
	card.expanded = true

	out := cardRender(card)
	if !strings.Contains(out, "line-000") {
		t.Errorf("expanded arguments should start at the beginning of the value:\n%s", out)
	}
	if strings.Contains(out, fmt.Sprintf("line-%03d", cardExpandedLines+49)) {
		t.Errorf("expanded arguments should stop at the ceiling:\n%s", out)
	}
	if !strings.Contains(out, "more argument lines (see /export)") {
		t.Errorf("a bounded argument section should say where the rest is:\n%s", out)
	}
}

// TestToolCardExpandedShowsColumnsOfWideValue verifies a value that the collapsed
// card clipped at cardArgValueLimit is shown past that limit once expanded.
func TestToolCardExpandedShowsColumnsOfWideValue(t *testing.T) {
	tail := "TAIL-MARKER-XYZ"
	value := strings.Repeat("abc ", cardArgValueLimit) + tail
	card := newToolCard("w2", "write", map[string]any{"path": "x.txt", "content": value})

	if collapsed := cardRender(card); strings.Contains(collapsed, tail) {
		t.Errorf("collapsed value should be clipped at %d columns:\n%s", cardArgValueLimit, collapsed)
	}
	card.expanded = true
	if expanded := cardRender(card); !strings.Contains(expanded, tail) {
		t.Errorf("expanded value should run to its end:\n%s", expanded)
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

// TestClipLineMultibyte verifies clipping is measured in runes, not bytes. The
// panic this replaced was a byte offset from strings.LastIndex used as a rune
// index: on CJK text the space sits far past the rune budget, so slicing the rune
// slice at it ran out of range and took the whole TUI down while rendering a card.
func TestClipLineMultibyte(t *testing.T) {
	// 70 CJK runes, a space, then 58 more: the space is rune 70 but byte 210, and
	// the budget is 128 runes — the exact shape that panicked.
	long := strings.Repeat("中", 70) + " " + strings.Repeat("文", 58)
	got := clipLine(long, 128)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clipLine(%d runes) = %q, want a clipped value", len([]rune(long)), got)
	}
	if runes := []rune(got); len(runes) > 129 {
		t.Errorf("clipped value is %d runes, want the 128-rune budget plus the ellipsis", len(runes))
	}
	if !strings.HasPrefix(got, strings.Repeat("中", 70)+"…") {
		t.Errorf("clipLine should back up to the word boundary, got %q", got)
	}

	// No boundary anywhere: it must still truncate instead of panicking.
	dense := strings.Repeat("中文无空格", 40)
	got = clipLine(dense, 40)
	if runes := []rune(got); len(runes) != 41 || runes[40] != '…' {
		t.Errorf("dense CJK clip = %d runes, want a 40-rune cut plus the ellipsis", len(runes))
	}

	// A short value passes through untouched.
	if got := clipLine("短文本", 40); got != "短文本" {
		t.Errorf("short value = %q, want it unchanged", got)
	}
}

// TestToolCardRendersMultibyteArgs verifies rendering a card whose arguments are
// long CJK text (an MCP call in a Chinese conversation) does not panic end to end,
// including through the transcript's reflow.
func TestToolCardRendersMultibyteArgs(t *testing.T) {
	card := newToolCard("c1", "mcp__notes__write", map[string]any{
		"text": strings.Repeat("会议纪要：这是一段很长的中文内容", 8),
	})
	card.complete(cardSuccess, parseToolResult(strings.Repeat("结果行\n", 30)), "")
	if out := cardRender(card); !strings.Contains(out, "mcp notes/write") {
		t.Errorf("card header missing:\n%s", out)
	}

	tr := newTranscript(DefaultTheme())
	tr.setSize(70, 30)
	tr.addToolCard(card)
	if got := stripANSI(tr.vp.GetContent()); !strings.Contains(got, "会议纪要") {
		t.Errorf("card content missing after reflow:\n%s", got)
	}
}
