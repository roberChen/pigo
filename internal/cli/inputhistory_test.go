// Tests for the input-history seed shared by the REPL and the TUI: which turns
// become ↑/↓ recall entries when a conversation is resumed.

package cli

import (
	"reflect"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// TestUserInputsFiltersToHumanPrompts verifies only human-typed user turns become
// recall entries: harness-injected turns (background sub-agent reports) and
// text-less turns are skipped, and consecutive duplicates collapse.
func TestUserInputsFiltersToHumanPrompts(t *testing.T) {
	messages := []agentcore.Message{
		agentcore.UserMessage{Content: agentcore.ContentList{agentcore.NewTextContent("first prompt")}},
		agentcore.AssistantMessage{Content: agentcore.ContentList{agentcore.NewTextContent("reply")}},
		agentcore.UserMessage{Content: agentcore.ContentList{agentcore.NewTextContent("  second prompt  ")}},
		agentcore.UserMessage{
			Content: agentcore.ContentList{agentcore.NewTextContent("child report")},
			Origin:  agentcore.UserMessageOriginSubAgentReport,
		},
		// An image-only prompt carries no text to recall.
		agentcore.UserMessage{Content: agentcore.ContentList{
			agentcore.ImageContent{Type: agentcore.ContentTypeImage, Data: "AA==", MimeType: "image/png"},
		}},
		// A consecutive duplicate is not stored twice, matching the live lists.
		agentcore.UserMessage{Content: agentcore.ContentList{agentcore.NewTextContent("second prompt")}},
		agentcore.UserMessage{Content: agentcore.ContentList{agentcore.NewTextContent("third prompt")}},
	}

	want := []string{"first prompt", "second prompt", "third prompt"}
	if got := UserInputs(messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("UserInputs = %q, want %q", got, want)
	}
}

// TestUserInputsEmpty verifies an empty or prompt-less conversation yields nothing
// (nil, so a caller can fall back without treating "" as a real entry).
func TestUserInputsEmpty(t *testing.T) {
	if got := UserInputs(nil); got != nil {
		t.Fatalf("UserInputs(nil) = %q, want nil", got)
	}
	only := []agentcore.Message{
		agentcore.AssistantMessage{Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
	}
	if got := UserInputs(only); got != nil {
		t.Fatalf("UserInputs(assistant only) = %q, want nil", got)
	}
}
