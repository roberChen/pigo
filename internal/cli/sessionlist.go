// This file implements the shared session-restore descriptions used by the
// /resume command in both front-ends (FR-16): the TUI picker and the line REPL
// render the same row text, so a stored conversation is described identically
// wherever it is listed.
package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/session"
)

// DescribeSession renders the one-line description shown in a /resume listing:
// how long ago the session was updated, the project it ran in, the model, and
// the first prompt. Missing pieces are omitted rather than left as empty
// fields. now anchors the relative age so a listing looks consistent across
// rows.
func DescribeSession(sum session.SessionSummary, now time.Time) string {
	parts := []string{relativeAge(sum.Header.UpdatedAt, now)}
	if project := projectLabel(sum.Header.Cwd); project != "" {
		parts = append(parts, project)
	}
	if sum.Header.Model != "" {
		parts = append(parts, sum.Header.Model)
	}
	preview := sum.Preview
	if preview == "" {
		preview = "(no prompt yet)"
	}
	parts = append(parts, preview)
	return strings.Join(parts, " · ")
}

// ResolveSessionArg maps a /resume argument to a stored session id: a positive
// number selects the n-th row of the listing, a full id matches directly, and a
// unique id prefix is accepted for convenience. It returns ok=false when the
// argument selects nothing or is an ambiguous prefix.
func ResolveSessionArg(arg string, sums []session.SessionSummary) (string, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", false
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(sums) {
			return "", false
		}
		return sums[n-1].Header.ID, true
	}
	match := ""
	for _, sum := range sums {
		if sum.Header.ID == arg {
			return arg, true
		}
		if strings.HasPrefix(sum.Header.ID, arg) {
			if match != "" {
				return "", false // ambiguous prefix
			}
			match = sum.Header.ID
		}
	}
	return match, match != ""
}

// relativeAge formats how long ago t was updated, coarsening with distance so a
// listing stays one short field: "just now", "12m ago", "3h ago", "2d ago", and
// a plain date for anything older than a month.
func relativeAge(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// projectLabel reduces a session's working directory to its base name, which is
// what identifies the project in a listing. A directory that cannot be reduced
// (root, empty) is returned as-is.
func projectLabel(cwd string) string {
	if cwd == "" {
		return ""
	}
	base := filepath.Base(cwd)
	if base == "." || base == string(filepath.Separator) {
		return cwd
	}
	return base
}
