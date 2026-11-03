// This file implements the local-image input syntax for prompts (US-010/#126).
// A prompt may reference local image files with either `@image:<path>` or the
// Markdown image form `![alt](<path>)`. Each reference is read from disk,
// base64-encoded, and attached to the user message as an agentcore.ImageContent
// block so a multimodal model can see it. The remaining (non-reference) text is
// kept as a TextContent block. References that cannot be read are reported as an
// error rather than silently dropped, so the user knows the image was not sent.
package ui

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/imageload"
)

// imageRefPattern matches the two supported image-reference syntaxes:
//   - @image:<path>              (path runs to the next whitespace)
//   - ![alt](<path>)             (Markdown image; alt text is ignored)
//
// The path in the Markdown form may contain spaces; the @image form may not
// (whitespace terminates it), matching the convention that @image is a bare
// token while the Markdown form is explicitly delimited by parentheses.
var imageRefPattern = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)|@image:(\S+)`)

// BuildUserContent turns a raw prompt line into a content list. When the prompt
// contains no image references it returns a single TextContent (the common
// case). Otherwise it returns the interleaved text and image blocks, reading and
// base64-encoding each referenced file. It returns an error if any referenced
// file cannot be read, so the caller can surface it instead of sending a prompt
// that silently omits the image.
func BuildUserContent(prompt string) (agentcore.ContentList, error) {
	locs := imageRefPattern.FindAllStringSubmatchIndex(prompt, -1)
	if len(locs) == 0 {
		return agentcore.ContentList{agentcore.NewTextContent(prompt)}, nil
	}

	var content agentcore.ContentList
	// addText appends a text block for the given [lo,hi) slice of prompt,
	// trimming surrounding whitespace and skipping empties so we don't emit
	// blank text blocks around the references.
	addText := func(lo, hi int) {
		if lo >= hi {
			return
		}
		if t := strings.TrimSpace(prompt[lo:hi]); t != "" {
			content = append(content, agentcore.NewTextContent(t))
		}
	}

	prev := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		addText(prev, start)
		// Group 1 is the Markdown path; group 2 is the @image path. Exactly one
		// is set per match.
		var path string
		if loc[2] >= 0 {
			path = prompt[loc[2]:loc[3]]
		} else if loc[4] >= 0 {
			path = prompt[loc[4]:loc[5]]
		}
		img, err := loadImageContent(strings.TrimSpace(path))
		if err != nil {
			return nil, err
		}
		content = append(content, img)
		prev = end
	}
	addText(prev, len(prompt))

	if len(content) == 0 {
		content = agentcore.ContentList{agentcore.NewTextContent("")}
	}
	return content, nil
}

// loadImageContent reads an image file and returns an ImageContent with the
// data base64-encoded and the mime type sniffed from the file extension (with a
// content-sniff fallback). It errors if the file cannot be read, exceeds
// imageload.MaxImageBytes, or is not a recognizable image type. The work lives in
// package imageload so the read/view_image tools and this prompt syntax share one
// size limit and one sniffing rule.
func loadImageContent(path string) (agentcore.ImageContent, error) {
	return imageload.Load(path)
}

// mimeFromPath maps a file extension to an image mime type, returning "" for
// unknown extensions so the caller can fall back to content sniffing.
func mimeFromPath(path string) string {
	return imageload.MIMEFromPath(path)
}

// ImageRef renders path as the prompt image reference most likely to round-trip
// through imageRefPattern. The compact "@image:<path>" form terminates at
// whitespace, so a path containing a space is emitted as the Markdown form,
// which allows spaces inside its parentheses. Callers that already hold a
// content list should attach the block directly instead (imageload.Load) rather
// than going through a string.
func ImageRef(path string) string {
	if strings.ContainsAny(path, " \t\n") {
		return "![image](" + path + ")"
	}
	return "@image:" + path
}

// AttachImages returns prompt prefixed with a reference for each path, in order,
// so a single --image list lands ahead of the question the user typed. The text
// is returned unchanged when paths is empty. Paths that cannot be made absolute
// are used verbatim.
func AttachImages(prompt string, paths []string) string {
	if len(paths) == 0 {
		return prompt
	}
	refs := make([]string, 0, len(paths))
	for _, p := range paths {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		refs = append(refs, ImageRef(p))
	}
	prefix := strings.Join(refs, " ")
	if prompt == "" {
		return prefix
	}
	return prefix + " " + prompt
}
