// This file tests the argv surface of the `pigo serve` / `pigo attach`
// subcommands (cmd/pigo/hostcmd.go): the checks that happen before any host is
// contacted. They live in package main because the flag sets do — hostcmd itself
// only sees the already-resolved options — and they all fail fast, so no host,
// socket or PIGO_HOME state is involved.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestIsHostSubcommand pins which argv shapes are routed to the host verbs: the
// check is positional (args[1]) and must not fire for a normal run or for a flag
// that merely contains the word.
func TestIsHostSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{args: []string{"pigo", "serve"}, want: true},
		{args: []string{"pigo", "attach"}, want: true},
		{args: []string{"pigo"}, want: false},
		{args: []string{"pigo", "-p", "serve"}, want: false},
		{args: []string{"pigo", "serves"}, want: false},
	}
	for _, tc := range cases {
		if got := isHostSubcommand(tc.args); got != tc.want {
			t.Fatalf("isHostSubcommand(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestRunHostSubcommandRejectsUnknownVerb documents the dispatch fallthrough: a
// third host verb exits 2 without starting anything. Only the fallthrough is
// exercised — the real verbs would start a host or a client.
func TestRunHostSubcommandRejectsUnknownVerb(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := runHostSubcommand([]string{"pigo", "serve-ish"}, "test", &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("unknown verb wrote to stdout: %q", out.String())
	}
}

// TestRunAttachRejectsUsageErrors pins the two ways `pigo attach` refuses to
// start: more than one positional argument, and --list combined with a session id
// (listing is per-connection, so a session id makes no sense there).
func TestRunAttachRejectsUsageErrors(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "two-positionals", args: []string{"sess-a", "sess-b"}, want: "usage: pigo attach"},
		{name: "list-with-session-id", args: []string{"--list", "sess-a"}, want: "--list does not take a session id"},
		{name: "list-shorthand-with-session-id", args: []string{"-l", "sess-a"}, want: "--list does not take a session id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := runAttach(tc.args, &out, &errOut)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (a usage error, not a connection attempt)", code)
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", errOut.String(), tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("usage error wrote to stdout: %q", out.String())
			}
		})
	}
}

// TestRunAttachUnknownFlag verifies a typo'd flag is a usage error (exit 2) rather
// than a defaults-run against the developer's host. The flag set returns the error
// without printing it, so only the exit code is asserted here; making the typo
// self-explanatory would be a change to the dispatch, not to this contract.
func TestRunAttachUnknownFlag(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := runAttach([]string{"--frobnicate"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("usage error wrote to stdout: %q", out.String())
	}
}

// TestRunServeRejectsInvalidCwd covers `pigo serve --cwd <missing>`: the chdir
// happens before anything cwd-derived is resolved, so a bad directory must fail
// with a usage exit code and the offending path in the message, without starting
// a host.
func TestRunServeRejectsInvalidCwd(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	missing := t.TempDir() + "/does-not-exist"

	var out, errOut bytes.Buffer
	code := runServe([]string{"--cwd", missing}, "test", &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage error)", code)
	}
	if !strings.Contains(errOut.String(), "--cwd") {
		t.Fatalf("stderr = %q, want it to mention --cwd", errOut.String())
	}
	if !strings.Contains(errOut.String(), missing) {
		t.Fatalf("stderr = %q, want it to name %q", errOut.String(), missing)
	}
	if strings.Contains(out.String(), "listening on") {
		t.Fatalf("a host was started despite the bad --cwd: %q", out.String())
	}
}

// TestRunServeHelpIsSuccess pins that `pigo serve --help` exits 0 (help is not an
// error) and prints the host-only flags on the error writer, which is where the
// flag set is pointed.
func TestRunServeHelpIsSuccess(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := runServe([]string{"--help"}, "test", &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"--daemon", "--dir"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("help output %q does not document %s", errOut.String(), want)
		}
	}
}
