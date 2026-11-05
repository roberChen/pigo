package agentcore

// This file implements the tests for the image-repair primitives: removing
// images from the conversation itself (StripImagesInHistory, including the
// tool-result-becomes-a-failure rule).

import (
	"strings"
	"testing"
)

// TestStripImagesInHistory: after a failure, the conversation must hold no image
// at all — otherwise a later run resends the same picture and fails again.
func TestStripImagesInHistory(t *testing.T) {
	msgs := MessageList{
		UserMessage{RoleField: RoleUser, Content: ContentList{
			NewTextContent("what is on this screenshot?"),
			NewImageContent("QUJD", "image/png"),
		}},
		ToolResultMessage{
			RoleField:  RoleToolResult,
			ToolCallID: "call-1",
			ToolName:   "view_image",
			Content: ContentList{
				NewImageContent("QUJD", "image/png"),
				NewTextContent("viewed shot.png"),
			},
		},
		ToolResultMessage{
			RoleField:  RoleToolResult,
			ToolCallID: "call-2",
			ToolName:   "read",
			Content:    ContentList{NewTextContent("plain text")},
		},
	}

	repaired := StripImagesInHistory(msgs, "upstream 500: internal server error")

	if MessagesHaveImage(msgs) {
		t.Fatalf("history still carries an image: %+v", msgs)
	}
	if len(repaired) != 1 {
		t.Fatalf("repaired = %d results, want 1", len(repaired))
	}
	if repaired[0].ToolCallID != "call-1" {
		t.Errorf("repaired call = %q, want call-1", repaired[0].ToolCallID)
	}
	if !repaired[0].IsError {
		t.Error("degraded result is not marked as an error")
	}
	// The tool result keeps its descriptor and gains the reason.
	toolText := textOf(repaired[0].Content)
	for _, want := range []string{"viewed shot.png", "internal server error", ImageStrippedNoticeText} {
		if !strings.Contains(toolText, want) {
			t.Errorf("tool text = %q, want it to contain %q", toolText, want)
		}
	}
	// The list is modified in place, so the caller's history is repaired too.
	if tr := msgs[1].(ToolResultMessage); !tr.IsError || ContentHasImage(tr.Content) {
		t.Error("msgs was not repaired in place")
	}
	// A tool result without an image is untouched.
	if msgs[2].(ToolResultMessage).IsError {
		t.Error("a tool result without an image was degraded")
	}
	// The user's prompt keeps its text: only the picture goes.
	userText := textOf(msgs[0].(UserMessage).Content)
	if !strings.Contains(userText, "what is on this screenshot?") {
		t.Errorf("user text = %q, want it preserved", userText)
	}
	if !strings.Contains(userText, ImageStrippedNoticeText) {
		t.Errorf("user text = %q, want the omission notice appended", userText)
	}
	// A user message is not the model's mistake: it is not turned into an error.
	if len(repaired) != 1 {
		t.Errorf("repaired = %d, want the user message left out", len(repaired))
	}
}

// TestStripImagesInHistoryNoImages: no images means no change, which is what
// lets the loop tell an image-related failure from an ordinary one.
func TestStripImagesInHistoryNoImages(t *testing.T) {
	msgs := MessageList{
		UserMessage{RoleField: RoleUser, Content: ContentList{NewTextContent("hi")}},
		ToolResultMessage{RoleField: RoleToolResult, ToolCallID: "call-1", Content: ContentList{NewTextContent("ok")}},
	}
	if repaired := StripImagesInHistory(msgs, "boom"); len(repaired) != 0 {
		t.Fatalf("repaired = %d, want 0", len(repaired))
	}
	if msgs[1].(ToolResultMessage).IsError {
		t.Error("an image-free conversation was modified")
	}
}

// textOf concatenates the text blocks of list.
func textOf(list ContentList) string {
	var b strings.Builder
	for _, c := range list {
		if tc, ok := c.(TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}
