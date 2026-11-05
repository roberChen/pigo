// This file implements image-block stripping for models that do not accept
// image input. A session may hold images that the current model cannot see: the
// model was switched to a text-only one mid-session, or a session recorded
// against a vision model is resumed on a different model. Sending those blocks
// anyway fails the whole request at the wire, which would take the turn down
// instead of the picture.
//
// Stripping is a request-assembly concern, not a persistence one: the session
// keeps its images so switching back to a vision model (or exporting) still
// shows them. Only the copy bound for the LLM is flattened.
//
// The same file implements the repair side: removing images from the
// conversation itself (StripImagesInHistory) for the one failure shape the loop
// repairs (runtime/image_fallback.go) — a request that added a picture through a
// tool call, in a conversation that had none before it, for a model the user did
// not declare vision-capable, so the run can retry the turn without the picture.
package agentcore

// ImageOmittedNotice replaces an image block the target model cannot accept. It
// is visible to the model so it knows a picture existed and can say so, rather
// than silently losing the reference.
const ImageOmittedNotice = "[image omitted: this model does not accept image input]"

// StripImages returns a shallow copy of msgs with every ImageContent block
// replaced by a text notice. Messages without images are returned unchanged
// (same value), so a text-only conversation pays no allocation per turn.
//
// Tool results are stripped too: a vision tool (read on a PNG, view_image) hands
// the model an image block inside the tool result, which a text-only model must
// not receive for the same reason.
func StripImages(msgs MessageList) MessageList {
	needs := false
	for _, m := range msgs {
		if messageHasImage(m) {
			needs = true
			break
		}
	}
	if !needs {
		return msgs
	}
	out := make(MessageList, len(msgs))
	for i, m := range msgs {
		switch v := m.(type) {
		case UserMessage:
			v.Content, _ = StripImagesFromContent(v.Content)
			out[i] = v
		case ToolResultMessage:
			v.Content, _ = StripImagesFromContent(v.Content)
			out[i] = v
		case AssistantMessage:
			// Assistant turns cannot carry images today, but handle it so a
			// future block type cannot leak one to a text-only wire.
			v.Content, _ = StripImagesFromContent(v.Content)
			out[i] = v
		default:
			out[i] = m
		}
	}
	return out
}

// ContentHasImage reports whether list carries at least one image block. Callers
// use it to warn a user whose prompt attached an image the model cannot accept,
// before the turn starts.
func ContentHasImage(list ContentList) bool {
	for _, c := range list {
		if _, ok := c.(ImageContent); ok {
			return true
		}
	}
	return false
}

// messageHasImage reports whether m carries an image block.
func messageHasImage(m Message) bool {
	var content ContentList
	switch v := m.(type) {
	case UserMessage:
		content = v.Content
	case ToolResultMessage:
		content = v.Content
	case AssistantMessage:
		content = v.Content
	default:
		return false
	}
	for _, c := range content {
		if _, ok := c.(ImageContent); ok {
			return true
		}
	}
	return false
}

// StripImagesFromContent returns list with images replaced by a text notice,
// reporting whether anything was dropped. A list with no images is returned
// as-is, so call sites can skip a user-facing warning when nothing changed.
func StripImagesFromContent(list ContentList) (ContentList, bool) {
	has := false
	for _, c := range list {
		if _, ok := c.(ImageContent); ok {
			has = true
			break
		}
	}
	if !has {
		return list, false
	}
	out := make(ContentList, 0, len(list))
	for _, c := range list {
		if _, ok := c.(ImageContent); ok {
			out = append(out, NewTextContent(ImageOmittedNotice))
			continue
		}
		out = append(out, c)
	}
	return out, true
}

// MessagesHaveImage reports whether any message in msgs carries an image block.
func MessagesHaveImage(msgs MessageList) bool {
	for _, m := range msgs {
		if messageHasImage(m) {
			return true
		}
	}
	return false
}

// ImageStrippedNoticeText explains, to the model, why the picture it was given
// earlier is gone: the request carrying it failed, and pigo removed it to rule
// that out. It is deliberately error-agnostic — the fallback never reads the
// provider's error text (see runtime/image_fallback.go), so the notice must not
// claim the provider named images as the cause.
const ImageStrippedNoticeText = "the conversation contained an image when a request failed, so the image was removed to rule it out and the turn was retried without it; the tool call that produced it is reported as failed"

// StripImagesInHistory removes every image block from msgs *in place*, which is
// the persistent counterpart of StripImages: StripImages flattens only the copy
// bound for the wire, this repairs the conversation itself so a later run (whose
// request assembly has no memory of the failure) cannot resend the same picture
// and fail the same way.
//
// Tool results that carried an image are rewritten as failed calls — the image
// is dropped, the remaining text is kept (so the model still learns which file
// it was), a notice quoting the provider error is appended, and IsError is set.
// User and assistant messages merely lose the picture: a prompt is not the
// model's mistake, so it is replaced by the omission notice rather than being
// turned into an error. The loop only calls this when the conversation had no
// image before the failed request, so in practice the stripped images are the
// tool-produced ones.
//
// The rewritten tool results are returned so the caller can re-emit their
// ToolExecutionEndEvent, flipping the cards the UI already rendered.
func StripImagesInHistory(msgs MessageList, providerErr string) []ToolResultMessage {
	notice := ImageStrippedNoticeText
	if providerErr != "" {
		notice += " (the failed request reported: " + providerErr + ")"
	}
	var repaired []ToolResultMessage
	for i, m := range msgs {
		switch v := m.(type) {
		case ToolResultMessage:
			if !ContentHasImage(v.Content) {
				continue
			}
			kept := make(ContentList, 0, len(v.Content)+1)
			for _, c := range v.Content {
				if _, isImage := c.(ImageContent); isImage {
					continue
				}
				kept = append(kept, c)
			}
			v.Content = append(kept, NewTextContent("["+notice+"]"))
			v.IsError = true
			msgs[i] = v
			repaired = append(repaired, v)
		case UserMessage:
			if !ContentHasImage(v.Content) {
				continue
			}
			v.Content = replaceImagesWithNotice(v.Content, "["+notice+"]")
			msgs[i] = v
		case AssistantMessage:
			if !ContentHasImage(v.Content) {
				continue
			}
			v.Content = replaceImagesWithNotice(v.Content, "["+notice+"]")
			msgs[i] = v
		}
	}
	return repaired
}

// replaceImagesWithNotice swaps every image block for text, leaving the other
// blocks in order. list is assumed to carry at least one image.
func replaceImagesWithNotice(list ContentList, text string) ContentList {
	out := make(ContentList, 0, len(list))
	for _, c := range list {
		if _, isImage := c.(ImageContent); isImage {
			continue
		}
		out = append(out, c)
	}
	return append(out, NewTextContent(text))
}
