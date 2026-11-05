// Package config implements pigo's optional user config file at
// ~/.config/pigo/config.toml (honoring $XDG_CONFIG_HOME when set) plus the
// provider-agnostic base-url env-var name derivation. Values in the file
// replace pigo's built-in defaults, but an explicit command-line flag always
// wins over the file:
//
//	command-line flag > config.toml > built-in default
//
// A missing file is not an error (defaults apply); a malformed file is surfaced
// to the caller so it can warn rather than silently ignore user intent.
//
// The package is intentionally free of any cliOptions/run-assembly concern: it
// only loads and decodes the file and derives env-var names. Overlaying a
// FileConfig onto the parsed CLI options lives in cmd/pigo, alongside the
// options struct it mutates.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/smallnest/pigo/internal/mcp"
)

// FileConfig is the on-disk shape of config.toml. Every field is optional; an
// absent (zero-value) field leaves the corresponding default/flag untouched.
// Keys are snake_case to read naturally in TOML.
type FileConfig struct {
	Model   string `toml:"model"`
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	// Credential is a named credential REFERENCE (issue #568): the literal key
	// lives in $PIGO_HOME/.credentials.yaml (mode 0600) under this name, and
	// config.toml carries only the name. It resolves to the API-key tier, below
	// an explicit --api-key flag. Empty means no reference.
	Credential    string `toml:"credential"`
	Protocol      string `toml:"protocol"`
	Provider      string `toml:"provider"`
	ThinkingLevel string `toml:"thinking_level"`
	OutputFormat  string `toml:"output_format"`
	NoTools       bool   `toml:"no_tools"`
	NoSkills      bool   `toml:"no_skills"`
	Approve       bool   `toml:"approve"`
	SystemPrompt  string `toml:"system_prompt"`
	// AllowedTools and DisallowedTools are the tool-level admission boundary:
	// the config-file tier of --allowed-tools / --disallowed-tools. Names match
	// case-insensitively and DisallowedTools wins when a name appears in both.
	// A CLI flag replaces the file value wholesale rather than merging with it,
	// so passing --allowed-tools can widen a boundary the file narrowed.
	AllowedTools    []string `toml:"allowed_tools"`
	DisallowedTools []string `toml:"disallowed_tools"`
	// Prompts is the config.toml `prompts` array: paths (files or dirs) to load
	// prompt templates from at the settings tier (mirrors pi's settings prompts).
	Prompts []string `toml:"prompts"`
	// MCP defines Model Context Protocol servers whose tools are exposed to the
	// agent (stdio: command/args/env; Streamable HTTP: url/headers). Written as
	// [[mcp]] tables or `mcp add ...` in a pigorc.
	MCP []mcp.ServerConfig `toml:"mcp"`
	// Providers defines third-party (custom) providers: endpoints that are not in
	// pigo's built-in registry. Each entry is registered at startup, so it behaves
	// like a built-in one: `--provider <name>`, model-id resolution, the preset
	// picker and /model all see it. Written as [[providers]] tables.
	Providers []ProviderConfig `toml:"providers"`
	// Models declares individual models, independently of the provider table.
	// A provider entry describes an endpoint (protocol, URL, credential); a model
	// entry describes one servable id and its capabilities, most importantly
	// whether it is a vision model. Listing the same model here and in a
	// provider's `models` is harmless: this table carries the attributes.
	// Written as [[models]] tables or `model add ...` in a pigorc.
	Models []ModelConfig `toml:"models"`
	// Memory, Checkpoint, and Compaction are nested TOML tables for the
	// persistent-memory / infinite-context feature. They are pure config
	// plumbing here; defaults/parsing live in memory.go (Resolve* helpers) and
	// the overlay into runtime options lives in cmd/pigo. See
	// tasks/spec-persistent-memory-infinite-context.md §3/§4/§5.2.
	Memory     MemoryConfig     `toml:"memory"`
	Checkpoint CheckpointConfig `toml:"checkpoint"`
	Compaction CompactionConfig `toml:"compaction"`
	// Dream is the [dream] TOML table for the /dream memory-consolidation
	// feature. Pure config plumbing here; defaults/normalization live in
	// internal/dream (Config). See tasks/spec-dream-memory-consolidation.md
	// §3.3.
	Dream DreamConfig `toml:"dream"`
}

// ProviderConfig defines one third-party provider. Only name is mandatory: the
// protocol defaults to "openai", the key is read from env_var (default
// <NAME>_API_KEY) or the named-credential file, and the auth scheme defaults from
// the protocol (bearer for openai, x-api-key for anthropic).
//
//	[[providers]]
//	name     = "mycorp"
//	protocol = "openai"                     # openai | anthropic
//	base_url = "https://llm.mycorp.com/v1"
//	env_var  = "MYCORP_API_KEY"             # optional
//	credential = "mycorp-work"              # optional: named credential reference
//	auth     = "bearer"                     # optional
//	models   = ["mycorp-large", "mycorp-small"]
type ProviderConfig struct {
	Name string `toml:"name"`
	// Protocol is the wire protocol: openai | anthropic.
	Protocol string `toml:"protocol"`
	// BaseURL is the endpoint. Required unless the provider speaks the public
	// Anthropic API (which has a default).
	BaseURL string `toml:"base_url"`
	// EnvVar names the environment variable holding the key; default
	// <NAME>_API_KEY. The generic convention already works, so this is only
	// needed for a non-standard variable name.
	EnvVar string `toml:"env_var"`
	// APIKey is a literal key. Prefer env_var or credential instead: the literal
	// lives in a config file and is readable by anything that can read it.
	APIKey string `toml:"api_key"`
	// Credential is a named-credential reference resolved from
	// $PIGO_HOME/.credentials.yaml at request time.
	Credential string `toml:"credential"`
	// Auth is the credential attachment scheme: bearer | x-api-key.
	Auth string `toml:"auth"`
	// Models lists the model ids this provider serves, as a shorthand. The first
	// one is the default, so `--model <name>` (bare provider name) selects it.
	// Declare a model in the [[models]] table instead when it needs attributes
	// (a display name, or a vision declaration).
	Models []string `toml:"models"`
}

// ModelConfig declares one model served by a provider. It is deliberately
// separate from ProviderConfig: a provider is an endpoint and its credential,
// while a model is an id with its own capabilities. Splitting them lets one
// gateway expose a mix of vision and text-only models.
//
//	[[models]]
//	id              = "mycorp-vl"
//	provider        = "mycorp"
//	display_name    = "MyCorp Vision"     # optional, defaults to id
//	supports_images = true                # declare it a vision model
//
// Image support is declared per model and only here; a provider has no such
// setting. The pointer keeps "absent" distinguishable from "false": absent means
// "take pigo's default for the provider kind" (text-only for a custom endpoint,
// vision-capable for a built-in one), while an explicit false marks one text-only
// model inside an otherwise vision-capable provider.
type ModelConfig struct {
	// ID is the model id sent on the wire, e.g. "mycorp-large".
	ID string `toml:"id"`
	// Provider names the provider that serves it. Required unless the id was
	// already listed by a provider definition.
	Provider string `toml:"provider"`
	// DisplayName is the label in the /model picker; defaults to ID.
	DisplayName string `toml:"display_name"`
	// SupportsImages declares whether the model accepts image input. nil means
	// "not declared": take pigo's default for the provider kind.
	SupportsImages *bool `toml:"supports_images"`
}

// Vision reports the declared vision capability and whether it was declared at
// all. ok=false means the model says nothing and the provider's default applies.
func (m ModelConfig) Vision() (vision, ok bool) {
	if m.SupportsImages == nil {
		return false, false
	}
	return *m.SupportsImages, true
}

// DreamConfig is the [dream] TOML table for /dream memory consolidation.
// Enabled is a pointer so an absent key (nil) is distinguishable from an
// explicit false: nil is treated as true, only enabled = false disables
// auto-trigger. IntervalDays and RecentSessions use zero as "apply default"
// (7 and 20 respectively); normalization lives in dream.Config.
type DreamConfig struct {
	Enabled        *bool `toml:"enabled"`
	IntervalDays   int   `toml:"interval_days"`
	RecentSessions int   `toml:"recent_sessions"`
}

// FileConfigPath returns the path to the user config file:
// $XDG_CONFIG_HOME/pigo/config.toml, or ~/.config/pigo/config.toml by default.
// It returns "" when neither can be resolved, so the caller treats the file as
// absent.
func FileConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "pigo", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "pigo", "config.toml")
}

// LoadFileConfig reads and decodes config.toml. A missing file (or an empty
// path) returns a zero config with no error; a malformed file is an error.
func LoadFileConfig(path string) (FileConfig, error) {
	cfg, _, err := LoadFileConfigWithWarnings(path)
	return cfg, err
}

// LoadFileConfigWithWarnings is LoadFileConfig plus the non-fatal notices about
// keys that are no longer read. Startup and reload use it so a stale key becomes
// a visible hint instead of a setting that silently does nothing; callers with no
// message channel keep using LoadFileConfig.
func LoadFileConfigWithWarnings(path string) (FileConfig, []string, error) {
	if path == "" {
		return FileConfig{}, nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileConfig{}, nil, nil
		}
		return FileConfig{}, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return FileConfig{}, nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, retiredKeyNotices(md), nil
}

// retiredKeyNotices turns keys the decoder did not recognise into hints for ones
// pigo used to read. TOML decoding ignores unknown keys, so a retired setting
// otherwise vanishes without a word — the worst outcome for a capability, since
// the user's models quietly fall back to a default.
func retiredKeyNotices(md toml.MetaData) []string {
	var notices []string
	seen := map[string]bool{}
	for _, key := range md.Undecoded() {
		parts := []string(key)
		if len(parts) != 2 || parts[0] != "providers" || parts[1] != "supports_images" {
			continue
		}
		if seen[key.String()] {
			continue
		}
		seen[key.String()] = true
		notices = append(notices, "config: [[providers]] supports_images is no longer read — image support is "+
			"declared per model; move it to a [[models]] entry (supports_images = true) or use "+
			"`model add <id> --provider <name> --supports-images`. Run /status or /model to see the "+
			"capability pigo currently assumes.")
	}
	return notices
}

// GenericBaseURLEnvVar derives the generic base-url override env var name for a
// provider: the provider name uppercased with hyphens rewritten to underscores,
// suffixed with _BASE_URL. For example "zai-coding-cn" → "ZAI_CODING_CN_BASE_URL"
// and "deepseek" → "DEEPSEEK_BASE_URL". An empty provider name yields "".
//
// It lives here (not with ResolveBaseURL) because it is a pure name derivation
// with no dependency on the provider registry — the provider-agnostic part of
// base-url resolution. ResolveBaseURL itself lives in internal/provider.
func GenericBaseURLEnvVar(providerName string) string {
	n := strings.TrimSpace(providerName)
	if n == "" {
		return ""
	}
	n = strings.ReplaceAll(n, "-", "_")
	return strings.ToUpper(n) + "_BASE_URL"
}
