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
// repairs (runtime/tool_fallback.go) — a request that added a picture through a
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
	return ReplaceImages(msgs, func(ImageContent) ContentList {
		return ContentList{NewTextContent(ImageOmittedNotice)}
	})
}

// ReplaceImages returns a shallow copy of msgs with every ImageContent block
// replaced by the blocks replace returns for it. Messages without images are
// returned unchanged (same value), so a text-only conversation pays no
// allocation per turn.
//
// It is the one place that knows which message types can carry a picture, so a
// new rewrite rule (strip for a text-only model, drop a malformed image before
// the wire, repair a failed request) only has to supply the replacement.
func ReplaceImages(msgs MessageList, replace func(ImageContent) ContentList) MessageList {
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
			v.Content = replaceImagesInContent(v.Content, replace)
			out[i] = v
		case ToolResultMessage:
			v.Content = replaceImagesInContent(v.Content, replace)
			out[i] = v
		case AssistantMessage:
			// Assistant turns cannot carry images today, but handle it so a
			// future block type cannot leak one to a text-only wire.
			v.Content = replaceImagesInContent(v.Content, replace)
			out[i] = v
		default:
			out[i] = m
		}
	}
	return out
}

// replaceImagesInContent rebuilds list, substituting each image block. A list
// with no image is returned as-is.
func replaceImagesInContent(list ContentList, replace func(ImageContent) ContentList) ContentList {
	has := false
	for _, c := range list {
		if _, ok := c.(ImageContent); ok {
			has = true
			break
		}
	}
	if !has {
		return list
	}
	out := make(ContentList, 0, len(list))
	for _, c := range list {
		if img, ok := c.(ImageContent); ok {
			out = append(out, replace(img)...)
			continue
		}
		out = append(out, c)
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
// earlier is gone: the request carrying it failed, and pigo removed every image
// and retried without them. It is deliberately error-agnostic in its first
// sentence — the provider's own text is appended by StripImagesInHistory when it
// named a cause — so the notice reads correctly whether the request failed on the
// picture or on something else.
const ImageStrippedNoticeText = "the conversation contained an image when a request failed, so the image was removed and the turn was retried without it; the tool call that produced it is reported as failed"

// StripImagesInHistory removes every image block from msgs *in place*, which is
// the persistent counterpart of StripImages: StripImages flattens only the copy
// bound for the wire, this repairs the conversation itself so a later run (whose
// request assembly has no memory of the failure) cannot resend the same picture
// and fail the same way.
//
// Tool results that carried an image are rewritten as abnormal placeholders — the
// image is dropped, the remaining text is kept (so the model still learns which
// file it was), and the call becomes an error that quotes the provider's error
// (abnormal.go). User and assistant messages merely lose the picture: a prompt is
// not the model's mistake, so it is replaced by the omission notice rather than
// being turned into an error. The loop calls this when an image is a reason the
// request failed — either because the provider named one, or because the request's
// shape isolates the one it just added (runtime/tool_fallback.go) — so the
// stripped images are in practice the tool-produced ones, or every image in a
// conversation the endpoint refuses to carry.
//
// The rewritten tool results are returned so the caller can re-emit their
// ToolExecutionEndEvent, flipping the cards the UI already rendered.
func StripImagesInHistory(msgs MessageList, providerErr string) []ToolResultMessage {
	var repaired []ToolResultMessage
	for i := 0; i < len(msgs); i++ {
		switch v := msgs[i].(type) {
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
			detail := AbnormalToolResult{
				ProviderError: providerErr,
				Reason:        ImageStrippedNoticeText,
			}
			// The arguments live on the assistant turn, not on the result: recover them
			// so the placeholder can tell the model what the call was asked to do.
			if call, ok := FindToolCall(msgs, v.ToolCallID); ok {
				detail.Arguments = string(call.Arguments)
			}
			ph := NewAbnormalToolResult(v.ToolCallID, v.ToolName, detail, v.Timestamp)
			// Keep whatever text the result already carried (the file it saw, prose
			// the model can still use) ahead of the placeholder explanation.
			ph.Content = append(kept, ph.Content...)
			msgs[i] = ph
			repaired = append(repaired, ph)
		case UserMessage:
			if !ContentHasImage(v.Content) {
				continue
			}
			notice := ImageStrippedNoticeText
			if providerErr != "" {
				notice += " (the failed request reported: " + providerErr + ")"
			}
			v.Content = replaceImagesWithNotice(v.Content, "["+notice+"]")
			msgs[i] = v
		case AssistantMessage:
			if !ContentHasImage(v.Content) {
				continue
			}
			notice := ImageStrippedNoticeText
			if providerErr != "" {
				notice += " (the failed request reported: " + providerErr + ")"
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
