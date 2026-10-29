// This file adapts MCP tools to agentcore.AgentTool: each tool the server lists
// becomes a normal tool named mcp__<server>__<tool>, with the server's JSON Schema
// passed through for argument validation and its content blocks mapped onto
// pigo's result blocks (text, images, and a textual rendering of embedded
// resources).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// ToolNamePrefix marks tools that come from an MCP server, so they are easy to
// allow/deny, to tell apart in a transcript, and to replace when a server's tool
// list changes.
const ToolNamePrefix = "mcp__"

// ToolName is the pigo tool name for an MCP tool: mcp__<server>__<tool>, with every
// character outside [a-z0-9_] folded to '_' (pigo tool names are lowercase
// identifiers, and the separator must stay unambiguous).
func ToolName(server, tool string) string {
	return ToolNamePrefix + sanitizeToolName(server) + "__" + sanitizeToolName(tool)
}

// sanitizeToolName lowercases s and replaces anything that is not a lowercase
// letter, digit or underscore.
func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unnamed"
	}
	return out
}

// wrapTools adapts a server's tool descriptions into pigo tools.
func wrapTools(client *Client, tools []Tool) []agentcore.AgentTool {
	out := make([]agentcore.AgentTool, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 || !json.Valid(schema) {
			// A tool without a (valid) schema still gets called with whatever
			// arguments the model produces.
			schema = json.RawMessage(`{"type":"object","additionalProperties":true}`)
		}
		out = append(out, &mcpTool{
			client:      client,
			server:      client.Name(),
			remoteName:  t.Name,
			name:        ToolName(client.Name(), t.Name),
			description: t.Description,
			schema:      schema,
		})
	}
	return out
}

// mcpTool is one remote MCP tool exposed as a pigo AgentTool.
type mcpTool struct {
	client      *Client
	server      string
	remoteName  string
	name        string
	description string
	schema      json.RawMessage
}

// Name implements AgentTool.
func (t *mcpTool) Name() string { return t.name }

// Description implements AgentTool.
func (t *mcpTool) Description() string {
	desc := strings.TrimSpace(t.description)
	if desc == "" {
		desc = t.remoteName
	}
	return fmt.Sprintf("%s (MCP server %q, tool %q)", desc, t.server, t.remoteName)
}

// Schema implements AgentTool: the server's argument schema, passed through.
func (t *mcpTool) Schema() json.RawMessage { return t.schema }

// ExecutionMode implements AgentTool. MCP calls are independent network/process
// operations, so they parallelize with everything else.
func (t *mcpTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements AgentTool: call the remote tool and map its content blocks.
func (t *mcpTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	res, err := t.client.CallTool(ctx, t.remoteName, args)
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	content, text := mcpContent(res.Content)
	if res.IsError {
		msg := strings.TrimSpace(text)
		if msg == "" {
			msg = "the MCP server reported an error with no message"
		}
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: %s", t.name, msg)
	}
	if len(content) == 0 {
		content = agentcore.ContentList{agentcore.NewTextContent("(the MCP tool returned no content)")}
	}
	var details any
	if len(res.StructuredContent) > 0 {
		details = res.StructuredContent
	}
	return agentcore.AgentToolResult{Content: content, Details: details}, nil
}

// mcpContent maps MCP content blocks onto pigo content blocks and returns a plain
// text rendering (used for error results and as a fallback).
func mcpContent(blocks []Content) (agentcore.ContentList, string) {
	var out agentcore.ContentList
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(text.String()) != "" {
				text.WriteByte('\n')
			}
			text.WriteString(b.Text)
			out = append(out, agentcore.NewTextContent(b.Text))
		case "image":
			if b.Data == "" {
				continue
			}
			mime := b.MIMEType
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, agentcore.NewImageContent(b.Data, mime))
			if strings.TrimSpace(text.String()) != "" {
				text.WriteByte('\n')
			}
			fmt.Fprintf(&text, "[image: %s]", mime)
		case "resource":
			if b.Resource == nil {
				continue
			}
			var rendered string
			switch {
			case b.Resource.Text != "":
				rendered = fmt.Sprintf("[resource %s]\n%s", b.Resource.URI, b.Resource.Text)
			case b.Resource.Blob != "":
				rendered = fmt.Sprintf("[resource %s: %d bytes of base64 data]", b.Resource.URI, len(b.Resource.Blob))
			default:
				rendered = fmt.Sprintf("[resource %s]", b.Resource.URI)
			}
			if strings.TrimSpace(text.String()) != "" {
				text.WriteByte('\n')
			}
			text.WriteString(rendered)
			out = append(out, agentcore.NewTextContent(rendered))
		default:
			// Unknown block kinds (audio, future types) are rendered as a note so
			// the model is not silently handed an empty result.
			rendered := fmt.Sprintf("[unsupported MCP content type %q]", b.Type)
			text.WriteString(rendered)
			out = append(out, agentcore.NewTextContent(rendered))
		}
	}
	return out, text.String()
}
