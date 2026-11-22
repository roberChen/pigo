// Tests for how the compaction window is prepared for the summarizer: the marker
// that makes "the most recent turn" mechanically identifiable, the tool-result
// truncation that keeps the outcome of a long run, and the prompt rules that tell
// the model what to do with the technical detail it finds there.
package compaction

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/roberChen/pigo/internal/agentcore"
)

// lastTurnDivider is a contract between serializeConversation and both
// summarization templates, which quote it verbatim when telling the model what it
// means. If they ever disagree the instruction silently points at nothing, so the
// prompt side is asserted too (see TestPromptsDescribeLastTurnDivider).
func TestSerializeConversationMarksLastTurn(t *testing.T) {
	msgs := []agentcore.Message{
		userMsg("first request"),
		agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content:   agentcore.ContentList{agentcore.NewTextContent("first answer")},
		},
		userMsg("second request"),
		agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content:   agentcore.ContentList{agentcore.NewTextContent("second answer")},
		},
	}
	got := serializeConversation(msgs)

	if n := strings.Count(got, lastTurnDivider); n != 1 {
		t.Fatalf("divider should appear exactly once, got %d:\n%s", n, got)
	}
	// The divider must sit immediately before the newest user turn, so everything
	// after it is that turn and nothing else.
	idx := strings.Index(got, lastTurnDivider)
	if tail := got[idx+len(lastTurnDivider):]; !strings.HasPrefix(strings.TrimSpace(tail), "[User]: second request") {
		t.Errorf("divider should precede the newest user turn, tail was:\n%s", tail)
	}
	if head := got[:idx]; !strings.Contains(head, "[User]: first request") {
		t.Errorf("older turns should stay above the divider, head was:\n%s", head)
	}
}

func TestSerializeConversationOmitsDividerForSingleTurn(t *testing.T) {
	// A window that is entirely the current turn needs no separator: there is
	// nothing above it to separate from, and a leading divider would read as if
	// earlier context had been cut off mid-thought.
	msgs := []agentcore.Message{
		userMsg("only request"),
		agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content:   agentcore.ContentList{agentcore.NewTextContent("only answer")},
		},
	}
	if got := serializeConversation(msgs); strings.Contains(got, lastTurnDivider) {
		t.Errorf("single-turn window should not carry a divider:\n%s", got)
	}
}

func TestSerializeConversationOmitsDividerWithoutUserText(t *testing.T) {
	// A user message with no text blocks (synthetic or content-only) is not a turn
	// boundary the model can read, so putting the divider above it would point at
	// a blank line.
	msgs := []agentcore.Message{
		userMsg("real request"),
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
		agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content:   agentcore.ContentList{agentcore.NewTextContent("answer")},
		},
	}
	got := serializeConversation(msgs)
	if idx := strings.Index(got, lastTurnDivider); idx >= 0 {
		if tail := got[idx+len(lastTurnDivider):]; !strings.Contains(tail, "[User]: real request") {
			t.Errorf("divider should precede the last turn with readable text:\n%s", got)
		}
	}
}

// TestTruncateForSummaryKeepsBothEnds pins the reason the truncation changed from
// head-only: the beginning says what ran, the end says how it ended, and the
// middle of a long tool result is the least informative part.
func TestTruncateForSummaryKeepsBothEnds(t *testing.T) {
	head := "=== RUN TestSomething\n"
	middle := strings.Repeat("ok ... noise\n", 400)
	tail := "--- FAIL: TestSomething\n    assertion failed: got 3, want 4\nexit status 1"

	got := truncateForSummary(head+middle+tail, toolResultMaxChars)

	if !strings.Contains(got, "=== RUN TestSomething") {
		t.Errorf("head lost:\n%s", got)
	}
	if !strings.Contains(got, "exit status 1") || !strings.Contains(got, "assertion failed: got 3, want 4") {
		t.Errorf("tail (the outcome) lost:\n%s", got)
	}
	if !strings.Contains(got, "characters truncated") {
		t.Errorf("marker missing:\n%s", got)
	}
	if len(got) > toolResultMaxChars+200 {
		t.Errorf("truncated text is %d bytes, budget was %d", len(got), toolResultMaxChars)
	}
}

func TestTruncateForSummaryNeverSplitsRune(t *testing.T) {
	// A naive text[:n] on CJK/emoji input yields invalid UTF-8, which then travels
	// into the prompt as mojibake.
	text := strings.Repeat("测试数据", 600)
	got := truncateForSummary(text, 500)

	if !utf8.ValidString(got) {
		t.Fatalf("truncated text is not valid UTF-8: %q", got[:60])
	}
	if !strings.Contains(got, "测试数据") {
		t.Errorf("kept text should still be readable CJK:\n%s", got)
	}
}

func TestTruncateForSummaryTinyBudgetFallsBackToHead(t *testing.T) {
	// With no room for two sides plus a marker, degrade to the head rather than
	// slicing a negative tail or emitting an empty string.
	got := truncateForSummary(strings.Repeat("abc", 100), 10)
	if !strings.HasPrefix(got, "abc") {
		t.Errorf("tiny budget should keep the head, got %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("tiny budget should still mark the cut, got %q", got)
	}
}

// TestPromptsPointAtTechnicalDetail verifies the templates ask for the things the
// prompt cannot infer on its own: non-obvious discoveries from the newest turn,
// exact commands, a resumable location inside a file, and environment facts.
func TestPromptsPointAtTechnicalDetail(t *testing.T) {
	checks := map[string][]string{
		"first-time": {
			"most recent turn",
			"exact invocations",
			"Where to resume inside a file",
			"Environment facts",
		},
		"update": {
			"newest messages",
			"Critical Context",
		},
	}
	for name, wants := range checks {
		prompt := summarizationPrompt
		if name == "update" {
			prompt = updateSummarizationPrompt
		}
		for _, want := range wants {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt should mention %q", name, want)
			}
		}
	}
}

// TestPromptsDescribeLastTurnDivider verifies the templates explain the marker
// serializeConversation inserts. An unexplained divider is just noise in the
// transcript, and a renamed divider would leave the instruction pointing at
// nothing.
func TestPromptsDescribeLastTurnDivider(t *testing.T) {
	fragment := "--- most recent turn below"
	for name, prompt := range map[string]string{
		"first-time": summarizationPrompt,
		"update":     updateSummarizationPrompt,
	} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("%s prompt should describe the last-turn divider", name)
		}
	}
	// Keep the constant and the prompts from drifting apart: the text the model
	// is told about must be the text the serializer emits.
	if !strings.Contains(lastTurnDivider, fragment) {
		t.Errorf("lastTurnDivider %q no longer contains %q", lastTurnDivider, fragment)
	}
}

// TestPromptsKeepTheCodeMapHeading pins the heading itself, in both templates.
// The section is the only place a checkpoint can say what a file CONTAINS as
// opposed to that it was touched (the harness appends the bare path lists), so a
// template that loses it silently reverts to a summary that makes the next turn
// re-read the same files. The update template needs it independently: every
// re-compaction is rendered from that template, so dropping it there would keep
// the section on the first checkpoint only and erase it on the second.
func TestPromptsKeepTheCodeMapHeading(t *testing.T) {
	for name, prompt := range map[string]string{
		"first-time": summarizationPrompt,
		"update":     updateSummarizationPrompt,
	} {
		if !strings.Contains(prompt, codeMapHeading) {
			t.Errorf("%s prompt is missing the %q section", name, codeMapHeading)
		}
	}
}

// TestPromptsKeepSectionOrder verifies the code map sits where a reader (and the
// Markdown renderer) expects it: after the decisions it explains and before the
// failures and the handoff sections. The checkpoint is rendered as Markdown in
// every front-end, so an out-of-order heading is a visible inconsistency, not
// just a prompt detail.
func TestPromptsKeepSectionOrder(t *testing.T) {
	for name, prompt := range map[string]string{
		"first-time": summarizationPrompt,
		"update":     updateSummarizationPrompt,
	} {
		mapIdx := strings.Index(prompt, codeMapHeading)
		decisions := strings.Index(prompt, "## Key Decisions")
		errorsIdx := strings.Index(prompt, "## Errors & Failed Attempts")
		if mapIdx < 0 || decisions < 0 || errorsIdx < 0 {
			t.Fatalf("%s prompt is missing a required section", name)
		}
		if !(decisions < mapIdx && mapIdx < errorsIdx) {
			t.Errorf("%s prompt section order is Key Decisions(%d) < Files & Code Map(%d) < Errors(%d)",
				name, decisions, mapIdx, errorsIdx)
		}
	}
}

// TestPromptsForbidReExploration verifies both templates carry the rule that
// actually changes behaviour: a discovered fact is the last thing to cut for
// length. Without it a summarizer treats the code map as optional detail and
// compresses it away, which is exactly the failure the section exists to prevent
// — the next turn then re-runs the same reads and greps.
func TestPromptsForbidReExploration(t *testing.T) {
	for name, prompt := range map[string]string{
		"first-time": summarizationPrompt,
		"update":     updateSummarizationPrompt,
	} {
		if !strings.Contains(prompt, "rediscover") {
			t.Errorf("%s prompt should say the next reader must not have to rediscover facts", name)
		}
	}
}
