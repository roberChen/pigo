// This file implements the tests for the image-rejection classifier: the
// text-based trigger that lets the repair reach a picture already in the
// conversation (a resumed session, a user attachment, a second screenshot).
//
// The asymmetry is deliberate: a false negative leaves the session permanently
// broken, a false positive costs one retry that drops images from a turn which
// had already failed. The table therefore pins the messages providers really
// send — including their non-English and unstructured shapes — and the errors
// that must *not* be mistaken for one.
package runtime

import "testing"

func TestImageRejectionError(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "openai unsupported image",
			text: `transport: upstream 400: {"error":{"message":"input[207].image[0]: You have uploaded an unsupported image. Please make sure your image is valid and has one of the following formats: webp, png, jpeg, and gif."}}`,
			want: true,
		},
		{
			name: "anthropic media type",
			text: `transport: upstream 400: {"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.image.source.base64.media_type: Input should be 'image/jpeg', 'image/png', 'image/gif' or 'image/webp'"}}`,
			want: true,
		},
		{
			name: "openai supported values",
			text: `400 Invalid value: 'image/bmp'. Supported values are: 'image/png', 'image/jpeg', 'image/webp', and 'image/gif'.`,
			want: true,
		},
		{
			name: "gemini unable to process",
			text: `Unable to process input image. Please retry or report in https://help.gemini.com`,
			want: true,
		},
		{
			name: "generic invalid image",
			text: `invalid image data: failed to decode image`,
			want: true,
		},
		{
			name: "image too large",
			text: `upstream 413: image is too large`,
			want: true,
		},
		{
			name: "chinese gateway",
			text: `{"error":{"message":"图片格式不支持，请上传 png 或 jpeg"}}`,
			want: true,
		},
		{
			name: "structured gateway says HEIC unsupported",
			text: `upstream 400: {"error":{"message":"image/heic is not supported"}}`,
			want: true,
		},
		{
			name: "opaque 400 must not match",
			text: `transport: upstream 400 Bad Request`,
			want: false,
		},
		{
			name: "empty",
			text: ``,
			want: false,
		},
		{
			name: "context length",
			text: `transport: upstream 400: maximum context length exceeded`,
			want: false,
		},
		{
			name: "auth",
			text: `transport: upstream 401: invalid api key`,
			want: false,
		},
		{
			name: "rate limit",
			text: `transport: upstream 429: rate limit exceeded`,
			want: false,
		},
		{
			name: "image generation, not image input",
			text: `invalid request: image generation is not enabled for this model`,
			want: false,
		},
		{
			name: "chinese without an image word",
			text: `请求无效：上下文过长`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageRejectionError(tc.text); got != tc.want {
				t.Fatalf("imageRejectionError(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
