// This file implements the input-history seed shared by the REPL line editor and
// the TUI composer: when a conversation is resumed, the prompts the human typed
// earlier go back into the ↑/↓ recall list, not only into the transcript.
package cli

import (
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// UserInputs returns the human-typed prompts in messages, oldest first, for
// seeding an input-history list.
//
// Only user-role turns qualify, and two kinds are skipped because they are not
// the human talking: harness-injected turns (a background sub-agent's report —
// see UserMessage.IsSynthetic) and turns whose text is empty (an image-only
// prompt leaves nothing to recall). A consecutive duplicate is collapsed, the
// same rule both recall lists apply while accepting input.
//
// Callers pass the full branch they are entering, so a resumed session whose
// context starts at a compaction checkpoint still offers the prompts summarized
// away: the recall list is about what the human typed, which the checkpoint does
// not change.
func UserInputs(messages []agentcore.Message) []string {
	var inputs []string
	for _, m := range messages {
		user, ok := m.(agentcore.UserMessage)
		if !ok || user.IsSynthetic() {
			continue
		}
		text := strings.TrimSpace(agentcore.ContentToText(user.Content))
		if text == "" {
			continue
		}
		if n := len(inputs); n > 0 && inputs[n-1] == text {
			continue
		}
		inputs = append(inputs, text)
	}
	return inputs
}
