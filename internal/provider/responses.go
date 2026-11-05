// This file implements the OpenAI Responses API driver (US-003, #539;
// reworked in #546): the backing for --protocol openai/resp_api. It speaks the
// wire protocol directly (POST {base_url}/responses) instead of going through a
// vendor SDK.
//
// Why wire-level: the Responses API is served by OpenAI *and* by gateways that
// implement it with their own extensions (DeepSeek streams raw reasoning as
// response.reasoning_text.delta, which a pinned SDK's event union does not
// model). Routing every event through a hand-written decoder keeps the driver
// current with the vendors instead of with whichever SDK release is in go.mod,
// and it lets it share the transport (retries, idle/stall watchdogs, dual
// failure model) with the Chat Completions and Anthropic drivers.
//
// Only wire parsing lives here. Accumulation, content ordering, usage and
// stop-reason mapping come from the shared assistantTurn (turn.go), so the
// thinking block this driver emits is byte-identical in shape to the one the
// other two protocols emit.
//
// Failure model (FR-13): a missing API key or a non-2xx at connect time is a
// returned error (the "cannot build the stream" case, same as every other
// driver); every runtime failure after that rides the stream as a terminal
// StreamErrorEvent.
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

// responsesDriver is the Provider backing --protocol openai/resp_api. It holds
// the provider identity, the resolved endpoint, the model catalog, and whether
// an API key is required.
type responsesDriver struct {
	name    string
	baseURL string
	models  []Model
	// requiresAuth reports whether an API key must be present. Public OpenAI /
	// Azure require it; a local gateway may not.
	requiresAuth bool
	// httpClient overrides the HTTP client (tests inject a stub transport).
	httpClient *http.Client
	// extraHeaders are attached to every request.
	extraHeaders map[string]string
}

// NewOpenAIResponsesProvider builds a Responses API provider targeting baseURL.
// baseURL must be the fully resolved endpoint (e.g. https://api.openai.com/v1);
// the driver appends the /responses path.
func NewOpenAIResponsesProvider(name, baseURL string, models []Model) Provider {
	return newResponsesDriver(name, baseURL, models)
}

// newResponsesDriver builds the concrete driver; kept unexported so the
// constructor's Provider return type stays the public seam.
func newResponsesDriver(name, baseURL string, models []Model) *responsesDriver {
	return &responsesDriver{
		name:         name,
		baseURL:      baseURL,
		models:       models,
		requiresAuth: true,
	}
}

func (d *responsesDriver) Name() string    { return d.name }
func (d *responsesDriver) Models() []Model { return d.models }

// StreamCompletion issues a streaming Responses API call and surfaces the result
// on an AssistantMessageEventStream: a start event, incremental text/thinking/
// tool-call events as deltas arrive, and a terminal done event carrying the
// aggregated message.
func (d *responsesDriver) StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error) {
	if d.requiresAuth && strings.TrimSpace(req.Config.APIKey) == "" {
		// Early "cannot build the stream": reference the provider, never a value.
		return nil, fmt.Errorf("%s: missing API key (set %s)", d.name, APIKeyEnvHint(d.name))
	}
	if err := checkImageSupport(d.name, req.Model, d.models, req.Context.Messages); err != nil {
		return nil, err
	}
	body, err := encodeResponsesRequest(req)
	if err != nil {
		return nil, fmt.Errorf("%s: build request body: %w", d.name, err)
	}

	endpoint := strings.TrimSuffix(d.baseURL, "/") + "/responses"
	newReq := func(ctx context.Context) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if key := strings.TrimSpace(req.Config.APIKey); key != "" {
			httpReq.Header.Set("Authorization", "Bearer "+key)
		}
		for k, v := range d.extraHeaders {
			httpReq.Header.Set(k, v)
		}
		return httpReq, nil
	}
	return StreamRequest(ctx, TransportConfig{
		Client:     d.httpClient,
		NewRequest: newReq,
		Decoder:    NewResponsesDecoder(d.name),
	})
}

// ---------------------------------------------------------------------------
// Wire types (only the fields pigo consumes).
// ---------------------------------------------------------------------------

// responsesEvent is the SSE payload envelope shared by every event type. Every
// field is optional; the decoder switches on Type and reads what is present, so
// an event type introduced after this build is ignored rather than fatal.
type responsesEvent struct {
	Type string `json:"type"`
	// Delta carries text/reasoning/argument fragments (a string on the delta
	// events; ignored elsewhere).
	Delta string `json:"delta"`
	// Text is the full text on *.done events.
	Text string `json:"text"`
	// OutputIndex / ItemID identify the output item a delta belongs to.
	OutputIndex int64  `json:"output_index"`
	ItemID      string `json:"item_id"`
	// Item is present on output_item.added/done.
	Item *responsesItem `json:"item"`
	// Response is present on completed/incomplete/failed.
	Response *responsesResponse `json:"response"`
	// Message/Code describe an in-band error event.
	Message string `json:"message"`
	Code    string `json:"code"`
}

// responsesItem is one output item: a message, a reasoning block, or a
// function call.
type responsesItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// message
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// reasoning
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

// responsesResponse is the snapshot embedded in completed/incomplete/failed.
type responsesResponse struct {
	ID     string          `json:"id"`
	Model  string          `json:"model"`
	Status string          `json:"status"`
	Output []responsesItem `json:"output"`
	Usage  *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

// ResponsesDecoder is the stateful SSE decoder for the Responses API. It
// implements the transport Decoder interface and is not safe for concurrent
// use — the transport drives it from a single goroutine.
type ResponsesDecoder struct {
	turn *assistantTurn
	// toolByOutput routes function-call argument deltas (keyed by output_index)
	// onto the block opened when the item was announced.
	toolByOutput map[int64]*turnBlock
	started      bool
	done         bool
}

// NewResponsesDecoder builds a fresh decoder whose terminal message is tagged
// with the given provider name.
func NewResponsesDecoder(provider string) *ResponsesDecoder {
	return &ResponsesDecoder{
		turn:         newAssistantTurn("openai", provider),
		toolByOutput: make(map[int64]*turnBlock),
	}
}

// Decode turns one Responses SSE payload into zero or more StreamEvents.
func (d *ResponsesDecoder) Decode(payload []byte) ([]StreamEvent, error) {
	var ev responsesEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("openai responses: parse event: %w", err)
	}

	var events []StreamEvent
	// Every stream opens with a start event so the loop can back-fill the
	// partial before the first delta, whatever the vendor's first event is.
	if !d.started {
		d.started = true
		events = append(events, StreamStartEvent{Partial: d.turn.message()})
	}

	switch ev.Type {
	case "response.output_text.delta":
		d.turn.textDelta(ev.Delta)
		events = append(events, StreamTextEvent{Partial: d.turn.message()})

	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_summary.delta":
		// Raw reasoning (DeepSeek, other Responses implementations) and
		// reasoning summaries (OpenAI) are the same thing to pigo: the model's
		// thinking, rendered above the answer.
		d.turn.thinkingDeltaLeading(ev.Delta)
		events = append(events, StreamThinkingEvent{Partial: d.turn.message()})

	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		// The done event repeats the full text; ignore it rather than append a
		// duplicate. Reconcile below still fills in an item that streamed no
		// deltas at all.
		d.turn.endBlock()

	case "response.output_item.added":
		if ev.Item != nil {
			switch ev.Item.Type {
			case "function_call":
				block := d.turn.beginTool(ev.Item.CallID, ev.Item.Name)
				d.toolByOutput[ev.OutputIndex] = block
				events = append(events, StreamToolCallEvent{Partial: d.turn.message()})
			case "reasoning":
				// Just open the block; announcing it now would emit an empty
				// thinking partial that renders as a blank card. The first delta
				// (or the done item) announces it with content.
				d.turn.beginThinking()
			case "message":
				d.turn.endBlock()
			}
		}

	case "response.function_call_arguments.delta":
		block := d.toolBlock(ev.OutputIndex, ev.ItemID)
		block.args.WriteString(ev.Delta)
		events = append(events, StreamToolCallEvent{Partial: d.turn.message()})

	case "response.output_item.done":
		if ev.Item != nil {
			d.reconcileItem(ev.OutputIndex, ev.Item)
			// A finalized function_call is new content worth surfacing even when
			// the wire sent it as one done item with no argument deltas (several
			// Responses implementations do exactly that).
			if ev.Item.Type == "function_call" {
				events = append(events, StreamToolCallEvent{Partial: d.turn.message()})
			}
		}
		d.turn.endBlock()

	case "response.completed":
		d.reconcileResponse(ev.Response)
		d.done = true
		return append(events, StreamDoneEvent{Message: d.turn.doneMessage()}), nil

	case "response.incomplete":
		d.reconcileResponse(ev.Response)
		d.turn.setStopReason(agentcore.StopReasonLength)
		d.done = true
		return append(events, StreamDoneEvent{Message: d.turn.doneMessage()}), nil

	case "response.failed":
		return events, responsesFailure(ev.Response)

	case "error":
		msg := "responses stream error"
		if ev.Code != "" {
			msg = "responses " + ev.Code
		}
		if ev.Message != "" {
			msg += ": " + ev.Message
		}
		return events, fmt.Errorf("%s", msg)

	case "response.created", "response.in_progress", "response.queued",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.done", "response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done", "response.refusal.delta",
		"response.refusal.done", "response.output_text.annotation.added":
		// Metadata/lifecycle events: nothing to add to the message.
	default:
		// Unknown event types are ignored so a newer protocol revision cannot
		// break an older binary the way a pinned SDK union would.
	}
	return events, nil
}

// Finish flushes a terminal done event when the stream ended without an
// explicit response.completed (e.g. a clean EOF mid-stream), delivering the
// partial rather than losing it.
func (d *ResponsesDecoder) Finish() ([]StreamEvent, error) {
	if d.done {
		return nil, nil
	}
	d.done = true
	return []StreamEvent{StreamDoneEvent{Message: d.turn.doneMessage()}}, nil
}

// toolBlock returns the block for an output index, opening one when the wire
// sent arguments before the item announcement.
func (d *ResponsesDecoder) toolBlock(outputIndex int64, itemID string) *turnBlock {
	if block := d.toolByOutput[outputIndex]; block != nil {
		return block
	}
	block := d.turn.beginTool("", "")
	d.toolByOutput[outputIndex] = block
	return block
}

// reconcileItem fills in authoritative data from a finalized output item:
// function-call arguments (which may have streamed in fragments), and reasoning
// text for an implementation that only reports it on the item rather than as
// deltas.
func (d *ResponsesDecoder) reconcileItem(outputIndex int64, item *responsesItem) {
	switch item.Type {
	case "function_call":
		block := d.toolBlock(outputIndex, item.ID)
		if item.CallID != "" {
			block.toolID = item.CallID
		}
		if item.Name != "" {
			block.toolName = item.Name
		}
		if strings.TrimSpace(item.Arguments) != "" {
			block.args.Reset()
			block.args.WriteString(item.Arguments)
		}
	case "reasoning":
		if text := reasoningItemText(item); text != "" {
			d.fillMissingThinking(text)
		}
	}
}

// reconcileResponse adopts identity and usage from the terminal snapshot and
// fills in any content the stream never sent as deltas.
func (d *ResponsesDecoder) reconcileResponse(resp *responsesResponse) {
	if resp == nil {
		return
	}
	if resp.ID != "" {
		d.turn.responseID = resp.ID
	}
	if resp.Model != "" {
		d.turn.responseModel = resp.Model
	}
	if resp.Usage != nil {
		d.turn.usage(resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	if hasBlockKind(d.turn, turnBlockText) && hasBlockKind(d.turn, turnBlockThinking) {
		return
	}
	for i := range resp.Output {
		item := &resp.Output[i]
		switch item.Type {
		case "reasoning":
			if !hasBlockKind(d.turn, turnBlockThinking) {
				d.fillMissingThinking(reasoningItemText(item))
			}
		case "message":
			if !hasBlockKind(d.turn, turnBlockText) {
				if text := itemText(item); text != "" {
					d.turn.textDelta(text)
					d.turn.endBlock()
				}
			}
		}
	}
}

// fillMissingThinking records reasoning reported only on an item (no deltas).
func (d *ResponsesDecoder) fillMissingThinking(text string) {
	if text == "" || hasBlockKind(d.turn, turnBlockThinking) {
		return
	}
	d.turn.thinkingDeltaLeading(text)
	d.turn.endBlock()
}

// hasBlockKind reports whether the turn already holds a block of that kind.
func hasBlockKind(t *assistantTurn, kind string) bool {
	for _, b := range t.blocks {
		if b.kind == kind {
			return true
		}
	}
	return false
}

// reasoningItemText joins a reasoning item's summary parts, falling back to its
// raw reasoning content parts (DeepSeek reports the chain of thought there and
// leaves summary empty).
func reasoningItemText(item *responsesItem) string {
	var b strings.Builder
	for _, s := range item.Summary {
		b.WriteString(s.Text)
	}
	if b.Len() > 0 {
		return b.String()
	}
	for _, c := range item.Content {
		if c.Type == "reasoning_text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// itemText joins the output_text parts of a message item.
func itemText(item *responsesItem) string {
	var b strings.Builder
	for _, c := range item.Content {
		if c.Type == "output_text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// responsesFailure turns a response.failed snapshot into an error naming the
// upstream code, or a generic failure when the payload carries none.
func responsesFailure(resp *responsesResponse) error {
	if resp != nil && resp.Error != nil {
		msg := "response failed"
		if resp.Error.Code != "" {
			msg = "response failed: " + resp.Error.Code
		}
		if resp.Error.Message != "" {
			msg += ": " + resp.Error.Message
		}
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("response failed")
}

// ---------------------------------------------------------------------------
// Request encoding
// ---------------------------------------------------------------------------

// encodeResponsesRequest maps a CompletionRequest onto a Responses API JSON
// body. The system prompt becomes instructions; the thinking level becomes a
// reasoning effort (with an auto summary so reasoning is returned); pigo tools
// become function tools; and each message is replayed as the matching input
// item(s): assistant tool calls as function_call items, tool results as
// function_call_output items, and text (plus any images) as a role-tagged
// message.
func encodeResponsesRequest(req CompletionRequest) ([]byte, error) {
	body := map[string]any{
		"model":  req.Model,
		"stream": true,
	}
	if sp := strings.TrimSpace(req.Context.SystemPrompt); sp != "" {
		body["instructions"] = sp
	}
	if effort := responsesReasoningEffort(req.Config.ThinkingLevel); effort != "" {
		// Requesting a summary makes OpenAI return the model's reasoning so pigo
		// can render it as a thinking block, matching the chat driver.
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if tools := encodeResponsesTools(req.Context.Tools); len(tools) > 0 {
		body["tools"] = tools
	}

	items := make([]map[string]any, 0, len(req.Context.Messages))
	items = appendResponsesHistory(items, repairToolCallHistory(req.Context.Messages))
	body["input"] = items
	return json.Marshal(body)
}

// encodeResponsesTools converts pigo tools into Responses function tools. Each
// tool's JSON Schema becomes the function parameters; a schema that is empty or
// not a JSON object falls back to an empty object schema so the wire stays
// valid. Strict mode is off: pigo schemas are not authored against the Responses
// strict-function contract (which requires additionalProperties:false etc.).
func encodeResponsesTools(tools []agentcore.AgentTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		params := map[string]any{}
		if raw := t.Schema(); len(raw) > 0 {
			if err := json.Unmarshal(raw, &params); err != nil {
				params = map[string]any{}
			}
		}
		tool := map[string]any{
			"type":       "function",
			"name":       t.Name(),
			"parameters": params,
			"strict":     false,
		}
		if desc := t.Description(); desc != "" {
			tool["description"] = desc
		}
		out = append(out, tool)
	}
	return out
}

// appendResponsesHistory encodes a history onto the Responses input, keeping the
// function_call_output items of one batch adjacent.
//
// A tool result can expand into the output item plus a follow-up message carrying
// images the tool returned (see encodeResponsesMessage). Emitting that message
// immediately would place it between the outputs of a parallel batch; the
// carry-ins are deferred until the batch ends so every call keeps its output.
func appendResponsesHistory(items []map[string]any, history agentcore.MessageList) []map[string]any {
	var carry []map[string]any
	flush := func() {
		if len(carry) > 0 {
			items = append(items, carry...)
			carry = nil
		}
	}
	for _, m := range history {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			parts := encodeResponsesMessage(tr)
			items = append(items, parts[0])
			carry = append(carry, parts[1:]...)
			continue
		}
		flush()
		items = append(items, encodeResponsesMessage(m)...)
	}
	flush()
	return items
}

// encodeResponsesMessage replays one pigo message as its Responses input item(s).
func encodeResponsesMessage(m agentcore.Message) []map[string]any {
	switch msg := m.(type) {
	case agentcore.ToolResultMessage:
		// A tool result is backfilled against the model's call_id so the model
		// can pair it with the request it issued the previous turn. function_call_output
		// carries a string, so an image a tool returned is replayed as a follow-up
		// user message with input_image parts — the same shape the Responses API
		// accepts for user input — instead of being flattened to text.
		out := []map[string]any{{
			"type":    "function_call_output",
			"call_id": msg.ToolCallID,
			"output":  contentText(msg.Content),
		}}
		if imgs := imageOnlyList(msg.Content); len(imgs) > 0 {
			if parts, ok := responsesImageParts(agentcore.UserMessage{Content: withImageNote(imgs, msg.ToolName)}); ok {
				out = append(out, map[string]any{
					"type":    "message",
					"role":    agentcore.RoleUser,
					"content": parts,
				})
			}
		}
		return out
	case agentcore.AssistantMessage:
		var items []map[string]any
		if text := contentText(msg.Content); text != "" {
			items = append(items, map[string]any{
				"type":    "message",
				"role":    agentcore.RoleAssistant,
				"content": text,
			})
		}
		for _, call := range msg.ToolCalls() {
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   call.ID,
				"name":      call.Name,
				"arguments": replayToolArguments(call.Arguments),
			})
		}
		return items
	default:
		// A user (or other non-assistant) message with images is replayed as a
		// content-part list (input_text + input_image data URIs); a text-only
		// message stays a plain string.
		if parts, ok := responsesImageParts(m); ok {
			return []map[string]any{{
				"type":    "message",
				"role":    responsesRole(m.Role()),
				"content": parts,
			}}
		}
		if text := messageText(m); text != "" {
			return []map[string]any{{
				"type":    "message",
				"role":    responsesRole(m.Role()),
				"content": text,
			}}
		}
		return nil
	}
}

// responsesImageParts builds a content-part list for a message that carries at
// least one image: leading input_text (the concatenated text, if any) followed
// by one input_image per image, each as a data URI. It returns ok=false when the
// message has no images, so the caller keeps the plain-text path.
func responsesImageParts(m agentcore.Message) ([]map[string]any, bool) {
	content := messageContent(m)
	var hasImage bool
	for _, c := range content {
		if _, ok := c.(agentcore.ImageContent); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return nil, false
	}
	parts := make([]map[string]any, 0, len(content))
	if text := contentText(content); text != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": text})
	}
	for _, c := range content {
		img, ok := c.(agentcore.ImageContent)
		if !ok {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": fmt.Sprintf("data:%s;base64,%s", img.MimeType, img.Data),
			"detail":    "auto",
		})
	}
	return parts, true
}

// responsesReasoningEffort maps pigo's thinking level to a Responses API
// reasoning effort. The Responses reasoning field supports only low/medium/high,
// so "minimal" collapses to "low" (unlike the chat driver, which forwards
// "minimal" verbatim). off/unset yields "", signalling no reasoning param.
func responsesReasoningEffort(level agentcore.ThinkingLevel) string {
	switch level {
	case agentcore.ThinkingMinimal, agentcore.ThinkingLow:
		return "low"
	case agentcore.ThinkingMedium:
		return "medium"
	case agentcore.ThinkingHigh, agentcore.ThinkingXHigh, agentcore.ThinkingMax:
		return "high"
	default:
		return ""
	}
}

// responsesRole maps a pigo message role to the Responses API input role.
func responsesRole(role string) string {
	if role == agentcore.RoleAssistant {
		return agentcore.RoleAssistant
	}
	return agentcore.RoleUser
}

// messageContent returns the content list of a message regardless of its
// concrete role type, so callers can inspect it for images.
func messageContent(m agentcore.Message) agentcore.ContentList {
	switch msg := m.(type) {
	case agentcore.UserMessage:
		return msg.Content
	case agentcore.AssistantMessage:
		return msg.Content
	case agentcore.ToolResultMessage:
		return msg.Content
	}
	return nil
}

// messageText concatenates the text blocks of a message, ignoring non-text
// content.
func messageText(m agentcore.Message) string {
	var b strings.Builder
	collectText(&b, messageContent(m))
	return b.String()
}

func collectText(b *strings.Builder, content agentcore.ContentList) {
	for _, c := range content {
		if tc, ok := c.(agentcore.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
}

// contentText concatenates the text blocks of a content list.
func contentText(content agentcore.ContentList) string {
	var b strings.Builder
	collectText(&b, content)
	return b.String()
}
