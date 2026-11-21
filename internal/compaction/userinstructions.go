// This file implements the user-instruction section of a compaction checkpoint:
// extracting the human's own words from the window being summarized, and parsing
// the consolidated digest the summarization call is asked to produce.
//
// Why a separate section exists at all: the technical summary is a lossy,
// model-authored account of the work, and every re-compaction rewrites it from a
// thinner input. Across several compactions the original requirements drift out
// of it. The instruction section pins them down with two halves that age
// differently (see agentcore.UserInstructionBlock):
//
//   - the rolling digest is what the model produces in the same summarization
//     call, seeded with the previous checkpoint's section, so it accumulates and
//     can only be replaced deliberately;
//   - the verbatim list is extracted here, by code, from the very window being
//     summarized — it never passes through the model, so "what the user actually
//     said most recently" is exact by construction.
//
// Extraction is deliberately conservative about what counts as a user
// instruction. A user-role message is not automatically something the human
// typed: background sub-agent reports, Stop-hook guidance and autonomous /goal
// nudges are injected by the harness (agentcore.UserMessage.IsSynthetic), and a
// skill selection arrives as the skill's whole SKILL.md body prepended to the
// real prompt. Both are excluded, or the ledger would fill up with harness text
// and read as if the user had asked for it.
package compaction

import (
	"regexp"
	"strings"

	"github.com/roberChen/pigo/internal/agentcore"
)

const (
	// maxVerbatimItemChars caps one verbatim request. The section is re-sent on
	// every request for the rest of the session, so it is a permanent tax: a
	// pasted 500-line log must not become a thumb on the context meter.
	maxVerbatimItemChars = 1000
	// maxVerbatimSectionChars caps the whole verbatim list (roughly 1.5k tokens,
	// about 7% of the default 20k keep-recent budget). Over budget, the OLDEST
	// entries degrade first: they have already been absorbed by the rolling
	// digest, so their exact wording matters least.
	maxVerbatimSectionChars = 6000
	// verbatimDegradeChars is how much of an over-budget entry survives the first
	// degradation step (its opening line, which usually carries the ask).
	verbatimDegradeChars = 200
	// verbatimPerItemOverhead is the "- " plus newline a rendered entry costs.
	verbatimPerItemOverhead = 2
)

// skillBlockRE matches a <skill …>…</skill> block: the expansion the front-ends
// prepend when the user picks skills for a message. Case-insensitive and
// non-greedy so several blocks in one message do not collapse into one match.
var skillBlockRE = regexp.MustCompile(`(?is)<skill\b[^>]*>.*?</skill>`)

// skillsLeadIn is the sentence prepended alongside skill blocks (see the TUI's
// composeWithSkills and the REST of the /skill path). It is harness framing, not
// the user's words, so it is removed with the blocks.
const skillsLeadIn = "The user selected the following skills for this message. Follow their instructions."

// instructionsOpen and instructionsClose bracket the digest the summarization
// call is asked to emit, inside an otherwise Markdown response. Tags rather than
// a heading on purpose: the summary template is Markdown with headings of its
// own, and a heading would need guessing which of two "## …" blocks is which.
const (
	instructionsOpen  = "<user-instructions>"
	instructionsClose = "</user-instructions>"
)

// stripSkillExpansion removes the skill framing from a user turn, returning only
// what the human typed. A message that is nothing but skill instructions yields
// "" and is therefore skipped.
func stripSkillExpansion(text string) string {
	text = skillBlockRE.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, skillsLeadIn, "")
	return strings.TrimSpace(text)
}

// normalizeInstruction collapses runs of whitespace (including newlines) into a
// single space. The rendered list is a Markdown bullet per request, so a multi-
// line body would break the list; only the layout is normalized, the words are
// the user's own.
func normalizeInstruction(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// truncateInstruction caps one request at maxVerbatimItemChars, marking the cut
// so a reader (and the model) knows the wording continues.
func truncateInstruction(text string) string {
	r := []rune(text)
	if len(r) <= maxVerbatimItemChars {
		return text
	}
	return strings.TrimSpace(string(r[:maxVerbatimItemChars])) + " …[truncated]"
}

// ExtractUserInstructions returns the human's own requests in msgs, in
// chronological order, already normalized, deduped against the immediately
// preceding entry and trimmed to budget. Harness-injected turns are skipped.
//
// It is the verbatim half of the instruction section, and the reason that half
// can be trusted: no model touches it.
func ExtractUserInstructions(msgs []agentcore.Message) []string {
	var out []string
	for _, m := range msgs {
		user, ok := m.(agentcore.UserMessage)
		if !ok || user.IsSynthetic() {
			continue
		}
		text := normalizeInstruction(stripSkillExpansion(agentcore.ContentToText(user.Content)))
		if text == "" {
			continue
		}
		text = truncateInstruction(text)
		if n := len(out); n > 0 && out[n-1] == text {
			continue // a resubmitted prompt is one instruction, not two
		}
		out = append(out, text)
	}
	return applyVerbatimBudget(out)
}

// SectionChars reports the rendered cost of a verbatim list, so callers (and the
// token estimator) agree on what the section weighs.
func SectionChars(items []string) int {
	total := 0
	for _, it := range items {
		total += len([]rune(it)) + verbatimPerItemOverhead
	}
	return total
}

// applyVerbatimBudget shrinks the list until it fits maxVerbatimSectionChars,
// degrading the oldest entries first. Each pass either replaces the oldest entry
// with a head-truncated form (which strictly shrinks it) or drops it, so the loop
// always terminates.
func applyVerbatimBudget(items []string) []string {
	for SectionChars(items) > maxVerbatimSectionChars && len(items) > 0 {
		oldest := items[0]
		degraded := degradeInstruction(oldest)
		if degraded == "" || len(degraded) >= len(oldest) {
			items = items[1:] // nothing left to shed: drop it
			continue
		}
		items[0] = degraded
	}
	return items
}

// degradeInstruction shortens one over-budget entry to its opening words. It
// returns "" when the entry is short enough that degrading would not help.
func degradeInstruction(text string) string {
	r := []rune(text)
	if len(r) <= verbatimDegradeChars {
		return ""
	}
	return strings.TrimSpace(string(r[:verbatimDegradeChars])) + " …"
}

// splitUserInstructions separates the summarization response into the technical
// summary and the consolidated instruction digest. found reports whether the
// model emitted the block at all.
//
// When the block is missing (a model that ignored the format, or an aborted
// response) the whole text is kept as the technical summary and the digest is
// empty: the checkpoint still lands, only its rolling half does not advance. The
// caller pairs that with a diagnostic; it must never fail the compaction, because
// losing the summary would cost the model far more than a thin ledger.
func splitUserInstructions(raw string) (summary, instructions string, found bool) {
	open := strings.Index(raw, instructionsOpen)
	if open < 0 {
		return strings.TrimSpace(raw), "", false
	}
	rest := raw[open+len(instructionsOpen):]
	closeIdx := strings.Index(rest, instructionsClose)
	if closeIdx < 0 {
		return strings.TrimSpace(raw), "", false
	}
	instructions = strings.TrimSpace(rest[:closeIdx])
	summary = strings.TrimSpace(raw[:open] + "\n" + rest[closeIdx+len(instructionsClose):])
	return summary, instructions, true
}

// PromptInstructionsAddendum is appended to both summarization templates. It asks
// for the digest in the SAME call that writes the technical summary — the section
// must not cost a second round trip, and it must be written with the conversation
// already in the model's context.
const PromptInstructionsAddendum = `
Finally, after every section above, emit exactly one block in this shape:

` + instructionsOpen + `
[A consolidated statement of what the user has asked for: goals, constraints, preferences, prohibitions, output-format demands, and anything the user said NOT to do. Merge everything in <previous-user-instructions> (when present) with the user messages above into a single list. Preserve file paths, directory restrictions, and explicit "do not" rules. Keep every requirement that is still in force; drop only one the user explicitly retracted or that is clearly obsolete. Leave the block empty when there are no user instructions at all.]
` + instructionsClose + `

This block is read by the harness, so it must be the only such block and must come last. State standing requirements as requirements — do NOT quote individual messages verbatim, because the harness attaches the user's own words itself.`

// renderPreviousInstructions renders the previous checkpoint's section for the
// summarization prompt, or "" when there is none. It reuses the same Markdown
// shape the section has in a checkpoint, so the model sees its own prior output
// in the format it produced.
func renderPreviousInstructions(prev *agentcore.UserInstructionBlock) string {
	if prev.IsEmpty() {
		return ""
	}
	return prev.Render()
}
