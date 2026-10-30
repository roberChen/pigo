package tui

// Tests for the TUI model picker (the interactive counterpart of "/models" and
// "/model <id>"): opening it from a bare /model, filtering, switching the live
// model, and reserving its rows in the shell layout.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/cli/prompts"
)

// modelPickerTestModel builds a sized TUI model whose live config starts on
// startModel, so the picker has a real LiveConfig to mutate.
func modelPickerTestModel(t *testing.T, startModel string) Model {
	t.Helper()
	m := NewModel(Options{Model: startModel, ProviderName: "openrouter"})
	return apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

// TestModelPickerOpensOnBareModel verifies that "/model" (no argument) opens the
// chooser instead of printing the one-line status, and that the invocation is
// echoed so the transcript shows what the user did.
func TestModelPickerOpensOnBareModel(t *testing.T) {
	m := modelPickerTestModel(t, "openai/gpt-4o")

	next, _ := m.runSlash("/model")
	m = next.(Model)
	if !m.modelPicker.active {
		t.Fatalf("bare /model should open the picker; transcript:\n%s", stripANSI(m.View().Content))
	}
	if got := m.input.Value(); got != "" {
		t.Errorf("composer should be cleared, got %q", got)
	}
	if len(m.modelPicker.all) == 0 {
		t.Fatal("picker should hold the preset catalog")
	}
	content := stripANSI(m.View().Content)
	if !strings.Contains(content, "select a model") {
		t.Errorf("picker overlay not rendered:\n%s", content)
	}
	if !strings.Contains(content, "(current)") {
		t.Errorf("the live model should be tagged in the picker:\n%s", content)
	}
}

// TestModelPickerAlsoListsModels verifies "/models" (the listing command) opens
// the same chooser in the TUI, while "/models <provider>" keeps its filtered
// text listing.
func TestModelPickerAlsoListsModels(t *testing.T) {
	m := modelPickerTestModel(t, "glm-4.7")
	next, _ := m.runSlash("/models")
	m = next.(Model)
	if !m.modelPicker.active {
		t.Fatal("bare /models should open the picker in the TUI")
	}

	m.modelPicker.close()
	m.relayout()
	next, _ = m.runSlash("/models nvidia")
	m = next.(Model)
	if m.modelPicker.active {
		t.Error("/models nvidia should keep the filtered text listing")
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "nvidia") {
		t.Errorf("filtered listing missing nvidia:\n%s", content)
	}
}

// TestModelPickerFiltersAndSwitches drives the picker: typing narrows the list,
// arrows move the highlight, and Enter switches the live model to the highlighted
// id (the same switch "/model <id>" performs).
func TestModelPickerFiltersAndSwitches(t *testing.T) {
	m := modelPickerTestModel(t, "openai/gpt-4o")
	next, _ := m.runSlash("/model")
	m = next.(Model)

	m = typeInto(t, m, "glm").(Model)
	if len(m.modelPicker.filtered) == 0 {
		t.Fatalf("typing 'glm' should leave matches, filter=%q", m.modelPicker.filter)
	}
	for _, c := range m.modelPicker.filtered {
		if !strings.Contains(strings.ToLower(c.ID+" "+c.Label+" "+c.Provider), "glm") {
			t.Errorf("row %q does not match the filter", c.ID)
		}
	}

	want, ok := m.modelPicker.currentChoice()
	if !ok {
		t.Fatal("the filtered list should highlight a row")
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.modelPicker.active {
		t.Error("Enter should close the picker")
	}
	if m.live.Model != want.ID {
		t.Errorf("live.Model = %q, want %q", m.live.Model, want.ID)
	}
	if m.statusBar.model != m.live.Model {
		t.Errorf("status bar model = %q, want %q", m.statusBar.model, m.live.Model)
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "model switched to "+want.ID) {
		t.Errorf("switch outcome not reported:\n%s", content)
	}
}

// TestModelPickerFilterBackspace verifies Backspace edits the filter and that a
// filter matching nothing shows the empty-state line without crashing Enter.
func TestModelPickerFilterBackspace(t *testing.T) {
	m := modelPickerTestModel(t, "openai/gpt-4o")
	next, _ := m.runSlash("/model")
	m = next.(Model)

	m = typeInto(t, m, "zzz").(Model)
	if len(m.modelPicker.filtered) != 0 {
		t.Fatalf("filter 'zzz' should match nothing, got %d rows", len(m.modelPicker.filtered))
	}
	if content := stripANSI(m.View().Content); !strings.Contains(content, "no matching model") {
		t.Errorf("empty-state row missing:\n%s", content)
	}
	// Enter with no match is a no-op that just closes the picker.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.modelPicker.active {
		t.Error("Enter on an empty result should close the picker")
	}
	if m.live.Model != "openai/gpt-4o" {
		t.Errorf("live.Model = %q, want it unchanged", m.live.Model)
	}

	// Backspace restores matches.
	next, _ = m.runSlash("/model")
	m = next.(Model)
	m = typeInto(t, m, "glm").(Model)
	before := len(m.modelPicker.filtered)
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.modelPicker.filter != "gl" {
		t.Errorf("filter = %q, want %q", m.modelPicker.filter, "gl")
	}
	if len(m.modelPicker.filtered) < before {
		t.Errorf("widening the filter shrank the list: %d -> %d", before, len(m.modelPicker.filtered))
	}
}

// TestModelPickerIsModal verifies the overlay swallows keys (the composer is not
// edited behind it) and Esc dismisses it without switching models.
func TestModelPickerIsModal(t *testing.T) {
	m := modelPickerTestModel(t, "openai/gpt-4o")
	next, _ := m.runSlash("/model")
	m = next.(Model)
	liveBefore := *m.live

	m = typeInto(t, m, "sonnet").(Model)
	if got := m.input.Value(); got != "" {
		t.Errorf("typing while the picker is open changed the composer: %q", got)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.modelPicker.active {
		t.Error("Esc should dismiss the picker")
	}
	if m.live.Model != liveBefore.Model || m.live.ProviderName != liveBefore.ProviderName {
		t.Errorf("Esc must not switch models, got (%q, %q)", m.live.Model, m.live.ProviderName)
	}
}

// TestModelPickerKeepsShellHeight verifies the overlay's rows are reserved out of
// the transcript so the shell keeps exactly Height rows while the picker is up.
func TestModelPickerKeepsShellHeight(t *testing.T) {
	m := modelPickerTestModel(t, "openai/gpt-4o")
	base := m.transcript.viewportHeight()

	next, _ := m.runSlash("/model")
	m = next.(Model)
	wantRows := m.modelPicker.rows()
	if wantRows <= 0 {
		t.Fatalf("open picker should occupy rows, got %d", wantRows)
	}
	if got := m.transcript.viewportHeight(); got != base-wantRows {
		t.Errorf("transcript height with the picker open = %d, want %d (base %d - %d)", got, base-wantRows, base, wantRows)
	}
	if got := strings.Count(m.View().Content, "\n"); got != 29 {
		t.Errorf("newline count = %d, want 29 (30 rows)", got)
	}
}

// TestModelChoicesIncludeFetchedFirst verifies the picker catalog prefers the
// live endpoint's fetched ids, keeps them ahead of the presets, and does not
// repeat an id that is both fetched and a preset.
func TestModelChoicesIncludeFetchedFirst(t *testing.T) {
	live := &cli.LiveConfig{
		Model:         "m-fetched",
		ProviderName:  "openai",
		FetchedModels: []string{"m-fetched", "glm-4.7"},
	}

	choices := prompts.ModelChoices(live)
	if len(choices) == 0 {
		t.Fatal("ModelChoices returned no rows")
	}
	if choices[0].ID != "m-fetched" {
		t.Errorf("first choice = %q, want the fetched id", choices[0].ID)
	}
	seen := map[string]int{}
	for _, c := range choices {
		seen[c.ID]++
	}
	if seen["glm-4.7"] != 1 {
		t.Errorf("glm-4.7 appears %d times, want exactly 1 (fetched + preset dedupe)", seen["glm-4.7"])
	}
	for _, c := range choices {
		if c.ID == "" {
			t.Error("choice with an empty id")
		}
	}
}

// openPickerForTest opens the chooser on a sized model and returns it, failing
// when the catalog is too short for the navigation assertions below.
func openPickerForTest(t *testing.T) Model {
	t.Helper()
	m := modelPickerTestModel(t, "openai/gpt-4o")
	next, _ := m.runSlash("/model")
	m = next.(Model)
	if !m.modelPicker.active {
		t.Fatal("bare /model should open the picker")
	}
	if len(m.modelPicker.filtered) <= maxModelPickerRows {
		t.Fatalf("catalog has %d rows, need more than %d for paging tests", len(m.modelPicker.filtered), maxModelPickerRows)
	}
	return m
}

// TestModelPickerArrowNavigation drives ↑/↓ through the keyboard: each press
// moves the highlight one row and the walk wraps at both ends, so arrowing down
// past the last row returns to the first and vice versa.
func TestModelPickerArrowNavigation(t *testing.T) {
	m := openPickerForTest(t)
	n := len(m.modelPicker.filtered)
	// The picker opens on the current model, which is not necessarily row 0; home
	// first so the expectations below are absolute.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.modelPicker.selected != 0 {
		t.Fatalf("Home selected = %d, want 0", m.modelPicker.selected)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.modelPicker.selected != 1 {
		t.Errorf("↓ selected = %d, want 1", m.modelPicker.selected)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.modelPicker.selected != 0 {
		t.Errorf("↑ selected = %d, want 0", m.modelPicker.selected)
	}

	// One ↑ at the top wraps to the last row; one ↓ there wraps back to the first.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.modelPicker.selected != n-1 {
		t.Errorf("↑ at the top selected = %d, want %d (wrap)", m.modelPicker.selected, n-1)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.modelPicker.selected != 0 {
		t.Errorf("↓ at the bottom selected = %d, want 0 (wrap)", m.modelPicker.selected)
	}

	// Ctrl+P / Ctrl+N are the readline spellings of the same walk.
	m = apply(t, m, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if m.modelPicker.selected != 1 {
		t.Errorf("Ctrl+N selected = %d, want 1", m.modelPicker.selected)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if m.modelPicker.selected != 0 {
		t.Errorf("Ctrl+P selected = %d, want 0", m.modelPicker.selected)
	}

	// Moving the highlight must not edit the composer or switch models.
	if got := m.input.Value(); got != "" {
		t.Errorf("composer changed while navigating: %q", got)
	}
	if m.live.Model != "openai/gpt-4o" {
		t.Errorf("live.Model = %q, want it unchanged", m.live.Model)
	}
}

// TestModelPickerPageAndJumpKeys verifies PgUp/PgDn travel a screenful and clamp
// at the ends instead of wrapping, and that Home/End jump to the first/last row.
func TestModelPickerPageAndJumpKeys(t *testing.T) {
	m := openPickerForTest(t)
	n := len(m.modelPicker.filtered)
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.modelPicker.selected != 0 {
		t.Fatalf("Home selected = %d, want 0", m.modelPicker.selected)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if want := maxModelPickerRows; m.modelPicker.selected != want {
		t.Errorf("PgDn selected = %d, want %d", m.modelPicker.selected, want)
	}
	// A second page from there lands a screenful further, not wrapped around.
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if want := 2 * maxModelPickerRows; m.modelPicker.selected != want {
		t.Errorf("second PgDn selected = %d, want %d", m.modelPicker.selected, want)
	}
	// Repeated paging stops at the last row.
	for range n {
		m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	if m.modelPicker.selected != n-1 {
		t.Errorf("PgDn at the bottom selected = %d, want %d (clamped)", m.modelPicker.selected, n-1)
	}
	// And paging up stops at the first.
	for range n {
		m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	if m.modelPicker.selected != 0 {
		t.Errorf("PgUp at the top selected = %d, want 0 (clamped)", m.modelPicker.selected)
	}

	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.modelPicker.selected != n-1 {
		t.Errorf("End selected = %d, want %d", m.modelPicker.selected, n-1)
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.modelPicker.selected != 0 {
		t.Errorf("Home selected = %d, want 0", m.modelPicker.selected)
	}
}

// TestModelPickerKeepsSelectionVisible verifies the window scrolls with a paged
// selection: the highlighted row is drawn, the position readout counts it, and
// the overlay still occupies at most maxModelPickerRows rows.
func TestModelPickerKeepsSelectionVisible(t *testing.T) {
	m := openPickerForTest(t)
	n := len(m.modelPicker.filtered)
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	want, ok := m.modelPicker.currentChoice()
	if !ok {
		t.Fatal("no highlighted choice after End")
	}

	start, end := m.modelPicker.window()
	if m.modelPicker.selected < start || m.modelPicker.selected >= end {
		t.Fatalf("selection %d outside window [%d,%d)", m.modelPicker.selected, start, end)
	}
	if end-start > maxModelPickerRows {
		t.Errorf("window shows %d rows, want at most %d", end-start, maxModelPickerRows)
	}
	content := stripANSI(m.View().Content)
	label := want.Label
	if label == "" {
		label = want.ID
	}
	if !strings.Contains(content, label) {
		t.Errorf("highlighted row %q not rendered:\n%s", label, content)
	}
	if readout := fmt.Sprintf("(%d/%d)", n, n); !strings.Contains(content, readout) {
		t.Errorf("position readout %q missing:\n%s", readout, content)
	}
}

// TestModelPickerMouseWheelMovesSelection verifies the wheel walks the picker
// while it is open instead of scrolling the transcript behind it.
func TestModelPickerMouseWheelMovesSelection(t *testing.T) {
	m := openPickerForTest(t)
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyHome})

	m = apply(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.modelPicker.selected != 1 {
		t.Errorf("wheel down selected = %d, want 1", m.modelPicker.selected)
	}
	m = apply(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.modelPicker.selected != 0 {
		t.Errorf("wheel up selected = %d, want 0", m.modelPicker.selected)
	}
	if !m.modelPicker.active {
		t.Error("the wheel should not dismiss the picker")
	}
}

// TestModelPickerFilterResetsNavigation verifies a narrowed list restarts the
// highlight at the first match — so Enter after typing picks a visible row
// rather than a stale index from before the filter — and that the arrows then
// walk only the matches.
func TestModelPickerFilterResetsNavigation(t *testing.T) {
	m := openPickerForTest(t)
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.modelPicker.selected == 0 {
		t.Fatal("End should have moved away from the first row")
	}

	m = typeInto(t, m, "glm").(Model)
	if m.modelPicker.selected != 0 {
		t.Errorf("selected after filtering = %d, want 0", m.modelPicker.selected)
	}
	if len(m.modelPicker.filtered) < 2 {
		t.Skipf("filter 'glm' left %d rows, need 2 to test walking", len(m.modelPicker.filtered))
	}
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.modelPicker.selected != 1 {
		t.Errorf("↓ in the filtered list selected = %d, want 1", m.modelPicker.selected)
	}
	want, ok := m.modelPicker.currentChoice()
	if !ok {
		t.Fatal("filtered list should still highlight a row")
	}
	for _, c := range m.modelPicker.filtered {
		if got := strings.ToLower(c.ID + " " + c.Label + " " + c.Provider); !strings.Contains(got, "glm") {
			t.Errorf("row %q is not a filter match", c.ID)
		}
	}
	// Enter switches to that filtered row, and the switch goes through the very
	// same command path as "/model <id>".
	m = apply(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.live.Model != want.ID {
		t.Errorf("live.Model = %q, want %q", m.live.Model, want.ID)
	}
}
