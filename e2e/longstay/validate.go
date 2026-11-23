// This file implements the mock LLM's request validator: every Chat Completions
// request the host sends is checked against the wire contract before a reply is
// produced. A violation is evidence of a real defect (a malformed history, a
// broken tool-call pairing, a leaked internal field), so it is recorded as a
// protocol error and answered with HTTP 400 rather than being papered over.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// toolNamePattern is the charset both OpenAI and Anthropic accept for a tool name
// or tool-call id. pigo's replay repair enforces it, so a violation here means the
// request would be rejected by a real provider too.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// allowedTopLevelFields is the set of request fields the validator tolerates
// beyond the ones it requires. Anything outside it is flagged: an unknown field is
// either a typo or a leak of internal state onto the wire, and both are defects.
var allowedTopLevelFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"reasoning_effort": true, "tools": true,
	"temperature": true, "top_p": true, "stop": true, "seed": true, "user": true,
	"max_tokens": true, "max_completion_tokens": true, "tool_choice": true,
	"parallel_tool_calls": true, "response_format": true, "n": true,
	"presence_penalty": true, "frequency_penalty": true,
}

// allowedMessageFields is the per-role field set the OpenAI wire defines. Any
// other field on a message is a leak of a struct tag that no provider accepts.
var allowedMessageFields = map[string]map[string]bool{
	"system":    {"role": true, "content": true, "name": true},
	"user":      {"role": true, "content": true, "name": true},
	"assistant": {"role": true, "content": true, "tool_calls": true, "name": true, "refusal": true},
	"tool":      {"role": true, "content": true, "tool_call_id": true},
}

// validateOptions describes what a healthy request for this harness looks like.
type validateOptions struct {
	// Models is the set of model ids a request may name: the boot model plus
	// every model the rotation can switch a session onto. Accepting the whole
	// rotation is the point — a session that switches models must actually put
	// the new id on the wire, and an id outside the set is a defect.
	Models []string
	// APIKey is the key the host was started with; empty skips the auth check.
	APIKey string
	// Tools records whether the host advertised a tool set. When false, no
	// assistant message may carry tool_calls: the model could not have called one.
	Tools bool
}

// wireMessage is the decoded form of one chat message. Content is kept raw
// because the wire allows a string, null or an array of parts.
type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []wireToolCall  `json:"tool_calls"`
}

// wireToolCall is one assistant tool call.
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// validateCompletionRequest checks headers and body. It returns a descriptive
// error naming the first violation; nil means the request is well formed.
func validateCompletionRequest(r *http.Request, body []byte, opts validateOptions) error {
	if err := validateHeaders(r, opts); err != nil {
		return err
	}
	return validateBody(body, opts)
}

// validateHeaders checks the metadata the transport must send for a streaming
// JSON request.
func validateHeaders(r *http.Request, opts validateOptions) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return fmt.Errorf("content-type %q is not application/json", ct)
	}
	if acc := r.Header.Get("Accept"); acc != "" && !strings.Contains(acc, "text/event-stream") {
		return fmt.Errorf("accept %q does not allow text/event-stream", acc)
	}
	if opts.APIKey != "" {
		if got, want := r.Header.Get("Authorization"), "Bearer "+opts.APIKey; got != want {
			// The key itself is never echoed: only the scheme and whether a key was
			// present, so a failure cannot leak a credential into an artifact.
			scheme := "missing"
			if strings.HasPrefix(got, "Bearer ") {
				scheme = "bearer"
			}
			return fmt.Errorf("authorization %s, want bearer with the configured key", scheme)
		}
	}
	return nil
}

// validateBody checks the JSON shape and every cross-message invariant pigo's
// encoder is supposed to uphold.
func validateBody(body []byte, opts validateOptions) error {
	if !json.Valid(body) {
		return errors.New("body is not valid JSON")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("body is not a JSON object: %w", err)
	}
	for key := range top {
		if !allowedTopLevelFields[key] {
			return fmt.Errorf("unknown request field %q", key)
		}
	}
	var model string
	if err := json.Unmarshal(top["model"], &model); err != nil || model == "" {
		return errors.New("model is missing or not a string")
	}
	if len(opts.Models) > 0 && !slices.Contains(opts.Models, model) {
		return fmt.Errorf("model %q is not part of the long-stay rotation (%s)", model, strings.Join(opts.Models, ", "))
	}
	var stream bool
	if err := json.Unmarshal(top["stream"], &stream); err != nil || !stream {
		return errors.New("stream must be true")
	}
	var streamOpts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := json.Unmarshal(top["stream_options"], &streamOpts); err != nil || !streamOpts.IncludeUsage {
		return errors.New("stream_options.include_usage must be true")
	}
	if raw, ok := top["reasoning_effort"]; ok {
		var effort string
		if err := json.Unmarshal(raw, &effort); err != nil {
			return errors.New("reasoning_effort is not a string")
		}
		switch effort {
		case "minimal", "low", "medium", "high":
		default:
			return fmt.Errorf("reasoning_effort %q is not a supported level", effort)
		}
	}

	advertised, err := validateToolSet(top["tools"])
	if err != nil {
		return err
	}
	if !opts.Tools && len(advertised) > 0 {
		return errors.New("tools were advertised for a host started with tools disabled")
	}

	var rawMessages []json.RawMessage
	if err := json.Unmarshal(top["messages"], &rawMessages); err != nil {
		return fmt.Errorf("messages is not an array: %w", err)
	}
	if len(rawMessages) == 0 {
		return errors.New("messages is empty")
	}
	messages := make([]wireMessage, len(rawMessages))
	for i, raw := range rawMessages {
		var msg wireMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return fmt.Errorf("message %d is not an object: %w", i, err)
		}
		messages[i] = msg
	}
	return validateMessages(rawMessages, messages, advertised, opts.Tools)
}

// validateToolSet checks the advertised function tools and returns their names.
func validateToolSet(raw json.RawMessage) (map[string]bool, error) {
	names := make(map[string]bool)
	if len(raw) == 0 {
		return names, nil
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("tools is not an array of function tools: %w", err)
	}
	for _, tool := range tools {
		if tool.Type != "function" {
			return nil, fmt.Errorf("tool type %q is not function", tool.Type)
		}
		if !toolNamePattern.MatchString(tool.Function.Name) {
			return nil, fmt.Errorf("advertised tool name %q is not a valid tool name", tool.Function.Name)
		}
		if names[tool.Function.Name] {
			return nil, fmt.Errorf("tool %q is advertised twice", tool.Function.Name)
		}
		if !isJSONObject(tool.Function.Parameters) {
			return nil, fmt.Errorf("tool %q parameters are not a JSON Schema object", tool.Function.Name)
		}
		names[tool.Function.Name] = true
	}
	return names, nil
}

// validateMessages walks the conversation enforcing role shapes, call/result
// pairing, batch contiguity and the "a request never ends with an assistant
// message" rule every completion endpoint assumes.
func validateMessages(raw []json.RawMessage, messages []wireMessage, advertised map[string]bool, toolsEnabled bool) error {
	// seenIDs holds the ids of *assistant* tool calls. It is deliberately not
	// shared with the tool-result bookkeeping: the wire requires a tool message
	// to repeat its call's id, so comparing both against one set would flag every
	// correct answer as a duplicate.
	seenIDs := make(map[string]bool)
	// pending holds the call ids of the assistant message currently being
	// answered; answered counts how many of them already have a tool result.
	var pending []string
	answered := 0

	for i, msg := range messages {
		allowed, ok := allowedMessageFields[msg.Role]
		if !ok {
			return fmt.Errorf("message %d: unknown role %q", i, msg.Role)
		}
		if i == 0 && msg.Role != "system" && msg.Role != "user" {
			return fmt.Errorf("message %d: the conversation must start with a system or user message, got %q", i, msg.Role)
		}
		if msg.Role == "system" && i != 0 {
			return fmt.Errorf("message %d: a system message must be first", i)
		}
		if err := rejectUnknownMessageFields(i, raw[i], allowed); err != nil {
			return err
		}

		if msg.Role == "tool" {
			if len(pending) == 0 {
				return fmt.Errorf("message %d: tool result without a preceding tool call", i)
			}
			if msg.ToolCallID == "" {
				return fmt.Errorf("message %d: tool_call_id is required", i)
			}
			if !slices.Contains(pending, msg.ToolCallID) {
				return fmt.Errorf("message %d: tool_call_id %q does not match an unanswered tool call", i, msg.ToolCallID)
			}
			// Answering a call twice means the history cannot be replayed: the
			// provider would attach two results to one call.
			pending = slices.DeleteFunc(pending, func(id string) bool { return id == msg.ToolCallID })
			answered++
			if err := validateTextContent(i, msg.Content, true); err != nil {
				return err
			}
			if len(pending) == 0 {
				answered = 0
			}
			continue
		}

		if len(pending) > 0 {
			// A batch must be answered completely before the conversation moves on.
			return fmt.Errorf("message %d: assistant tool calls were answered %d of %d times",
				i, answered, len(pending))
		}

		switch msg.Role {
		case "system", "user":
			if msg.ToolCalls != nil {
				return fmt.Errorf("message %d: a %s message cannot carry tool_calls", i, msg.Role)
			}
			if err := validateTextContent(i, msg.Content, false); err != nil {
				return err
			}
		case "assistant":
			if err := validateAssistant(i, msg, advertised, toolsEnabled, seenIDs); err != nil {
				return err
			}
			for _, call := range msg.ToolCalls {
				pending = append(pending, call.ID)
			}
			answered = 0
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("the last assistant message's tool calls were answered %d of %d times", answered, len(pending)+answered)
	}
	last := messages[len(messages)-1]
	if last.Role == "assistant" {
		return errors.New("the request ends with an assistant message")
	}
	if last.Role == "system" {
		return errors.New("the request ends with a system message")
	}
	return nil
}

// rejectUnknownMessageFields flags fields the wire does not define for a role, so
// an internal struct tag cannot silently leak into a request.
func rejectUnknownMessageFields(index int, raw json.RawMessage, allowed map[string]bool) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("message %d is not a JSON object: %w", index, err)
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("message %d: unknown field %q", index, key)
		}
	}
	return nil
}

// validateAssistant checks one assistant message: legal content shape, well-formed
// calls, and that every call names an advertised tool.
func validateAssistant(index int, msg wireMessage, advertised map[string]bool, toolsEnabled bool, seenIDs map[string]bool) error {
	if len(msg.ToolCalls) == 0 {
		return validateTextContent(index, msg.Content, false)
	}
	if !toolsEnabled {
		return fmt.Errorf("message %d: tool_calls present but the host advertises no tools", index)
	}
	// With tool calls present the content must be null or a non-empty string:
	// gateways read an empty string as a missing message.
	if !isJSONNull(msg.Content) {
		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			return fmt.Errorf("message %d: assistant content is neither null nor a string", index)
		}
		if text == "" {
			return fmt.Errorf("message %d: assistant content is an empty string beside tool_calls", index)
		}
	}
	for _, call := range msg.ToolCalls {
		if !toolNamePattern.MatchString(call.ID) {
			return fmt.Errorf("message %d: tool call id %q is not replayable", index, call.ID)
		}
		if seenIDs[call.ID] {
			return fmt.Errorf("message %d: tool call id %q is duplicated in the request", index, call.ID)
		}
		seenIDs[call.ID] = true
		if call.Type != "function" {
			return fmt.Errorf("message %d: tool call type %q is not function", index, call.Type)
		}
		if !toolNamePattern.MatchString(call.Function.Name) {
			return fmt.Errorf("message %d: tool name %q is not replayable", index, call.Function.Name)
		}
		if len(advertised) > 0 && !advertised[call.Function.Name] {
			return fmt.Errorf("message %d: tool %q is not advertised by this request", index, call.Function.Name)
		}
		if !isJSONObject([]byte(call.Function.Arguments)) {
			return fmt.Errorf("message %d: tool %q arguments are not a JSON object", index, call.Function.Name)
		}
	}
	return nil
}

// validateTextContent checks a content field: a string (allowed to be empty when
// emptyOK) or an array of legal parts (text and base64 image_url).
func validateTextContent(index int, raw json.RawMessage, emptyOK bool) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return fmt.Errorf("message %d: content is missing", index)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" && !emptyOK {
			return fmt.Errorf("message %d: content is an empty string", index)
		}
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("message %d: content is neither a string nor an array of parts", index)
	}
	if len(parts) == 0 {
		return fmt.Errorf("message %d: content is an empty array", index)
	}
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text == "" {
				return fmt.Errorf("message %d: empty text part", index)
			}
		case "image_url":
			if !strings.HasPrefix(part.ImageURL.URL, "data:image/") || !strings.Contains(part.ImageURL.URL, ";base64,") {
				return fmt.Errorf("message %d: image_url is not a base64 data URI", index)
			}
		default:
			return fmt.Errorf("message %d: unknown content part type %q", index, part.Type)
		}
	}
	return nil
}

// isJSONNull reports whether raw is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// isJSONObject reports whether raw is a JSON object literal.
func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
