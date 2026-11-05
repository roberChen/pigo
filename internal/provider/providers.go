// This file implements the first concrete Providers (US-013): Bedrock,
// OpenRouter, and Ollama. Each satisfies the Provider interface by building an
// *http.Request and delegating to the shared transport (StreamRequest) with a
// provider-appropriate Decoder — no bespoke HTTP/SSE handling per provider.
//
// Two backing driver shapes cover all three:
//
//   - openAICompatDriver — POSTs to {baseURL}/chat/completions in the OpenAI
//     Chat Completions wire format, decoded by OpenAIDecoder. OpenRouter and
//     Ollama are instances of it; it is the generic OpenAI-compatible layer,
//     reusable for any gateway (Groq, together, local servers, …).
//   - anthropicCompatDriver — POSTs the Anthropic Messages wire format, decoded
//     by AnthropicDecoder. Bedrock rides this: Anthropic-on-Bedrock speaks the
//     Messages API, so the decoder is reused wholesale.
//
// Failures follow the dual failure model (FR-13): only the earliest "cannot
// build the stream" case (missing key, bad request construction) is a returned
// error; every runtime failure rides the stream as a terminal error event,
// which StreamRequest already guarantees.
//
// Security (US-012 / US-026): API keys are referenced by provider name in any
// error; secret values are never logged or embedded in error text.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// providerBaseURLs holds the default endpoint per built-in provider. A base URL
// is a transport concern (the decoders are URL-agnostic), so overriding it —
// e.g. pointing Ollama at a remote host — needs no decoder change.
const (
	openRouterBaseURL = "https://openrouter.ai/api/v1"
	ollamaBaseURL     = "http://localhost:11434/v1"
	// nvidiaBaseURL is NVIDIA's hosted NIM endpoint. It speaks the OpenAI
	// Chat Completions wire format, so it rides openAICompatDriver unchanged.
	nvidiaBaseURL = "https://integrate.api.nvidia.com/v1"
	// bedrockBaseURL is a placeholder default; real Bedrock endpoints are
	// region-specific (bedrock-runtime.<region>.amazonaws.com) and supplied at
	// construction. It is exported as a field so callers set the resolved URL.
	bedrockBaseURL = "https://bedrock-runtime.us-east-1.amazonaws.com"
	// anthropicBaseURL is the public Anthropic Messages API endpoint, the default
	// for a --protocol=anthropic provider when no --base-url is given.
	anthropicBaseURL = "https://api.anthropic.com/v1"
	// anthropicAPIVersion is the required anthropic-version header value sent with
	// every direct-Anthropic request.
	anthropicAPIVersion = "2023-06-01"
)

// ---------------------------------------------------------------------------
// OpenAI-compatible driver (OpenRouter, Ollama, and any OpenAI-compatible API).
// ---------------------------------------------------------------------------

// openAICompatDriver is the shared backing for every OpenAI-compatible
// provider. It holds the provider identity, endpoint, model catalog, and the
// auth scheme; StreamCompletion builds the chat-completions request and hands
// it to the transport with a fresh OpenAIDecoder.
type openAICompatDriver struct {
	name    string
	baseURL string
	models  []Model
	// requiresAuth reports whether an Authorization: Bearer header is sent.
	// Ollama (local) needs none; OpenRouter does.
	requiresAuth bool
	// extraHeaders are attached to every request (e.g. OpenRouter attribution).
	extraHeaders map[string]string
}

func (d *openAICompatDriver) Name() string    { return d.name }
func (d *openAICompatDriver) Models() []Model { return d.models }

// StreamCompletion builds the OpenAI Chat Completions request and streams it.
func (d *openAICompatDriver) StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error) {
	if d.requiresAuth && strings.TrimSpace(req.Config.APIKey) == "" {
		// Early "cannot build the stream": reference the provider, never a value.
		return nil, fmt.Errorf("%s: missing API key (set %s)", d.name, APIKeyEnvHint(d.name))
	}
	if err := checkImageSupport(d.name, req.Model, d.models, req.Context.Messages); err != nil {
		return nil, err
	}
	body, err := encodeOpenAIRequest(req)
	if err != nil {
		return nil, fmt.Errorf("%s: build request body: %w", d.name, err)
	}
	newReq := func(ctx context.Context) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if d.requiresAuth {
			httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)
		}
		for k, v := range d.extraHeaders {
			httpReq.Header.Set(k, v)
		}
		return httpReq, nil
	}
	return StreamRequest(ctx, TransportConfig{NewRequest: newReq, Decoder: NewOpenAIDecoder()})
}

// encodeOpenAIRequest serializes a CompletionRequest into an OpenAI Chat
// Completions JSON body with streaming enabled and usage requested.
func encodeOpenAIRequest(req CompletionRequest) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(req.Context.Messages)+1)
	if sp := req.Context.SystemPrompt; sp != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sp})
	}
	msgs = appendOpenAIHistory(msgs, repairToolCallHistory(req.Context.Messages))
	body := map[string]any{
		"model":          req.Model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	// Reasoning effort: when a thinking level is requested, forward it as the
	// OpenAI `reasoning_effort` field. Reasoning models (o-series, DeepSeek-R1,
	// GLM-thinking, …) read this to open their reasoning channel; omitting it
	// leaves them at their default and effectively disables extended reasoning.
	if effort := openAIReasoningEffort(req.Config.ThinkingLevel); effort != "" {
		body["reasoning_effort"] = effort
	}
	if tools := encodeOpenAITools(req.Context.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	return json.Marshal(body)
}

// appendOpenAIHistory encodes a history onto the Chat Completions wire, keeping
// the tool messages of one batch contiguous.
//
// A tool result can expand into two wire entries: the role:"tool" message and a
// follow-up user message carrying images the tool returned (see
// encodeOpenAIMessage). Emitting the follow-up immediately would place it
// *between* the tool messages of a parallel batch, and this wire pairs results
// positionally — an assistant tool_calls turn must be followed directly by one
// tool message per call. The calls after the image would then be reported as
// having no output ("No tool output found for tool call …") and the whole
// request would fail, permanently, because replays hit the same ordering.
//
// The carry-ins are therefore deferred until the batch ends. A user message
// after the last tool message is still valid; one in the middle is not.
func appendOpenAIHistory(msgs []map[string]any, history agentcore.MessageList) []map[string]any {
	var carry []map[string]any
	flush := func() {
		if len(carry) > 0 {
			msgs = append(msgs, carry...)
			carry = nil
		}
	}
	for _, m := range history {
		// Every tool result keeps its tool entry in the batch, whether or not it
		// also produces a carry-in; only a non-tool message ends the batch.
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			parts := encodeOpenAIMessage(tr)
			msgs = append(msgs, parts[0])
			carry = append(carry, parts[1:]...)
			continue
		}
		flush()
		msgs = append(msgs, encodeOpenAIMessage(m)...)
	}
	flush()
	return msgs
}

// openAIReasoningEffort maps the unified ThinkingLevel onto the OpenAI
// `reasoning_effort` wire value. "off"/"" yields "" (field omitted, default
// behavior preserved). OpenAI accepts minimal|low|medium|high; xhigh maps to
// high (the strongest supported value).
func openAIReasoningEffort(level agentcore.ThinkingLevel) string {
	switch level {
	case agentcore.ThinkingMinimal:
		return "minimal"
	case agentcore.ThinkingLow:
		return "low"
	case agentcore.ThinkingMedium:
		return "medium"
	case agentcore.ThinkingHigh, agentcore.ThinkingXHigh, agentcore.ThinkingMax:
		return "high"
	default: // off or unset
		return ""
	}
}

// encodeOpenAIMessage maps one pigo message onto the OpenAI wire shape. An
// assistant message may expand to content + tool_calls in a single entry; a
// tool result becomes a role:"tool" entry keyed by tool_call_id.
func encodeOpenAIMessage(m agentcore.Message) []map[string]any {
	switch msg := m.(type) {
	case agentcore.UserMessage:
		return []map[string]any{{"role": "user", "content": openAIUserContent(msg.Content)}}
	case agentcore.CompactionMessage:
		// A compaction checkpoint stands in for compacted history as user text.
		u := msg.AsUserMessage()
		return []map[string]any{{"role": "user", "content": openAIUserContent(u.Content)}}
	case agentcore.AssistantMessage:
		entry := map[string]any{"role": "assistant"}
		text := agentcore.ContentToText(msg.Content)
		var toolCalls []map[string]any
		for _, c := range msg.Content {
			if tc, ok := c.(agentcore.ToolCallContent); ok {
				toolCalls = append(toolCalls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": replayToolArguments(tc.Arguments),
					},
				})
			}
		}
		if len(toolCalls) > 0 {
			entry["tool_calls"] = toolCalls
			// With tool calls present, send content as JSON null when there is no
			// accompanying text: an empty string trips strict gateways (e.g. vLLM)
			// that expect null | non-empty for an assistant tool-call turn.
			if text == "" {
				entry["content"] = nil
			} else {
				entry["content"] = text
			}
		} else {
			entry["content"] = text
		}
		return []map[string]any{entry}
	case agentcore.ToolResultMessage:
		imgs := imageOnlyList(msg.Content)
		text := agentcore.ContentToText(msg.Content)
		if text == "" && len(imgs) > 0 {
			// The tool returned a picture and nothing else. An empty tool message
			// reads as a missing output to some gateways, so leave the descriptor
			// that names where the picture came from.
			text = "Image output from " + displayToolName(msg.ToolName)
		}
		tool := map[string]any{
			"role":         "tool",
			"tool_call_id": msg.ToolCallID,
			"content":      text,
		}
		// The Chat Completions tool role carries text only, so an image a tool
		// returned (view_image, a read of a screenshot) would otherwise vanish
		// from the request and the model would answer blind. Carry the images in
		// a follow-up user message right after the tool result: the pairing with
		// tool_call_id stays intact and the image reaches the model as ordinary
		// multimodal input, which every VL gateway accepts.
		if len(imgs) > 0 {
			return []map[string]any{tool, {
				"role":    "user",
				"content": openAIUserContent(withImageNote(imgs, msg.ToolName)),
			}}
		}
		return []map[string]any{tool}
	default:
		return nil
	}
}

// openAIUserContent shapes a user content list for the OpenAI wire. When there
// are no images it collapses to a plain string (the common case, and what most
// OpenAI-compatible gateways expect). When images are present it emits the
// multimodal array form: text parts plus image_url parts carrying a base64 data
// URI (data:<mime>;base64,<data>).
func openAIUserContent(content agentcore.ContentList) any {
	hasImage := false
	for _, c := range content {
		if _, ok := c.(agentcore.ImageContent); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return agentcore.ContentToText(content)
	}
	parts := make([]map[string]any, 0, len(content))
	for _, c := range content {
		switch b := c.(type) {
		case agentcore.TextContent:
			if b.Text == "" {
				continue
			}
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case agentcore.ImageContent:
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": fmt.Sprintf("data:%s;base64,%s", b.MimeType, b.Data),
				},
			})
		}
	}
	return parts
}

// encodeOpenAITools maps AgentTools onto the OpenAI function-tool schema.
func encodeOpenAITools(tools []agentcore.AgentTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		params := json.RawMessage(t.Schema())
		if len(params) == 0 {
			params = json.RawMessage("{}")
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name(),
				"description": t.Description(),
				"parameters":  params,
			},
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Anthropic-compatible driver (Bedrock).
// ---------------------------------------------------------------------------

// anthropicCompatDriver backs Anthropic-wire providers that are not the direct
// Anthropic API — Bedrock being the case here. It POSTs the Messages wire
// format and decodes with AnthropicDecoder.
type anthropicCompatDriver struct {
	name    string
	baseURL string
	models  []Model
	// path is the endpoint path appended to baseURL (Bedrock's invoke path
	// embeds the model id, so it is derived per request).
	pathFor func(model string) string
	// authHeader sets provider auth on the request (never logs the value).
	authHeader func(req *http.Request, apiKey string)
}

func (d *anthropicCompatDriver) Name() string    { return d.name }
func (d *anthropicCompatDriver) Models() []Model { return d.models }

// StreamCompletion builds the Anthropic Messages request and streams it,
// decoding with AnthropicDecoder.
func (d *anthropicCompatDriver) StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error) {
	if strings.TrimSpace(req.Config.APIKey) == "" {
		return nil, fmt.Errorf("%s: missing API key (set %s)", d.name, APIKeyEnvHint(d.name))
	}
	if err := checkImageSupport(d.name, req.Model, d.models, req.Context.Messages); err != nil {
		return nil, err
	}
	body, err := encodeAnthropicRequest(req, d.models)
	if err != nil {
		return nil, fmt.Errorf("%s: build request body: %w", d.name, err)
	}
	path := "/messages"
	if d.pathFor != nil {
		path = d.pathFor(req.Model)
	}
	newReq := func(ctx context.Context) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if d.authHeader != nil {
			d.authHeader(httpReq, req.Config.APIKey)
		}
		return httpReq, nil
	}
	return StreamRequest(ctx, TransportConfig{NewRequest: newReq, Decoder: NewAnthropicDecoder()})
}

// encodeAnthropicRequest serializes a CompletionRequest into an Anthropic
// Messages JSON body with streaming enabled. The system prompt is a top-level
// field; tool results and tool calls follow the Messages content-block shape.
func encodeAnthropicRequest(req CompletionRequest, models []Model) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(req.Context.Messages))
	for _, m := range repairToolCallHistory(req.Context.Messages) {
		if enc := encodeAnthropicMessage(m); enc != nil {
			msgs = append(msgs, enc)
		}
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}
	if sp := req.Context.SystemPrompt; sp != "" {
		body["system"] = sp
	}
	maxTok := maxOutputTokensFor(req)
	if maxTok <= 0 {
		// Anthropic requires max_tokens. Prefer the model's declared cap; fall
		// back to a coding-friendly default (4096 was too low and caused
		// truncation/retry loops on longer edits).
		maxTok = anthropicDefaultMaxTokens(req.Model, models)
	}
	// Extended thinking: when a thinking level is requested, enable the Anthropic
	// thinking block with a budget derived from the level. Omitted for off/unset
	// so non-thinking requests keep their prior shape.
	if budget := anthropicThinkingBudget(req.Config.ThinkingLevel); budget > 0 {
		// Anthropic counts thinking tokens toward max_tokens and requires
		// budget_tokens < max_tokens (else a 400). Guarantee headroom for the
		// visible reply by lifting max_tokens above the budget when the caller's
		// cap is too low to fit both the reasoning and a real answer.
		if minTok := budget + anthropicResponseHeadroom; maxTok < minTok {
			maxTok = minTok
		}
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	body["max_tokens"] = maxTok
	if tools := encodeAnthropicTools(req.Context.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	return json.Marshal(body)
}

// anthropicResponseHeadroom is the token margin reserved for the visible reply
// on top of the thinking budget, so max_tokens always exceeds budget_tokens (an
// Anthropic hard requirement) with room left for a real answer.
const anthropicResponseHeadroom = 4096

// anthropicDefaultMaxTokens picks the max_tokens fallback when no explicit hint
// is given: the model's declared MaxOutputTokens if present in the driver's
// model catalog, otherwise 8192 (a coding-friendly default that avoids
// premature truncation while staying within common model caps).
func anthropicDefaultMaxTokens(model string, models []Model) int {
	for _, m := range models {
		if m.ID == model && m.MaxOutputTokens > 0 {
			return m.MaxOutputTokens
		}
	}
	return 8192
}

// anthropicThinkingBudget maps a unified ThinkingLevel onto an Anthropic
// thinking budget_tokens value. off/"" yields 0 (thinking block omitted).
func anthropicThinkingBudget(level agentcore.ThinkingLevel) int {
	switch level {
	case agentcore.ThinkingMinimal:
		return 1024
	case agentcore.ThinkingLow:
		return 2048
	case agentcore.ThinkingMedium:
		return 8192
	case agentcore.ThinkingHigh:
		return 16384
	case agentcore.ThinkingXHigh:
		return 32768
	case agentcore.ThinkingMax:
		return 65536
	default: // off or unset
		return 0
	}
}

// encodeAnthropicMessage maps one pigo message onto the Anthropic Messages
// wire shape. Assistant tool calls become tool_use blocks; tool results become
// a user message carrying a tool_result block (Anthropic's convention).
func encodeAnthropicMessage(m agentcore.Message) map[string]any {
	switch msg := m.(type) {
	case agentcore.UserMessage:
		return map[string]any{"role": "user", "content": anthropicUserContent(msg.Content)}
	case agentcore.CompactionMessage:
		u := msg.AsUserMessage()
		return map[string]any{"role": "user", "content": anthropicUserContent(u.Content)}
	case agentcore.AssistantMessage:
		var blocks []map[string]any
		// Thinking blocks must precede tool_use in the same assistant turn:
		// Anthropic extended-thinking requires the prior thinking block (and its
		// signature) to be echoed back verbatim on tool-use turns, or the API
		// rejects/degrades the request. Emit them first.
		for _, c := range msg.Content {
			if t, ok := c.(agentcore.ThinkingContent); ok {
				if t.Redacted {
					blocks = append(blocks, map[string]any{
						"type": "redacted_thinking", "data": t.ThinkingSignature,
					})
					continue
				}
				if t.Thinking == "" {
					continue
				}
				block := map[string]any{"type": "thinking", "thinking": t.Thinking}
				if t.ThinkingSignature != "" {
					block["signature"] = t.ThinkingSignature
				}
				blocks = append(blocks, block)
			}
		}
		for _, c := range msg.Content {
			switch b := c.(type) {
			case agentcore.TextContent:
				if b.Text == "" {
					continue
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
			case agentcore.ToolCallContent:
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": b.ID, "name": b.Name, "input": anthropicToolInput(b.Arguments),
				})
			}
		}
		if len(blocks) == 0 {
			// No usable content: emit a single space rather than an empty text
			// block ("text must be non-empty" is rejected by strict endpoints).
			blocks = []map[string]any{{"type": "text", "text": " "}}
		}
		return map[string]any{"role": "assistant", "content": blocks}
	case agentcore.ToolResultMessage:
		return map[string]any{"role": "user", "content": []map[string]any{{
			"type":        "tool_result",
			"tool_use_id": msg.ToolCallID,
			"content":     anthropicToolResultContent(msg.Content),
		}}}
	default:
		return nil
	}
}

// anthropicToolInput decodes a tool call's arguments for the Messages wire. The
// API requires tool_use.input to be a JSON object, so arguments that do not
// decode to one — the malformed JSON a model can stream, which pigo keeps
// verbatim so the executor can report it to the model, or a non-object value —
// degrade to an empty object instead of null or a bogus shape. Echoing them
// as-is would make every later request in the conversation fail with a 400; the
// model has already been told the arguments did not parse.
// replayToolArguments returns the argument payload to send when re-playing a
// tool call the model produced earlier in the conversation.
//
// A ToolCallContent deliberately keeps malformed arguments verbatim (see
// agentcore.ToolCallContent.MarshalJSON): the executor reports "arguments are
// not valid JSON" and the model gets another turn to re-issue the call. That
// assistant message stays in history, and every later request re-plays it —
// upstreams validate replayed arguments (OpenAI requires arguments to parse as a
// JSON object, Anthropic requires a JSON object for tool_use.input) and answer a
// 400 for the whole request. One bad call would then poison the session: the
// recovery turn can never reach the model again.
//
// Unusable arguments are therefore replayed as "{}". The call still pairs with
// the tool result already recorded for it, so the conversation stays valid, and
// the in-memory message and the session file keep the original bytes for
// diagnostics. Well-formed objects are returned unchanged, byte for byte, so a
// gateway relying on a vendor extension in them sees what the model sent.
func replayToolArguments(args json.RawMessage) string {
	trimmed := bytes.TrimSpace(args)
	// A JSON object only. This rejects truncated/duplicated-key payloads, `null`,
	// arrays and bare scalars in one check; anything starting with '{' but
	// unparseable fails json.Valid.
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return "{}"
	}
	return string(trimmed)
}

func anthropicToolInput(args json.RawMessage) map[string]any {
	// Same sanitizing policy as the other wires (see replayToolArguments), then a
	// second decode into a map because this wire wants an object, not a string.
	// UseNumber keeps integers exact: decoding into any would turn a large number
	// into a float64 and replay `1e+06` where the model wrote `1000000`.
	dec := json.NewDecoder(bytes.NewReader([]byte(replayToolArguments(args))))
	dec.UseNumber()
	var input map[string]any
	if err := dec.Decode(&input); err != nil || input == nil {
		return map[string]any{}
	}
	return input
}

// anthropicUserContent shapes a user content list for the Anthropic Messages
// wire. When there are no images it collapses to a plain string. When images
// are present it emits the content-block array form: text blocks plus image
// blocks with a base64 source ({"type":"image","source":{"type":"base64",
// "media_type":<mime>,"data":<b64>}}).
func anthropicUserContent(content agentcore.ContentList) any {
	hasImage := false
	for _, c := range content {
		if _, ok := c.(agentcore.ImageContent); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return agentcore.ContentToText(content)
	}
	blocks := make([]map[string]any, 0, len(content))
	for _, c := range content {
		switch b := c.(type) {
		case agentcore.TextContent:
			if b.Text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case agentcore.ImageContent:
			blocks = append(blocks, anthropicImageBlock(b))
		}
	}
	return blocks
}

// anthropicImageBlock renders one image as the Messages API's base64 source
// block. Shared by user content and tool results, which accept the same shape.
func anthropicImageBlock(b agentcore.ImageContent) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": b.MimeType,
			"data":       b.Data,
		},
	}
}

// anthropicToolResultContent shapes a tool result's content for the Anthropic
// wire. Text-only results stay a plain string (the common case). A result that
// carries an image becomes a block array, which is how the Messages API accepts
// images inside a tool_result — without this the image was flattened away by
// ContentToText and a view_image call came back empty.
func anthropicToolResultContent(content agentcore.ContentList) any {
	hasImage := false
	for _, c := range content {
		if _, ok := c.(agentcore.ImageContent); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return agentcore.ContentToText(content)
	}
	blocks := make([]map[string]any, 0, len(content))
	for _, c := range content {
		switch b := c.(type) {
		case agentcore.TextContent:
			if b.Text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case agentcore.ImageContent:
			blocks = append(blocks, anthropicImageBlock(b))
		}
	}
	if len(blocks) == 0 {
		// A tool_result block must not be empty.
		blocks = []map[string]any{{"type": "text", "text": " "}}
	}
	return blocks
}

// imageOnlyList returns just the image blocks of content, or nil when it holds
// none. It is the "does this result carry a picture?" test shared by the wires.
func imageOnlyList(content agentcore.ContentList) agentcore.ContentList {
	var out agentcore.ContentList
	for _, c := range content {
		if img, ok := c.(agentcore.ImageContent); ok {
			out = append(out, img)
		}
	}
	return out
}

// withImageNote prepends a one-line text block naming the tool the images came
// from, so the follow-up user message is self-describing instead of a bare
// picture the model must guess the provenance of.
func withImageNote(imgs agentcore.ContentList, toolName string) agentcore.ContentList {
	out := make(agentcore.ContentList, 0, len(imgs)+1)
	out = append(out, agentcore.NewTextContent("Image output from "+displayToolName(toolName)+":"))
	return append(out, imgs...)
}

// displayToolName names the tool a picture came from in a sentence, falling back
// to "a tool" when the result carries no name.
func displayToolName(toolName string) string {
	if toolName == "" {
		return "a tool"
	}
	return toolName
}

// contextHasImage reports whether any message in the context carries an image
// content block, so a driver can reject image input on a non-multimodal model.
func contextHasImage(msgs []agentcore.Message) bool {
	for _, m := range msgs {
		var content agentcore.ContentList
		switch msg := m.(type) {
		case agentcore.UserMessage:
			content = msg.Content
		case agentcore.CompactionMessage:
			content = msg.AsUserMessage().Content
		case agentcore.ToolResultMessage:
			// A tool can return an image (view_image, a read of a screenshot).
			// Without this case the capability check saw no image and a text-only
			// model silently accepted a call whose whole point was the picture.
			content = msg.Content
		default:
			continue
		}
		for _, c := range content {
			if _, ok := c.(agentcore.ImageContent); ok {
				return true
			}
		}
	}
	return false
}

// checkImageSupport returns a clear error when the request carries image input
// but the named model (looked up in models) does not declare SupportsImages.
// A model absent from the catalog is treated permissively (unknown capability),
// deferring to the provider's own validation. This turns the silent drop of
// image blocks on a text-only model into an actionable message.
func checkImageSupport(providerName, model string, models []Model, msgs []agentcore.Message) error {
	if !contextHasImage(msgs) {
		return nil
	}
	for _, m := range models {
		if m.ID == model {
			if !m.SupportsImages {
				return fmt.Errorf("%s: model %q does not support image input", providerName, model)
			}
			return nil
		}
	}
	return nil
}

// encodeAnthropicTools maps AgentTools onto the Anthropic tool schema.
func encodeAnthropicTools(tools []agentcore.AgentTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := json.RawMessage(t.Schema())
		if len(schema) == 0 {
			schema = json.RawMessage("{}")
		}
		out = append(out, map[string]any{
			"name":         t.Name(),
			"description":  t.Description(),
			"input_schema": schema,
		})
	}
	return out
}

// maxOutputTokensFor pulls a max-output hint from the request Extra map, if any,
// so callers can bound Anthropic responses without a registry lookup here.
func maxOutputTokensFor(req CompletionRequest) int {
	if req.Config.Extra == nil {
		return 0
	}
	switch v := req.Config.Extra["max_tokens"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Constructors.
// ---------------------------------------------------------------------------

// openAICompatPreset captures the per-gateway differences among the
// OpenAI-compatible providers: everything the constructors used to repeat
// (provider name, default endpoint, whether auth is required, and any extra
// headers). Collapsing the near-identical constructors onto this table means
// "add a gateway" is a one-line entry plus a thin exported wrapper.
type openAICompatPreset struct {
	name         string
	defaultURL   string // "" ⇒ no default; baseURL must be supplied by the caller
	requiresAuth bool
	extraHeaders map[string]string
}

// newOpenAICompat builds an OpenAI-compatible driver from a preset, falling back
// to the preset's default endpoint when baseURL is empty.
func newOpenAICompat(p openAICompatPreset, baseURL string, models []Model) Provider {
	if baseURL == "" {
		baseURL = p.defaultURL
	}
	return &openAICompatDriver{
		name:         p.name,
		baseURL:      baseURL,
		models:       models,
		requiresAuth: p.requiresAuth,
		extraHeaders: p.extraHeaders,
	}
}

// NewOpenRouterProvider builds the OpenRouter provider — the reference
// OpenAI-compatible gateway. baseURL defaults to the public endpoint when empty.
func NewOpenRouterProvider(baseURL string, models []Model) Provider {
	return newOpenAICompat(openAICompatPreset{
		name:         "openrouter",
		defaultURL:   openRouterBaseURL,
		requiresAuth: true,
		extraHeaders: map[string]string{
			// OpenRouter attribution headers (optional but recommended).
			"HTTP-Referer": "https://github.com/smallnest/pigo",
			"X-Title":      "pigo",
		},
	}, baseURL, models)
}

// NewOllamaProvider builds the Ollama provider (local, OpenAI-compatible, no
// auth). baseURL defaults to the local daemon when empty.
func NewOllamaProvider(baseURL string, models []Model) Provider {
	return newOpenAICompat(openAICompatPreset{
		name:         "ollama",
		defaultURL:   ollamaBaseURL,
		requiresAuth: false,
	}, baseURL, models)
}

// NewNvidiaProvider builds the NVIDIA provider (hosted NIM, OpenAI-compatible,
// Bearer auth). baseURL defaults to the public integrate endpoint when empty.
// The API key is resolved by the "nvidia" provider name (NVIDIA_API_KEY);
// secret values are never logged.
func NewNvidiaProvider(baseURL string, models []Model) Provider {
	return newOpenAICompat(openAICompatPreset{
		name:         "nvidia",
		defaultURL:   nvidiaBaseURL,
		requiresAuth: true,
	}, baseURL, models)
}

// NewOpenAICompatibleProvider builds a generic OpenAI-compatible provider for an
// arbitrary gateway reached by baseURL (Bearer auth). It is the target of an
// explicit --protocol=openai selection: unlike the preset constructors it has no
// default endpoint (baseURL must be supplied) and carries the neutral provider
// name "openai", so an API key resolves from OPENAI_API_KEY (or the --api-key
// override bound to that name). Secret values are never logged.
func NewOpenAICompatibleProvider(baseURL string, models []Model) Provider {
	return newOpenAICompat(openAICompatPreset{
		name:         "openai",
		defaultURL:   "", // no default: caller must supply the endpoint
		requiresAuth: true,
	}, baseURL, models)
}

// newNamedOpenAICompatProvider builds the OpenAI-compatible driver under a
// specific provider name. A user-defined (or otherwise named) provider uses it so
// its error messages, including the "missing API key" hint, name its own
// credential variable instead of a generic OPENAI_API_KEY.
func newNamedOpenAICompatProvider(name, baseURL string, models []Model) Provider {
	return newOpenAICompat(openAICompatPreset{
		name:         name,
		defaultURL:   "", // caller must supply the endpoint
		requiresAuth: true,
	}, baseURL, models)
}

// newAnthropicCompat builds an Anthropic-Messages driver, falling back to
// defaultURL when baseURL is empty. It is the shared body of the two
// Anthropic-wire constructors, which differ only in name, default endpoint, and
// auth header.
func newAnthropicCompat(name, defaultURL, baseURL string, models []Model, authHeader func(*http.Request, string)) Provider {
	if baseURL == "" {
		baseURL = defaultURL
	}
	return &anthropicCompatDriver{
		name:       name,
		baseURL:    baseURL,
		models:     models,
		authHeader: authHeader,
	}
}

// anthropicAuthHeaderFor returns the auth-header setter for an Anthropic-Messages
// provider given its registry AuthScheme (spec.AuthScheme). The two shapes seen
// among anthropic-protocol providers are:
//
//   - AuthBearer      → Authorization: Bearer <key>. Used by anthropic-protocol
//     gateways that authenticate with a plain bearer token on their /anthropic
//     endpoint.
//   - AuthXAPIKey     → x-api-key: <key> plus the required anthropic-version
//     header. This is the direct-Anthropic convention and, per pi's behavior,
//     also what MiniMax (minimax / minimax-cn) uses on its /anthropic endpoint.
//
// Any other scheme (e.g. AuthAWS for Bedrock, AuthSpecial) falls back to the
// x-api-key convention so a generic anthropic-protocol provider does not crash;
// bespoke auth for those is layered by a later node. The returned func never
// logs the secret value.
func anthropicAuthHeaderFor(authScheme string) func(*http.Request, string) {
	if authScheme == AuthBearer {
		return func(req *http.Request, apiKey string) {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	return func(req *http.Request, apiKey string) {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
	}
}

// NewAnthropicProvider builds a provider that speaks the Anthropic Messages wire
// format directly (POST {baseURL}/messages), the target of an explicit
// --protocol=anthropic selection. baseURL defaults to the public Anthropic API
// when empty. Auth uses the Anthropic conventions: an x-api-key header plus the
// required anthropic-version header. The API key resolves by the "anthropic"
// provider name (ANTHROPIC_API_KEY / CLAUDE_API_KEY, or the --api-key override);
// secret values are never logged.
func NewAnthropicProvider(baseURL string, models []Model) Provider {
	return newAnthropicCompat("anthropic", anthropicBaseURL, baseURL, models,
		anthropicAuthHeaderFor(AuthXAPIKey))
}

// NewAnthropicProtocolProvider builds a named Anthropic-Messages provider whose
// auth header follows the given registry AuthScheme. It is the target of an
// explicit --provider selection for any anthropic-protocol built-in (anthropic,
// minimax, minimax-cn, and — routed generically for now — bedrock/
// cloudflare-ai-gateway): the driver identity is the provider's own name (so
// errors reference it), baseURL is the already-resolved endpoint (spec default
// or override), and authScheme selects the header shape (see
// anthropicAuthHeaderFor). Secret values are never logged.
func NewAnthropicProtocolProvider(name, baseURL, authScheme string, models []Model) Provider {
	return newAnthropicCompat(name, anthropicBaseURL, baseURL, models,
		anthropicAuthHeaderFor(authScheme))
}

// NewBedrockProvider builds the Bedrock provider, reusing the Anthropic Messages
// decoder (Anthropic-on-Bedrock speaks the Messages wire format). baseURL
// defaults to a us-east-1 runtime endpoint when empty; real deployments pass
// the region-specific URL. Auth is a Bearer token (Bedrock API keys); SigV4
// signing, when required, is layered by the caller's HTTP client.
func NewBedrockProvider(baseURL string, models []Model) Provider {
	return newAnthropicCompat("bedrock", bedrockBaseURL, baseURL, models,
		func(req *http.Request, apiKey string) {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		})
}
