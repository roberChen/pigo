// This file (US-003) ties the compaction pieces together: given a message list
// and settings, it finds the cut point, extracts file operations from the
// summarized range, generates the structured summary, and returns a
// CompactionResult ready to be persisted as a session compaction entry and used
// to rebuild the agent context. Mirrors pi's prepareCompaction + compact.
package compaction

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/provider"
)

// CompactionDetails records the files touched in the compacted history, stored
// alongside a compaction entry so a later iterative compaction can seed its
// file lists. Mirrors pi's CompactionDetails.
type CompactionDetails struct {
	// ReadFiles are files read but not modified in the compacted range.
	ReadFiles []string `json:"readFiles"`
	// ModifiedFiles are files written or edited in the compacted range.
	ModifiedFiles []string `json:"modifiedFiles"`
}

// CompactionResult is the outcome of a compaction: the summary text (with file
// metadata appended), the index where retained history begins, the estimated
// tokens before compaction, and the extracted file details. Mirrors pi's
// CompactionResult, adapted to pigo's flat message list (index vs entry id).
type CompactionResult struct {
	// Summary is the structured summary that replaces the compacted history.
	Summary string
	// UserInstructions is the user-instruction section of the checkpoint: the
	// rolling digest (consolidated by the summarization call, or carried over
	// from the previous checkpoint when the model omitted the block) plus the
	// verbatim requests extracted from the compacted window. Nil when the window
	// contained no user instructions and there was no prior digest.
	UserInstructions *agentcore.UserInstructionBlock
	// FirstKeptIndex is the index of the first retained message.
	FirstKeptIndex int
	// TokensBefore is the estimated context tokens before compaction.
	TokensBefore int
	// Details holds the file operations extracted from the compacted range.
	Details CompactionDetails
}

// Compact prepares and generates a compaction over msgs. It cuts at
// FindCutPoint(msgs, settings.KeepRecentTokens), summarizes messages in
// [prevCompactionIndex+1, firstKeptIndex) (seeding file ops from a prior
// compaction's details when provided), and appends <read-files>/<modified-files>
// metadata to the summary. previousSummary, when non-empty, switches to the
// iterative update template.
//
// prevCompactionIndex is the index of the last already-applied compaction point
// (-1 when none); summarization starts just after it so each compaction only
// covers the newly accumulated history. prevDetails carries that compaction's
// file lists so long-lived reads/edits survive across successive compactions.
// prevInstructions is that compaction's user-instruction section: its digest is
// fed back into the summarization prompt so the ledger accumulates, and the
// verbatim half is replaced (not appended to) with this window's requests.
//
// It returns (nil, nil) when there is nothing to compact (no valid cut point or
// an empty summarization range).
func Compact(
	ctx context.Context,
	stream provider.StreamFn,
	model provider.Model,
	msgs []agentcore.Message,
	settings CompactionSettings,
	prevCompactionIndex int,
	prevDetails *CompactionDetails,
	previousSummary string,
	prevInstructions *agentcore.UserInstructionBlock,
	cfg provider.StreamConfig,
) (*CompactionResult, error) {
	cut := FindCutPoint(msgs, settings.KeepRecentTokens)

	start := prevCompactionIndex + 1
	if start < 0 {
		start = 0
	}
	if start >= cut.FirstKeptIndex {
		// Nothing new to summarize.
		return nil, nil
	}
	toSummarize := msgs[start:cut.FirstKeptIndex]

	// Seed file ops from the previous compaction, then fold in this range.
	ops := NewFileOps()
	if prevDetails != nil {
		for _, f := range prevDetails.ReadFiles {
			ops.Read[f] = struct{}{}
		}
		for _, f := range prevDetails.ModifiedFiles {
			ops.Edited[f] = struct{}{}
		}
	}
	for _, m := range toSummarize {
		extractFileOpsFromMessage(m, ops)
	}
	readFiles, modifiedFiles := computeFileLists(ops)

	out, err := GenerateSummaryOutput(ctx, stream, model, toSummarize, settings.ReserveTokens, previousSummary, prevInstructions, cfg)
	if err != nil {
		return nil, err
	}
	summary := out.Summary + formatFileOperations(readFiles, modifiedFiles)

	return &CompactionResult{
		Summary:          summary,
		UserInstructions: buildUserInstructions(out, prevInstructions, toSummarize),
		FirstKeptIndex:   cut.FirstKeptIndex,
		TokensBefore:     EstimateContextTokens(msgs).Tokens,
		Details:          CompactionDetails{ReadFiles: readFiles, ModifiedFiles: modifiedFiles},
	}, nil
}

// Prior is the state a re-compaction continues from: the last checkpoint already
// present in the context, unpacked into the arguments Compact takes. The zero
// value (Index -1, everything else empty) means "no checkpoint yet", which is the
// first-compaction case.
type Prior struct {
	// Index is the position of the previous compaction checkpoint in the message
	// list, or -1 when there is none.
	Index int
	// Details holds that checkpoint's file lists (nil when absent or undecodable).
	Details *CompactionDetails
	// Summary is the previous technical summary, used to switch to the iterative
	// update template.
	Summary string
	// UserInstructions is the previous checkpoint's instruction section.
	UserInstructions *agentcore.UserInstructionBlock
}

// PriorCheckpoint finds the last compaction checkpoint in msgs and unpacks it for
// Compact. It scans from the end because a chain of checkpoints keeps only the
// newest one relevant (each absorbs the ones before it).
//
// Undecodable Details are dropped rather than reported: a lost file list costs
// far less than refusing to compact, and older checkpoints may simply not carry
// one.
func PriorCheckpoint(msgs []agentcore.Message) Prior {
	for i := len(msgs) - 1; i >= 0; i-- {
		cp, ok := msgs[i].(agentcore.CompactionMessage)
		if !ok {
			continue
		}
		prior := Prior{Index: i, Summary: cp.Summary, UserInstructions: cp.UserInstructions}
		if len(cp.Details) > 0 {
			var d CompactionDetails
			if err := json.Unmarshal(cp.Details, &d); err == nil {
				prior.Details = &d
			}
		}
		return prior
	}
	return Prior{Index: -1}
}

// buildUserInstructions assembles the checkpoint's instruction section from the
// two halves described in agentcore.UserInstructionBlock:
//
//   - digest: what the model consolidated this turn. When it emitted no block,
//     the previous digest is carried over verbatim rather than lost — a model
//     that drops the format on one turn must not erase the session's requirements.
//   - verbatim: this window's user requests, extracted by code. It replaces the
//     previous list wholesale; the previous list was already folded into the
//     digest, so keeping it would duplicate it on every re-compaction.
//
// It returns nil when both halves are empty, so an old-style summary-only
// checkpoint keeps its exact previous shape.
func buildUserInstructions(out *SummaryOutput, prev *agentcore.UserInstructionBlock, window []agentcore.Message) *agentcore.UserInstructionBlock {
	digest := strings.TrimSpace(out.Instructions)
	if !out.InstructionsFound && prev != nil {
		digest = strings.TrimSpace(prev.Summary)
	}
	verbatim := ExtractUserInstructions(window)
	block := &agentcore.UserInstructionBlock{Summary: digest, Verbatim: verbatim}
	if block.IsEmpty() {
		return nil
	}
	return block
}

// Message builds the CompactionMessage to persist for this result: the summary
// text plus the estimated tokens-before and the file details (as raw JSON).
func (r *CompactionResult) Message(now int64) agentcore.CompactionMessage {
	var details json.RawMessage
	if b, err := json.Marshal(r.Details); err == nil {
		details = b
	}
	return agentcore.CompactionMessage{
		RoleField:        agentcore.RoleCompaction,
		Summary:          r.Summary,
		UserInstructions: r.UserInstructions,
		TokensBefore:     r.TokensBefore,
		CoveredMessages:  r.FirstKeptIndex,
		Details:          details,
		Timestamp:        now,
	}
}

// Text renders the checkpoint exactly as the model will read it on later turns:
// the technical summary followed by the user-instruction section (see
// agentcore.CompactionMessage.Text). Front-ends report this on the CompactionEvent
// so an operator can see what replaced the summarized history, rather than having
// to reload the checkpoint from disk to find out.
func (r *CompactionResult) Text() string {
	return agentcore.CompactionMessage{Summary: r.Summary, UserInstructions: r.UserInstructions}.Text()
}

// RebuildContext returns the post-compaction message list: the compaction
// checkpoint followed by the retained recent messages (msgs[FirstKeptIndex:]).
// The summarized prefix is dropped and replaced by the single checkpoint,
// keeping context continuous while staying within the window. Mirrors pi's
// post-compact context reconstruction (summary entry + retained tail).
func (r *CompactionResult) RebuildContext(msgs []agentcore.Message, now int64) agentcore.MessageList {
	out := make(agentcore.MessageList, 0, len(msgs)-r.FirstKeptIndex+1)
	out = append(out, r.Message(now))
	out = append(out, msgs[r.FirstKeptIndex:]...)
	return out
}
