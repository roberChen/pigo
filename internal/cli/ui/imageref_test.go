// Tests for the prompt image-reference syntax (US-010, #126): @image:<path> and
// the Markdown ![alt](<path>) form. They write a tiny real PNG to a temp file so
// loadImageContent exercises the real read + base64 + mime path, and assert that
// a missing file is an error (not a silently dropped image).
package ui

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// pngBytes is a minimal 1x1 PNG (valid signature + IHDR) so mime detection and
// the image/ prefix check pass without pulling in an image library.
var pngBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

func writeTempPNG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pixel.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatalf("write temp png: %v", err)
	}
	return path
}

// TestBuildUserContentNoImage: a plain prompt yields a single text block.
func TestBuildUserContentNoImage(t *testing.T) {
	content, err := BuildUserContent("just some text")
	if err != nil {
		t.Fatalf("BuildUserContent: %v", err)
	}
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	if tc, ok := content[0].(agentcore.TextContent); !ok || tc.Text != "just some text" {
		t.Errorf("content[0] = %#v, want text \"just some text\"", content[0])
	}
}

// TestBuildUserContentAtImageSyntax: @image:<path> attaches an ImageContent and
// keeps the surrounding text.
func TestBuildUserContentAtImageSyntax(t *testing.T) {
	path := writeTempPNG(t)
	content, err := BuildUserContent("look at @image:" + path + " please")
	if err != nil {
		t.Fatalf("BuildUserContent: %v", err)
	}
	assertTextThenImage(t, content, "look at", "image/png")
}

// TestBuildUserContentMarkdownSyntax: ![alt](path) attaches an ImageContent.
func TestBuildUserContentMarkdownSyntax(t *testing.T) {
	path := writeTempPNG(t)
	content, err := BuildUserContent("before ![a pixel](" + path + ") after")
	if err != nil {
		t.Fatalf("BuildUserContent: %v", err)
	}
	// text "before", image, text "after"
	if len(content) != 3 {
		t.Fatalf("content len = %d, want 3", len(content))
	}
	if _, ok := content[1].(agentcore.ImageContent); !ok {
		t.Errorf("content[1] = %T, want ImageContent", content[1])
	}
}

// TestBuildUserContentMissingFile: a missing referenced file is an error.
func TestBuildUserContentMissingFile(t *testing.T) {
	_, err := BuildUserContent("@image:/no/such/file.png")
	if err == nil {
		t.Fatal("BuildUserContent accepted a missing image file, want error")
	}
}

// TestLoadImageContentBase64: the encoded data must match the file bytes.
func TestLoadImageContentBase64(t *testing.T) {
	path := writeTempPNG(t)
	img, err := loadImageContent(path)
	if err != nil {
		t.Fatalf("loadImageContent: %v", err)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", img.MimeType)
	}
	if want := base64.StdEncoding.EncodeToString(pngBytes); img.Data != want {
		t.Errorf("data mismatch")
	}
}

func assertTextThenImage(t *testing.T, content agentcore.ContentList, wantText, wantMime string) {
	t.Helper()
	var sawText, sawImage bool
	for _, c := range content {
		switch b := c.(type) {
		case agentcore.TextContent:
			if strings.Contains(b.Text, wantText) {
				sawText = true
			}
		case agentcore.ImageContent:
			if b.MimeType == wantMime {
				sawImage = true
			}
		}
	}
	if !sawText {
		t.Errorf("no text block containing %q in %#v", wantText, content)
	}
	if !sawImage {
		t.Errorf("no image block with mime %q in %#v", wantMime, content)
	}
}

// TestImageRefKeepsSpacesRoundTrippable: the compact @image form stops at
// whitespace, so a path with a space must be emitted as the Markdown form or the
// reference would be truncated when it is parsed back.
func TestImageRefKeepsSpacesRoundTrippable(t *testing.T) {
	if got, want := ImageRef("/tmp/shot.png"), "@image:/tmp/shot.png"; got != want {
		t.Errorf("ImageRef(no space) = %q, want %q", got, want)
	}
	spaced := "/tmp/my shot.png"
	if got, want := ImageRef(spaced), "![image]("+spaced+")"; got != want {
		t.Errorf("ImageRef(space) = %q, want the Markdown form %q", got, want)
	}
}

// TestAttachImagesPrefixesPrompt: --image references land ahead of the question,
// absolute so the model is not left guessing the working directory, and an empty
// prompt yields just the references (the interactive pre-fill case).
func TestAttachImagesPrefixesPrompt(t *testing.T) {
	dir := t.TempDir()
	path := writeTempPNG(t)

	if got := AttachImages("what is this?", nil); got != "what is this?" {
		t.Errorf("AttachImages(no images) = %q, want the prompt unchanged", got)
	}
	got := AttachImages("what is this?", []string{path})
	if !strings.HasSuffix(got, " what is this?") {
		t.Errorf("AttachImages = %q, want the prompt last", got)
	}
	if !strings.Contains(got, "@image:") || !strings.Contains(got, path) {
		t.Errorf("AttachImages = %q, want an @image reference", got)
	}

	prefill := AttachImages("", []string{path})
	if prefill != "@image:"+path {
		t.Errorf("AttachImages(empty prompt) = %q, want just the reference", prefill)
	}

	// A relative path is made absolute so it resolves no matter what the model's
	// tools later treat as their root.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	rel := filepath.Join(dir, "pixel.png")
	if err := os.WriteFile(rel, pngBytes, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if got := AttachImages("", []string{"pixel.png"}); got != "@image:"+rel {
		t.Errorf("AttachImages(relative) = %q, want the absolute path %q", got, "@image:"+rel)
	}
}

// TestAttachImagesContentAttaches parses its own output back through
// BuildUserContent, which is the round trip cmd/pigo relies on for headless
// runs: the emitted reference must actually attach the image.
func TestAttachImagesContentAttaches(t *testing.T) {
	path := writeTempPNG(t)
	content, err := BuildUserContent(AttachImages("describe it", []string{path}))
	if err != nil {
		t.Fatalf("BuildUserContent: %v", err)
	}
	assertTextThenImage(t, content, "describe it", "image/png")
}
