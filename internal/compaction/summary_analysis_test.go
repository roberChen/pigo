// Tests for the summarization contract: what the templates ask the model for,
// and how the response is cleaned before it becomes the stored checkpoint.
//
// The <analysis> pass is the part with a parser behind it, so it gets the
// coverage: a tag the model writes differently case, extra prose around it, or an
// unterminated block must all land somewhere sane rather than dropping the
// summary.
package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/provider"
)

func TestStripAnalysisRemovesLeadingPass(t *testing.T) {
	raw := "<analysis>\nGoal is X. Tried Y, failed.\n</analysis>\n\n## Goal\nX"
	if got := stripAnalysis(raw); got != "## Goal\nX" {
		t.Errorf("stripAnalysis = %q, want the text after </analysis>", got)
	}
}

func TestStripAnalysisKeepsTextWithoutAPass(t *testing.T) {
	const raw = "## Goal\nX\n\n## Next Steps\n1. Y"
	if got := stripAnalysis(raw); got != raw {
		t.Errorf("stripAnalysis altered a summary with no analysis block: %q", got)
	}
}

func TestStripAnalysisIsCaseInsensitive(t *testing.T) {
	// Models are not consistent about tag case, and a leading pass left in the
	// checkpoint would be paid for on every later request.
	raw := "<Analysis>work</ANALYSIS>\n\n## Goal\nX"
	if got := stripAnalysis(raw); got != "## Goal\nX" {
		t.Errorf("stripAnalysis should match tags case-insensitively, got %q", got)
	}
}

func TestStripAnalysisKeepsSummaryMentioningTheTag(t *testing.T) {
	// A checkpoint may legitimately discuss the analysis block (this very change
	// does). Only the first, real pass is stripped; later text is left intact.
	raw := "<analysis>work</analysis>\n\n## Goal\nstrip the <analysis> block"
	if got := stripAnalysis(raw); got != "## Goal\nstrip the <analysis> block" {
		t.Errorf("stripAnalysis mangled the summary body: %q", got)
	}
}

func TestStripAnalysisKeepsUnterminatedBlock(t *testing.T) {
	// The model was cut off mid-pass: it never wrote the summary. Returning the
	// raw text lets the caller's own emptiness check see that, instead of handing
	// back an empty checkpoint that silently loses the conversation.
	raw := "<analysis>\nthe conversation was about X and then"
	if got := stripAnalysis(raw); got != raw {
		t.Errorf("stripAnalysis should keep an unterminated block: %q", got)
	}
}

func TestStripAnalysisKeepsTextWhenNothingFollows(t *testing.T) {
	// A pass with nothing after it is not a usable checkpoint either; keep the
	// raw text so the result is never empty.
	raw := "<analysis>work</analysis>\n\n"
	if got := stripAnalysis(raw); strings.TrimSpace(got) == "" {
		t.Errorf("stripAnalysis returned an empty checkpoint: %q", got)
	}
}

// TestStripAnalysisHandlesMultibyteBeforeTag pins the reason stripAnalysis scans
// the original string instead of a lowercased copy: lowering 'İ' (U+0130, two
// bytes) yields 'i' plus U+0307 (three bytes), so an offset taken from the lowered
// copy would slice the original one byte off.
func TestStripAnalysisHandlesMultibyteBeforeTag(t *testing.T) {
	raw := "İİİ <analysis>work</analysis>\n\n## Goal\nX"
	if got := stripAnalysis(raw); got != "## Goal\nX" {
		t.Errorf("stripAnalysis mis-sliced around multibyte text: %q", got)
	}
}

// TestPromptsRequestAnalysisPass verifies both templates ask for the pass that
// makes the model read the conversation before writing, and that the tags they
// ask for are the ones the parser looks for. The two are a contract: changing one
// without the other silently disables either the accuracy gain or the stripping.
func TestPromptsRequestAnalysisPass(t *testing.T) {
	for name, prompt := range map[string]string{
		"first-time": summarizationPrompt,
		"update":     updateSummarizationPrompt,
	} {
		if !strings.Contains(prompt, analysisOpen) || !strings.Contains(prompt, analysisClose) {
			t.Errorf("%s prompt must ask for an %s…%s pass", name, analysisOpen, analysisClose)
		}
	}
}

// TestPromptsForbidToolsAndContinuation verifies the system prompt keeps the two
// rules whose absence produces a wasted turn: continuing the conversation, and
// calling a tool the summarization request cannot serve.
func TestPromptsForbidToolsAndContinuation(t *testing.T) {
	for _, want := range []string{"Do NOT continue the conversation", "Do NOT call any tools"} {
		if !strings.Contains(SUMMARIZATION_SYSTEM_PROMPT, want) {
			t.Errorf("system prompt should contain %q", want)
		}
	}
}

// TestGenerateSummaryStripsAnalysis verifies the pass is removed on the real path,
// not just in the helper: the checkpoint that reaches the session file must not
// carry the model's working-out.
func TestGenerateSummaryStripsAnalysis(t *testing.T) {
	stream := fakeStreamFn(assistantText(
		"<analysis>\nwalked the messages\n</analysis>\n\n## Goal\nX"), nil)

	got, err := GenerateSummary(context.Background(), stream,
		provider.Model{ID: "m", MaxOutputTokens: 8000},
		[]agentcore.Message{userMsg("do X")}, 16384, "", provider.StreamConfig{})
	if err != nil {
		t.Fatalf("GenerateSummary: %v", err)
	}
	if strings.Contains(got, analysisOpen) || strings.Contains(got, "walked the messages") {
		t.Errorf("checkpoint still carries the analysis pass:\n%s", got)
	}
	if !strings.HasPrefix(got, "## Goal") {
		t.Errorf("checkpoint should start at the summary, got:\n%s", got)
	}
}
