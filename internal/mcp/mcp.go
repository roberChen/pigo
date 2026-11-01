// Package mcp implements a Model Context Protocol (MCP) client: it connects to
// external MCP servers and exposes their tools to the agent as ordinary
// agentcore.AgentTool values.
//
// Scope: the `tools` capability (list and call) over the two common transports —
// stdio (a server process speaking line-delimited JSON-RPC on stdin/stdout) and
// Streamable HTTP (JSON-RPC POSTed to a URL, with either a JSON or an SSE
// response). Resources and prompts are not exposed yet; a server that offers them
// still works, its tools are just the ones pigo uses.
//
// Lifecycle: Connect performs the MCP initialize handshake per server, lists its
// tools (following pagination) and wraps each one. A server that fails to start or
// initialize is logged and skipped, so one broken server never takes the session
// down. The caller owns the Manager and must Close it when the run ends.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/jsonrpc"
)

// ProtocolVersion is the MCP revision this client announces. Servers may reply
// with their own supported revision; that is accepted (the core methods used here
// are stable across the revisions in the wild).
const ProtocolVersion = "2024-11-05"

// DefaultTimeout is the per-request timeout used when a server does not set one.
const DefaultTimeout = 60 * time.Second

// ServerConfig describes one MCP server. It is shared by config.toml (`[[mcp]]`)
// and the pigorc (`mcp add ...`), so its fields are the documented configuration
// surface.
type ServerConfig struct {
	// Name identifies the server; MCP tools are exposed as mcp__<name>__<tool>.
	Name string `toml:"name"`
	// Command starts a stdio server (mutually exclusive with URL).
	Command string `toml:"command"`
	// Args are the command's arguments.
	Args []string `toml:"args"`
	// Env adds environment variables for a stdio server (the parent environment is
	// inherited, so PATH and friends work).
	Env map[string]string `toml:"env"`
	// URL is the endpoint of a Streamable HTTP server.
	URL string `toml:"url"`
	// Headers adds HTTP headers (e.g. Authorization) for an HTTP server.
	Headers map[string]string `toml:"headers"`
	// TimeoutSeconds bounds each request; 0 means DefaultTimeout.
	TimeoutSeconds int `toml:"timeout_seconds"`
	// Transport selects the HTTP flavor: "" (auto: stdio for command, Streamable
	// HTTP for url), "http" (Streamable HTTP) or "sse" (legacy HTTP+SSE).
	Transport string `toml:"transport"`
	// OAuth enables the MCP authorization flow for an HTTP server that answers 401:
	// protected-resource discovery, dynamic client registration, authorization code
	// with PKCE, and a token cached under $PIGO_HOME/mcp-oauth/.
	OAuth bool `toml:"oauth"`
	// Disabled skips the server without removing its definition.
	Disabled bool `toml:"disabled"`
}

// timeout returns the effective per-request timeout.
func (c ServerConfig) timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

// validate checks a definition before connecting.
func (c ServerConfig) validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("name is required")
	}
	hasCmd := strings.TrimSpace(c.Command) != ""
	hasURL := strings.TrimSpace(c.URL) != ""
	switch {
	case hasCmd && hasURL:
		return fmt.Errorf("set either command (stdio) or url (http), not both")
	case !hasCmd && !hasURL:
		return fmt.Errorf("one of command (stdio) or url (http) is required")
	}
	return nil
}

// Tool is one tool description returned by tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// listToolsResult is the tools/list response payload.
type listToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// callToolParams is the tools/call request payload.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Content is one content block of a tool result.
type Content struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MIMEType string            `json:"mimeType,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is an embedded resource content block (a resource carried inside
// a tool result, as opposed to the Resource entries resources/list returns).
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// CallToolResult is the tools/call response payload.
type CallToolResult struct {
	Content           []Content       `json:"content"`
	IsError           bool            `json:"isError,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
}

// initializeParams is the initialize request payload.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// initializeResult is the initialize response payload (only the fields pigo uses).
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Capabilities serverCapabilities `json:"capabilities"`
}

// serverCapabilities is the subset of the server's advertised capabilities pigo
// acts on: whether tools/resources/prompts exist, and whether the server promises
// list_changed notifications for them.
type serverCapabilities struct {
	Tools *struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools,omitempty"`
	Resources *struct {
		ListChanged bool `json:"listChanged"`
		Subscribe   bool `json:"subscribe"`
	} `json:"resources,omitempty"`
	Prompts *struct {
		ListChanged bool `json:"listChanged"`
	} `json:"prompts,omitempty"`
}

// Client is one initialized MCP server connection.
type Client struct {
	name    string
	version string
	timeout time.Duration
	tr      Transport

	// onChange is called when the server notifies that one of its lists changed
	// (tools/resources/prompts). The manager uses it to refresh lazily.
	onChange func(kind string)

	mu     sync.Mutex
	server initializeResult
	closed bool
	dirty  map[string]bool // list kinds with a pending change notification
}

// clientVersion is announced in initialize's clientInfo.
const clientVersion = "pigo"

// NewClient dials cfg, performs the MCP initialize handshake and returns an
// initialized client. The caller must Close it.
func NewClient(ctx context.Context, cfg ServerConfig, transport Transport) (*Client, error) {
	// The transport is already built here, so only the name still matters; the
	// transport requirement is checked where a transport is chosen (Connect /
	// defaultTransport).
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	c := &Client{
		name:    cfg.Name,
		version: clientVersion,
		timeout: cfg.timeout(),
		tr:      transport,
		dirty:   make(map[string]bool),
	}
	// The transport must reach the client for server->client messages before the
	// initialize exchange, since a server may ping or list roots during it.
	if hs, ok := transport.(handlerSetter); ok {
		hs.SetHandler(c)
	}
	initCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: clientVersion},
	}
	raw, err := c.tr.Call(initCtx, "initialize", params)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("initialize: decode result: %w", err)
	}
	c.server = res
	if err := c.tr.Notify("notifications/initialized", nil); err != nil {
		return nil, fmt.Errorf("initialized notification: %w", err)
	}
	return c, nil
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// ServerInfo returns the server's self-reported name and version.
func (c *Client) ServerInfo() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.server.ServerInfo.Name, c.server.ServerInfo.Version
}

// Capabilities reports which capabilities the server advertised.
func (c *Client) Capabilities() (tools, resources, prompts bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.server.Capabilities.Tools != nil, c.server.Capabilities.Resources != nil, c.server.Capabilities.Prompts != nil
}

// setChangeHandler installs the list-changed observer (manager-owned).
func (c *Client) setChangeHandler(fn func(kind string)) {
	c.mu.Lock()
	c.onChange = fn
	c.mu.Unlock()
}

// markDirty records a list_changed notification and notifies the observer.
func (c *Client) markDirty(kind string) {
	c.mu.Lock()
	c.dirty[kind] = true
	fn := c.onChange
	c.mu.Unlock()
	if fn != nil {
		fn(kind)
	}
}

// takeDirty reports and clears whether any list changed since the last check.
func (c *Client) takeDirty() (tools, resources, prompts bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tools, resources, prompts = c.dirty["tools"], c.dirty["resources"], c.dirty["prompts"]
	c.dirty = make(map[string]bool)
	return tools, resources, prompts
}

// HandleRequest answers a server->client MCP request. pigo implements the
// lifecycle methods a server may send (ping, roots/list) and declines sampling,
// which would require a second model call pigo does not make from here.
func (c *Client) HandleRequest(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "ping":
		return struct{}{}, nil
	case "roots/list":
		roots := []map[string]any{}
		if cwd, err := os.Getwd(); err == nil {
			roots = append(roots, map[string]any{"uri": "file://" + cwd, "name": filepath.Base(cwd)})
		}
		return map[string]any{"roots": roots}, nil
	case "sampling/createMessage":
		return nil, &jsonrpc.Error{Code: -32601, Message: "sampling is not supported by pigo"}
	case "elicitation/create":
		return nil, &jsonrpc.Error{Code: -32601, Message: "elicitation is not supported by pigo"}
	default:
		return nil, &jsonrpc.Error{Code: -32601, Message: "method not found: " + method}
	}
}

// HandleNotification reacts to a server notification. List changes mark the
// corresponding capability dirty so the manager re-lists on the next turn.
func (c *Client) HandleNotification(method string, _ json.RawMessage) {
	switch method {
	case "notifications/tools/list_changed":
		c.markDirty("tools")
	case "notifications/resources/list_changed":
		c.markDirty("resources")
	case "notifications/prompts/list_changed":
		c.markDirty("prompts")
	}
}

// ListTools returns every tool the server offers, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res listToolsResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list: decode result: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// CallTool invokes one tool. A JSON-RPC error becomes a Go error; a tool-level
// failure arrives in the result's IsError field.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallToolResult, error) {
	raw, err := c.call(ctx, "tools/call", callToolParams{Name: name, Arguments: args})
	if err != nil {
		return CallToolResult{}, err
	}
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return CallToolResult{}, fmt.Errorf("tools/call: decode result: %w", err)
	}
	return res, nil
}

// call performs one request under the server's timeout.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mcp server %q is closed", c.name)
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	raw, err := c.tr.Call(reqCtx, method, params)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return raw, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.tr.Close()
}

// Manager owns every connected MCP server and the tools they expose, plus the
// full set of configured servers (connected or not) so a session can switch an
// individual server on and off at runtime (/mcp). It is safe for concurrent
// use: a server can notify a list change while a turn is reading the tool set,
// and a front-end can toggle a server while a turn is being prepared.
type Manager struct {
	mu sync.Mutex
	// toggleMu serializes Enable/Disable so two switches cannot dial the same
	// server twice (the second client would leak) or race a close.
	toggleMu    sync.Mutex
	configs     []ServerConfig
	configIndex map[string]int
	clients     []*Client
	order       []string
	serverTools map[string][]agentcore.AgentTool
	lastError   map[string]error
	// dirty reports that the active server set changed through Enable/Disable,
	// so the next Refresh tells the loop to re-advertise its tool set even when
	// no server sent a list_changed notification.
	dirty bool
	// toolFilter, when set, decides which server tools are exposed at all. The
	// run assembly installs the --allowed-tools/--disallowed-tools predicate here
	// so a server enabled at runtime (/mcp) cannot bypass the tool boundary that
	// filtered the launch-time set.
	toolFilter func(name string) bool
	registry   *agenttool.ToolRegistry
	logw       io.Writer
}

// Connect dials every enabled server in cfg (best-effort: a server that fails to
// start, initialize or list its capabilities is reported to logw and skipped),
// and wraps each capability it advertises. A nil/empty config returns an empty
// manager. The disabled definitions are remembered too so /mcp can activate
// them later, and a failed server keeps its error for the same listing.
func Connect(ctx context.Context, cfg []ServerConfig, logw io.Writer) *Manager {
	if logw == nil {
		logw = io.Discard
	}
	m := &Manager{
		serverTools: make(map[string][]agentcore.AgentTool),
		lastError:   make(map[string]error),
		configIndex: make(map[string]int, len(cfg)),
		logw:        logw,
	}
	seen := make(map[string]bool, len(cfg))
	for _, sc := range cfg {
		if seen[sc.Name] {
			fmt.Fprintf(logw, "pigo: mcp server %q skipped: duplicate name\n", sc.Name)
			continue
		}
		seen[sc.Name] = true
		m.configs = append(m.configs, sc)
		m.configIndex[sc.Name] = len(m.configs) - 1
		if sc.Disabled {
			continue
		}
		if err := sc.validate(); err != nil {
			fmt.Fprintf(logw, "pigo: mcp server %q skipped: %v\n", sc.Name, err)
			m.lastError[sc.Name] = err
			continue
		}
		if err := m.connectServer(ctx, sc); err != nil {
			fmt.Fprintf(logw, "pigo: mcp server %q skipped: %v\n", sc.Name, err)
			m.lastError[sc.Name] = err
		}
	}
	// The initial dial is not a runtime change: there is nothing to re-advertise
	// yet, so the first turn starts from a clean slate.
	m.dirty = false
	return m
}

// connectServer performs one server's handshake, lists its capabilities and
// publishes the resulting tools under the manager lock. It is the single place
// Connect and Enable share.
func (m *Manager) connectServer(ctx context.Context, sc ServerConfig) error {
	tr, err := newTransport(sc, m.logw)
	if err != nil {
		return err
	}
	client, err := NewClient(ctx, sc, tr)
	if err != nil {
		_ = tr.Close()
		return err
	}
	tools, err := m.buildTools(client)
	if err != nil {
		_ = client.Close()
		return err
	}
	m.mu.Lock()
	tools = m.filterToolsLocked(tools)
	m.clients = append(m.clients, client)
	m.order = append(m.order, sc.Name)
	m.serverTools[sc.Name] = tools
	if i, ok := m.configIndex[sc.Name]; ok {
		m.configs[i].Disabled = false
	}
	delete(m.lastError, sc.Name)
	reg := m.registry
	m.dirty = true
	m.mu.Unlock()
	if reg != nil {
		for _, t := range tools {
			_ = reg.Register(t)
		}
	}
	return nil
}

// ServerStatus is one row of a configured MCP server's runtime state, used by
// the /mcp command and its picker. Active means the server is connected and its
// tools are advertised; Error carries the last connect failure, if any.
type ServerStatus struct {
	Name   string
	Active bool
	Tools  int
	Error  string
}

// Statuses lists every configured server in configuration order, connected or
// not, so a front-end can show what /mcp can switch.
func (m *Manager) Statuses() []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.configs))
	for _, sc := range m.configs {
		st := ServerStatus{Name: sc.Name}
		st.Tools = len(m.filterToolsLocked(m.serverTools[sc.Name]))
		st.Active = m.connectedLocked(sc.Name)
		if err := m.lastError[sc.Name]; err != nil {
			st.Error = err.Error()
		}
		out = append(out, st)
	}
	return out
}

// connectedLocked reports whether name has a live client. Caller holds m.mu.
func (m *Manager) connectedLocked(name string) bool {
	for _, n := range m.order {
		if n == name {
			return true
		}
	}
	return false
}

// Active reports whether the named server is currently connected.
func (m *Manager) Active(name string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectedLocked(name)
}

// Enable connects a configured server that is currently off (disabled in
// configuration, or skipped because its first connect failed) and registers its
// tools. It is a no-op when the server is already connected. The next Refresh
// reports a change so the loop re-advertises the tool set.
func (m *Manager) Enable(ctx context.Context, name string) error {
	if m == nil {
		return fmt.Errorf("mcp: no manager")
	}
	m.toggleMu.Lock()
	defer m.toggleMu.Unlock()
	m.mu.Lock()
	i, ok := m.configIndex[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp: unknown server %q", name)
	}
	if m.connectedLocked(name) {
		m.mu.Unlock()
		return nil
	}
	sc := m.configs[i]
	m.mu.Unlock()

	if err := sc.validate(); err != nil {
		m.mu.Lock()
		m.lastError[name] = err
		m.mu.Unlock()
		return err
	}
	if err := m.connectServer(ctx, sc); err != nil {
		m.mu.Lock()
		m.lastError[name] = err
		m.mu.Unlock()
		return err
	}
	return nil
}

// Disable disconnects a connected server, unregisters its tools from the bound
// registry and marks the tool set dirty so the next Refresh re-advertises it.
// Disabling a server that is already off is a no-op.
func (m *Manager) Disable(name string) error {
	if m == nil {
		return fmt.Errorf("mcp: no manager")
	}
	m.toggleMu.Lock()
	defer m.toggleMu.Unlock()
	m.mu.Lock()
	var client *Client
	for i, n := range m.order {
		if n == name {
			client = m.clients[i]
			m.clients = append(m.clients[:i], m.clients[i+1:]...)
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	removed := m.serverTools[name]
	delete(m.serverTools, name)
	reg := m.registry
	// Only a real disconnect is a state change. A server that was already off
	// keeps whatever reason its last connect failed with, so /mcp still explains
	// why it is not running.
	if client != nil {
		delete(m.lastError, name)
		m.dirty = true
	}
	m.mu.Unlock()

	if client == nil {
		return nil
	}
	if reg != nil {
		for _, t := range removed {
			reg.Unregister(t.Name())
		}
	}
	if err := client.Close(); err != nil {
		return fmt.Errorf("mcp server %q: close: %w", name, err)
	}
	return nil
}

// buildTools lists a server's capabilities and wraps each one. A tools-only server
// therefore contributes exactly its tools; a server advertising resources or
// prompts contributes the list/read (or list/get) adapters as well. Duplicate tool
// names within one server keep their first definition.
func (m *Manager) buildTools(client *Client) ([]agentcore.AgentTool, error) {
	tools, err := client.ListTools(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	out := wrapTools(client, tools)

	hasTools, hasResources, hasPrompts := client.Capabilities()
	if hasResources {
		out = append(out, wrapResources(client)...)
	}
	if hasPrompts {
		out = append(out, wrapPrompts(client)...)
	}
	_ = hasTools

	seen := make(map[string]bool, len(out))
	deduped := out[:0]
	for _, t := range out {
		if seen[t.Name()] {
			fmt.Fprintf(m.logw, "pigo: mcp server %q: duplicate tool name %q ignored\n", client.Name(), t.Name())
			continue
		}
		seen[t.Name()] = true
		deduped = append(deduped, t)
	}
	return deduped, nil
}

// BindRegistry attaches the run's tool registry so tools discovered later (a
// server's list_changed notification) become executable, not just advertised.
func (m *Manager) BindRegistry(reg *agenttool.ToolRegistry) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.registry = reg
	m.mu.Unlock()
	if reg != nil {
		for _, t := range m.Tools() {
			_ = reg.Register(t)
		}
	}
}

// SetToolFilter installs the predicate that decides which server tools are
// exposed. The run assembly passes the --allowed-tools/--disallowed-tools policy
// so a server switched on at runtime (/mcp) is held to the same boundary as the
// launch-time set: a filtered-out tool is neither advertised nor registered, and
// therefore not dispatchable. A nil filter exposes everything.
func (m *Manager) SetToolFilter(keep func(name string) bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.toolFilter = keep
	m.mu.Unlock()
}

// filterToolsLocked applies the tool boundary. Caller holds m.mu.
func (m *Manager) filterToolsLocked(tools []agentcore.AgentTool) []agentcore.AgentTool {
	if m.toolFilter == nil {
		return tools
	}
	out := make([]agentcore.AgentTool, 0, len(tools))
	for _, t := range tools {
		if m.toolFilter(t.Name()) {
			out = append(out, t)
		}
	}
	return out
}

// Tools returns the agent tools exposed by every connected server, in connection
// order.
func (m *Manager) Tools() []agentcore.AgentTool {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]agentcore.AgentTool, 0, len(m.serverTools))
	for _, name := range m.order {
		out = append(out, m.filterToolsLocked(m.serverTools[name])...)
	}
	return out
}

// Servers returns the names of the connected servers.
func (m *Manager) Servers() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// Refresh re-lists the servers that notified a list change and reports whether any
// tool set changed. New tools are registered into the bound registry so the
// executor can run them; the caller (the run loop) then advertises the refreshed
// list to the model.
func (m *Manager) Refresh(ctx context.Context) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	clients := append([]*Client(nil), m.clients...)
	changed := m.dirty
	m.dirty = false
	m.mu.Unlock()

	for _, client := range clients {
		dt, dr, dp := client.takeDirty()
		if !dt && !dr && !dp {
			continue
		}
		tools, err := m.buildTools(client)
		if err != nil {
			fmt.Fprintf(m.logw, "pigo: mcp server %q: refresh failed, keeping the previous tools: %v\n", client.Name(), err)
			continue
		}
		m.mu.Lock()
		tools = m.filterToolsLocked(tools)
		m.serverTools[client.Name()] = tools
		reg := m.registry
		m.mu.Unlock()
		if reg != nil {
			for _, t := range tools {
				_ = reg.Register(t)
			}
		}
		changed = true
	}
	return changed
}

// Close disconnects every server (best-effort, joining errors).
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	clients := m.clients
	m.clients = nil
	m.order = nil
	m.serverTools = make(map[string][]agentcore.AgentTool)
	m.mu.Unlock()

	var errs []error
	for _, c := range clients {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mcp close: %v", errs)
	}
	return nil
}

// transportFactory builds the transport for a server. It is a variable so tests can
// substitute in-memory transports without spawning processes.
var transportFactory = defaultTransport

// newTransport builds a server's transport through the injected factory.
func newTransport(cfg ServerConfig, logw io.Writer) (Transport, error) {
	return transportFactory(cfg, logw)
}
