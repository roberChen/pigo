// This file implements the shared "may the active model receive a picture?"
// check used by the two tools that can attach one (read on an image file, and
// view_image). Keeping it in one place matters so a third image-bearing tool does
// not invent its own answer.
//
// There is exactly one cause: the model is not declared vision-capable. The
// check is static and live (the probe is called per execution, so a /model switch
// takes effect on the next call).
//
// A failed request no longer arms a run-wide block. When the loop repairs a turn
// (runtime/image_fallback.go) it removes the picture that request added, but it
// does not disable the tools: a later view_image still returns the image, and if
// the endpoint refuses it the model sees the provider's error and decides what to
// do next. Deciding "this endpoint cannot take images at all" from one failure is
// the model's call, not a latch pigo sets behind its back.
package agenttool

// imageUnavailableReason returns a sentence telling the model why it is not
// getting a picture, or "" when the model is declared vision-capable. declared is
// the capability probe handed to the tool at registration; nil keeps the
// permissive default built-in models have always had.
func imageUnavailableReason(declared func() bool) string {
	if declared != nil && !declared() {
		return "the configured model is not declared as a vision model, so it cannot accept image input " +
			"(declare one with supports_images = true for this model in the [[models]] table of config.toml, " +
			"or `model add <id> --supports-images` in the pigorc)"
	}
	return ""
}
