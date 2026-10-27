package cli

// Tests for the shared session-restore descriptions behind /resume (#FR-16):
// the row text both front-ends render, and the argument resolution that maps a
// typed selection to a stored session id.

import (
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/session"
)

func summary(id, model, cwd, preview string, updated time.Time) session.SessionSummary {
	return session.SessionSummary{
		Header:  session.SessionHeader{ID: id, Model: model, Cwd: cwd, UpdatedAt: updated},
		Preview: preview,
	}
}

// TestDescribeSession verifies a restore row carries the age, project, model and
// first prompt, omits the pieces a session does not have, and never leaves the
// preview blank.
func TestDescribeSession(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	full := summary("s1", "glm-4.6", "/home/dev/pigo", "fix the parser", now.Add(-2*time.Hour))
	if got, want := DescribeSession(full, now), "2h ago · pigo · glm-4.6 · fix the parser"; got != want {
		t.Errorf("DescribeSession = %q, want %q", got, want)
	}

	sparse := summary("s2", "", "", "", now.Add(-90*time.Minute))
	if got, want := DescribeSession(sparse, now), "1h ago · (no prompt yet)"; got != want {
		t.Errorf("DescribeSession(sparse) = %q, want %q", got, want)
	}

	old := summary("s3", "m", "/w/p", "p", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if got, want := DescribeSession(old, now), "2026-05-01 · p · m · p"; got != want {
		t.Errorf("DescribeSession(old) = %q, want %q", got, want)
	}
}

// TestResolveSessionArg verifies selection by index, exact id and unique prefix,
// and that out-of-range or ambiguous arguments are rejected.
func TestResolveSessionArg(t *testing.T) {
	sums := []session.SessionSummary{
		summary("20260710-120000-000001", "", "", "", time.Time{}),
		summary("20260710-130000-000002", "", "", "", time.Time{}),
	}
	cases := []struct {
		arg  string
		want string
		ok   bool
	}{
		{"1", sums[0].Header.ID, true},
		{"2", sums[1].Header.ID, true},
		{"0", "", false},
		{"3", "", false},
		{sums[1].Header.ID, sums[1].Header.ID, true},
		{"20260710-130000", sums[1].Header.ID, true},
		{"20260710", "", false}, // ambiguous prefix
		{"20990101", "", false}, // no match
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ResolveSessionArg(c.arg, sums)
		if got != c.want || ok != c.ok {
			t.Errorf("ResolveSessionArg(%q) = (%q, %v), want (%q, %v)", c.arg, got, ok, c.want, c.ok)
		}
	}
}
