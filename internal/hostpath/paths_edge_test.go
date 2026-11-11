// This file tests the host's on-disk footprint, the half that is the same on
// every platform: state-directory resolution and the state and token files
// (content, permissions, atomicity). These are the bits a *client* relies on to
// find a running host, so the cases that matter are the awkward ones: an
// unusable $PIGO_HOME, a corrupt or half-written state file, and repeated writes
// racing a reader.
//
// The transport is platform-specific and lives next door: endpoint_posix_test.go
// covers the unix socket path (length fallback, mode bits, stale-file cleanup)
// and endpoint_windows_test.go the named pipe (hashing, DACL, no residue). The
// POSIX-only cases here are the ones asserting permission bits, which Windows
// does not have.
package hostpath

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// skipOnWindows skips a test whose subject is unix sockets or POSIX file modes.
// The repository targets linux CI (AGENTS.md), so the skip is only a courtesy
// for a developer on Windows.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets and POSIX permission bits are not portable to windows")
	}
}

func TestDefaultDirHonoursPigoHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	if got := DefaultDir(); got != home {
		t.Fatalf("DefaultDir() = %q, want %q", got, home)
	}

	// A relative $PIGO_HOME is used verbatim: the state directory is whatever the
	// user pointed at, never silently rewritten.
	t.Setenv("PIGO_HOME", "relative/state")
	if got := DefaultDir(); got != "relative/state" {
		t.Fatalf("DefaultDir() with relative home = %q", got)
	}
}

func TestDefaultDirFallsBackToUserHome(t *testing.T) {
	skipOnWindows(t)
	// Inside the fallback branch the answer must come from the home directory,
	// not from some leftover default, so pin HOME to a temp dir and compute the
	// expectation from it.
	home := t.TempDir()
	t.Setenv("PIGO_HOME", "")
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".pigo")
	if got := DefaultDir(); got != want {
		t.Fatalf("DefaultDir() = %q, want %q", got, want)
	}
}

func TestReadStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	// PIGO_HOME points at the temp dir so nothing can read the developer's host.
	t.Setenv("PIGO_HOME", dir)

	if _, err := ReadState(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadState on a missing file = %v, want os.ErrNotExist", err)
	}
	// Same when the whole directory is missing (a host that never started).
	if _, err := ReadState(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadState in a missing dir = %v, want os.ErrNotExist", err)
	}
}

func TestReadStateMalformedContents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)

	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"whitespace", "   \n\t"},
		{"truncated", `{"pid": 12, "version": 1`},
		{"not_an_object", `[1,2,3]`},
		{"wrong_field_type", `{"pid": "not-a-number"}`},
		{"garbage", "not json at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(StatePath(dir), []byte(tc.data), 0o600); err != nil {
				t.Fatalf("seed state file: %v", err)
			}
			_, err := ReadState(dir)
			if err == nil {
				t.Fatal("ReadState accepted a malformed state file")
			}
			if errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a corrupt file must not be reported as missing: %v", err)
			}
			// The error must name the file: a bare json error leaves the operator
			// guessing which of several state files came from an old version.
			if !strings.Contains(err.Error(), StatePath(dir)) {
				t.Fatalf("error %q does not mention %q", err, StatePath(dir))
			}
		})
	}
}

func TestReadStateToleratesUnknownFieldsAndPartialState(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)

	// A newer host may add fields; an older client must still read the ones it
	// knows rather than failing the whole handshake.
	body := `{"pid": 7, "version": 1, "startedAt": "2024-05-01T12:30:15Z",
	          "socket": "/tmp/x.sock", "futureField": {"nested": true}}`
	if err := os.WriteFile(StatePath(dir), []byte(body), 0o600); err != nil {
		t.Fatalf("seed state file: %v", err)
	}
	st, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState with an unknown field: %v", err)
	}
	if st.PID != 7 || st.Socket != "/tmp/x.sock" {
		t.Fatalf("state = %+v", st)
	}
	if want := time.Date(2024, 5, 1, 12, 30, 15, 0, time.UTC); !st.StartedAt.Equal(want) {
		t.Fatalf("startedAt = %s, want %s", st.StartedAt, want)
	}

	// A state file with only some fields is valid, not an error: the writer may
	// be an older version. The zero values are what the caller sees.
	if err := os.WriteFile(StatePath(dir), []byte(`{"pid": 9}`), 0o600); err != nil {
		t.Fatalf("seed partial state file: %v", err)
	}
	st, err = ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState of a partial state: %v", err)
	}
	if st.PID != 9 || st.Version != 0 || st.Socket != "" || !st.StartedAt.IsZero() {
		t.Fatalf("partial state = %+v", st)
	}
}

func TestWriteStateRoundTripAndPermissions(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "pigo-state")
	t.Setenv("PIGO_HOME", dir)

	want := State{
		PID:       os.Getpid(),
		Version:   3,
		StartedAt: time.Now().UTC().Truncate(time.Second),
		Socket:    SocketPath(dir),
	}
	if err := WriteState(dir, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.PID != want.PID || got.Version != want.Version || got.Socket != want.Socket {
		t.Fatalf("state = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("startedAt = %s, want %s", got.StartedAt, want.StartedAt)
	}

	// The state directory and file are private: both may reveal the socket, pid
	// and therefore the token path of a running host.
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat state dir: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir mode = %04o, want 0700", perm)
	}
	if info, err := os.Stat(StatePath(dir)); err != nil {
		t.Fatalf("stat state file: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %04o, want 0600", perm)
	}

	// Overwriting must fully replace the content, not leave a tail of the longer
	// previous value behind.
	if err := WriteState(dir, State{PID: 1, Version: 1, StartedAt: time.Unix(1, 0).UTC(), Socket: "/s"}); err != nil {
		t.Fatalf("second WriteState: %v", err)
	}
	raw, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("state file is not valid JSON after overwrite: %v\n%s", err, raw)
	}
	if got := int(check["pid"].(float64)); got != 1 {
		t.Fatalf("overwritten state pid = %d, want 1", got)
	}

	assertNoTempFiles(t, dir)
}

// TestWriteStateNeverPartiallyVisible races a writer against a reader: the
// rename must make each version appear whole, so a client never parses half a
// state file (which an in-place rewrite would allow).
func TestWriteStateNeverPartiallyVisible(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIGO_HOME", dir)

	first := State{PID: 11, Version: 1, StartedAt: time.Unix(100, 0).UTC(), Socket: "/first.sock"}
	second := State{PID: 22, Version: 2, StartedAt: time.Unix(200, 0).UTC(), Socket: "/second.sock"}
	if err := WriteState(dir, first); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}

	const rounds = 100
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < rounds; i++ {
			st := first
			if i%2 == 1 {
				st = second
			}
			if err := WriteState(dir, st); err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Read through the production reader: it is the one that must see a
			// whole state file, and on Windows it shares delete so a concurrent
			// replacement is allowed underneath it.
			st, err := ReadState(dir)
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
			if st.PID != first.PID && st.PID != second.PID {
				select {
				case errs <- errors.New("read a state that was never written"):
				default:
				}
				return
			}
		}
	}()

	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFileAtomicCreatesParents(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	target := filepath.Join(nested, "file.txt")

	if err := WriteFileAtomic(target, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "hello" {
		t.Fatalf("content = %q", raw)
	}
	if info, err := os.Stat(target); err != nil {
		t.Fatalf("stat target: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", perm)
	}
	for _, dir := range []string{filepath.Join(root, "a"), nested} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %04o, want 0700", dir, perm)
		}
	}
	assertNoTempFiles(t, nested)
}

func TestWriteFileAtomicReplacesAndHonoursMode(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "token")

	if err := WriteFileAtomic(target, []byte(strings.Repeat("a", 4096)), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFileAtomic(target, []byte("short"), 0o640); err != nil {
		t.Fatalf("second write: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "short" {
		t.Fatalf("content = %q, want the full replacement", raw)
	}
	if info, err := os.Stat(target); err != nil {
		t.Fatalf("stat: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o640 {
		t.Fatalf("file mode = %04o, want 0640", perm)
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFileAtomicFailureLeavesNoTempFile(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	// The destination is a directory, so the final rename fails. The temp file
	// must still be cleaned up: a failed write may not litter the private state
	// directory (which is also where the token lives).
	victim := filepath.Join(dir, "occupied")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := WriteFileAtomic(victim, []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic onto a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "occupied" {
		t.Fatalf("failed write left files behind: %v", names(entries))
	}
}

// assertNoTempFiles fails when a .tmp-* staging file survived a write, which
// would slowly fill a state directory the user never inspects.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %q in %s", e.Name(), dir)
		}
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestNewTokenFormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool, 32)
	for i := 0; i < 32; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if len(tok) != 64 {
			t.Fatalf("token %q is %d chars, want 64 hex chars", tok, len(tok))
		}
		if _, err := hex.DecodeString(tok); err != nil {
			t.Fatalf("token %q is not hex: %v", tok, err)
		}
		if seen[tok] {
			t.Fatalf("NewToken repeated %q", tok)
		}
		seen[tok] = true
	}
}

func TestWriteReadToken(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("PIGO_HOME", dir)

	if _, err := ReadToken(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadToken before any host = %v, want os.ErrNotExist", err)
	}

	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := WriteToken(dir, tok); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	got, err := ReadToken(dir)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if got != tok {
		t.Fatalf("ReadToken = %q, want %q", got, tok)
	}
	if info, err := os.Stat(TokenPath(dir)); err != nil {
		t.Fatalf("stat token: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %04o, want 0600", perm)
	}

	// A restart replaces the token; the old one must not linger.
	next, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := WriteToken(dir, next); err != nil {
		t.Fatalf("rewrite token: %v", err)
	}
	if got, err := ReadToken(dir); err != nil || got != next {
		t.Fatalf("ReadToken after rewrite = %q, %v", got, err)
	}
	assertNoTempFiles(t, dir)
}

func TestReadTokenStripsLineEndings(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()

	cases := []struct {
		name string
		data string
		want string
	}{
		{"newline", "abc\n", "abc"},
		{"crlf", "abc\r\n", "abc"},
		{"no_newline", "abc", "abc"},
		{"only_newlines_stripped", "abc\r\n\r\n\n", "abc"},
		{"empty_file", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(TokenPath(dir), []byte(tc.data), 0o600); err != nil {
				t.Fatalf("seed token: %v", err)
			}
			got, err := ReadToken(dir)
			if err != nil {
				t.Fatalf("ReadToken: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ReadToken(%q) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestTokenMatches(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !TokenMatches(tok, tok) {
		t.Fatal("identical tokens did not match")
	}
	if TokenMatches(tok, tok[:len(tok)-1]) {
		t.Fatal("a token prefix matched: comparison must not accept truncation")
	}
	if TokenMatches(tok, tok+"x") {
		t.Fatal("a longer token matched")
	}
	if TokenMatches(tok, strings.ToUpper(tok)) {
		t.Fatal("comparison is not case-sensitive")
	}
	// Note: two empty strings compare equal by construction. The server guards
	// this with an explicit "host has no token yet" check before calling here.
}

func TestProbeMissingSocket(t *testing.T) {
	if Probe("", 50*time.Millisecond) {
		t.Fatal("empty socket path probed as live")
	}
	dir := t.TempDir()
	if Probe(SocketPath(dir), 50*time.Millisecond) {
		t.Fatal("missing socket file probed as live")
	}
	if Probe(filepath.Join(dir, "no", "such", "dir", "server.sock"), 50*time.Millisecond) {
		t.Fatal("socket in a missing directory probed as live")
	}
}

func TestDefaultDirWithoutHome(t *testing.T) {
	skipOnWindows(t)
	// Neither PIGO_HOME nor HOME: DefaultDir has nothing to work with and says so
	// with an empty string rather than inventing a path in the working directory.
	t.Setenv("PIGO_HOME", "")
	t.Setenv("HOME", "")
	if got := DefaultDir(); got != "" {
		t.Fatalf("DefaultDir() with no home = %q, want empty", got)
	}
}

func TestWriteStateWhenDirIsAFile(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	// A file sitting where the state directory should be is not something the
	// host can recover from, but it must be a plain error, not a panic.
	blocker := filepath.Join(root, "state")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if err := WriteState(blocker, State{PID: 1, Version: 1}); err == nil {
		t.Fatal("WriteState succeeded with a file as the state directory")
	}
	if err := WriteFileAtomic(filepath.Join(blocker, "file"), []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic succeeded under a file")
	}
	// The blocker is untouched: a failed write never truncates what is there.
	if raw, err := os.ReadFile(blocker); err != nil || string(raw) != "not a directory" {
		t.Fatalf("blocker damaged: %q, %v", raw, err)
	}
}
