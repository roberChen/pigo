package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// This file covers the model-level key routing for the full-screen modal
// composer: the keys the root model intercepts before the editor (Esc, Enter,
// ↑/↓) must defer to modal editing when the composer is in normal mode, Ctrl+E
// must hand the terminal over and back, and an in-flight run keeps its
// Esc-interrupts behavior.

// newVimModel builds a sized model, opens the full-screen composer (where modal
// editing lives) and types a draft in it.
func newVimModel(t *testing.T, draft string) Model {
	t.Helper()
	next, _ := NewModel(Options{}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(Model)
	for _, r := range draft {
		next, _ = m.Update(runeKey(r))
		m = next.(Model)
	}
	return m
}

func TestModelVimEscEntersNormalInsteadOfQuitting(t *testing.T) {
	m := newVimModel(t, "abc")
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := next.(Model)
	if cmd != nil {
		t.Fatalf("Esc from insert mode returned a command (%T); it must stay in the program", cmd())
	}
	if got.quitting {
		t.Fatal("Esc from insert mode marked the model as quitting")
	}
	if got.input.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal", got.input.Mode())
	}
	if got.input.Value() != "abc" {
		t.Fatalf("Esc changed the draft: %q", got.input.Value())
	}
}

func TestModelVimEscInNormalDoesNotQuit(t *testing.T) {
	m := newVimModel(t, "abc")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := next.(Model)
	if cmd != nil || got.quitting {
		t.Fatal("Esc in normal mode must neither quit nor return a command")
	}
	if got.input.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal", got.input.Mode())
	}
}

func TestModelVimEnterInNormalDoesNotSubmit(t *testing.T) {
	m := newVimModel(t, "hello")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := next.(Model)
	if got.input.Value() != "hello" {
		t.Fatalf("Enter in normal mode changed the draft: %q", got.input.Value())
	}
	if joined := strings.Join(blockTexts(got.transcript), "\n"); strings.Contains(joined, "hello") {
		t.Fatalf("Enter in normal mode submitted the prompt:\n%s", joined)
	}
	if got.input.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal after a swallowed Enter", got.input.Mode())
	}
}

func TestModelVimArrowsAreMotionsNotHistory(t *testing.T) {
	m := newVimModel(t, "a")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // submit "a" to fill history
	m = next.(Model)
	m.input.SetValue("x\ny")
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) // caret on the last line
	m = next.(Model)
	if line, _ := m.input.caret(); line != 1 {
		t.Fatalf("caret row before ↑ = %d, want 1", line)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = next.(Model)
	if line, _ := m.input.caret(); line != 0 {
		t.Fatalf("↑ in normal mode should be k (row 0), got row %d", line)
	}
	if m.input.Value() != "x\ny" {
		t.Fatalf("↑ replaced the draft with history: %q", m.input.Value())
	}
}

func TestModelVimEscStillInterruptsRunningAgent(t *testing.T) {
	interrupted := false
	next, _ := NewModel(Options{}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := next.(Model)
	m.running = true
	m.interruptFn = func() { interrupted = true }
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := next.(Model)
	if !interrupted {
		t.Fatal("Esc during a run must still interrupt")
	}
	if got.quitting {
		t.Fatal("Esc during a run must not quit")
	}
	if got.input.Mode() != vimInsert {
		t.Fatalf("mode = %v, want insert while running", got.input.Mode())
	}
}

// TestFullscreenToggleRoundTrip covers Ctrl+E: the editor takes the whole
// terminal (transcript hidden), shows the hint bar, and hands the screen back —
// with the draft intact and inline editing non-modal again.
func TestFullscreenToggleRoundTrip(t *testing.T) {
	next, _ := NewModel(Options{}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := next.(Model)
	for _, r := range "draft text" {
		next, _ = m.Update(runeKey(r))
		m = next.(Model)
	}
	inlineHeight := m.input.Height()

	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(Model)
	if !m.input.Fullscreen() {
		t.Fatal("ctrl+e did not enter the full-screen composer")
	}
	if got := m.input.Height(); got != 23 {
		t.Fatalf("full-screen height = %d, want 23 (all rows above the status bar)", got)
	}
	if m.input.Height() <= inlineHeight {
		t.Fatalf("full-screen height %d did not grow beyond inline %d", m.input.Height(), inlineHeight)
	}
	view := m.View().Content
	if strings.Count(view, "\n") != 23 {
		t.Fatalf("full-screen view has %d newlines, want 23 rows", strings.Count(view, "\n"))
	}
	if !strings.Contains(view, "-- INSERT --") || !strings.Contains(view, "ctrl+e leave full screen") {
		t.Fatalf("hint bar missing from full-screen view:\n%s", view)
	}
	if strings.Contains(view, "draft text") == false {
		t.Fatalf("draft missing from full-screen view:\n%s", view)
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(Model)
	if m.input.Fullscreen() {
		t.Fatal("ctrl+e did not leave the full-screen composer")
	}
	if m.input.Value() != "draft text" {
		t.Fatalf("draft lost on exit: %q", m.input.Value())
	}
	if m.input.Mode() != vimInsert {
		t.Fatalf("mode after exit = %v, want insert", m.input.Mode())
	}
	if got := m.input.Height(); got != inlineHeight {
		t.Fatalf("inline height after exit = %d, want %d", got, inlineHeight)
	}
	// Inline editing is plain again: letters type and Esc quits.
	next, _ = m.Update(runeKey('x'))
	m = next.(Model)
	if m.input.Value() != "draft textx" {
		t.Fatalf("inline typing after exit: %q", m.input.Value())
	}
}

// TestFullscreenHintReflectsMode verifies the hint bar is mode-aware: insert
// mode advertises Esc → normal, normal mode advertises the commands.
func TestFullscreenHintReflectsMode(t *testing.T) {
	m := newVimModel(t, "abc")
	view := m.View().Content
	if !strings.Contains(view, "-- INSERT --") || !strings.Contains(view, "esc normal mode") {
		t.Fatalf("insert-mode hints missing:\n%s", view)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	view = m.View().Content
	if !strings.Contains(view, "-- NORMAL --") {
		t.Fatalf("normal-mode label missing:\n%s", view)
	}
	for _, hint := range []string{"i a o insert", "w b e move", "dd cc yy", "u undo", "ctrl+e leave full screen"} {
		if !strings.Contains(view, hint) {
			t.Errorf("normal-mode hints missing %q:\n%s", hint, view)
		}
	}
}

// TestFullscreenNarrowTerminalKeepsOneHintLine verifies the hint bar degrades to
// one line instead of spending rows on truncated noise.
func TestFullscreenNarrowTerminalKeepsOneHintLine(t *testing.T) {
	next, _ := NewModel(Options{}).Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m := next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(Model)
	if got := m.input.Height(); got != 19 {
		t.Fatalf("height = %d, want 19", got)
	}
	view := m.View().Content
	if strings.Count(view, "\n") != 19 {
		t.Fatalf("view rows = %d, want 19", strings.Count(view, "\n"))
	}
	if strings.Contains(view, "enter sends in the inline composer") {
		t.Fatalf("second hint line rendered on a narrow terminal:\n%s", view)
	}
}

// TestFullscreenResizeRefillsTerminal verifies a resize keeps the full-screen
// editor sized to the new terminal instead of staying at the old height.
func TestFullscreenResizeRefillsTerminal(t *testing.T) {
	m := newVimModel(t, "abc")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(Model)
	if got := m.input.Height(); got != 39 {
		t.Fatalf("after resize height = %d, want 39", got)
	}
	if strings.Count(m.View().Content, "\n") != 39 {
		t.Fatalf("after resize view rows = %d, want 39", strings.Count(m.View().Content, "\n"))
	}
}

// TestFullscreenEnterBreaksLineInsteadOfSending pins the full-screen contract:
// Enter is a newline (vim's meaning), never a send. A vim user pressing Enter to
// start a paragraph must not fire a half-written prompt, and the draft and
// layout must survive. Sending is the inline composer's Enter.
func TestFullscreenEnterBreaksLineInsteadOfSending(t *testing.T) {
	m := newVimModel(t, "hello")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := next.(Model)
	if !got.input.Fullscreen() {
		t.Fatal("Enter left the full-screen composer")
	}
	if got.input.Value() != "hello\n" {
		t.Fatalf("Enter value = %q, want a trailing newline", got.input.Value())
	}
	if joined := strings.Join(blockTexts(got.transcript), "\n"); strings.Contains(joined, "hello") {
		t.Fatalf("Enter from full screen sent the prompt:\n%s", joined)
	}
}

// TestFullscreenColonQuitsToInline covers :q: it hands the screen back to the
// inline composer with the draft and caret intact, without sending. Enter then
// sends from there, which is the documented two-step.
func TestFullscreenColonQuitsToInline(t *testing.T) {
	m := newVimModel(t, "hello")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if m.input.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal", m.input.Mode())
	}
	next, _ = m.Update(runeKey(':'))
	m = next.(Model)
	next, _ = m.Update(runeKey('q'))
	got := next.(Model)
	if got.input.Fullscreen() {
		t.Fatal(":q left the composer full screen")
	}
	if got.input.Value() != "hello" {
		t.Fatalf(":q changed the draft: %q", got.input.Value())
	}
	if joined := strings.Join(blockTexts(got.transcript), "\n"); strings.Contains(joined, "hello") {
		t.Fatalf(":q sent the prompt:\n%s", joined)
	}

	// Back inline, Enter sends again.
	next, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got = next.(Model)
	if got.input.Value() != "" {
		t.Fatalf("inline Enter did not send: %q", got.input.Value())
	}
	if joined := strings.Join(blockTexts(got.transcript), "\n"); !strings.Contains(joined, "hello") {
		t.Fatalf("inline Enter did not record the prompt:\n%s", joined)
	}
}

// TestFullscreenUnknownColonCommandKeepsDraft guards the command line against
// swallowing the buffer: an unsupported ex command is dropped, like any
// unmapped normal-mode key.
func TestFullscreenUnknownColonCommandKeepsDraft(t *testing.T) {
	m := newVimModel(t, "hello")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	for _, r := range ":wq" {
		next, _ = m.Update(runeKey(r))
		m = next.(Model)
	}
	if !m.input.Fullscreen() {
		t.Fatal("an unknown ex command left full screen")
	}
	if m.input.Value() != "hello" {
		t.Fatalf("an unknown ex command changed the draft: %q", m.input.Value())
	}
}

// TestFullscreenIgnoredWhileRunningOrAsking verifies the toggle never steals the
// keyboard from a run or from the question tool's answer field.
func TestFullscreenIgnoredWhileRunningOrAsking(t *testing.T) {
	next, _ := NewModel(Options{}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := next.(Model)
	m.running = true
	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	if next.(Model).input.Fullscreen() {
		t.Fatal("ctrl+e entered full screen during a run")
	}

	m = next.(Model)
	m.running = false
	m.ask = &askPrompt{question: "q?"}
	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	if next.(Model).input.Fullscreen() {
		t.Fatal("ctrl+e entered full screen while the question tool owns the composer")
	}
}

// TestFullscreenEscAfterResize verifies a terminal resize does not detach the
// editor from the full-screen layout: Esc still enters normal mode afterwards.
func TestFullscreenEscAfterResize(t *testing.T) {
	m := newVimModel(t, "abc")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	if !m.input.Fullscreen() {
		t.Fatal("resize dropped full-screen mode")
	}
	if got := m.input.Height(); got != 39 {
		t.Fatalf("height after resize = %d, want 39", got)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if m.input.Mode() != vimNormal {
		t.Fatalf("esc after resize did not enter normal mode (mode %v)", m.input.Mode())
	}
}

// TestFullscreenKeepsSlashMenuClosed verifies the full-screen composer never
// opens the autocomplete popup — including on a paste — and that the popup comes
// back when the inline composer returns, since a hidden menu would otherwise
// steal normal-mode Enter.
func TestFullscreenKeepsSlashMenuClosed(t *testing.T) {
	next, _ := NewModel(Options{}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := next.(Model)
	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(Model)

	next, _ = m.Update(runeKey('/'))
	m = next.(Model)
	if m.menu.active {
		t.Fatal("slash menu opened inside the full-screen composer")
	}
	next, _ = m.Update(tea.PasteMsg{Content: "mo"})
	m = next.(Model)
	if m.menu.active {
		t.Fatalf("paste opened the slash menu inside the full-screen composer")
	}
	if m.input.Value() != "/mo" {
		t.Fatalf("paste into full screen: %q", m.input.Value())
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = next.(Model)
	next, _ = m.Update(runeKey('d'))
	m = next.(Model)
	if !m.menu.active {
		t.Fatalf("slash menu did not come back after leaving full screen (draft %q)", m.input.Value())
	}
}

// TestFullscreenNormalModeEnterNeverSubmits verifies a draft that looks like a
// slash command still needs insert mode before Enter sends it: normal mode Enter
// is a vim key, and a hidden menu selection must not bypass that.
func TestFullscreenNormalModeEnterNeverSubmits(t *testing.T) {
	m := newVimModel(t, "/model")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(Model)
	if m.input.Mode() != vimNormal {
		t.Fatalf("mode = %v, want normal", m.input.Mode())
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := next.(Model)
	if !got.input.Fullscreen() {
		t.Fatal("normal-mode Enter submitted and left full screen")
	}
	if got.input.Value() != "/model" {
		t.Fatalf("normal-mode Enter changed the draft: %q", got.input.Value())
	}
	if joined := strings.Join(blockTexts(got.transcript), "\n"); strings.Contains(joined, "/model") {
		t.Fatalf("normal-mode Enter submitted the draft:\n%s", joined)
	}
}
