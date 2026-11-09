// This file implements the tests for the wire-safety half of the loader: which
// images may be put in a request at all, and what happens to the ones that may
// not.
//
// The rule under test is not "does the model support images" (that is the
// capability declaration) but "can this payload be transmitted at all". A hosted
// multimodal endpoint accepts exactly PNG, JPEG, GIF and WebP; anything else
// fails the whole request with a 400, and because the picture is already in the
// conversation, every later request fails the same way. These cases pin both
// halves of the answer: refuse the bad format at ingestion, and keep a session
// that already recorded one usable.
package imageload

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// jpegBytes is a real 1x1 JPEG, so the validity check has something to decode.
func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// bmpBytes is the header of a 1x1 BMP — recognized by Go's sniffer, refused by
// every hosted multimodal endpoint.
func bmpBytes() []byte {
	return []byte{'B', 'M', 0x3A, 0, 0, 0, 0, 0, 0, 0, 0x36, 0, 0, 0, 0x28, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0x18, 0}
}

// heicBytes is an ISO-BMFF header with the HEIC brand — the iPhone screenshot
// format, which Go's sniffer does not know and no hosted endpoint accepts.
func heicBytes() []byte {
	return append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00heic"), make([]byte, 32)...)
}

// webpBytes is a RIFF/WEBP header. Go cannot decode WebP, so the loader accepts
// it on the magic number alone — the same thing the providers do.
func webpBytes() []byte {
	return append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
}

// TestLoadRejectsUnsupportedFormat: a BMP is an image, but not one the wire
// accepts. The error must name the fix (convert to PNG/JPEG), because a model
// reads it and has to decide what to do next.
func TestLoadRejectsUnsupportedFormat(t *testing.T) {
	path := writeImage(t, t.TempDir(), "shot.bmp", bmpBytes())
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a BMP")
	}
	for _, want := range []string{"bmp", "not accepted", "PNG"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, missing %q", err, want)
		}
	}
}

// TestLoadRejectsHEIC is the same contract for the format that most often shows
// up by accident: an iPhone screenshot. The bytes, not the extension, decide, so
// a HEIC renamed to .png is refused as HEIC rather than labelled PNG.
func TestLoadRejectsHEIC(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"IMG_0001.heic", "IMG_0001.png"} {
		_, err := Load(writeImage(t, dir, name, heicBytes()))
		if err == nil {
			t.Fatalf("Load accepted a HEIC named %s", name)
		}
		if !strings.Contains(err.Error(), "heic") {
			t.Errorf("%s: error = %v, want the detected format named", name, err)
		}
	}
}

// TestLoadTrustsContentOverExtension is the fix for the other half of the
// problem: a JPEG saved as .png used to be labelled image/png and rejected by the
// provider as "unsupported image". The bytes win, so the same file is sent as
// what it is.
func TestLoadTrustsContentOverExtension(t *testing.T) {
	path := writeImage(t, t.TempDir(), "screenshot.png", jpegBytes(t))
	img, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if img.MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg from the bytes", img.MimeType)
	}
}

// TestLoadRejectsInvalidImage: a file that merely starts like a PNG is not one.
// Sending it would fail the whole request with "make sure your image is valid".
func TestLoadRejectsInvalidImage(t *testing.T) {
	path := writeImage(t, t.TempDir(), "broken.png", append(append([]byte{}, pngHeader...), []byte("not really a png")...))
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a truncated PNG")
	}
}

// TestCheckContentMismatch: the declared type and the bytes must agree — the
// exact contradiction the providers report as an unsupported image.
func TestCheckContentMismatch(t *testing.T) {
	if _, err := CheckContent("image/png", jpegBytes(t)); err == nil {
		t.Fatal("CheckContent accepted JPEG bytes declared as PNG")
	}
	if got, err := CheckContent("image/jpg", jpegBytes(t)); err != nil || got != "image/jpeg" {
		t.Fatalf("CheckContent(image/jpg) = %q, %v; want image/jpeg alias folded", got, err)
	}
	if _, err := CheckContent("image/png", nil); err == nil {
		t.Fatal("CheckContent accepted an empty payload")
	}
	if _, err := CheckContent("image/webp", webpBytes()); err != nil {
		t.Fatalf("CheckContent(webp) = %v, want nil", err)
	}
}

// TestSanitizeWireDropsUnsendableImages covers the resumed-session repair: the
// conversation on disk still holds an image no endpoint will take, so the request
// must carry a notice instead of failing every turn.
func TestSanitizeWireDropsUnsendableImages(t *testing.T) {
	okImage := agentcore.NewImageContent(base64.StdEncoding.EncodeToString(pngBytes), "image/png")
	history := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewTextContent("what is this?"),
			agentcore.NewImageContent(base64.StdEncoding.EncodeToString(heicBytes()), "image/heic"),
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-1", ToolName: "view_image", Content: agentcore.ContentList{
			okImage,
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-2", ToolName: "view_image", Content: agentcore.ContentList{
			// Declared PNG, actually JPEG: the mislabelled case.
			agentcore.NewImageContent(base64.StdEncoding.EncodeToString(jpegBytes(t)), "image/png"),
			agentcore.NewTextContent("viewed shot.png"),
		}},
	}

	out, dropped := SanitizeWire(history)

	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2 (the HEIC and the mislabelled PNG)", dropped)
	}
	if agentcore.MessagesHaveImage(history) != true {
		t.Fatal("SanitizeWire mutated the history")
	}
	if n := countImages(out); n != 1 {
		t.Errorf("request carries %d images, want 1 (the valid PNG)", n)
	}
	// The dropped pictures leave an explanation the model can act on, and the
	// tool's own text survives.
	text := agentcore.ContentToText(out[2].(agentcore.ToolResultMessage).Content)
	if !strings.Contains(text, "image omitted") || !strings.Contains(text, "viewed shot.png") {
		t.Errorf("degraded tool result = %q, want a notice plus the original descriptor", text)
	}
}

// TestSanitizeWireKeepsSupportedImages: the rule must not touch a healthy vision
// conversation, or it would silently blind every model.
func TestSanitizeWireKeepsSupportedImages(t *testing.T) {
	history := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewImageContent(base64.StdEncoding.EncodeToString(pngBytes), "image/png"),
			agentcore.NewImageContent(base64.StdEncoding.EncodeToString(jpegBytes(t)), "image/jpeg"),
			agentcore.NewImageContent(base64.StdEncoding.EncodeToString(webpBytes()), "image/webp"),
		}},
	}
	out, dropped := SanitizeWire(history)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if n := countImages(out); n != 3 {
		t.Errorf("request carries %d images, want all 3", n)
	}
}

// TestValidateRejectsUnsupportedFormat keeps the --image start-up check in step
// with Load: a path that would fail at the first turn must fail at launch.
func TestValidateRejectsUnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	if err := Validate(writeImage(t, dir, "shot.bmp", bmpBytes())); err == nil {
		t.Error("Validate accepted a BMP")
	}
	if err := Validate(writeImage(t, dir, "broken.png", []byte("nope"))); err == nil {
		t.Error("Validate accepted an invalid PNG")
	}
	if err := Validate(writeImage(t, dir, "ok.jpg", jpegBytes(t))); err != nil {
		t.Errorf("Validate(valid jpeg) = %v", err)
	}
}

// TestLoadAcceptsImageWithoutExtension guards the model's natural mistake: it
// writes a screenshot to a file with no suffix. Content sniffing still names it.
func TestLoadAcceptsImageWithoutExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot")
	if err := os.WriteFile(path, jpegBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if img.MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", img.MimeType)
	}
}

// countImages returns the number of image blocks in msgs.
func countImages(msgs agentcore.MessageList) int {
	n := 0
	for _, m := range msgs {
		var list agentcore.ContentList
		switch v := m.(type) {
		case agentcore.UserMessage:
			list = v.Content
		case agentcore.ToolResultMessage:
			list = v.Content
		}
		for _, c := range list {
			if _, ok := c.(agentcore.ImageContent); ok {
				n++
			}
		}
	}
	return n
}

// TestSVGIsNamedSoTheErrorIsActionable: SVG is text, so `read` must keep reading
// it as text, but handing it to view_image or @image: has to say "render it to
// PNG" rather than "not a recognized image".
func TestSVGIsNamedSoTheErrorIsActionable(t *testing.T) {
	if LooksLikeImage("diagram.svg") {
		t.Fatal("LooksLikeImage(.svg) = true; read would stop treating SVG source as text")
	}
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	path := writeImage(t, t.TempDir(), "diagram.svg", svg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted an SVG")
	}
	for _, want := range []string{"SVG", "PNG"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, missing %q", err, want)
		}
	}
}
