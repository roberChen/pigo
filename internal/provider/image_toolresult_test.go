package provider

// Tests for images returned by tools (view_image, or read on a screenshot). The
// three wires accept images in different places, so each encoder has its own
// assertion: Anthropic takes an image block inside tool_result, while the OpenAI
// chat and Responses shapes (whose tool messages are text-only) need a follow-up
// message carrying the picture. The capability probe must also see these images,
// or a text-only model would silently accept a call whose point was the picture.

import (
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// imageToolResult builds the shape the tools return: image block first, a short
// descriptor after it.
func imageToolResult(tool string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call_img",
		ToolName:   tool,
		Content: agentcore.ContentList{
			agentcore.NewImageContent("QUJD", "image/png"),
			agentcore.NewTextContent("viewed shot.png (image/png)"),
		},
	}
}

// TestEncodeOpenAIToolResultWithImage: the tool entry keeps only text, and the
// image follows in a user message so it survives onto the wire.
func TestEncodeOpenAIToolResultWithImage(t *testing.T) {
	out := encodeOpenAIMessage(imageToolResult("view_image"))
	if len(out) != 2 {
		t.Fatalf("entries = %d, want 2 (tool + follow-up user)", len(out))
	}
	if out[0]["role"] != "tool" || out[0]["tool_call_id"] != "call_img" {
		t.Errorf("tool entry = %#v", out[0])
	}
	if _, ok := out[0]["content"].(string); !ok {
		t.Errorf("tool content = %T, want the flattened string form", out[0]["content"])
	}
	if out[1]["role"] != "user" {
		t.Fatalf("follow-up role = %v, want user", out[1]["role"])
	}
	parts, ok := out[1]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("follow-up content = %T, want the multimodal array", out[1]["content"])
	}
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("follow-up parts = %#v, want note + image_url", parts)
	}
	if iu := parts[1]["image_url"].(map[string]any); iu["url"] != "data:image/png;base64,QUJD" {
		t.Errorf("image url = %v", iu["url"])
	}
}

// TestEncodeOpenAIToolResultTextOnly: no image means no extra message — the
// common case must not grow the request.
func TestEncodeOpenAIToolResultTextOnly(t *testing.T) {
	msg := agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call_txt",
		Content:    agentcore.ContentList{agentcore.NewTextContent("package main")},
	}
	if out := encodeOpenAIMessage(msg); len(out) != 1 {
		t.Fatalf("entries = %d, want 1", len(out))
	}
}

// TestEncodeAnthropicToolResultWithImage: the image stays inside the tool_result
// block array, which is where the Messages API expects it.
func TestEncodeAnthropicToolResultWithImage(t *testing.T) {
	entry := encodeAnthropicMessage(imageToolResult("view_image"))
	blocks, ok := entry["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content = %T, want a block array", entry["content"])
	}
	if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
		t.Fatalf("blocks = %#v", blocks)
	}
	inner, ok := blocks[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("tool_result content = %T, want blocks for an image result", blocks[0]["content"])
	}
	if len(inner) != 2 {
		t.Fatalf("inner blocks = %d, want 2 (image + text)", len(inner))
	}
	if inner[0]["type"] != "image" {
		t.Errorf("inner[0] = %#v, want the image first", inner[0])
	}
	src, ok := inner[0]["source"].(map[string]any)
	if !ok || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "QUJD" {
		t.Errorf("image source = %#v", inner[0]["source"])
	}
	if inner[1]["type"] != "text" {
		t.Errorf("inner[1] = %#v, want the descriptor text", inner[1])
	}
}

// TestEncodeAnthropicToolResultTextOnlyIsString keeps the plain string form, so
// the encoder does not reshape every ordinary tool result.
func TestEncodeAnthropicToolResultTextOnlyIsString(t *testing.T) {
	msg := agentcore.ToolResultMessage{
		RoleField: agentcore.RoleToolResult,
		Content:   agentcore.ContentList{agentcore.NewTextContent("ok")},
	}
	blocks := encodeAnthropicMessage(msg)["content"].([]map[string]any)
	if s, ok := blocks[0]["content"].(string); !ok || s != "ok" {
		t.Errorf("content = %#v, want the string \"ok\"", blocks[0]["content"])
	}
}

// TestAnthropicToolResultNeverEmpty: a result holding only whitespace text and
// an image still must not produce an empty block array (the API rejects it).
func TestAnthropicToolResultNeverEmpty(t *testing.T) {
	got := anthropicToolResultContent(agentcore.ContentList{agentcore.NewTextContent("")})
	if s, ok := got.(string); !ok || s != "" {
		t.Fatalf("empty text result = %#v, want the empty string form", got)
	}
	blocks := anthropicToolResultContent(agentcore.ContentList{agentcore.NewImageContent("QUJD", "image/png")})
	arr, ok := blocks.([]map[string]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("image-only result = %#v, want at least one block", blocks)
	}
}

// TestEncodeResponsesToolResultWithImage: function_call_output keeps the text,
// and the image rides in a follow-up message item.
func TestEncodeResponsesToolResultWithImage(t *testing.T) {
	out := encodeResponsesMessage(imageToolResult("view_image"))
	if len(out) != 2 {
		t.Fatalf("items = %d, want 2 (function_call_output + message)", len(out))
	}
	if out[0]["type"] != "function_call_output" || out[0]["call_id"] != "call_img" {
		t.Errorf("output item = %#v", out[0])
	}
	if out[1]["type"] != "message" || out[1]["role"] != agentcore.RoleUser {
		t.Fatalf("follow-up item = %#v", out[1])
	}
	parts, ok := out[1]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("follow-up content = %T", out[1]["content"])
	}
	var sawImage bool
	for _, p := range parts {
		if p["type"] == "input_image" {
			sawImage = true
		}
	}
	if !sawImage {
		t.Errorf("parts = %#v, want an input_image part", parts)
	}
}

// TestEncodeResponsesToolResultTextOnly keeps the single-item shape.
func TestEncodeResponsesToolResultTextOnly(t *testing.T) {
	msg := agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call_txt",
		Content:    agentcore.ContentList{agentcore.NewTextContent("ok")},
	}
	if out := encodeResponsesMessage(msg); len(out) != 1 {
		t.Fatalf("items = %d, want 1", len(out))
	}
}

// TestContextHasImageSeesToolResults: the probe must look inside tool results,
// otherwise a text-only model is picked for an image-only turn.
func TestContextHasImageSeesToolResults(t *testing.T) {
	if !contextHasImage([]agentcore.Message{imageToolResult("view_image")}) {
		t.Error("contextHasImage missed an image inside a tool result")
	}
	text := agentcore.ToolResultMessage{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}
	if contextHasImage([]agentcore.Message{text}) {
		t.Error("contextHasImage reported an image for a text-only tool result")
	}
	if !contextHasImage([]agentcore.Message{imageUserMessage("hi", "QUJD", "image/png")}) {
		t.Error("contextHasImage missed a user image")
	}
	if contextHasImage(nil) {
		t.Error("contextHasImage reported an image for an empty context")
	}
}

// TestCheckImageSupportToolImage: the guard rejects a text-only model even when
// the picture arrived through a tool rather than the prompt.
func TestCheckImageSupportToolImage(t *testing.T) {
	models := []Model{{ID: "text-only", SupportsImages: false}, {ID: "vl", SupportsImages: true}}
	msgs := []agentcore.Message{imageToolResult("view_image")}
	if err := checkImageSupport("p", "text-only", models, msgs); err == nil {
		t.Error("checkImageSupport accepted an image on a text-only model")
	}
	if err := checkImageSupport("p", "vl", models, msgs); err != nil {
		t.Errorf("checkImageSupport rejected a VL model: %v", err)
	}
	if err := checkImageSupport("p", "unknown-model", models, msgs); err != nil {
		t.Errorf("checkImageSupport rejected an uncatalogued model: %v", err)
	}
}

// TestWithImageNoteNamesTheTool keeps the follow-up message self-describing.
func TestWithImageNoteNamesTheTool(t *testing.T) {
	note := withImageNote(agentcore.ContentList{agentcore.NewImageContent("QUJD", "image/png")}, "view_image")
	if len(note) != 2 {
		t.Fatalf("note len = %d, want 2", len(note))
	}
	text, ok := note[0].(agentcore.TextContent)
	if !ok || text.Text == "" {
		t.Fatalf("note[0] = %#v", note[0])
	}
	if !strings.Contains(text.Text, "view_image") {
		t.Errorf("note = %q, want it to name the tool", text.Text)
	}
	// An unnamed tool still produces a readable sentence.
	anon := withImageNote(agentcore.ContentList{agentcore.NewImageContent("QUJD", "image/png")}, "")
	if s := anon[0].(agentcore.TextContent).Text; s == "" {
		t.Error("anonymous note is empty")
	}
}

// TestImageOnlyListFindsImages covers the shared helper.
func TestImageOnlyListFindsImages(t *testing.T) {
	if got := imageOnlyList(agentcore.ContentList{agentcore.NewTextContent("x")}); got != nil {
		t.Errorf("imageOnlyList(text) = %#v, want nil", got)
	}
	got := imageOnlyList(agentcore.ContentList{
		agentcore.NewTextContent("x"),
		agentcore.NewImageContent("QUJD", "image/png"),
	})
	if len(got) != 1 {
		t.Fatalf("imageOnlyList = %d blocks, want 1", len(got))
	}
	if _, ok := got[0].(agentcore.ImageContent); !ok {
		t.Errorf("block = %T, want ImageContent", got[0])
	}
}
