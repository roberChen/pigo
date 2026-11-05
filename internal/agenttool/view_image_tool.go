// This file implements the view_image tool for multimodal (VL) models: it reads
// a local image file and returns it as an agentcore.ImageContent block so the
// model actually sees the picture, plus a short text line naming the path and
// type so a text-only model (and the transcript) still knows what was attached.
//
// Why a separate tool rather than only teaching `read` to branch: `read` is a
// text tool with line/offset semantics, and the prompt reference syntax
// (@image:<path>) is a human shortcut. A model working through a repository has
// no way to use either — it needs a tool whose description says "look at this
// image". `read` also accepts images (a model naturally tries it first), and both
// share package imageload for the size limit and mime sniffing.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/imageload"
)

// ViewImageTool loads an image file for the model to look at. It is read-only,
// so it executes in parallel with other read-only tools.
type ViewImageTool struct {
	// Root bounds the paths it may read. Empty defaults to the working directory.
	Root string
	// ExtraRoots are additional trusted directories (the skills dir), matching
	// ReadTool so the model can view images referenced from a skill.
	ExtraRoots []string
	// ImagesSupported reports whether the active model accepts image input. It is
	// called per execution rather than captured at assembly, so a /model switch
	// takes effect immediately. Nil means "assume supported": the permissive
	// default every built-in model has always had.
	ImagesSupported func() bool
}

// SetImagesSupported installs the capability probe used by Execute. It exists so
// a driver that rebuilds its tool set can attach the current model's capability
// without knowing the concrete type.
func (t *ViewImageTool) SetImagesSupported(f func() bool) { t.ImagesSupported = f }

// viewImageToolArgs is the decoded argument shape for ViewImageTool.
type viewImageToolArgs struct {
	// Path is the image file to view, relative to Root (or absolute within it).
	Path string `json:"path"`
}

// Name implements AgentTool. Lowercase multi-word with underscores, per the
// project's tool-name convention.
func (t *ViewImageTool) Name() string { return "view_image" }

// Description implements AgentTool.
func (t *ViewImageTool) Description() string {
	return "View a local image file (PNG, JPEG, GIF, WebP, BMP, TIFF, ...) with a vision-capable " +
		"model. Returns the image itself plus its path and mime type. Use it to inspect screenshots, " +
		"diagrams, charts, photos or UI mockups in the workspace. Only works when the configured " +
		"model accepts image input."
}

// Schema implements AgentTool.
func (t *ViewImageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Image file path, relative to the workspace root (or absolute within it)."}
  },
  "required": ["path"],
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. Reads are side-effect free → parallel.
func (t *ViewImageTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements AgentTool. Read/format failures are encoded as error results
// so the model can correct its path; the Go error is reserved for decode
// failures.
func (t *ViewImageTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[viewImageToolArgs](args, "view_image")
	if bad != nil {
		return *bad, nil
	}
	// Refuse before loading the file: a model that is not declared vision-capable
	// cannot act on a picture, so loading one would only waste the read.
	if reason := imageUnavailableReason(t.ImagesSupported); reason != "" {
		return errorResult("view_image: " + reason + "; describe the file by path instead"), nil
	}
	if a.Path == "" {
		return errorResult("view_image: path is required"), nil
	}
	full, err := resolveImagePath(t.Root, t.ExtraRoots, a.Path)
	if err != nil {
		return errorResult("view_image: " + err.Error()), nil
	}
	img, err := imageload.Load(full)
	if err != nil {
		return errorResult("view_image: " + err.Error()), nil
	}
	// The image block goes first so the model's attention lands on the picture,
	// with the descriptor after it. The text also survives on a text-only model,
	// which is told the file exists instead of receiving a silently empty turn.
	return agentcore.AgentToolResult{Content: agentcore.ContentList{
		img,
		agentcore.NewTextContent(fmt.Sprintf("viewed %s", imageload.Describe(a.Path, img))),
	}}, nil
}

// resolveImagePath applies the shared workspace-boundary policy, accepting the
// configured extra roots (the skills dir) exactly like ReadTool.resolvePath.
func resolveImagePath(root string, extraRoots []string, p string) (string, error) {
	if len(extraRoots) == 0 {
		return resolveWithin(root, p)
	}
	return resolveWithinAny(append([]string{root}, extraRoots...), p)
}
