package cli

// Tests for the exit hint both interactive front-ends print (resumehint.go):
// which sessions get one, what the command looks like, and when the hint stays
// silent.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/session"
)

// saveSession writes one turn so the store holds a resumable session file, the
// same way a real run does after its first turn.
func saveSession(t *testing.T, store *session.Store, header session.SessionHeader) session.SessionHeader {
	t.Helper()
	if header.ID == "" {
		header.ID = session.NewID(time.Now().UTC())
	}
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
	}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return header
}

// TestPrintResumeHintForPersistedSession pins the line an operator sees on the
// way out: the resume command for the session that just ended.
func TestPrintResumeHintForPersistedSession(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	header := saveSession(t, store, session.SessionHeader{Model: "faux"})

	var out bytes.Buffer
	PrintResumeHint(&out, store, header)
	want := "to resume this session: pigo --resume " + header.ID
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("hint = %q, want it to contain %q", got, want)
	}
}

// TestPrintResumeHintSilentCases covers everything that must not advertise a
// command: no asker at all (nil store), no session, a sub-agent session (which
// cannot be resumed directly), and a session that never persisted a turn (no file
// exists, so the printed command would fail).
func TestPrintResumeHintSilentCases(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	id := session.NewID(time.Now().UTC())
	cases := []struct {
		name   string
		store  *session.Store
		header session.SessionHeader
	}{
		{"nil store", nil, session.SessionHeader{ID: id}},
		{"no id", store, session.SessionHeader{}},
		{"sub-agent session", store, session.SessionHeader{ID: id, SubAgent: "task"}},
		{"never persisted", store, session.SessionHeader{ID: id}},
		{"missing file", store, session.SessionHeader{ID: "20260101-000000-000000-abcdef"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			PrintResumeHint(&out, tc.store, tc.header)
			if out.Len() != 0 {
				t.Errorf("hint = %q, want silence", out.String())
			}
		})
	}
}

// TestResumeCommandIsPasteable verifies the command is safe to paste for any id,
// including one an imported session carried in: an unsafe id is quoted rather
// than spliced into the shell command raw.
func TestResumeCommandIsPasteable(t *testing.T) {
	if got, want := ResumeCommand("20260720-1530-abcd"), "pigo --resume 20260720-1530-abcd"; got != want {
		t.Errorf("ResumeCommand(generated id) = %q, want %q", got, want)
	}
	if got, want := ResumeCommand("a b; rm -rf /"), `pigo --resume 'a b; rm -rf /'`; got != want {
		t.Errorf("ResumeCommand(unsafe id) = %q, want %q", got, want)
	}
	if got, want := ResumeCommand("it's"), `pigo --resume 'it'\''s'`; got != want {
		t.Errorf("ResumeCommand(quoted id) = %q, want %q", got, want)
	}
}

// TestResumeCommandRepeatsTheInvocation verifies the hint names pigo the way the
// operator started it — a path invocation keeps working when pigo is not on PATH —
// while a temporary build, whose path is gone by paste time, falls back to the
// plain name.
func TestResumeCommandRepeatsTheInvocation(t *testing.T) {
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	cases := []struct{ arg0, want string }{
		{"/usr/local/bin/pigo", "/usr/local/bin/pigo --resume s1"},
		{"./pigo", "./pigo --resume s1"},
		{"pigo", "pigo --resume s1"},
		{"/tmp/go-build123/b001/exe/pigo", "pigo --resume s1"},
		{"/tmp/go-build123/b001/repl.test", "pigo --resume s1"},
	}
	for _, tc := range cases {
		os.Args = []string{tc.arg0}
		if got := ResumeCommand("s1"); got != tc.want {
			t.Errorf("ResumeCommand with argv[0]=%q = %q, want %q", tc.arg0, got, tc.want)
		}
	}
}
