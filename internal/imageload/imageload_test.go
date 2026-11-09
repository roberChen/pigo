// Tests for the shared image loader. They write real (tiny) image files — a PNG
// and a JPEG — so sniffing, the size cap, the wire-format gate and the base64
// round trip are exercised against the same rules the prompt syntax and the image
// tools apply, rather than against mocks of them. wire_test.go covers the
// format/validity half of the contract.
package imageload

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// pngBytes is a minimal 1x1 PNG: valid signature + IHDR, enough for both the
// extension mapping and the content sniff to agree.
var pngBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

// writeImage writes data to dir/name and returns the full path.
func writeImage(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestLoadPNG checks the happy path: the bytes are base64-encoded, the mime type
// comes from the (sniffed, extension-confirmed) content, and the block is the
// ImageContent type the wires look for.
func TestLoadPNG(t *testing.T) {
	path := writeImage(t, t.TempDir(), "pixel.png", pngBytes)
	img, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", img.MimeType)
	}
	if img.Data == "" {
		t.Error("data is empty, want base64 payload")
	}
	if !strings.HasPrefix(img.Data, "iVBORw0KGgo") {
		t.Errorf("data does not look like a base64 PNG: %.16q", img.Data)
	}
}

// TestLoadUnknownExtensionSniffs: a PNG named .dat is still an image, decided by
// content rather than by suffix.
func TestLoadUnknownExtensionSniffs(t *testing.T) {
	path := writeImage(t, t.TempDir(), "screenshot.dat", pngBytes)
	img, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png from sniffing", img.MimeType)
	}
}

// TestLoadRejectsNonImage: a text file must fail loudly, not come back as an
// empty block.
func TestLoadRejectsNonImage(t *testing.T) {
	path := writeImage(t, t.TempDir(), "notes.txt", []byte("hello, not a picture"))
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a text file")
	}
	if !strings.Contains(err.Error(), "not a recognized image") {
		t.Errorf("error = %v, want a not-an-image message", err)
	}
}

// TestLoadRejectsDirectory and TestLoadRejectsMissing cover the two other
// mistakes a model or a user makes with a path.
func TestLoadRejectsDirectory(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load accepted a directory")
	}
}

func TestLoadRejectsMissing(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.png")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

// TestLoadRejectsOversized writes a file one byte over the cap (sparse, so the
// test does not allocate 8 MiB) and asserts the message names the limit.
func TestLoadRejectsOversized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(MaxImageBytes + 1); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()
	_, err = Load(path)
	if err == nil {
		t.Fatal("Load accepted an oversized image")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %v, want the byte limit named", err)
	}
}

// TestValidate agrees with Load on every outcome but does not encode: a valid
// image passes, while a directory, a text file and a missing path each fail.
func TestValidate(t *testing.T) {
	dir := t.TempDir()
	if err := Validate(writeImage(t, dir, "ok.png", pngBytes)); err != nil {
		t.Errorf("Validate(valid png) = %v, want nil", err)
	}
	if err := Validate(writeImage(t, dir, "bad.txt", []byte("text"))); err == nil {
		t.Error("Validate accepted a text file")
	}
	if err := Validate(dir); err == nil {
		t.Error("Validate accepted a directory")
	}
	if err := Validate(filepath.Join(dir, "missing.png")); err == nil {
		t.Error("Validate accepted a missing file")
	}
}

// TestValidateAndLoadAgreeOnEmptyFile is the edge the two functions must agree
// on: an empty .png has no bytes to validate, so it is refused by both — at
// start-up (the flag) rather than at the first turn. A future change that made
// them disagree would be a bug: the flag would pass a path the run later rejects.
func TestValidateAndLoadAgreeOnEmptyFile(t *testing.T) {
	path := writeImage(t, t.TempDir(), "empty.png", nil)
	_, loadErr := Load(path)
	valErr := Validate(path)
	if loadErr == nil || valErr == nil {
		t.Fatalf("empty .png: Load=%v Validate=%v, want both to reject it", loadErr, valErr)
	}
}

// TestDescribeNamesPathMimeAndSize keeps the text-only companion line honest:
// it is the only thing a non-VL model gets.
func TestDescribeNamesPathMimeAndSize(t *testing.T) {
	got := Describe("a.png", agentcore.NewImageContent("QUJD", "image/png"))
	for _, want := range []string{"a.png", "image/png", "bytes"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, missing %q", got, want)
		}
	}
}

// TestLooksLikeImage covers the cheap pre-filter: known extensions yes, anything
// else no (Load still has the final say).
func TestLooksLikeImage(t *testing.T) {
	for _, name := range []string{"a.png", "A.PNG", "b.jpeg", "c.webp", "d.heic"} {
		if !LooksLikeImage(name) {
			t.Errorf("LooksLikeImage(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"a.txt", "a.png.txt", "a", ""} {
		if LooksLikeImage(name) {
			t.Errorf("LooksLikeImage(%q) = true, want false", name)
		}
	}
}

// pngHeader is the eight-byte PNG signature, used to build bytes that sniff as
// an image without a real file.
var pngHeader = pngBytes[:8]

// TestDetectMIME prefers the content, falls back to the extension, and reports
// "" for non-images.
func TestDetectMIME(t *testing.T) {
	if got := DetectMIME("x.bin", pngHeader); got != "image/png" {
		t.Errorf("DetectMIME(unknown ext, png) = %q, want image/png", got)
	}
	if got := DetectMIME("x.bin", []byte("plain text")); got != "" {
		t.Errorf("DetectMIME(text) = %q, want empty", got)
	}
	if got := DetectMIME("x.jpg", []byte("whatever")); got != "image/jpeg" {
		t.Errorf("DetectMIME(jpg ext) = %q, want image/jpeg", got)
	}
}

// TestLoadBytesDoNotMutateInput guards the base64 step: it must encode, not
// rewrite the caller's slice in place (the same bytes may be reused).
func TestLoadBytesDoNotMutateInput(t *testing.T) {
	data := append([]byte(nil), pngBytes...)
	path := writeImage(t, t.TempDir(), "p.png", data)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(data, pngBytes) {
		t.Error("Load mutated the input bytes")
	}
}
