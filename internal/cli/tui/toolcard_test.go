package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// ctrlKey builds a Ctrl+<letter> key press matching String()=="ctrl+<letter>".
func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

// TestParseToolResult verifies depth inference from leading spaces and trailing
// blank-line trimming.
func TestParseToolResult(t *testing.T) {
	nodes := parseToolResult("root\n  child\n    grandchild\n\n")
	if len(nodes) != 3 {
		t.Fatalf("node count = %d, want 3 (trailing blank trimmed)", len(nodes))
	}
	want := []respNode{
		{text: "root", depth: 0},
		{text: "child", depth: 1},
		{text: "grandchild", depth: 2},
	}
	for i, w := range want {
		if nodes[i] != w {
			t.Errorf("node[%d] = %+v, want %+v", i, nodes[i], w)
		}
	}
}

// TestToolCardRender checks the header (name + status icon), the input section,
// and the response tree lines appear in the rendered card, and that the status
// icon reflects the state.
func TestToolCardRender(t *testing.T) {
	theme := DefaultTheme()
	cases := []struct {
		name  string
		state cardState
		icon  string
	}{
		{"running", cardRunning, "…"},
		{"success", cardSuccess, "✓"},
		{"warn", cardWarn, "!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := toolCard{
				id:       "1",
				name:     "read_file",
				input:    map[string]any{"path": "/tmp/x"},
				response: parseToolResult("line one\n  nested"),
				state:    tc.state,
			}
			out := card.render(theme, 60)
			for _, want := range []string{"read_file", tc.icon, "path: /tmp/x", "line one", "nested"} {
				if !strings.Contains(out, want) {
					t.Errorf("render missing %q\n%s", want, out)
				}
			}
		})
	}
}

// TestToolCardExpandTruncation verifies the collapsed card caps the response and
// shows the Ctrl+O hint, while the expanded card reveals every line.
func TestToolCardExpandTruncation(t *testing.T) {
	theme := DefaultTheme()
	var b strings.Builder
	for i := 0; i < cardPreviewLines+3; i++ {
		b.WriteString("resp-line-")
		b.WriteByte(byte('a' + i))
		b.WriteByte('\n')
	}
	card := toolCard{name: "grep", response: parseToolResult(b.String()), state: cardSuccess}

	collapsed := card.render(theme, 60)
	if !strings.Contains(collapsed, "Ctrl+O") {
		t.Errorf("collapsed card should show Ctrl+O hint\n%s", collapsed)
	}
	lastLine := "resp-line-" + string(byte('a'+cardPreviewLines+2))
	if strings.Contains(collapsed, lastLine) {
		t.Errorf("collapsed card should not show %q\n%s", lastLine, collapsed)
	}

	card.expanded = true
	expanded := card.render(theme, 60)
	if strings.Contains(expanded, "more lines") {
		t.Errorf("expanded card should not show a truncation hint\n%s", expanded)
	}
	if !strings.Contains(expanded, lastLine) {
		t.Errorf("expanded card should show %q\n%s", lastLine, expanded)
	}
}

// TestModelToolCardFlow drives the model through a tool start/end and asserts the
// card is created, transitions running→success, and that a failed tool yields
// warn.
func TestModelToolCardFlow(t *testing.T) {
	m := NewModel(Options{})
	next, _ := m.Update(toolStartMsg{id: "t1", name: "read_file", input: map[string]any{"path": "a.go"}})
	mm := next.(Model)
	card, ok := mm.toolCards["t1"]
	if !ok {
		t.Fatalf("toolStartMsg should create a card")
	}
	if card.state != cardRunning {
		t.Errorf("new card state = %v, want cardRunning", card.state)
	}

	next, _ = mm.Update(toolEndMsg{id: "t1", ok: true, result: "done\n  detail"})
	mm = next.(Model)
	if mm.toolCards["t1"].state != cardSuccess {
		t.Errorf("state after ok end = %v, want cardSuccess", mm.toolCards["t1"].state)
	}
	if len(mm.toolCards["t1"].response) != 2 {
		t.Errorf("response nodes = %d, want 2", len(mm.toolCards["t1"].response))
	}

	// A failed tool flips the same card to warn.
	next, _ = m.Update(toolStartMsg{id: "t2", name: "bash"})
	mm = next.(Model)
	next, _ = mm.Update(toolEndMsg{id: "t2", ok: false, result: "boom"})
	mm = next.(Model)
	if mm.toolCards["t2"].state != cardWarn {
		t.Errorf("state after failed end = %v, want cardWarn", mm.toolCards["t2"].state)
	}
}

// TestToolCardDiffSection verifies a card carrying a diff renders a dedicated
// Diff section whose lines carry the per-line theme colors: red removals,
// green additions, cyan @@ markers, dim headers and context (#560).
func TestToolCardDiffSection(t *testing.T) {
	theme := DefaultTheme()
	diff := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n alpha\n-beta\n+BETA\n gamma\n"
	card := toolCard{
		name:     "edit",
		input:    map[string]any{"path": "f.txt"},
		response: parseToolResult("Edited f.txt (1 replacement(s))"),
		diff:     diff,
		state:    cardSuccess,
	}
	out := card.render(theme, 60)

	for _, want := range []string{
		"edit(f.txt)",
		"Edited f.txt (1 replacement(s))",
		"diff",
		"--- a/f.txt",
		"@@ -1,3 +1,3 @@",
		"-beta",
		"+BETA",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
	// The diff lines are styled, not plain body text.
	for _, styled := range []string{
		theme.DiffDel.Render("  -beta"),
		theme.DiffAdd.Render("  +BETA"),
		theme.DiffHunk.Render("  @@ -1,3 +1,3 @@"),
		theme.DiffCtx.Render("  --- a/f.txt"),
		theme.DiffCtx.Render("   alpha"),
	} {
		if !strings.Contains(out, styled) {
			t.Errorf("render missing styled diff line %q\n%s", styled, out)
		}
	}
}

// TestToolCardDiffCollapseExpand verifies the Diff section obeys the same
// collapse/expand behavior as the response: capped with a Ctrl+O hint when
// collapsed, fully shown once expanded.
func TestToolCardDiffCollapseExpand(t *testing.T) {
	theme := DefaultTheme()
	var b strings.Builder
	b.WriteString("--- a/f.txt\n+++ b/f.txt\n")
	for i := 0; i < collapsedDiffLines+3; i++ {
		b.WriteString("+line\n")
	}
	card := toolCard{
		name:     "edit",
		response: parseToolResult("Edited f.txt (1 replacement(s))"),
		diff:     b.String(),
		state:    cardSuccess,
	}

	collapsed := card.render(theme, 60)
	if !strings.Contains(collapsed, "Ctrl+O") {
		t.Errorf("collapsed card should show Ctrl+O hint\n%s", collapsed)
	}
	// The cap counts all diff lines, so the 2 header lines leave room for
	// collapsedDiffLines-2 additions.
	if got := strings.Count(collapsed, "+line"); got != collapsedDiffLines-2 {
		t.Errorf("collapsed card shows %d additions, want %d\n%s", got, collapsedDiffLines-2, collapsed)
	}

	card.expanded = true
	expanded := card.render(theme, 60)
	if strings.Contains(expanded, "more lines") {
		t.Errorf("expanded card should not show a truncation hint\n%s", expanded)
	}
	if got := strings.Count(expanded, "+line"); got != collapsedDiffLines+3 {
		t.Errorf("expanded card shows %d additions, want %d\n%s", got, collapsedDiffLines+3, expanded)
	}
}

// TestStripDiffTail verifies the response text keeps only its summary once the
// diff moves to its own section: the cut happens at the diff header, and a
// text without a diff passes through untouched.
func TestStripDiffTail(t *testing.T) {
	diff := "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n alpha\n-beta\n+BETA\n"
	text := "Edited f.txt (1 replacement(s))\n" + diff
	if got := stripDiffTail(text); got != "Edited f.txt (1 replacement(s))\n" {
		t.Errorf("stripDiffTail = %q, want the summary line only", got)
	}
	// A result clipped mid-diff still splits at the header.
	if got := stripDiffTail(text[:len(text)-5]); got != "Edited f.txt (1 replacement(s))\n" {
		t.Errorf("stripDiffTail on clipped text = %q, want the summary line only", got)
	}
	// Text without a diff header is left alone.
	plain := "done\nsome output\n"
	if got := stripDiffTail(plain); got != plain {
		t.Errorf("stripDiffTail on plain text = %q, want unchanged", got)
	}
}

// TestModelToolEndDiff drives a tool start/end pair whose end event carries
// edit-style Details, and verifies the model stores the diff on the card and
// keeps only the summary in the response (no duplicated diff).
func TestModelToolEndDiff(t *testing.T) {
	m := NewModel(Options{})
	next, _ := m.Update(toolStartMsg{id: "e1", name: "edit", input: map[string]any{"path": "f.txt"}})
	mm := next.(Model)

	diff := "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n alpha\n-beta\n+BETA\n"
	details := map[string]any{"path": "f.txt", "replacements": 1, "diff": diff}
	next, _ = mm.Update(toolEndMsg{
		id:      "e1",
		ok:      true,
		result:  "Edited f.txt (1 replacement(s))\n" + diff,
		details: details,
	})
	mm = next.(Model)

	card, ok := mm.toolCards["e1"]
	if !ok {
		t.Fatalf("toolEndMsg should keep the card")
	}
	if card.diff != diff {
		t.Errorf("card.diff = %q, want the diff from Details", card.diff)
	}
	if len(card.response) != 1 || card.response[0].text != "Edited f.txt (1 replacement(s))" {
		t.Errorf("card.response = %+v, want only the summary line", card.response)
	}

	// Without Details the card renders as before: full text, no diff section.
	next, _ = m.Update(toolStartMsg{id: "e2", name: "edit"})
	mm = next.(Model)
	next, _ = mm.Update(toolEndMsg{id: "e2", ok: true, result: "Edited g.txt (1 replacement(s))\n" + diff})
	mm = next.(Model)
	card2 := mm.toolCards["e2"]
	if card2.diff != "" {
		t.Errorf("card without Details should carry no diff, got %q", card2.diff)
	}
	if len(card2.response) == 1 && card2.response[0].text == "Edited g.txt (1 replacement(s))" {
		// The response keeps the embedded diff text when there is no Details to
		// split on, so more than the summary should be present.
		t.Errorf("response should keep the embedded diff when Details is absent: %+v", card2.response)
	}
}

// TestModelCtrlOTogglesExpanded verifies Ctrl+O flips the most-recent card's
// expanded flag so more response lines become visible.
func TestModelCtrlOTogglesExpanded(t *testing.T) {
	m := NewModel(Options{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	mm := next.(Model)

	var b strings.Builder
	for i := 0; i < cardPreviewLines+3; i++ {
		b.WriteString("row")
		b.WriteByte(byte('0' + i))
		b.WriteByte('\n')
	}
	next, _ = mm.Update(toolStartMsg{id: "t1", name: "grep"})
	mm = next.(Model)
	next, _ = mm.Update(toolEndMsg{id: "t1", ok: true, result: b.String()})
	mm = next.(Model)

	if mm.lastToolCard.expanded {
		t.Fatalf("card should start collapsed")
	}
	next, _ = mm.Update(ctrlKey('o'))
	mm = next.(Model)
	if !mm.lastToolCard.expanded {
		t.Errorf("Ctrl+O should expand the most-recent card")
	}
	// Toggling again collapses it.
	next, _ = mm.Update(ctrlKey('o'))
	mm = next.(Model)
	if mm.lastToolCard.expanded {
		t.Errorf("second Ctrl+O should collapse the card")
	}
}

// TestToolCardRendersUnparsedArguments verifies a call whose arguments did not
// decode to a JSON object still shows what the model sent, labeled as unparsed,
// instead of rendering a card with no arguments at all. That is what the user
// sees when the model emits malformed tool-call JSON and the executor rejects it.
func TestToolCardRendersUnparsedArguments(t *testing.T) {
	theme := DefaultTheme()
	card := newToolCard("c1", "todo", []byte(`{"todos": []{}}`))
	if card.input != nil {
		t.Fatalf("input = %#v, want nil for arguments that do not parse", card.input)
	}
	if card.rawArgs == "" {
		t.Fatal("rawArgs should keep the malformed payload")
	}
	out := card.render(theme, 60)
	for _, want := range []string{"todo", "args (unparsed)", `{"todos": []{}}`} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

// TestRawToolArgs verifies which payloads are kept for display: a parsed object
// needs no raw fallback, the empty-object forms are not noise, and a long
// malformed payload is clipped.
func TestRawToolArgs(t *testing.T) {
	parsed := map[string]any{"path": "a.go"}
	if got := rawToolArgs(map[string]any{"path": "a.go"}, parsed); got != "" {
		t.Errorf("parsed arguments should not keep raw text, got %q", got)
	}
	for _, empty := range []any{[]byte(`{}`), `null`, json.RawMessage("  "), nil} {
		if got := rawToolArgs(empty, nil); got != "" {
			t.Errorf("rawToolArgs(%v) = %q, want empty", empty, got)
		}
	}
	if got := rawToolArgs(`{"path":`, nil); got != `{"path":` {
		t.Errorf("rawToolArgs = %q, want the payload verbatim", got)
	}
	long := strings.Repeat("x", cardRawArgsLimit+50)
	got := rawToolArgs(long, nil)
	if runes := []rune(got); len(runes) != cardRawArgsLimit+1 || runes[len(runes)-1] != '…' {
		t.Errorf("long payload not clipped with an ellipsis: len=%d", len(runes))
	}
}
