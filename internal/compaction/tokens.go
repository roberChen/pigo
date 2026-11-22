// Package compaction implements context-window token accounting and the
// decision of when a long session must be compacted, mirroring pi's
// harness/compaction/compaction.ts.
//
// This file (US-001) covers the token side: estimating a message's token
// footprint from a character heuristic, deriving the current context-token
// usage (preferring provider-reported Usage over estimation), and the
// ShouldCompact threshold check. Cut-point finding (US-002) and summarization
// (US-003) live in sibling files.
package compaction

import (
	"encoding/json"

	"github.com/roberChen/pigo/internal/agentcore"
)

// CompactionSettings holds the thresholds and retention knobs for compaction,
// mirroring pi's CompactionSettings.
type CompactionSettings struct {
	// Enabled gates automatic compaction decisions.
	Enabled bool
	// ReserveTokens is reserved for the summary prompt and its output; the
	// effective usable window is contextWindow - ReserveTokens.
	ReserveTokens int
	// KeepRecentTokens is the approximate recent-context token budget to retain
	// after compaction (consumed by FindCutPoint in US-002).
	KeepRecentTokens int
}

// DefaultCompactionSettings matches pi's DEFAULT_COMPACTION_SETTINGS.
var DefaultCompactionSettings = CompactionSettings{
	Enabled:          true,
	ReserveTokens:    16384,
	KeepRecentTokens: 20000,
}

// estimatedImageChars is the fixed character budget attributed to an image
// block, matching pi's ESTIMATED_IMAGE_CHARS.
const estimatedImageChars = 4800

// charsPerToken is the conservative characters-per-token divisor pi uses.
const charsPerToken = 4

// ceilDiv returns ceil(a / b) for non-negative a and positive b.
func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// contentListChars sums the character footprint of a content list, counting
// text/thinking/toolCall blocks by their text length and each image block as a
// fixed estimatedImageChars, mirroring pi's estimateTextAndImageContentChars
// plus its assistant-block handling.
func contentListChars(content agentcore.ContentList) int {
	chars := 0
	for _, block := range content {
		switch c := block.(type) {
		case agentcore.TextContent:
			chars += len(c.Text)
		case agentcore.ThinkingContent:
			chars += len(c.Thinking)
		case agentcore.ToolCallContent:
			// name + serialized arguments, matching pi's toolCall accounting.
			chars += len(c.Name)
			if len(c.Arguments) > 0 {
				chars += len(c.Arguments)
			} else {
				// nil/empty RawMessage serializes to "null" downstream.
				b, _ := json.Marshal(json.RawMessage(c.Arguments))
				chars += len(b)
			}
		case agentcore.ImageContent:
			chars += estimatedImageChars
		}
	}
	return chars
}

// EstimateTokens returns a conservative token estimate for one message using
// the same character heuristic as pi's estimateTokens (ceil(chars / 4)).
func EstimateTokens(msg agentcore.Message) int {
	switch m := msg.(type) {
	case agentcore.UserMessage:
		return ceilDiv(contentListChars(m.Content), charsPerToken)
	case agentcore.AssistantMessage:
		return ceilDiv(contentListChars(m.Content), charsPerToken)
	case agentcore.ToolResultMessage:
		return ceilDiv(contentListChars(m.Content), charsPerToken)
	case agentcore.CompactionMessage:
		// A compaction checkpoint replays as its summary text plus the
		// user-instruction section; estimate the same rendering the encoder
		// sends so the section's permanent cost is counted, not discovered later.
		return ceilDiv(len(m.Text()), charsPerToken)
	default:
		return 0
	}
}

// calculateContextTokens derives total context tokens from a provider usage
// block. pigo's Usage only reports input/output, so we sum them (pi additionally
// folds cache read/write, which pigo does not track).
func calculateContextTokens(u agentcore.Usage) int {
	return u.InputTokens + u.OutputTokens
}

// assistantUsage returns a usable Usage from an assistant message, skipping
// aborted/error responses and zero-token usage, mirroring pi's getAssistantUsage.
func assistantUsage(msg agentcore.Message) (agentcore.Usage, bool) {
	a, ok := msg.(agentcore.AssistantMessage)
	if !ok || a.Usage == nil {
		return agentcore.Usage{}, false
	}
	if a.StopReason == agentcore.StopReasonAborted || a.StopReason == agentcore.StopReasonError {
		return agentcore.Usage{}, false
	}
	if calculateContextTokens(*a.Usage) <= 0 {
		return agentcore.Usage{}, false
	}
	return *a.Usage, true
}

// ContextUsageEstimate reports the derived context-token usage for a message
// list, mirroring pi's ContextUsageEstimate.
type ContextUsageEstimate struct {
	// Tokens is the estimated total context tokens.
	Tokens int
	// UsageTokens is the tokens reported by the most recent assistant usage block.
	UsageTokens int
	// TrailingTokens is the estimated tokens after that usage block.
	TrailingTokens int
	// LastUsageIndex is the index of the message that provided usage, or -1 when
	// none exists.
	LastUsageIndex int
}

// EstimateContextTokens computes context-token usage for messages, preferring
// the most recent valid assistant Usage block and estimating only the messages
// that follow it. When no usage is available it estimates every message. This
// mirrors pi's estimateContextTokens.
func EstimateContextTokens(msgs []agentcore.Message) ContextUsageEstimate {
	lastIdx := -1
	var lastUsage agentcore.Usage
	for i := len(msgs) - 1; i >= 0; i-- {
		if u, ok := assistantUsage(msgs[i]); ok {
			lastIdx = i
			lastUsage = u
			break
		}
	}

	if lastIdx < 0 {
		estimated := estimateAllTokens(msgs)
		return ContextUsageEstimate{
			Tokens:         estimated,
			UsageTokens:    0,
			TrailingTokens: estimated,
			LastUsageIndex: -1,
		}
	}

	usageTokens := calculateContextTokens(lastUsage)
	trailing := 0
	for i := lastIdx + 1; i < len(msgs); i++ {
		trailing += EstimateTokens(msgs[i])
	}
	return ContextUsageEstimate{
		Tokens:         usageTokens + trailing,
		UsageTokens:    usageTokens,
		TrailingTokens: trailing,
		LastUsageIndex: lastIdx,
	}
}

// estimateAllTokens sums the per-message estimates, paying no attention to any
// provider usage block. It is the "measure this list as it stands" reading, used
// when the usage blocks in the list cannot speak for the list's current shape.
func estimateAllTokens(msgs []agentcore.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m)
	}
	return total
}

// EstimatePostCompactionTokens measures a context that has just been rebuilt by
// a compaction (checkpoint + retained tail).
//
// It deliberately ignores every provider usage block. A usage block reports the
// size of the request that ended in that assistant message, and the retained tail
// was produced BEFORE the cut: the request it measured still contained the prefix
// this compaction just replaced with a summary. Preferring that block (what
// EstimateContextTokens does) therefore reports the pre-compaction size back —
// a 90k context that folds to 12k was announced as "90,502 → 90,500 tokens",
// which reads as if /compact did nothing.
//
// The estimate is the only honest reading available at this instant: the messages
// are exactly what the next request will carry, so summing them measures the
// rebuild. It is also self-correcting — the next turn's response reports real
// provider usage for a request that already contains the checkpoint, and
// EstimateContextTokens prefers that block again from then on.
func EstimatePostCompactionTokens(msgs []agentcore.Message) int {
	return estimateAllTokens(msgs)
}

// ShouldCompact reports whether context usage has exceeded the usable window,
// matching pi: contextTokens > contextWindow - reserveTokens. Disabled settings
// or a non-positive contextWindow (unknown) never trigger compaction.
func ShouldCompact(contextTokens, contextWindow int, settings CompactionSettings) bool {
	if !settings.Enabled {
		return false
	}
	if contextWindow <= 0 {
		return false
	}
	return contextTokens > contextWindow-settings.ReserveTokens
}
