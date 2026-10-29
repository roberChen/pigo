// Tests for the pigo_config report: the sections it renders, the state it reads
// (config paths, hooks, tools, skills, MCP, sub-agents, memory) and the way the
// tool is registered by SetupEnv. The report is deliberately built from the same
// values the run uses, so these tests assert on those values rather than on a
// hand-written expectation of what pigo "should" be configured with.
package run

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	pigoconfig "github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/runtime"
)

// configToolFrom returns the registered pigo_config tool, or nil.
func configToolFrom(tools []agentcore.AgentTool) agentcore.AgentTool {
	for _, tool := range tools {
		if tool.Name() == agenttool.ConfigToolName {
			return tool
		}
	}
	return nil
}

// runConfigTool executes the pigo_config tool with args and returns its text.
func runConfigTool(t *testing.T, tool agentcore.AgentTool, args string) string {
	t.Helper()
	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("execute %s: %v", tool.Name(), err)
	}
	var b strings.Builder
	for _, block := range res.Content {
		if text, ok := block.(agentcore.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// TestSetupEnvRegistersConfigTool verifies the tool joins the advertised set, that
// --no-tools removes it with every other tool, and that the report describes the
// run that was actually assembled.
func TestSetupEnvRegistersConfigTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	pigoconfig.SetCurrentMCP(nil)
	pigoconfig.SetCurrentAgents(nil)
	t.Cleanup(func() {
		pigoconfig.SetCurrentMCP(nil)
		pigoconfig.SetCurrentAgents(nil)
	})

	env, err := SetupEnv("openrouter/free", "", "", "", "", false, false, "", nil, false, ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	if env.Report == nil {
		t.Fatal("Env.Report should expose the report so a driver can add its settings")
	}
	tool := configToolFrom(env.Tools)
	if tool == nil {
		t.Fatalf("%s missing from the tool set: %v", agenttool.ConfigToolName, toolNames(env.Tools))
	}

	text := runConfigTool(t, tool, `{}`)
	for _, want := range []string{"pigo", "working dir", "openrouter/free", "tools (", "skills", "hooks", "mcp servers"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}

	// --no-tools means no tools, the report tool included.
	disabled, err := SetupEnv("openrouter/free", "", "", "", "", true, true, "", nil, false, ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv(noTools): %v", err)
	}
	if configToolFrom(disabled.Tools) != nil {
		t.Error("--no-tools must not register the config tool")
	}
	if disabled.Report != nil {
		t.Error("--no-tools should not build a config report")
	}
}

// TestConfigReportSections verifies one section can be requested alone and that an
// unknown section is rejected with the accepted names.
func TestConfigReportSections(t *testing.T) {
	report := &ConfigReport{
		Cwd:          t.TempDir(),
		Model:        "test-model",
		ProviderName: "test-provider",
		Tools:        []agentcore.AgentTool{stubTool("read")},
		Skills:       []*runtime.Skill{{Frontmatter: runtime.SkillFrontmatter{Name: "demo", Description: "Does demo things"}}},
	}
	tool := newConfigTool(report)

	full := runConfigTool(t, tool, `{}`)
	toolsOnly := runConfigTool(t, tool, `{"section":"tools"}`)
	if !strings.Contains(toolsOnly, "tools (1)") {
		t.Errorf("tools section missing its header:\n%s", toolsOnly)
	}
	if strings.Contains(toolsOnly, "hooks") {
		t.Errorf("tools section leaked another section:\n%s", toolsOnly)
	}
	if len(toolsOnly) >= len(full) {
		t.Error("a filtered section should be shorter than the whole report")
	}

	bad := runConfigTool(t, tool, `{"section":"nope"}`)
	if !strings.Contains(bad, "unknown section") || !strings.Contains(bad, string(agenttool.ConfigSectionHooks)) {
		t.Errorf("unknown section should list the accepted names, got:\n%s", bad)
	}
}

// TestConfigReportState verifies the report describes the effective state: the
// tool boundary, the resolved hook set, connected vs configured MCP servers, custom
// sub-agents, and why memory is off.
func TestConfigReportState(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())

	// A global hook layer, an MCP definition (one connected, one disabled) and a
	// custom sub-agent: the three things a report is most often asked about.
	writeFile(t, filepath.Join(home, "config.json"), `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"echo guarded"}]}]}}`)
	pigoconfig.SetCurrentMCP([]mcp.ServerConfig{
		{Name: "fake", Command: "fake-server"},
		{Name: "off", URL: "https://example.test/mcp", Disabled: true},
	})
	pigoconfig.SetCurrentAgents([]pigoconfig.AgentConfig{{Name: "researcher", Description: "finds things"}})
	t.Cleanup(func() {
		pigoconfig.SetCurrentMCP(nil)
		pigoconfig.SetCurrentAgents(nil)
	})

	manager := &mcp.Manager{}
	original := connectMCP
	connectMCP = func(context.Context, []mcp.ServerConfig, io.Writer) *mcp.Manager { return manager }
	t.Cleanup(func() { connectMCP = original })

	env, err := SetupEnv("openrouter/free", "", "", "", "", false, true, "", nil, false, ToolPolicy{Allow: []string{"read", "grep", agenttool.ConfigToolName}})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	env.Report.Cwd = cwd
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	tool := configToolFrom(env.Tools)
	if tool == nil {
		t.Fatal("config tool not registered")
	}
	text := runConfigTool(t, tool, `{"section":"all"}`)

	for _, want := range []string{
		"matcher=bash",      // the resolved hook set
		"echo guarded",      // and the command itself
		"fake",              // the configured MCP server
		"connected",         // with its connection state
		"off",               // the disabled one
		"researcher",        // the custom sub-agent
		"registered",        // and whether it made it into the tool set
		"disallowed=(none)", // the tool boundary
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
	// The policy keeps only the allowed tools.
	if strings.Contains(text, "webfetch") {
		t.Errorf("---allowed-tools should have narrowed the reported tool set:\n%s", text)
	}
	if !strings.Contains(text, agenttool.ConfigToolName) {
		t.Errorf("the report itself is a registered tool and should be listed:\n%s", text)
	}
}

// TestConfigReportSkipsUntrustedProjectLayer verifies an untrusted directory's
// project hook file is reported as skipped rather than silently ignored.
func TestConfigReportSkipsUntrustedProjectLayer(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	t.Setenv("PIGO_SKILLS_DIR", t.TempDir())
	writeFile(t, filepath.Join(cwd, ".pigo", "config.json"), `{"hooks":{}}`)

	report := &ConfigReport{Cwd: cwd}
	text := report.Render("files")
	if !strings.Contains(text, "not trusted") {
		t.Errorf("untrusted project layer should be reported as skipped:\n%s", text)
	}
	if !strings.Contains(text, filepath.Join(cwd, ".pigo", "config.json")) {
		t.Errorf("the project config path should still be named:\n%s", text)
	}
}

// TestConfigReportMemorySummary verifies the memory section renders the resolved
// tables the driver passed in, and the reason a store is absent.
func TestConfigReportMemorySummary(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	report := &ConfigReport{MemoryNote: "memory.enabled = false"}
	text := report.Render("memory")
	if !strings.Contains(text, "off") || !strings.Contains(text, "memory.enabled = false") {
		t.Errorf("memory section should explain why the store is off:\n%s", text)
	}

	enabled := boolPtr(true)
	report.MemoryOn = true
	report.Snapshot = pigoconfig.RuntimeSnapshot{Memory: pigoconfig.MemorySettings{
		Memory:               pigoconfig.ResolvedMemory{Enabled: true, ReconcileOnSearch: true, SearchScoreFloor: 0.15},
		CheckpointThresholds: []float64{0.5, 0.75},
	}}
	text = report.Render("memory")
	for _, want := range []string{"store       on", "thresholds=0.5,0.75", "compaction", "score-floor=0.15"} {
		if !strings.Contains(text, want) {
			t.Errorf("memory section missing %q:\n%s", want, text)
		}
	}
	_ = enabled
}

// TestConfigReportRuntimeSettings verifies the driver-provided snapshot wins over
// the assembly defaults, so the report matches the run's own settings.
func TestConfigReportRuntimeSettings(t *testing.T) {
	report := &ConfigReport{Cwd: t.TempDir(), Model: "startup-model", ProviderName: "startup-provider"}
	report.SetSnapshot(pigoconfig.RuntimeSnapshot{
		Model:        "snapshot-model",
		Provider:     "snapshot-provider",
		Protocol:     "openai",
		BaseURL:      "https://example.test/v1",
		Thinking:     "high",
		OutputFormat: "stream-json",
		Approve:      true,
		NoSkills:     true,
	})
	text := report.Render("runtime")
	for _, want := range []string{
		"snapshot-model", "snapshot-provider", "protocol=openai",
		"endpoint=https://example.test/v1", "thinking     high", "stream-json", "approve      on",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runtime section missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "startup-model") {
		t.Errorf("the snapshot should win over the assembly default:\n%s", text)
	}
}

// TestConfigReportMCPStates verifies the MCP section distinguishes a server that
// connected, one that did not, and one that is disabled by config — the difference
// between "I cannot reach it" and "I was told not to run it" matters when
// diagnosing a missing tool.
func TestConfigReportMCPStates(t *testing.T) {
	pigoconfig.SetCurrentMCP([]mcp.ServerConfig{
		{Name: "live", Command: "server"},
		{Name: "down", URL: "https://example.test/mcp", OAuth: true},
		{Name: "off", Command: "server", Disabled: true},
	})
	t.Cleanup(func() { pigoconfig.SetCurrentMCP(nil) })

	report := &ConfigReport{}
	text := report.Render("mcp")
	for _, want := range []string{"(0 connected, 3 configured)", "down", "not connected", "off", "disabled", "oauth=on"} {
		if !strings.Contains(text, want) {
			t.Errorf("mcp section missing %q:\n%s", want, text)
		}
	}
}

// TestServerToolPrefix verifies the prefix helper the report uses to attribute a
// flat tool list back to its server, including a name that needs sanitizing.
func TestServerToolPrefix(t *testing.T) {
	prefix := mcp.ServerToolPrefix("my-server")
	if prefix != "mcp__my_server__" {
		t.Errorf("ServerToolPrefix = %q, want the sanitized server segment", prefix)
	}
	name := mcp.ToolName("my-server", "read.docs")
	if !strings.HasPrefix(name, prefix) {
		t.Errorf("ToolName(%q) = %q should start with %q", "read.docs", name, prefix)
	}
	if strings.HasPrefix("mcp__other__read", prefix) {
		t.Error("a different server must not share the prefix")
	}
}

// boolPtr is a tiny helper for the pointer-typed bool config fields.
func boolPtr(b bool) *bool { return &b }

// writeFile writes content to path, creating the parent directory.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
