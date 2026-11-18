package tui

// Tests for the TUI's exit hint: after the alt screen is restored, the last line
// names the command that reopens the session the panel ended on.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/cli"
	"github.com/roberChen/pigo/internal/session"
)

// hintedSession builds a runSession bound to a temp store that either holds this
// session's file (persisted) or not.
func hintedSession(t *testing.T, persisted bool) *runSession {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	header := session.SessionHeader{ID: session.NewID(time.Now().UTC()), Model: "faux"}
	if persisted {
		msgs := agentcore.MessageList{agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
		}}
		if err := store.Save(header, msgs); err != nil {
			t.Fatalf("save session: %v", err)
		}
	}
	return &runSession{store: store, header: header}
}

// TestPrintResumeHintNamesTheSession covers the exit path: a session with turns on
// disk gets a resume command, a session with nothing written stays silent (the
// command would fail), and a model with no session at all prints nothing.
func TestPrintResumeHintNamesTheSession(t *testing.T) {
	s := hintedSession(t, true)
	var out bytes.Buffer
	Model{session: s}.printResumeHint(&out)
	if want := cli.ResumeCommand(s.header.ID); !strings.Contains(out.String(), want) {
		t.Errorf("hint = %q, want it to contain %q", out.String(), want)
	}

	out.Reset()
	Model{session: hintedSession(t, false)}.printResumeHint(&out)
	if out.Len() != 0 {
		t.Errorf("unpersisted session printed %q, want silence", out.String())
	}

	out.Reset()
	Model{}.printResumeHint(&out)
	if out.Len() != 0 {
		t.Errorf("model without a session printed %q, want silence", out.String())
	}
}
