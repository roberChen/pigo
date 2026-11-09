// Package imageload reads local image files into agentcore.ImageContent blocks,
// the single shared implementation behind every image-input path in pigo: the
// prompt reference syntax (@image:<path> / ![alt](<path>), see internal/cli/ui),
// the read tool when it is pointed at an image, and the view_image tool.
//
// It also owns the one question every one of those paths must answer the same
// way: *may this picture be put on the wire?* A hosted multimodal endpoint
// accepts exactly four image types (PNG, JPEG, GIF, WebP); anything else — BMP,
// TIFF, ICO, AVIF, HEIC, a PNG whose bytes are actually a JPEG, a truncated
// file — comes back as a 400 that fails the whole request, and because the
// picture is already in the conversation, every later request (including a
// resumed session's) fails the same way. So the loader refuses those at
// ingestion with an actionable message, and SanitizeWire re-checks messages that
// were recorded before this rule existed.
//
// Sizes are enforced here rather than at each call site: a VL model receives the
// image inline, so an unbounded file would blow the context budget (and, for the
// tools, the executor's result clipping cannot help — image blocks bypass it by
// design). The limit is per image; callers that accept several images are
// expected to check the total themselves.
package imageload

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	// Register the decoders CheckContent validates with image.DecodeConfig. The
	// blank imports are load-bearing: without them DecodeConfig answers
	// image.ErrFormat for every format.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/diag"
)

// MaxImageBytes caps a single image file. 8 MiB matches the common per-image
// ceiling of the hosted multimodal APIs (Anthropic: 5 MB base64 ≈ 3.75 MB raw,
// OpenAI: 20 MB) with headroom for PNG screenshots, while staying well below the
// point where base64 expansion would crowd out a conversation. Oversized files
// get a clear error naming the limit, not a truncated image.
const MaxImageBytes = 8 << 20

// WireMIMETypes is the set of image types a hosted multimodal endpoint accepts.
// It is the union of what OpenAI and Anthropic document (both list exactly these
// four), so a request built from this set cannot be rejected as an unsupported
// format by either. Everything pigo can *name* but not *send* (BMP, TIFF, ICO,
// AVIF, HEIC, JPEG XL) is refused at ingestion with a message telling the caller
// to convert it.
var WireMIMETypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// SupportedFormats is the human-readable form of WireMIMETypes, used in errors so
// the fix ("convert it to PNG") travels with the message to the model.
const SupportedFormats = "PNG, JPEG, GIF or WebP"

// imageExtensions maps the extensions pigo recognizes to their mime type. It is a
// *naming* table, not a permission list: an extension only decides the mime when
// the bytes have no recognizable magic number, and CheckContent then decides
// whether that type may go on the wire. Lookup is exact on the lowercased suffix.
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
	{".jxl", "image/jxl"},
}

// MIMEFromPath maps a file extension to an image mime type, returning "" for an
// unrecognized extension.
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

// CanonicalMIME lowercases mime, drops any parameters ("image/png; charset=…")
// and folds the aliases seen in the wild onto the registered names, so the
// comparison against WireMIMETypes is a plain map lookup.
func CanonicalMIME(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "image/jpg", "image/pjpeg":
		return "image/jpeg"
	case "image/x-png":
		return "image/png"
	case "image/x-webp":
		return "image/webp"
	}
	return m
}

// SupportedMIME reports whether mime is one of WireMIMETypes.
func SupportedMIME(mime string) bool { return WireMIMETypes[CanonicalMIME(mime)] }

// SniffMIME returns the image mime type of data by its magic number, or "" when
// the bytes are not a recognized image. It knows the formats the hosted APIs
// refuse (so the error can name them) as well as the four they accept.
func SniffMIME(data []byte) string {
	if m := sniffMagic(data); m != "" {
		return m
	}
	if m := http.DetectContentType(data); strings.HasPrefix(m, "image/") {
		return CanonicalMIME(m)
	}
	// SVG has no magic number, but it is the one format a user reaches for
	// constantly and no endpoint will take; naming it turns a confusing "not a
	// recognized image" into "render it to PNG". Only the prefix is searched, and
	// only after the binary formats above have failed, so a text file is never
	// mistaken for an image by a stray tag far down the file.
	if bytes.Contains(bytes.ToLower(head(data, 1024)), []byte("<svg")) {
		return "image/svg+xml"
	}
	return ""
}

// sniffMagic covers the formats http.DetectContentType does not know (AVIF,
// HEIC/HEIF, TIFF, JPEG XL) and WebP, whose RIFF header Go does recognize but
// only from a longer prefix than some callers hold.
func sniffMagic(data []byte) string {
	switch {
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		switch string(data[8:12]) {
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "heim", "heis", "hevc", "hevx":
			return "image/heic"
		case "mif1", "msf1":
			return "image/heif"
		}
	case len(data) >= 4 && (bytes.Equal(data[:4], []byte{'I', 'I', 0x2A, 0x00}) || bytes.Equal(data[:4], []byte{'M', 'M', 0x00, 0x2A})):
		return "image/tiff"
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0x0A:
		return "image/jxl"
	}
	return ""
}

// DetectMIME returns the image mime type for data read from path. The bytes win:
// a JPEG saved as screenshot.png is reported as image/jpeg, which is exactly the
// mix-up that used to reach the wire as a bogus image/png and come back as
// "unsupported image". The extension is only consulted when the magic number is
// unrecognized, so the error can name the format instead of saying "unknown".
// It returns "" when the data is neither a recognizable image nor named one.
func DetectMIME(path string, data []byte) string {
	if mime := SniffMIME(data); mime != "" {
		return mime
	}
	return CanonicalMIME(MIMEFromPath(path))
}

// CheckContent validates an image about to enter a conversation: its declared or
// detected type must be a wire type, and its bytes must be a real image of that
// type. It returns the canonical mime to store.
//
// The byte check is deliberately header-only (image.DecodeConfig): a full decode
// would decompress every screenshot twice and expose pigo to decompression
// bombs, while the header catches the failures the providers report — a file
// that is not the format it claims, or a truncated/empty one.
func CheckContent(mime string, data []byte) (string, error) {
	canonical := CanonicalMIME(mime)
	switch {
	case canonical == "":
		return "", fmt.Errorf("not a recognized image (detected %q)", http.DetectContentType(data))
	case canonical == "image/svg+xml":
		// A common mistake with a clear fix; SVG is text, not a bitmap.
		return "", fmt.Errorf("SVG images are not accepted by multimodal endpoints (supported: %s); render it to PNG first", SupportedFormats)
	case !WireMIMETypes[canonical]:
		return "", fmt.Errorf("%s images are not accepted by multimodal endpoints (supported: %s); convert the file to PNG or JPEG first",
			strings.TrimPrefix(canonical, "image/"), SupportedFormats)
	}
	if sniffed := SniffMIME(head(data, 512)); sniffed != "" && sniffed != canonical {
		return "", fmt.Errorf("declared %s but the bytes are %s; rename or re-encode the file so the type matches its contents",
			canonical, sniffed)
	}
	if canonical == "image/webp" {
		// Go has no WebP decoder, so the RIFF/WEBP magic above is all that can be
		// checked cheaply. The providers decode it; if the body is truncated they
		// will say so, and the runtime's repair handles that failure.
		return canonical, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("not a valid %s image (%v)", strings.TrimPrefix(canonical, "image/"), err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "", fmt.Errorf("not a valid %s image (empty dimensions)", strings.TrimPrefix(canonical, "image/"))
	}
	return canonical, nil
}

// CheckWire re-validates an ImageContent that is already in the conversation, for
// the request path. It answers only what the wire cares about — a supported type
// whose bytes agree with it — and reads a short base64 prefix rather than the
// whole payload, so a long vision session pays nothing measurable per turn.
func CheckWire(img agentcore.ImageContent) error {
	canonical := CanonicalMIME(img.MimeType)
	if !WireMIMETypes[canonical] {
		return fmt.Errorf("%s is not accepted by multimodal endpoints (supported: %s)",
			shortMIME(canonical), SupportedFormats)
	}
	head, err := decodeHead(img.Data, 512)
	if err != nil {
		return fmt.Errorf("the stored %s data is not valid base64 (%v)", shortMIME(canonical), err)
	}
	if len(head) == 0 {
		return fmt.Errorf("the stored %s image is empty", shortMIME(canonical))
	}
	if sniffed := SniffMIME(head); sniffed != "" && sniffed != canonical {
		return fmt.Errorf("the stored image claims %s but its bytes are %s", shortMIME(canonical), shortMIME(sniffed))
	}
	return nil
}

// SanitizeWire returns a copy of msgs in which every image the wire cannot carry
// is replaced by a text notice, plus the number dropped. History is untouched:
// the dropped picture is still in the session (the UI can show it, and a later
// model that accepts it — none do today — would see it again).
//
// This is the backstop for conversations recorded before ingestion was
// restricted, most importantly a *resumed* session: it contains the offending
// picture on disk, so without this every turn of every resume would fail with the
// same 400 and the session would be unusable forever.
func SanitizeWire(msgs agentcore.MessageList) (agentcore.MessageList, int) {
	dropped := 0
	out := agentcore.ReplaceImages(msgs, func(img agentcore.ImageContent) agentcore.ContentList {
		if err := CheckWire(img); err != nil {
			dropped++
			warnDroppedOnce(err)
			return agentcore.ContentList{agentcore.NewTextContent(ImageDroppedNotice(err))}
		}
		return agentcore.ContentList{img}
	})
	return out, dropped
}

// warnDroppedReason records the reasons already reported, so a session whose
// history holds an unsendable picture warns once instead of on every turn: the
// request is sanitized on each one, and a diagnostic per turn would flood the TUI
// transcript (diag lines are folded into it) for as long as the session lasts.
var warnDroppedReason sync.Map

// warnDroppedOnce emits the diagnostic for a wire drop the first time its reason
// is seen in this process.
func warnDroppedOnce(err error) {
	key := err.Error()
	if _, seen := warnDroppedReason.LoadOrStore(key, struct{}{}); seen {
		return
	}
	diag.Diagf("dropped an image from the request: %v", err)
}

// ImageDroppedNotice explains to the model (and through the transcript, to the
// user) why the picture is gone. It names the reason so the model can ask for a
// converted file instead of guessing.
func ImageDroppedNotice(reason error) string {
	return "[image omitted: " + reason.Error() + "]"
}

// Load reads path and returns it as a base64 ImageContent. It errors when the
// file cannot be read, exceeds MaxImageBytes, is not a recognizable image, is not
// a type the wire accepts, or does not match its own declared type — never
// silently returning an empty or unusable block, so the caller can tell the model
// (or the user) what to do instead.
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
	mime, err := CheckContent(DetectMIME(path, data), data)
	if err != nil {
		return agentcore.ImageContent{}, fmt.Errorf("image %q: %w", path, err)
	}
	enc := base64.StdEncoding.EncodeToString(data)
	return agentcore.NewImageContent(enc, mime), nil
}

// Validate checks that path names a readable image within MaxImageBytes that the
// wire can carry, without base64-encoding it. It exists for start-up validation
// of a user-supplied --image list, where the encoding would only be paid twice.
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
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read image %q: %w", path, err)
	}
	if _, err := CheckContent(DetectMIME(path, data), data); err != nil {
		return fmt.Errorf("image %q: %w", path, err)
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

// shortMIME strips the "image/" prefix for error text ("heic", not "image/heic").
func shortMIME(mime string) string { return strings.TrimPrefix(mime, "image/") }

// head returns the first n bytes of data.
func head(data []byte, n int) []byte {
	if len(data) <= n {
		return data
	}
	return data[:n]
}

// decodeHead decodes at most n bytes from the front of a base64 payload. Only
// whole 4-character groups are decoded, so a prefix never trips over padding.
func decodeHead(encoded string, n int) ([]byte, error) {
	groups := (n + 2) / 3
	end := groups * 4
	if end > len(encoded) {
		end = len(encoded)
	}
	end -= end % 4
	if end == 0 {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded[:end])
	if err != nil {
		return nil, err
	}
	return head(raw, n), nil
}
