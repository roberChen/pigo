// Tests for image input in the two file-reading tools: view_image (added for
// models that need a tool to look at a picture) and read (which branches on an
// image path so a PNG is not dumped as line-numbered binary). They use a real
// tiny PNG in a temp dir so the size cap, sniffing and base64 step run for real.
package agenttool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// toolPNG is a minimal 1x1 PNG (signature + IHDR), enough for extension and
// content sniffing to agree.
var toolPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

// writeToolPNG writes the shared PNG into dir under name and returns its path.
func writeToolPNG(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, toolPNG, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// runViewImage executes view_image with args and fails on a Go-level error.
func runViewImage(t *testing.T, tool *ViewImageTool, args map[string]any) agentcore.AgentToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, gerr := tool.Execute(t.Context(), "call-1", raw, nil)
	if gerr != nil {
		t.Fatalf("execute returned go error: %v", gerr)
	}
	return res
}

// imageBlockOf returns the single ImageContent in res, or nil.
func imageBlockOf(res agentcore.AgentToolResult) *agentcore.ImageContent {
	for _, c := range res.Content {
		if img, ok := c.(agentcore.ImageContent); ok {
			return &img
		}
	}
	return nil
}

// TestViewImageReturnsImage: the happy path yields an image block plus the
// descriptive text line, in that order, and the block carries a PNG payload.
func TestViewImageReturnsImage(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "shot.png")
	res := runViewImage(t, &ViewImageTool{Root: dir}, map[string]any{"path": "shot.png"})
	if len(res.Content) != 2 {
		t.Fatalf("content len = %d, want 2 (image + text)", len(res.Content))
	}
	if _, ok := res.Content[0].(agentcore.ImageContent); !ok {
		t.Fatalf("content[0] = %T, want ImageContent first", res.Content[0])
	}
	img := imageBlockOf(res)
	if img == nil || img.MimeType != "image/png" || img.Data == "" {
		t.Fatalf("image block = %#v", img)
	}
	if text := resultText(res); !strings.Contains(text, "shot.png") || !strings.Contains(text, "image/png") {
		t.Errorf("text = %q, want the path and mime type named", text)
	}
}

// TestViewImageMissingPath and its siblings keep the failure modes as model-
// correctable error results rather than Go errors.
func TestViewImageMissingPath(t *testing.T) {
	res := runViewImage(t, &ViewImageTool{Root: t.TempDir()}, map[string]any{})
	if !strings.Contains(resultText(res), "path is required") {
		t.Errorf("res = %#v, want an error naming the missing path", res)
	}
	if imageBlockOf(res) != nil {
		t.Error("error result carried an image block")
	}
}

func TestViewImageMissingFile(t *testing.T) {
	res := runViewImage(t, &ViewImageTool{Root: t.TempDir()}, map[string]any{"path": "nope.png"})
	if imageBlockOf(res) != nil {
		t.Errorf("res = %#v, a missing file must not yield an image", res)
	}
	if text := resultText(res); !strings.Contains(text, "read image") {
		t.Errorf("text = %q, want a read-image error", text)
	}
}

// TestViewImageRejectsNonImage ensures a text file routed to view_image reports
// the real problem instead of returning an empty image block.
func TestViewImageRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res := runViewImage(t, &ViewImageTool{Root: dir}, map[string]any{"path": "notes.txt"})
	if !strings.Contains(resultText(res), "not a recognized image") {
		t.Errorf("res = %#v, want a not-an-image error", res)
	}
}

// TestViewImagePathTraversal: the workspace boundary applies exactly as in read.
func TestViewImagePathTraversal(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "inside.png")
	res := runViewImage(t, &ViewImageTool{Root: dir}, map[string]any{"path": "../escape.png"})
	if text := resultText(res); !strings.Contains(text, "outside the workspace root") {
		t.Errorf("text = %q, want a boundary error", text)
	}
	if imageBlockOf(res) != nil {
		t.Error("escaped the root and still returned an image")
	}
}

// TestViewImageExtraRoots allows the skills directory, matching ReadTool.
func TestViewImageExtraRoots(t *testing.T) {
	root, extra := t.TempDir(), t.TempDir()
	writeToolPNG(t, extra, "skill.png")
	tool := &ViewImageTool{Root: root, ExtraRoots: []string{extra}}
	res := runViewImage(t, tool, map[string]any{"path": filepath.Join(extra, "skill.png")})
	if imageBlockOf(res) == nil {
		t.Fatalf("res = %#v, want the extra-root image", res)
	}
}

// TestViewImageSchemaAndMode pins the contract the registry validates against
// and the parallel read-only mode.
func TestViewImageSchemaAndMode(t *testing.T) {
	tool := &ViewImageTool{Root: t.TempDir()}
	if tool.Name() != "view_image" {
		t.Errorf("Name = %q, want view_image", tool.Name())
	}
	if tool.ExecutionMode() != agentcore.ToolExecutionParallel {
		t.Errorf("ExecutionMode = %v, want parallel", tool.ExecutionMode())
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "path" {
		t.Errorf("required = %v, want [path]", schema.Required)
	}
}

// TestReadToolImageBranch: read on a .png returns the image block instead of
// line-numbered binary — the natural first attempt a model makes.
func TestReadToolImageBranch(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "shot.png")
	res, _ := runRead(t, &ReadTool{Root: dir}, map[string]any{"path": "shot.png"})
	img := imageBlockOf(res)
	if img == nil {
		t.Fatalf("read returned no image block: %#v", res.Content)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", img.MimeType)
	}
	if !strings.Contains(resultText(res), "read image") {
		t.Errorf("text = %q, want the read-image descriptor", resultText(res))
	}
}

// TestReadToolTextFileStillText guards the branch against over-triggering.
func TestReadToolTextFileStillText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("plain text\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, _ := runRead(t, &ReadTool{Root: dir}, map[string]any{"path": "a.txt"})
	if imageBlockOf(res) != nil {
		t.Error("read attached an image block for a text file")
	}
	if !strings.Contains(resultText(res), "plain text") {
		t.Errorf("text = %q, want the file contents", resultText(res))
	}
}

// TestViewImageRefusedForTextOnlyModel: when the active model cannot accept image
// input the tool must return a corrective message, not a block the wire would
// reject. This is the tool-level gate from Issue 3b.
func TestViewImageRefusedForTextOnlyModel(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "shot.png")
	tool := &ViewImageTool{Root: dir, ImagesSupported: func() bool { return false }}
	res := runViewImage(t, tool, map[string]any{"path": "shot.png"})
	if img := imageBlockOf(res); img != nil {
		t.Fatal("view_image returned an image block for a text-only model")
	}
	if text := resultText(res); !strings.Contains(text, "does not accept image input") {
		t.Errorf("text = %q, want the capability explanation", text)
	}
}

// TestViewImageProbeIsLive: the gate is a function, so a /model switch takes
// effect on the next call without rebuilding the tool.
func TestViewImageProbeIsLive(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "shot.png")
	vision := false
	tool := &ViewImageTool{Root: dir, ImagesSupported: func() bool { return vision }}
	if res := runViewImage(t, tool, map[string]any{"path": "shot.png"}); imageBlockOf(res) != nil {
		t.Fatal("image returned while the model was text-only")
	}
	vision = true
	if res := runViewImage(t, tool, map[string]any{"path": "shot.png"}); imageBlockOf(res) == nil {
		t.Fatal("no image returned after switching to a vision model")
	}
}

// TestViewImageNilProbeKeepsDefault: a nil probe (a tool assembled without a
// capability resolver) must keep the permissive behavior built-in models had.
func TestViewImageNilProbeKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "shot.png")
	res := runViewImage(t, &ViewImageTool{Root: dir}, map[string]any{"path": "shot.png"})
	if imageBlockOf(res) == nil {
		t.Fatal("a nil probe suppressed the image")
	}
}

// TestReadToolImageBranchTextOnlyModel: read keeps the descriptor text (so the
// model can report the file exists) but omits the picture.
func TestReadToolImageBranchTextOnlyModel(t *testing.T) {
	dir := t.TempDir()
	writeToolPNG(t, dir, "shot.png")
	tool := &ReadTool{Root: dir, ImagesSupported: func() bool { return false }}
	res, _ := runRead(t, tool, map[string]any{"path": "shot.png"})
	if img := imageBlockOf(res); img != nil {
		t.Fatal("read attached an image block for a text-only model")
	}
	text := resultText(res)
	if !strings.Contains(text, "read image") {
		t.Errorf("text = %q, want the descriptor preserved", text)
	}
	if !strings.Contains(text, "does not accept image input") {
		t.Errorf("text = %q, want the omission reason", text)
	}
}
