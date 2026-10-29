// This file implements the pigo_config built-in tool: a read-only window into the
// configuration the running process actually ended up with — which files were
// loaded, which tools, skills, MCP servers, hooks, sub-agents and plugins are
// active, and which limits are in force. The report itself is rendered by the
// assembly layer (internal/cli/run), the only place that knows all of those at
// once; the tool just carries a render callback, so a call reflects state as of
// the moment it runs (a hook set that changed after an interactive trust prompt,
// a server that announced a new tool list, and so on).
package agenttool

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// ConfigToolName is the name of the runtime-configuration tool. It is refused
// for child sub-agents (they get no delegation of the parent's own diagnostics).
const ConfigToolName = "pigo_config"

// ConfigSection selects one part of the report. An unknown or empty section means
// the whole report, so a caller that omits the argument always gets everything.
type ConfigSection string

const (
	ConfigSectionAll     ConfigSection = "all"
	ConfigSectionRuntime ConfigSection = "runtime"
	ConfigSectionFiles   ConfigSection = "files"
	ConfigSectionTools   ConfigSection = "tools"
	ConfigSectionSkills  ConfigSection = "skills"
	ConfigSectionMCP     ConfigSection = "mcp"
	ConfigSectionHooks   ConfigSection = "hooks"
	ConfigSectionAgents  ConfigSection = "agents"
	ConfigSectionMemory  ConfigSection = "memory"
)

// configSections is the accepted set, in report order. It is also the schema
// enum, so the model and the renderer cannot drift apart.
var configSections = []ConfigSection{
	ConfigSectionRuntime,
	ConfigSectionFiles,
	ConfigSectionTools,
	ConfigSectionSkills,
	ConfigSectionMCP,
	ConfigSectionHooks,
	ConfigSectionAgents,
	ConfigSectionMemory,
}

// ConfigTool reports the effective runtime configuration. Report is required: it
// receives the requested section and returns the rendered text.
type ConfigTool struct {
	// Report renders the report for section (empty means the whole report).
	Report func(section string) string
}

// configToolArgs is the decoded argument shape.
type configToolArgs struct {
	Section string `json:"section"`
}

// Name implements AgentTool.
func (t *ConfigTool) Name() string { return ConfigToolName }

// Description implements AgentTool.
func (t *ConfigTool) Description() string {
	return "Show pigo's effective runtime configuration: the config files actually " +
		"loaded, the model/provider endpoint, reasoning and output settings, the " +
		"enabled tools and the tool boundary, skills, MCP servers, hooks, custom " +
		"sub-agents, plugins and memory settings. Read-only. Use it to answer " +
		"questions like 'is this hook active?', 'which skills are available?', " +
		"'why is a tool missing?' or 'which config file set this value?' instead of " +
		"guessing or re-reading config files."
}

// Schema implements AgentTool.
func (t *ConfigTool) Schema() json.RawMessage {
	names := make([]string, 0, len(configSections)+1)
	names = append(names, string(ConfigSectionAll))
	for _, s := range configSections {
		names = append(names, string(s))
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "section": {
      "type": "string",
      "enum": [` + strings.Join(quoted, ", ") + `],
      "description": "Restrict the report to one section. Omit or use all for the complete report."
    }
  },
  "additionalProperties": false
}`)
}

// ExecutionMode implements AgentTool. The report is pure observation, so several
// calls can run in the same batch.
func (t *ConfigTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements AgentTool: render the requested section and return it as
// text. An unavailable renderer degrades to an explanatory result rather than a
// Go error, matching the other built-ins.
func (t *ConfigTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	a, bad := decodeArgs[configToolArgs](args, ConfigToolName)
	if bad != nil {
		return *bad, nil
	}
	if t.Report == nil {
		return errorResult(ConfigToolName + ": no configuration snapshot is wired in this run"), nil
	}
	section := strings.ToLower(strings.TrimSpace(a.Section))
	if section == "" {
		section = string(ConfigSectionAll)
	}
	if section != string(ConfigSectionAll) && !validConfigSection(section) {
		return errorResult(ConfigToolName + ": unknown section " + section + "; want " + sectionsList()), nil
	}
	text := t.Report(section)
	if text == "" {
		text = ConfigToolName + ": no configuration to report"
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(text)},
		Details: map[string]any{"section": section},
	}, nil
}

// validConfigSection reports whether section is one of the known section names.
func validConfigSection(section string) bool {
	for _, s := range configSections {
		if string(s) == section {
			return true
		}
	}
	return false
}

// sectionsList renders the accepted section names for an error message.
func sectionsList() string {
	names := make([]string, 0, len(configSections)+1)
	names = append(names, string(ConfigSectionAll))
	for _, s := range configSections {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// ConfigSections returns the accepted section names in report order, so the
// renderer and the documentation share one definition.
func ConfigSections() []string {
	out := make([]string, 0, len(configSections))
	for _, s := range configSections {
		out = append(out, string(s))
	}
	return out
}
