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
// mirroring pi's CompactionSettings and extending it with the per-model trigger
// controls of the configurable-threshold feature.
type CompactionSettings struct {
	// Enabled gates automatic compaction decisions.
	Enabled bool
	// ReserveTokens is reserved for the summary prompt and its output; the
	// effective usable budget is inputBudget - ReserveTokens.
	ReserveTokens int
	// KeepRecentTokens is the approximate recent-context token budget to retain
	// after compaction (consumed by FindCutPoint in US-002).
	KeepRecentTokens int
	// MaxInputTokens is the model's input ceiling in tokens, when the provider
	// publishes one that is smaller than the combined context window. A model
	// that advertises "max input + max output = 1M" (DeepSeek and several other
	// gateways) will reject a request long before the combined window is full,
	// so the trigger must be computed against this budget rather than the
	// window. 0 means "no separate ceiling: the window is the input budget".
	MaxInputTokens int
	// TriggerFraction is the highest configured threshold ladder entry (0.8 for
	// the default 40/60/80 ladder). The trigger is capped at
	// TriggerFraction × input budget, so a ladder entry below the reserve-based
	// line fires earlier. 0 disables the percentage cap.
	TriggerFraction float64
	// TriggerTokens is an absolute trigger point from compaction.max_context or a
	// per-model compact_at, already resolved to tokens for the model. When > 0 it
	// caps the trigger as well (the smallest of the three wins). 0 disables it.
	TriggerTokens int
}

// DefaultCompactionSettings matches pi's DEFAULT_COMPACTION_SETTINGS, plus the
// built-in 80% ladder ceiling: the trigger is the earlier of
// 80% × input budget and input budget − reserve.
var DefaultCompactionSettings = CompactionSettings{
	Enabled:          true,
	ReserveTokens:    16384,
	KeepRecentTokens: 20000,
	TriggerFraction:  0.80,
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

// InputBudget returns the token budget a request may actually carry for a model.
//
// contextWindow is the combined "max input + max output" figure a provider
// advertises. Several gateways (DeepSeek, MiniMax, some OpenRouter upstreams)
// also publish a smaller maximum *input*; a request that fills 80% of the
// combined window can still exceed that input ceiling and be rejected outright.
// When MaxInputTokens states that ceiling, it becomes the budget; otherwise the
// combined window is the budget (the pi behavior).
func InputBudget(contextWindow int, settings CompactionSettings) int {
	if contextWindow <= 0 {
		return 0
	}
	if settings.MaxInputTokens > 0 && settings.MaxInputTokens < contextWindow {
		return settings.MaxInputTokens
	}
	return contextWindow
}

// TriggerSource names the cap that decided where auto-compaction fires. A
// front-end shows it beside the number, because "compact at 102,400" is only
// actionable once the user knows it is the 80% ladder rather than anything they
// configured — that is the difference between "this is the default" and "my
// compact_at did not apply".
type TriggerSource string

const (
	// TriggerSourceReserve means budget − ReserveTokens decided (pi's rule).
	TriggerSourceReserve TriggerSource = "reserve"
	// TriggerSourceLadder means the top of the [checkpoint] thresholds ladder
	// decided: TriggerFraction × input budget.
	TriggerSourceLadder TriggerSource = "ladder"
	// TriggerSourceConfigured means an absolute cap the user set decided
	// ([compaction] max_context or a per-model compact_at).
	TriggerSourceConfigured TriggerSource = "configured"
	// TriggerSourceOff means there is no trigger: automatic compaction is
	// disabled, or the model's window is unknown.
	TriggerSourceOff TriggerSource = "off"
)

// TriggerDetail is where auto-compaction fires and which cap put it there. The
// numbers are the ones the loop uses — the input budget, not the combined window
// — so a UI that renders them cannot promise headroom the loop does not have.
type TriggerDetail struct {
	// Window is the advertised combined budget (input + output).
	Window int
	// Budget is the input budget the point is measured against (the input
	// ceiling when one is known, else Window).
	Budget int
	// Reserve is the headroom kept for the summary, in tokens.
	Reserve int
	// Fraction is the ladder ceiling that was in force (0 when none).
	Fraction float64
	// Point is the token count at which compaction fires; 0 means never.
	Point int
	// Source is the cap that decided Point.
	Source TriggerSource
}

// ExplainTrigger returns the trigger point and the rule that produced it. It is
// what TriggerPoint computes, plus the attribution a UI needs to explain the
// number instead of presenting it as arbitrary.
func ExplainTrigger(contextWindow int, settings CompactionSettings) TriggerDetail {
	d := TriggerDetail{
		Window:   contextWindow,
		Budget:   InputBudget(contextWindow, settings),
		Reserve:  settings.ReserveTokens,
		Fraction: settings.TriggerFraction,
		Source:   TriggerSourceOff,
	}
	if !settings.Enabled || d.Budget <= 0 {
		return d
	}
	point := d.Budget - d.Reserve
	source := TriggerSourceReserve
	if d.Fraction > 0 {
		if byFraction := int(float64(d.Budget)*d.Fraction + 0.5); byFraction < point {
			point, source = byFraction, TriggerSourceLadder
		}
	}
	if settings.TriggerTokens > 0 && settings.TriggerTokens < point {
		point, source = settings.TriggerTokens, TriggerSourceConfigured
	}
	if point < 0 {
		return d
	}
	d.Point = point
	d.Source = source
	return d
}

// TriggerPoint returns the context-token count at which automatic compaction
// fires for a given window and settings.
//
// Three caps compose, and the smallest wins — so any one of them can only fire
// earlier, never later than the model's real input ceiling:
//
//  1. budget − ReserveTokens (pi's original rule),
//  2. TriggerFraction × budget (the top of the checkpoint threshold ladder, 80%
//     by default, which leaves headroom for the reply),
//  3. TriggerTokens, an absolute cap the user configured.
//
// It returns 0 when the trigger is undefined (disabled settings or an unknown
// window), which ShouldCompact reads as "never compact". ExplainTrigger reports
// the same number plus which of the three produced it.
func TriggerPoint(contextWindow int, settings CompactionSettings) int {
	return ExplainTrigger(contextWindow, settings).Point
}

// ShouldCompact reports whether context usage has reached the trigger point. It
// is the fixed-policy decision; a run that installs a custom trigger (see
// TriggerFunc) consults that instead, falling back to this when it declines to
// decide.
//
// Disabled settings or a non-positive contextWindow (unknown) never trigger.
func ShouldCompact(contextTokens, contextWindow int, settings CompactionSettings) bool {
	trigger := TriggerPoint(contextWindow, settings)
	if trigger <= 0 {
		return false
	}
	return contextTokens > trigger
}
