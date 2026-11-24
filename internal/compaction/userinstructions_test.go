// This file covers the user-instruction section of a compaction checkpoint: what
// counts as a user instruction, how the section is budgeted, how the model's
// digest block is parsed (and what happens when it is missing), and how a
// re-compaction continues the ledger instead of restarting it.
package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/provider"
)

// syntheticUser builds a harness-injected user turn of the given origin.
func syntheticUser(text, origin string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
		Origin:    origin,
	}
}

// TestExtractUserInstructionsSkipsSyntheticTurns verifies that only what the
// human typed reaches the ledger. Background sub-agent reports, Stop-hook
// guidance and /goal nudges all ride the user role, so without this filter the
// section would read as if the user had demanded the harness's boilerplate.
func TestExtractUserInstructionsSkipsSyntheticTurns(t *testing.T) {
	msgs := []agentcore.Message{
		userMsg("real request"),
		syntheticUser("sub-agent finished: here is a huge report", agentcore.UserMessageOriginSubAgentReport),
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}},
		syntheticUser("hook says keep going", agentcore.UserMessageOriginHookGuidance),
		syntheticUser("continue working toward the goal", agentcore.UserMessageOriginGoalContinuation),
		userMsg("second request"),
	}
	got := ExtractUserInstructions(msgs)
	want := []string{"real request", "second request"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("instructions = %q, want %q", got, want)
	}
}

// TestExtractUserInstructionsStripsSkillExpansion verifies a skill selection is
// reduced to the prompt the human typed. The expansion is the skill's whole
// SKILL.md body; keeping it would let one /skill invocation dominate the section
// for the rest of the session.
func TestExtractUserInstructionsStripsSkillExpansion(t *testing.T) {
	body := skillsLeadIn + "\n\n" + `<skill name="modern-go">
---
name: modern-go
---
A very long set of instructions that is not the user's own words.
</skill>

Now do the thing.`
	got := ExtractUserInstructions([]agentcore.Message{userMsg(body)})
	if len(got) != 1 || got[0] != "Now do the thing." {
		t.Fatalf("instructions = %q, want just the user's own prompt", got)
	}

	// A message that is ONLY skill instructions carries no request at all.
	only := skillsLeadIn + "\n\n" + `<skill name="x">body</skill>`
	if got := ExtractUserInstructions([]agentcore.Message{userMsg(only)}); len(got) != 0 {
		t.Fatalf("skill-only message must yield no instruction, got %q", got)
	}
}

// TestExtractUserInstructionsNormalizesAndDedupes verifies multi-line requests
// collapse onto one bullet (the section is a Markdown list) and a resubmitted
// identical prompt is one instruction rather than two.
func TestExtractUserInstructionsNormalizesAndDedupes(t *testing.T) {
	msgs := []agentcore.Message{
		userMsg("fix   the\n\nbuild"),
		userMsg("fix the build"),
		userMsg("then ship it"),
	}
	got := ExtractUserInstructions(msgs)
	want := []string{"fix the build", "then ship it"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("instructions = %q, want %q", got, want)
	}
}

// TestExtractUserInstructionsBudget verifies the section's cumulative size cap.
// Shedding starts at the oldest end — those entries were already absorbed by the
// rolling digest, so their exact wording matters least — while the newest
// requests always survive intact.
func TestExtractUserInstructionsBudget(t *testing.T) {
	// Distinct bodies: identical neighbours dedupe, which would never exercise
	// the budget.
	const body = maxVerbatimItemChars - 30
	raw := make([]string, 0, 21)
	msgs := make([]agentcore.Message, 0, 21)
	for i := 0; i < 20; i++ {
		entry := fmt.Sprintf("req-%02d ", i) + strings.Repeat("a", body)
		raw = append(raw, entry)
		msgs = append(msgs, userMsg(entry))
	}
	raw = append(raw, "the newest request")
	msgs = append(msgs, userMsg("the newest request"))

	got := ExtractUserInstructions(msgs)
	if SectionChars(got) > maxVerbatimSectionChars {
		t.Fatalf("section is %d chars, over the %d budget", SectionChars(got), maxVerbatimSectionChars)
	}
	if len(got) == 0 || got[len(got)-1] != "the newest request" {
		t.Fatalf("newest request must survive intact, got tail %q", got[len(got)-1])
	}
	if len(got) >= len(raw) {
		t.Fatalf("budget shed nothing: %d entries", len(got))
	}
	// The survivors are a suffix of the raw list. Its first entry may have been
	// degraded to a head of itself; every later entry must be intact.
	offset := len(raw) - len(got)
	for i, entry := range got {
		full := raw[offset+i]
		if i > 0 {
			if entry != full {
				t.Fatalf("entry %d should be intact, got %q", i, entry)
			}
			continue
		}
		if entry == full {
			continue
		}
		head := strings.TrimSuffix(entry, " …")
		if !strings.HasPrefix(full, head) || len([]rune(head)) > verbatimDegradeChars {
			t.Fatalf("oldest surviving entry should be a shortened head of %q, got %q", full, entry)
		}
	}
}

// TestExtractUserInstructionsTruncatesOneHugeRequest verifies a single pasted
// blob is capped and marked, so the section cannot become a permanent thumb on
// the context meter.
func TestExtractUserInstructionsTruncatesOneHugeRequest(t *testing.T) {
	got := ExtractUserInstructions([]agentcore.Message{userMsg(strings.Repeat("z", maxVerbatimItemChars*3))})
	if len(got) != 1 {
		t.Fatalf("want one entry, got %d", len(got))
	}
	if len([]rune(got[0])) > maxVerbatimItemChars+32 {
		t.Fatalf("entry not truncated: %d chars", len([]rune(got[0])))
	}
	if !strings.HasSuffix(got[0], "[truncated]") {
		t.Fatalf("truncated entry should say so: %q", got[0][:40])
	}
}

// TestSplitUserInstructions covers the response parser, including the model
// ignoring the requested format: the checkpoint must still land, as summary-only.
func TestSplitUserInstructions(t *testing.T) {
	raw := "## Goal\ndo X\n\n## Progress\nhalf\n\n<user-instructions>\n- no edits to internal/\n</user-instructions>"
	summary, instructions, found := splitUserInstructions(raw)
	if !found {
		t.Fatal("expected the block to be found")
	}
	if instructions != "- no edits to internal/" {
		t.Fatalf("instructions = %q", instructions)
	}
	if strings.Contains(summary, "<user-instructions>") || !strings.Contains(summary, "## Goal") {
		t.Fatalf("summary should keep the technical half only: %q", summary)
	}

	// No block: the whole text is the summary and found is false.
	summary, instructions, found = splitUserInstructions("## Goal\nplain summary")
	if found || instructions != "" || summary != "## Goal\nplain summary" {
		t.Fatalf("missing block: summary=%q instructions=%q found=%v", summary, instructions, found)
	}

	// Unterminated block: treated as missing rather than swallowing the text.
	if _, _, found := splitUserInstructions("## Goal\n<user-instructions>\n- x"); found {
		t.Fatal("unterminated block must not count as found")
	}
}

// TestBuildUserInstructionsCarriesPreviousDigest verifies the anti-drift rule: a
// model that omits the block on one turn must not erase the requirements
// accumulated so far.
func TestBuildUserInstructionsCarriesPreviousDigest(t *testing.T) {
	prev := &agentcore.UserInstructionBlock{Summary: "never touch deploy/", Verbatim: []string{"old words"}}
	window := []agentcore.Message{userMsg("add a flag")}

	got := buildUserInstructions(&SummaryOutput{Summary: "## Goal", InstructionsFound: false}, prev, window)
	if got == nil {
		t.Fatal("expected a block")
	}
	if got.Summary != "never touch deploy/" {
		t.Fatalf("digest = %q, want the previous digest carried over", got.Summary)
	}
	// Verbatim is replaced, not appended: the old list is already in the digest.
	if strings.Join(got.Verbatim, "|") != "add a flag" {
		t.Fatalf("verbatim = %q, want only this window's requests", got.Verbatim)
	}

	// A found-but-empty block clears the digest on purpose (the model decided the
	// old requirements are obsolete), while verbatim still tracks the window.
	got = buildUserInstructions(&SummaryOutput{InstructionsFound: true}, prev, window)
	if got.Summary != "" {
		t.Fatalf("an explicit empty digest must not be replaced by the old one, got %q", got.Summary)
	}

	// Nothing anywhere: no section at all, so an old-style checkpoint shape holds.
	if got := buildUserInstructions(&SummaryOutput{InstructionsFound: false}, nil, []agentcore.Message{assistantMsg("x", nil, "")}); got != nil {
		t.Fatalf("expected nil block, got %+v", got)
	}
}

// TestCompactAttachesUserInstructions verifies the end-to-end assembly: the
// summarization response's digest and the window's verbatim requests both land
// on the result, and the compressed message carries them into the rebuilt
// context.
func TestCompactAttachesUserInstructions(t *testing.T) {
	msgs := []agentcore.Message{
		userMsg("never write to deploy/"),
		assistantMsg("understood", nil, ""),
		bigUser(100),
		assistantMsg("recent", nil, ""),
	}
	summary := "## Goal\nx\n\n<user-instructions>\n- never write to deploy/\n</user-instructions>"
	stream := fakeStreamFn(assistantText(summary), nil)
	settings := CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 50}

	res, err := Compact(context.Background(), stream, provider.Model{ID: "m"}, msgs, settings, -1, nil, "", nil, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res == nil {
		t.Fatal("Compact returned nil")
	}
	if res.Summary != "## Goal\nx" {
		t.Fatalf("summary must not carry the digest block: %q", res.Summary)
	}
	block := res.UserInstructions
	if block == nil || block.Summary != "- never write to deploy/" {
		t.Fatalf("digest = %+v", block)
	}
	if len(block.Verbatim) != 1 || block.Verbatim[0] != "never write to deploy/" {
		t.Fatalf("verbatim = %q", block.Verbatim)
	}

	msg := res.Message(7)
	if msg.UserInstructions == nil {
		t.Fatal("compaction message must persist the section")
	}
	text := msg.AsUserMessage().Content[0].(agentcore.TextContent).Text
	if !strings.Contains(text, "never write to deploy/") || !strings.Contains(text, "## User Instructions") {
		t.Fatalf("replayed checkpoint should carry the section:\n%s", text)
	}
}

// TestCompactSecondPassReplacesVerbatim verifies a re-compaction builds on the
// previous checkpoint: the digest is fed back into the summarization prompt and
// the verbatim list covers only the new window, so entries are not duplicated
// turn after turn.
func TestCompactSecondPassReplacesVerbatim(t *testing.T) {
	prevBlock := &agentcore.UserInstructionBlock{
		Summary:  "- never write to deploy/",
		Verbatim: []string{"never write to deploy/"},
	}
	prevMsg := agentcore.CompactionMessage{
		RoleField:        agentcore.RoleCompaction,
		Summary:          "## Goal\nold",
		UserInstructions: prevBlock,
		CoveredMessages:  0,
	}
	msgs := []agentcore.Message{
		prevMsg,
		userMsg("also keep the API stable"),
		assistantMsg("ok", nil, ""),
		bigUser(100),
		assistantMsg("recent", nil, ""),
	}
	summary := "## Goal\nmerged\n\n<user-instructions>\n- never write to deploy/\n- keep the API stable\n</user-instructions>"

	var captured provider.LlmContext
	stream := fakeStreamFn(assistantText(summary), &captured)
	settings := CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 50}

	prior := PriorCheckpoint(msgs)
	if prior.Index != 0 {
		t.Fatalf("PriorCheckpoint index = %d, want 0", prior.Index)
	}
	res, err := Compact(context.Background(), stream, provider.Model{ID: "m"}, msgs, settings,
		prior.Index, prior.Details, prior.Summary, prior.UserInstructions, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res == nil {
		t.Fatal("Compact returned nil")
	}
	prompt := textOf(captured.Messages[0].(agentcore.UserMessage).Content)
	if !strings.Contains(prompt, "<previous-user-instructions>") || !strings.Contains(prompt, "never write to deploy/") {
		t.Fatalf("the previous ledger must be fed back into the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<previous-summary>") {
		t.Fatal("expected the iterative update template")
	}
	// Only the new window's request is quoted; the old one lives in the digest.
	if strings.Join(res.UserInstructions.Verbatim, "|") != "also keep the API stable" {
		t.Fatalf("verbatim = %q, want just the new window", res.UserInstructions.Verbatim)
	}
	if strings.Count(res.UserInstructions.Summary, "keep the API stable") != 1 {
		t.Fatalf("digest should be consolidated once: %q", res.UserInstructions.Summary)
	}
}

// TestCompactOldCheckpointWithoutSection verifies a checkpoint written before
// the section existed still re-compacts: the Details round-trip (which old
// checkpoints do carry) is unaffected, and the new checkpoint gains a section
// holding only the code-extracted verbatim half, because there is no prior
// digest to consolidate.
func TestCompactOldCheckpointWithoutSection(t *testing.T) {
	details, _ := json.Marshal(CompactionDetails{ReadFiles: []string{"a.go"}})
	msgs := []agentcore.Message{
		agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "## Goal\nold", Details: details},
		userMsg("new work"),
		bigUser(100),
		assistantMsg("recent", nil, ""),
	}
	stream := fakeStreamFn(assistantText("## Goal\nnew"), nil)
	settings := CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 50}

	prior := PriorCheckpoint(msgs)
	if prior.UserInstructions != nil {
		t.Fatalf("old checkpoint should have no section, got %+v", prior.UserInstructions)
	}
	res, err := Compact(context.Background(), stream, provider.Model{ID: "m"}, msgs, settings,
		prior.Index, prior.Details, prior.Summary, prior.UserInstructions, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res == nil {
		t.Fatal("Compact returned nil")
	}
	// a.go came from the previous checkpoint's file list.
	if strings.Join(res.Details.ReadFiles, ",") != "a.go" {
		t.Fatalf("readFiles = %v, want [a.go]", res.Details.ReadFiles)
	}
	// The model omitted the block and there was no prior digest, so the digest
	// half stays empty; the verbatim half is extracted by code and still lands.
	if res.UserInstructions == nil {
		t.Fatal("expected a section carrying the verbatim half")
	}
	if res.UserInstructions.Summary != "" {
		t.Errorf("digest must stay empty without a model block, got %q", res.UserInstructions.Summary)
	}
	if strings.Join(res.UserInstructions.Verbatim, "|") != "new work" {
		t.Errorf("verbatim = %q, want [new work]", res.UserInstructions.Verbatim)
	}
}

// TestPromptInstructionsAddendumPresent verifies every summarization request
// asks for the digest — the section must not depend on the caller remembering to
// add the instruction.
func TestPromptInstructionsAddendumPresent(t *testing.T) {
	var captured provider.LlmContext
	stream := fakeStreamFn(assistantText("## Goal\nx"), &captured)
	_, err := GenerateSummaryOutput(context.Background(), stream, provider.Model{ID: "m"},
		[]agentcore.Message{userMsg("x")}, 16384, 0, "", nil, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("GenerateSummaryOutput: %v", err)
	}
	prompt := textOf(captured.Messages[0].(agentcore.UserMessage).Content)
	if !strings.Contains(prompt, instructionsOpen) || !strings.Contains(prompt, instructionsClose) {
		t.Fatalf("prompt must request the digest block:\n%s", prompt)
	}
}
