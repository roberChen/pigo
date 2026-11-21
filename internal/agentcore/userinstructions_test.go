// This file covers the user-instruction section of a compaction checkpoint as
// agentcore sees it: rendering the section, parsing it back out of Markdown, and
// the Text() rendering that every consumer (LLM replay, checkpoint file,
// transcripts) shares. The parser has to be forgiving, because the section also
// travels through a plain, hand-editable Markdown file.
package agentcore

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUserInstructionBlockRenderRoundTrip verifies a rendered section survives
// the trip back through ExtractUserInstructionsSection. This is the on-disk
// checkpoint contract: the section is Markdown, not frontmatter.
func TestUserInstructionBlockRenderRoundTrip(t *testing.T) {
	block := &UserInstructionBlock{
		Summary:  "never write to deploy/\nkeep the public API stable",
		Verbatim: []string{"refactor the parser", "and add a test for it"},
	}
	rendered := block.Render()
	if !strings.HasPrefix(rendered, UserInstructionsHeading) {
		t.Fatalf("section must start with its heading:\n%s", rendered)
	}
	if !strings.Contains(rendered, UserInstructionsSummaryLabel) || !strings.Contains(rendered, UserInstructionsVerbatimLabel) {
		t.Fatalf("section must carry both labels:\n%s", rendered)
	}

	summary, got := ExtractUserInstructionsSection("## Goal\ndo the thing\n\n" + rendered)
	if got == nil {
		t.Fatal("section not parsed back")
	}
	if summary != "## Goal\ndo the thing" {
		t.Fatalf("summary = %q", summary)
	}
	if got.Summary != block.Summary {
		t.Fatalf("digest round-trip: %q, want %q", got.Summary, block.Summary)
	}
	if strings.Join(got.Verbatim, "|") != strings.Join(block.Verbatim, "|") {
		t.Fatalf("verbatim round-trip: %q", got.Verbatim)
	}
}

// TestEmptyBlockRendersNothing verifies an absent section is invisible: callers
// append Render() unconditionally.
func TestEmptyBlockRendersNothing(t *testing.T) {
	for _, b := range []*UserInstructionBlock{nil, {}, {Summary: "   "}} {
		if got := b.Render(); got != "" {
			t.Errorf("%+v should render empty, got %q", b, got)
		}
		if note := b.Note(); note != "" {
			t.Errorf("%+v should have no note, got %q", b, note)
		}
		if !b.IsEmpty() {
			t.Errorf("%+v should be empty", b)
		}
	}
}

// TestExtractUserInstructionsSectionWithoutSection verifies a pre-existing
// checkpoint (summary only, possibly mentioning the heading in prose) is left
// whole rather than being cut in two.
func TestExtractUserInstructionsSectionWithoutSection(t *testing.T) {
	body := "## Goal\ndo the thing\n\n## Notes\nSee the User Instructions section of the docs."
	if summary, block := ExtractUserInstructionsSection(body); summary != body || block != nil {
		t.Fatalf("summary-only body must pass through unchanged: summary=%q block=%+v", summary, block)
	}

	// A heading with no recognizable labels under it is prose too.
	body = "## Goal\nx\n\n" + UserInstructionsHeading + "\nbut no labels follow"
	if summary, block := ExtractUserInstructionsSection(body); summary != body || block != nil {
		t.Fatalf("unlabeled heading must not be treated as a section: summary=%q block=%+v", summary, block)
	}
}

// TestExtractUserInstructionsSectionToleratesWrapping verifies a hand-wrapped
// bullet (a continuation line indented under it) is joined back into one entry.
func TestExtractUserInstructionsSectionToleratesWrapping(t *testing.T) {
	body := "## Goal\nx\n\n" + UserInstructionsHeading + "\n" +
		UserInstructionsVerbatimLabel + "\n- a request that was\n  wrapped by an editor\n- next request"
	summary, block := ExtractUserInstructionsSection(body)
	if summary != "## Goal\nx" {
		t.Fatalf("summary = %q", summary)
	}
	if block == nil || len(block.Verbatim) != 2 {
		t.Fatalf("block = %+v", block)
	}
	if block.Verbatim[0] != "a request that was wrapped by an editor" {
		t.Fatalf("continuation line not folded: %q", block.Verbatim[0])
	}
}

// TestCompactionMessageText verifies Text is summary + section, and that a
// section-only checkpoint still renders something rather than an empty string.
func TestCompactionMessageText(t *testing.T) {
	block := &UserInstructionBlock{Summary: "digest", Verbatim: []string{"ask"}}
	m := CompactionMessage{Summary: "## Goal\nx", UserInstructions: block}
	text := m.Text()
	if !strings.HasPrefix(text, "## Goal\nx\n\n") || !strings.Contains(text, UserInstructionsHeading) {
		t.Fatalf("Text = %q", text)
	}
	if got := (CompactionMessage{UserInstructions: block}).Text(); strings.HasPrefix(got, "\n") || !strings.Contains(got, "digest") {
		t.Fatalf("section-only Text = %q", got)
	}
	if got := (CompactionMessage{Summary: "only"}).Text(); got != "only" {
		t.Fatalf("summary-only Text = %q", got)
	}
}

// TestCompactionMessageSectionRoundTrip verifies the section survives the JSONL
// session schema, so a resumed session keeps the ledger.
func TestCompactionMessageSectionRoundTrip(t *testing.T) {
	in := MessageList{CompactionMessage{
		RoleField:        RoleCompaction,
		Summary:          "## Goal\nx",
		UserInstructions: &UserInstructionBlock{Summary: "digest", Verbatim: []string{"ask"}},
		Timestamp:        5,
	}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out MessageList
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := out[0].(CompactionMessage)
	if got.UserInstructions == nil || got.UserInstructions.Summary != "digest" || len(got.UserInstructions.Verbatim) != 1 {
		t.Fatalf("section lost on round-trip: %+v", got.UserInstructions)
	}

	// An old line without the section must still decode (and stay nil).
	var legacy MessageList
	legacyRaw := []byte(`[{"role":"compaction","summary":"old","timestamp":1}]`)
	if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacy[0].(CompactionMessage).UserInstructions != nil {
		t.Fatal("legacy checkpoint should have no section")
	}
}

// TestCompactionMessageAsUserMessageCarriesSection verifies the LLM replay path:
// the encoder's rendering includes the ledger, which is what keeps a compacted
// session from drifting away from the user's requirements.
func TestCompactionMessageAsUserMessageCarriesSection(t *testing.T) {
	m := CompactionMessage{
		Summary:          "## Goal\nx",
		UserInstructions: &UserInstructionBlock{Summary: "digest", Verbatim: []string{"ask"}},
	}
	text := m.AsUserMessage().Content[0].(TextContent).Text
	if !strings.Contains(text, "<summary>") || !strings.Contains(text, "digest") || !strings.Contains(text, "- ask") {
		t.Fatalf("replayed checkpoint = %q", text)
	}
}

// TestUserInstructionBlockNote verifies the one-line note distinguishes the
// halves, so a transcript can say what survived.
func TestUserInstructionBlockNote(t *testing.T) {
	cases := []struct {
		block *UserInstructionBlock
		want  string
	}{
		{&UserInstructionBlock{Summary: "d"}, "user instructions preserved (standing instructions)"},
		{&UserInstructionBlock{Verbatim: []string{"a"}}, "user instructions preserved (1 verbatim request)"},
		{&UserInstructionBlock{Summary: "d", Verbatim: []string{"a", "b"}}, "user instructions preserved (standing instructions + 2 verbatim requests)"},
	}
	for _, tc := range cases {
		if got := tc.block.Note(); got != tc.want {
			t.Errorf("Note(%+v) = %q, want %q", tc.block, got, tc.want)
		}
	}
}
