// This file implements the classification of provider failures that name an
// image as the reason a request was rejected.
//
// The image triggers of the failed-request policy (tool_fallback.go) is built on two triggers, and
// this is the explicit one: the provider said the picture is the problem, so the
// request cannot succeed while it is attached. That is stronger evidence than the
// structural trigger, which guesses from the shape of the conversation, and it is
// the only trigger that can repair a picture that was *already* in the
// conversation — a user-attached image, a second screenshot, or a resumed session
// whose history carries it. Without it those sessions fail forever, because there
// is no request shape that "adds an image this time".
//
// The matching is text-based and therefore heuristic, which is why it is paired
// with the structural trigger rather than replacing it: a gateway that answers
// with a bare 400 is caught by the structural path, and an image-shaped error in a
// conversation with images is caught by this one. A false negative is the
// dangerous case (the session stays broken), so the phrase list errs toward
// matching; the false-positive risk is bounded by the policy itself — at most one
// retry per run, and only when the request actually carried an image.
package runtime

import "strings"

// imageErrorPhrases are the fragments providers use when an image is the reason
// for the rejection. They are lowercased; imageRejectionError lowercases its
// input. Each is paired with the requirement that the text mentions "image" at
// all, so a plain "invalid request" cannot match on "invalid" alone.
var imageErrorPhrases = []string{
	// OpenAI / OpenAI-compatible gateways.
	"unsupported image",
	"unsupported_image",
	"invalid image",
	"invalid_image",
	"image format",
	"failed to decode image",
	"could not decode image",
	"unable to process the image",
	"unable to process image",
	"image could not be processed",
	"supported formats",
	"supported values",
	// Anthropic.
	"media_type",
	"media type",
	"image.source",
	"input_image",
	"image_url",
	// Gemini.
	"unable to process input image",
	"invalid image data",
	// Generic validation echoes: an error that names an image mime type is about
	// the payload, not about the conversation.
	"image/png",
	"image/jpeg",
	"image/webp",
	"image/gif",
	// Formats only a rejection would name: no supported request ever mentions them.
	"image/heic",
	"image/heif",
	"image/avif",
	"image/bmp",
	"image/tiff",
	"heic",
	"avif",
	"image is too large",
	"image too large",
	"image_size",
}

// imageErrorPhrasesCN covers the same rejections from Chinese-language gateways,
// which the structural trigger would otherwise be the only answer for.
var imageErrorPhrasesCN = []string{"图片", "图像", "截图"}

// imageRejectionError reports whether the provider's error text says an image was
// the problem. An empty error, or one that never mentions an image, is not an
// image rejection: those keep the structural policy's narrower rules.
func imageRejectionError(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	if !strings.Contains(t, "image") {
		// Chinese gateways may never use the English word.
		for _, m := range imageErrorPhrasesCN {
			if strings.Contains(t, m) {
				return imageRejectionReaffirms(t)
			}
		}
		return false
	}
	for _, m := range imageErrorPhrases {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// imageRejectionReaffirms guards the Chinese markers: "图片" alone can appear in a
// prompt echo, so it must be joined by a rejection word. Kept deliberately small;
// the list covers "not supported" / "invalid" / "format error" / "failed".
func imageRejectionReaffirms(t string) bool {
	for _, m := range []string{"不支持", "无效", "格式", "失败", "不合法", "错误"} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}
