//go:build windows

// This file tests the Windows file and pipe ownership guarantees WriteFileAtomic
// and Dial rely on: the token really is owner-only, a concurrent reader cannot
// make a rotation fail, and a pipe that belongs to another user is rejected.
package hostpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWriteFileAtomicReplacesOpenTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "server.token")
	if err := WriteFileAtomic(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Go opens files without FILE_SHARE_DELETE, so Windows refuses to replace a
	// file another handle holds open. Readers are short-lived, and the write is
	// expected to outlast this one instead of failing the rotation.
	reader, err := os.Open(target)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = reader.Close()
	}()

	if err := WriteFileAtomic(target, []byte("new"), 0o600); err != nil {
		_ = reader.Close()
		t.Fatalf("write while a reader held the target open: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "new" {
		t.Fatalf("content = %q, want the replacement", raw)
	}
}

func TestWriteTokenHasOwnerOnlyACL(t *testing.T) {
	dir := t.TempDir()
	if err := WriteToken(dir, "secret"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	want, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(TokenPath(dir), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if !owner.Equals(want) {
		t.Fatalf("token owner = %v, want %v", owner, want)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("token DACL inherits parent ACEs; wanted a protected DACL")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("dacl: %v", err)
	}
	if acl == nil {
		t.Fatal("token has no DACL")
	}
	if acl.AceCount != 1 {
		t.Fatalf("token DACL has %d ACEs, want exactly one (the current user)", acl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatalf("GetAce: %v", err)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(want) {
		t.Fatalf("token ACE grants %v, want %v", aceSID, want)
	}
	// SetEntriesInAcl expands GENERIC_ALL into the file-specific rights, so the
	// stored mask is FILE_ALL_ACCESS rather than the generic bit.
	const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	if ace.Mask != fileAllAccess {
		t.Fatalf("token ACE mask = %#x, want FILE_ALL_ACCESS (%#x)", ace.Mask, fileAllAccess)
	}
}

func TestRemoveAllRetriesOpenFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteToken(dir, "secret"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	// A conventional os.Open handle blocks deletion on Windows; a concurrent
	// reader must not make shutdown leave the token behind.
	reader, err := os.Open(TokenPath(dir))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = reader.Close()
	}()

	RemoveAll(dir)
	if _, err := os.Stat(TokenPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("token survived RemoveAll (stat error: %v)", err)
	}
}

// TestLongStatePathRoundTrip covers a state directory past MAX_PATH: the raw
// CreateFile behind the shared reader needs the same \\?\-prefix the os package
// adds, or a deep PIGO_HOME would write its token but never read it back.
func TestLongStatePathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, "long-segment")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir deep dir: %v", err)
	}
	// The extended-path helper is what makes the raw CreateFile work; assert the
	// transformation itself, since this OS may also accept the plain path.
	long := TokenPath(dir)
	if !strings.HasPrefix(extendedPath(long), `\\?\`) {
		t.Fatalf("extendedPath(%q) = %q, want a \\\\?\\ prefix", long, extendedPath(long))
	}
	if extendedPath(`C:\short`) != `C:\short` {
		t.Fatalf("extendedPath mangled a short path: %q", extendedPath(`C:\short`))
	}
	if already := `\\?\C:\very\long`; extendedPath(already) != already {
		t.Fatalf("extendedPath re-prefixed an extended path: %q", extendedPath(already))
	}

	if err := WriteToken(dir, "long-path-token"); err != nil {
		t.Fatalf("WriteToken under a long path: %v", err)
	}
	got, err := ReadToken(dir)
	if err != nil {
		t.Fatalf("ReadToken under a long path: %v", err)
	}
	if got != "long-path-token" {
		t.Fatalf("ReadToken = %q, want %q", got, "long-path-token")
	}
	RemoveAll(dir)
	if _, err := os.Stat(TokenPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("RemoveAll left the token behind (stat error: %v)", err)
	}
}

func TestOwnerMatchesRejectsOtherUsers(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatalf("StringToSid: %v", err)
	}
	if !ownerMatches(me, me) {
		t.Fatal("the current user did not match its own SID")
	}
	if ownerMatches(system, me) {
		t.Fatal("a foreign owner (SYSTEM) matched the current user")
	}
	if ownerMatches(nil, me) || ownerMatches(me, nil) {
		t.Fatal("a missing SID matched")
	}
}

func TestSecondListenReportsEndpointInUse(t *testing.T) {
	endpoint := SocketPath(t.TempDir())
	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer ln.Close()

	ln2, err := Listen(endpoint)
	if err == nil {
		ln2.Close()
		t.Fatal("a second listener bound the same pipe")
	}
	if !IsEndpointInUse(err) {
		t.Fatalf("second Listen error = %v, want an in-use classification", err)
	}
}
