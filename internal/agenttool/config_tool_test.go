// Tests for the pigo_config tool: its metadata, the section argument it accepts,
// and the degradation paths (no renderer wired, unknown section). The report
// itself is rendered by the assembly layer, so its content is tested there.
package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
)

// runConfigTool executes the tool and returns its text result.
func runConfigTool(t *testing.T, tool *ConfigTool, args string) string {
	t.Helper()
	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("got %d content blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(agentcore.TextContent)
	if !ok {
		t.Fatalf("content block is %T, want TextContent", res.Content[0])
	}
	return text.Text
}

// TestConfigToolMetadata verifies the tool's advertised surface: its name, that the
// description says it is read-only, and that the schema enum matches the sections
// the renderer understands.
func TestConfigToolMetadata(t *testing.T) {
	tool := &ConfigTool{Report: func(string) string { return "x" }}
	if got := tool.Name(); got != ConfigToolName {
		t.Errorf("Name() = %q, want %q", got, ConfigToolName)
	}
	if !strings.Contains(tool.Description(), "Read-only") {
		t.Errorf("Description() should say the tool is read-only, got %q", tool.Description())
	}
	var schema struct {
		Type       string `json:"type"`
		Properties struct {
			Section struct {
				Enum []string `json:"enum"`
			} `json:"section"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	got := tool.Schema()
	for _, section := range append([]string{"all"}, ConfigSections()...) {
		if !strings.Contains(string(got), `"`+section+`"`) {
			t.Errorf("schema enum is missing section %q: %s", section, got)
		}
	}
}

// TestConfigToolPassesSection verifies the section argument reaches the renderer,
// defaulting to "all" when it is omitted.
func TestConfigToolPassesSection(t *testing.T) {
	var asked []string
	tool := &ConfigTool{Report: func(section string) string {
		asked = append(asked, section)
		return "report for " + section
	}}
	if got := runConfigTool(t, tool, `{}`); got != "report for all" {
		t.Errorf("default render = %q, want the whole report", got)
	}
	if got := runConfigTool(t, tool, `{"section":"hooks"}`); got != "report for hooks" {
		t.Errorf("filtered render = %q, want only hooks", got)
	}
	if len(asked) != 2 {
		t.Errorf("renderer called %d times, want 2", len(asked))
	}
}

// TestConfigToolUnknownSection verifies a bad section is a tool error result that
// lists the accepted names, so the model can retry instead of guessing.
func TestConfigToolUnknownSection(t *testing.T) {
	tool := &ConfigTool{Report: func(string) string { return "x" }}
	got := runConfigTool(t, tool, `{"section":"everything"}`)
	if !strings.Contains(got, "unknown section") {
		t.Errorf("want an unknown-section error, got %q", got)
	}
	for _, section := range ConfigSections() {
		if !strings.Contains(got, section) {
			t.Errorf("error message should list section %q, got %q", section, got)
		}
	}
}

// TestConfigToolWithoutRenderer verifies a run that wires no report degrades to an
// explanatory result rather than an empty answer or a panic.
func TestConfigToolWithoutRenderer(t *testing.T) {
	tool := &ConfigTool{}
	if got := runConfigTool(t, tool, `{}`); !strings.Contains(got, "no configuration snapshot") {
		t.Errorf("want an explanatory result, got %q", got)
	}
}
