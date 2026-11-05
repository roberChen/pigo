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
