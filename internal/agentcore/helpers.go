package agentcore

import (
	"context"
	"encoding/json"
	"strings"
)

// ContentToText flattens text blocks of a content list into a single string,
// the lowest-common-denominator representation accepted by every OpenAI-
// compatible gateway. Non-text blocks (thinking, tool calls) are surfaced
// through their own fields, so they are skipped here.
//
// The single-text-block case returns the block's string directly. That case is
// the streaming hot path — a decoder accumulates one text block and every
// MessageUpdateEvent re-flattens it — so copying the answer on each delta would
// make the accounting quadratic in the answer's length for no benefit. The
// caller only ever appends to or slices the result, so aliasing it is safe;
// callers that hand it out as a fresh string should copy (none do today).
func ContentToText(list ContentList) string {
	if len(list) == 1 {
		if tc, ok := list[0].(TextContent); ok {
			return tc.Text
		}
		return ""
	}
	// Most mixed lists still hold exactly one text block (text plus a tool call or
	// a thinking block), so this avoids the builder for them as well.
	var only string
	n := 0
	for _, c := range list {
		if tc, ok := c.(TextContent); ok {
			n++
			if n > 1 {
				break
			}
			only = tc.Text
		}
	}
	if n == 0 {
		return ""
	}
	if n == 1 {
		return only
	}
	var b strings.Builder
	for _, c := range list {
		if tc, ok := c.(TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// ThinkingText returns the concatenated thinking blocks of list. Reasoning models
// stream their chain of thought as ThinkingContent; this is how a front-end reads it
// without reaching into the block types.
func ThinkingText(list ContentList) string {
	var b strings.Builder
	for _, c := range list {
		if tc, ok := c.(ThinkingContent); ok {
			b.WriteString(tc.Thinking)
		}
	}
	return b.String()
}

// LastAssistantOf returns a pointer to the last AssistantMessage in msgs, or nil.
func LastAssistantOf(msgs []AgentMessage) *AssistantMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if a, ok := msgs[i].(AssistantMessage); ok {
			return &a
		}
	}
	return nil
}

// EmitFunc emits a loop-level AgentEvent honoring cancellation.
type EmitFunc func(ctx context.Context, ev AgentEvent) error

// PrepareArgumentsFunc optionally rewrites a tool's raw arguments before schema
// validation (e.g. injecting defaults). An error aborts the call with an error
// result. Optional (nil = identity).
type PrepareArgumentsFunc func(ctx context.Context, toolName string, args json.RawMessage) (json.RawMessage, error)

// BeforeToolCallDecision is the optional result of the beforeToolCall hook. When
// Block is true the tool is not executed and an error result is produced;
// Content/Details override the default block message when set. When Block is
// false and UpdatedInput is non-empty, it replaces the tool's raw arguments
// before execution (PreToolUse rewrite, FR-8); the replacement is re-validated
// against the tool schema.
type BeforeToolCallDecision struct {
	Block        bool
	Content      *ContentList
	Details      *any
	UpdatedInput json.RawMessage
}

// BeforeToolCallFunc runs after validation and may block the call (permission /
// sandbox checks, FR-4/FR-26). Returning nil allows the call. Optional.
type BeforeToolCallFunc func(ctx context.Context, call AgentToolCall) *BeforeToolCallDecision

// AfterToolCallFunc runs after execution and may override the result
// field-by-field via AfterToolCallResult (FR-5, no deep merge). Optional.
type AfterToolCallFunc func(ctx context.Context, call AgentToolCall, result AgentToolResult, isError bool) *AfterToolCallResult
