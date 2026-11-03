// Package imageload reads local image files into agentcore.ImageContent blocks,
// the single shared implementation behind every image-input path in pigo: the
// prompt reference syntax (@image:<path> / ![alt](<path>), see internal/cli/ui),
// the read tool when it is pointed at an image, and the view_image tool. It
// sniffs the mime type from the extension with a content-sniff fallback, rejects
// anything that is not an image, and base64-encodes the bytes.
//
// Sizes are enforced here rather than at each call site: a VL model receives the
// image inline, so an unbounded file would blow the context budget (and, for the
// tools, the executor's result clipping cannot help — image blocks bypass it by
// design). The limit is per image; callers that accept several images are
// expected to check the total themselves.
package imageload

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// MaxImageBytes caps a single image file. 8 MiB matches the common per-image
// ceiling of the hosted multimodal APIs (Anthropic: 5 MB base64 ≈ 3.75 MB raw,
// OpenAI: 20 MB) with headroom for PNG screenshots, while staying well below the
// point where base64 expansion would crowd out a conversation. Oversized files
// get a clear error naming the limit, not a truncated image.
const MaxImageBytes = 8 << 20

// imageExtensions maps the extensions pigo recognizes to their mime type. Order
// matters only for the doc comment; lookup is exact on the lowercased suffix.
var imageExtensions = []struct {
	ext  string
	mime string
}{
	{".png", "image/png"},
	{".jpg", "image/jpeg"},
	{".jpeg", "image/jpeg"},
	{".gif", "image/gif"},
	{".webp", "image/webp"},
	{".bmp", "image/bmp"},
	{".tif", "image/tiff"},
	{".tiff", "image/tiff"},
	{".ico", "image/x-icon"},
	{".avif", "image/avif"},
	{".heic", "image/heic"},
	{".heif", "image/heif"},
}

// MIMEFromPath maps a file extension to an image mime type, returning "" for an
// unrecognized extension so the caller can fall back to content sniffing.
func MIMEFromPath(path string) string {
	lower := strings.ToLower(path)
	for _, e := range imageExtensions {
		if strings.HasSuffix(lower, e.ext) {
			return e.mime
		}
	}
	return ""
}

// LooksLikeImage reports whether path's extension names a known image type. It
// is a cheap name check for tools that want to branch before touching the disk;
// a false positive still gets a definitive answer from Load.
func LooksLikeImage(path string) bool { return MIMEFromPath(path) != "" }

// DetectMIME returns the image mime type for data read from path: the extension
// mapping when it is known, otherwise http.DetectContentType. It returns "" when
// the bytes are not an image.
func DetectMIME(path string, data []byte) string {
	if mime := MIMEFromPath(path); mime != "" {
		return mime
	}
	if mime := http.DetectContentType(data); strings.HasPrefix(mime, "image/") {
		return mime
	}
	return ""
}

// Load reads path and returns it as a base64 ImageContent. It errors when the
// file cannot be read, exceeds MaxImageBytes, or is not a recognizable image —
// never silently returning an empty block, so the caller can tell the model (or
// the user) that no image was attached.
func Load(path string) (agentcore.ImageContent, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return agentcore.ImageContent{}, fmt.Errorf("read image %q: %w", path, err)
	}
	if fi.IsDir() {
		return agentcore.ImageContent{}, fmt.Errorf("%q is a directory, not an image file", path)
	}
	if fi.Size() > MaxImageBytes {
		return agentcore.ImageContent{}, fmt.Errorf("image %q is %d bytes, over the %d-byte limit (resize or crop it first)",
			path, fi.Size(), MaxImageBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return agentcore.ImageContent{}, fmt.Errorf("read image %q: %w", path, err)
	}
	mime := DetectMIME(path, data)
	if mime == "" {
		return agentcore.ImageContent{}, fmt.Errorf("%q is not a recognized image (detected %q)", path, http.DetectContentType(data))
	}
	enc := base64.StdEncoding.EncodeToString(data)
	return agentcore.NewImageContent(enc, mime), nil
}

// Validate checks that path names a readable image within MaxImageBytes without
// base64-encoding it. It exists for start-up validation of a user-supplied
// --image list, where paying for the encoding twice (once to check, once to
// build the message) would be wasteful. It reads only the first 512 bytes, which
// is all content sniffing needs.
func Validate(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("read image %q: %w", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("%q is a directory, not an image file", path)
	}
	if fi.Size() > MaxImageBytes {
		return fmt.Errorf("image %q is %d bytes, over the %d-byte limit (resize or crop it first)",
			path, fi.Size(), MaxImageBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read image %q: %w", path, err)
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fmt.Errorf("read image %q: %w", path, err)
	}
	if DetectMIME(path, head[:n]) == "" {
		return fmt.Errorf("%q is not a recognized image (detected %q)", path, http.DetectContentType(head[:n]))
	}
	return nil
}

// Describe summarizes an attached image for a text-only companion block: the
// original path, the mime type and the raw (pre-base64) byte size. Tools put it
// in the same result as the image so a model that cannot see the picture still
// knows what was attached and where it came from.
func Describe(path string, img agentcore.ImageContent) string {
	// 4 base64 chars carry 3 bytes; padding makes this an upper bound, and it is
	// only used for a human-readable size, so approximate is fine.
	raw := len(img.Data) / 4 * 3
	return fmt.Sprintf("%s (%s, ~%d bytes)", path, img.MimeType, raw)
}
