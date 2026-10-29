// This file exposes the MCP capabilities beyond tools: resources (list/read) and
// prompts (list/get). They are adapters over the server's JSON-RPC methods and are
// only registered for servers that advertise the capability, so a tools-only server
// contributes exactly the tools it has.
//
// Why tools and not a separate UI: every pigo front-end (REPL, TUI, headless, SDK)
// already knows how to run tools, so exposing resources and prompts this way needs
// no per-driver plumbing and works for sub-agents too. A resource URI is passed to
// `mcp__<server>__read_resource`; a prompt is fetched with
// `mcp__<server>__get_prompt` (arguments as a string map) and returned as text.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

// Resource is one MCP resource description.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// resourceListResult is the resources/list response payload.
type resourceListResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ResourceContent is one block returned by resources/read.
type ResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// readResourceResult is the resources/read response payload.
type readResourceResult struct {
	Contents []ResourceContent `json:"contents"`
}

// PromptArgument is one argument a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is one MCP prompt description.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// promptListResult is the prompts/list response payload.
type promptListResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// promptMessage is one message of a rendered prompt.
type promptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// getPromptResult is the prompts/get response payload.
type getPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []promptMessage `json:"messages"`
}

// ListResources returns every resource the server offers, following pagination.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var all []Resource
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "resources/list", params)
		if err != nil {
			return nil, err
		}
		var res resourceListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("resources/list: decode result: %w", err)
		}
		all = append(all, res.Resources...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// ReadResource reads one resource by URI.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	raw, err := c.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	var res readResourceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("resources/read: decode result: %w", err)
	}
	return res.Contents, nil
}

// ListPrompts returns every prompt the server offers, following pagination.
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	var all []Prompt
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "prompts/list", params)
		if err != nil {
			return nil, err
		}
		var res promptListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("prompts/list: decode result: %w", err)
		}
		all = append(all, res.Prompts...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// GetPrompt fetches and renders a prompt, returning its messages as text.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	raw, err := c.call(ctx, "prompts/get", params)
	if err != nil {
		return "", err
	}
	var res getPromptResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("prompts/get: decode result: %w", err)
	}
	var b strings.Builder
	if res.Description != "" {
		b.WriteString(res.Description)
		b.WriteString("\n\n")
	}
	for _, m := range res.Messages {
		text := renderPromptContent(m.Content)
		if text == "" {
			continue
		}
		if m.Role != "" {
			fmt.Fprintf(&b, "%s: ", m.Role)
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// renderPromptContent renders a prompt message body, which may be a single content
// block or an array of them.
func renderPromptContent(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "[") {
		var blocks []Content
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		_, text := mcpContent(blocks)
		return text
	}
	var block Content
	if err := json.Unmarshal(raw, &block); err != nil {
		return ""
	}
	_, text := mcpContent([]Content{block})
	return text
}

// wrapResources builds the resource tools for a server that advertises resources.
func wrapResources(client *Client) []agentcore.AgentTool {
	server := client.Name()
	return []agentcore.AgentTool{
		&mcpResourceTool{
			client:      client,
			name:        ToolName(server, "list_resources"),
			description: fmt.Sprintf("List the resources the MCP server %q exposes (uri, name, description). Read one with %s.", server, ToolName(server, "read_resource")),
			schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			list:        true,
		},
		&mcpResourceTool{
			client:      client,
			name:        ToolName(server, "read_resource"),
			description: fmt.Sprintf("Read one resource from the MCP server %q by its uri.", server),
			schema:      json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string","description":"Resource uri, as returned by list_resources."}},"required":["uri"],"additionalProperties":false}`),
		},
	}
}

// mcpResourceTool is the list or read adapter for one server's resources.
type mcpResourceTool struct {
	client      *Client
	name        string
	description string
	schema      json.RawMessage
	list        bool
}

func (t *mcpResourceTool) Name() string            { return t.name }
func (t *mcpResourceTool) Description() string     { return t.description }
func (t *mcpResourceTool) Schema() json.RawMessage { return t.schema }
func (t *mcpResourceTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute lists the resources or reads one.
func (t *mcpResourceTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if t.list {
		resources, err := t.client.ListResources(ctx)
		if err != nil {
			return agentcore.AgentToolResult{}, fmt.Errorf("%s: %w", t.name, err)
		}
		if len(resources) == 0 {
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("(the server exposes no resources)")}}, nil
		}
		var b strings.Builder
		for _, r := range resources {
			line := r.URI
			if r.Name != "" {
				line += "  " + r.Name
			}
			if r.Description != "" {
				line += "  " + r.Description
			}
			if r.MIMEType != "" {
				line += "  (" + r.MIMEType + ")"
			}
			b.WriteString(strings.TrimRight(line, " "))
			b.WriteByte('\n')
		}
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(strings.TrimRight(b.String(), "\n"))}}, nil
	}

	var a struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: decode args: %w", t.name, err)
	}
	if strings.TrimSpace(a.URI) == "" {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: uri is required", t.name)
	}
	contents, err := t.client.ReadResource(ctx, a.URI)
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if len(contents) == 0 {
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("(empty resource)")}}, nil
	}
	var out agentcore.ContentList
	for _, c := range contents {
		switch {
		case c.Text != "":
			out = append(out, agentcore.NewTextContent(c.Text))
		case strings.HasPrefix(c.MIMEType, "image/") && c.Blob != "":
			out = append(out, agentcore.NewImageContent(c.Blob, c.MIMEType))
		default:
			out = append(out, agentcore.NewTextContent(fmt.Sprintf("[binary resource %s (%s), %d bytes of base64 data]", c.URI, c.MIMEType, len(c.Blob))))
		}
	}
	return agentcore.AgentToolResult{Content: out}, nil
}

// wrapPrompts builds the prompt tools for a server that advertises prompts.
func wrapPrompts(client *Client) []agentcore.AgentTool {
	server := client.Name()
	return []agentcore.AgentTool{
		&mcpPromptTool{
			client:      client,
			name:        ToolName(server, "list_prompts"),
			description: fmt.Sprintf("List the prompt templates the MCP server %q exposes (name, description, arguments). Fetch one with %s.", server, ToolName(server, "get_prompt")),
			schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			list:        true,
		},
		&mcpPromptTool{
			client:      client,
			name:        ToolName(server, "get_prompt"),
			description: fmt.Sprintf("Fetch a prompt template from the MCP server %q and return its rendered messages.", server),
			schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Prompt name, as returned by list_prompts."},"arguments":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional argument values, keyed by argument name."}},"required":["name"],"additionalProperties":false}`),
		},
	}
}

// mcpPromptTool is the list or get adapter for one server's prompts.
type mcpPromptTool struct {
	client      *Client
	name        string
	description string
	schema      json.RawMessage
	list        bool
}

func (t *mcpPromptTool) Name() string            { return t.name }
func (t *mcpPromptTool) Description() string     { return t.description }
func (t *mcpPromptTool) Schema() json.RawMessage { return t.schema }
func (t *mcpPromptTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute lists the prompts or fetches one.
func (t *mcpPromptTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if t.list {
		prompts, err := t.client.ListPrompts(ctx)
		if err != nil {
			return agentcore.AgentToolResult{}, fmt.Errorf("%s: %w", t.name, err)
		}
		if len(prompts) == 0 {
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("(the server exposes no prompts)")}}, nil
		}
		var b strings.Builder
		for _, p := range prompts {
			b.WriteString(p.Name)
			if p.Description != "" {
				b.WriteString("  " + p.Description)
			}
			if len(p.Arguments) > 0 {
				names := make([]string, 0, len(p.Arguments))
				for _, a := range p.Arguments {
					name := a.Name
					if a.Required {
						name += "!"
					}
					names = append(names, name)
				}
				b.WriteString("  args: " + strings.Join(names, ","))
			}
			b.WriteByte('\n')
		}
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(strings.TrimRight(b.String(), "\n"))}}, nil
	}

	var a struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: decode args: %w", t.name, err)
	}
	if strings.TrimSpace(a.Name) == "" {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: name is required", t.name)
	}
	text, err := t.client.GetPrompt(ctx, a.Name, a.Arguments)
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if strings.TrimSpace(text) == "" {
		text = "(the prompt rendered no messages)"
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(text)}}, nil
}
