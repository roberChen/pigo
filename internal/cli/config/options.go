// This file is the single registry of pigorc `option` keys.
//
// It exists so that "every configurable item is documented" is a property of the
// code rather than a promise: the same table drives (a) applying an option to the
// decoded FileConfig, (b) the generated configuration reference that ships with
// the built-in pigorc skill, and (c) a coverage test that fails when a config.toml
// field has no registry entry. Adding a config key therefore cannot silently lose
// its documentation.
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// optionKind classifies an option's value shape. It selects the parser used when
// applying the option and the type shown in the generated reference.
type optionKind int

const (
	optString optionKind = iota
	optBool
	optBoolPtr
	optInt
	optFloatPtr
	optIntOrString
	optList
	optPushCaps
)

// optionSpec describes one configurable key.
type optionSpec struct {
	// Key is the canonical pigorc key (lowercase, dash-separated). Underscores are
	// accepted as an alias by canonicalOptionKey, so `thinking_level` also works.
	Key string
	// Kind is the value shape.
	Kind optionKind
	// TOML is the equivalent config.toml path, empty for pigorc-only keys. The
	// coverage test asserts every FileConfig field appears here.
	TOML string
	// Type is the human-readable value type shown in the reference.
	Type string
	// Summary explains what the option does, in one sentence.
	Summary string
	// Example is a valid example value for the reference.
	Example string
	// set applies a scalar option. Nil for list/push-caps kinds.
	set func(cfg *FileConfig, value string) error
	// setList applies a list option. Nil for scalar kinds.
	setList func(cfg *FileConfig, values []string)
}

// optionSpecs is the registry, in documentation order. Keys here are the complete
// supported `option` surface.
var optionSpecs = []optionSpec{
	{
		Key:     "model",
		Kind:    optString,
		TOML:    "model",
		Type:    "string",
		Summary: "Default model id. A bare provider name resolves to that provider's default model.",
		Example: "zai/glm-4.6",
		set:     stringSetter(func(c *FileConfig) *string { return &c.Model }),
	},
	{
		Key:     "provider",
		Kind:    optString,
		TOML:    "provider",
		Type:    "string",
		Summary: "Provider name to run against (a built-in, a custom provider, or a bare-model inference override).",
		Example: "zai",
		set:     stringSetter(func(c *FileConfig) *string { return &c.Provider }),
	},
	{
		Key:     "base-url",
		Kind:    optString,
		TOML:    "base_url",
		Type:    "string",
		Summary: "Override the provider endpoint. For a custom endpoint pair it with protocol.",
		Example: "https://api.z.ai/api",
		set:     stringSetter(func(c *FileConfig) *string { return &c.BaseURL }),
	},
	{
		Key:     "protocol",
		Kind:    optString,
		TOML:    "protocol",
		Type:    "string (openai | anthropic)",
		Summary: "Wire protocol for a custom endpoint.",
		Example: "anthropic",
		set:     stringSetter(func(c *FileConfig) *string { return &c.Protocol }),
	},
	{
		Key:     "api-key",
		Kind:    optString,
		TOML:    "api_key",
		Type:    "string",
		Summary: "Literal API key. Prefer exporting the provider's <NAME>_API_KEY or using credential.",
		Example: `"$MY_KEY"`,
		set:     stringSetter(func(c *FileConfig) *string { return &c.APIKey }),
	},
	{
		Key:     "credential",
		Kind:    optString,
		TOML:    "credential",
		Type:    "string",
		Summary: "Name of a credential in $PIGO_HOME/.credentials.yaml (the secret stays out of the config).",
		Example: "mycorp-work",
		set:     stringSetter(func(c *FileConfig) *string { return &c.Credential }),
	},
	{
		Key:     "thinking-level",
		Kind:    optString,
		TOML:    "thinking_level",
		Type:    "string (off|minimal|low|medium|high|xhigh|max)",
		Summary: "Reasoning effort for this run.",
		Example: "high",
		set:     stringSetter(func(c *FileConfig) *string { return &c.ThinkingLevel }),
	},
	{
		Key:     "output-format",
		Kind:    optString,
		TOML:    "output_format",
		Type:    "string (text | stream-json)",
		Summary: "Headless output format.",
		Example: "stream-json",
		set:     stringSetter(func(c *FileConfig) *string { return &c.OutputFormat }),
	},
	{
		Key:     "system-prompt",
		Kind:    optString,
		TOML:    "system_prompt",
		Type:    "string",
		Summary: "Replace the built-in system prompt.",
		Example: `"You are a terse reviewer."`,
		set:     stringSetter(func(c *FileConfig) *string { return &c.SystemPrompt }),
	},
	{
		Key:     "no-tools",
		Kind:    optBool,
		TOML:    "no_tools",
		Type:    "bool",
		Summary: "Disable the built-in file/shell tools.",
		Example: "false",
		set:     boolSetter(func(c *FileConfig) *bool { return &c.NoTools }),
	},
	{
		Key:     "no-skills",
		Kind:    optBool,
		TOML:    "no_skills",
		Type:    "bool",
		Summary: "Disable skill discovery.",
		Example: "false",
		set:     boolSetter(func(c *FileConfig) *bool { return &c.NoSkills }),
	},
	{
		Key:     "approve",
		Kind:    optBool,
		TOML:    "approve",
		Type:    "bool",
		Summary: "Trust the working directory for this run (skips the first-launch prompt).",
		Example: "true",
		set:     boolSetter(func(c *FileConfig) *bool { return &c.Approve }),
	},
	{
		Key:     "allowed-tools",
		Kind:    optList,
		TOML:    "allowed_tools",
		Type:    "list (repeatable)",
		Summary: "Tool allow list. Repeat the option to add names; `option reset allowed-tools` clears it.",
		Example: "read",
		setList: func(c *FileConfig, values []string) { c.AllowedTools = values },
	},
	{
		Key:     "disallowed-tools",
		Kind:    optList,
		TOML:    "disallowed_tools",
		Type:    "list (repeatable)",
		Summary: "Tool deny list; deny wins over allow.",
		Example: "bash",
		setList: func(c *FileConfig, values []string) { c.DisallowedTools = values },
	},
	{
		Key:     "prompts",
		Kind:    optList,
		TOML:    "prompts",
		Type:    "list (repeatable)",
		Summary: "Prompt-template sources (files or directories) at the settings tier.",
		Example: "~/.pigo/prompts",
		setList: func(c *FileConfig, values []string) { c.Prompts = values },
	},
	{
		Key:     "memory.enabled",
		Kind:    optBoolPtr,
		TOML:    "memory.enabled",
		Type:    "bool",
		Summary: "Master switch for persistent memory.",
		Example: "false",
		set:     boolPtrSetter(func(c *FileConfig) **bool { return &c.Memory.Enabled }),
	},
	{
		Key:     "memory.reconcile-on-search",
		Kind:    optBoolPtr,
		TOML:    "memory.reconcile_on_search",
		Type:    "bool",
		Summary: "Lazily re-index memory files before each search.",
		Example: "true",
		set:     boolPtrSetter(func(c *FileConfig) **bool { return &c.Memory.ReconcileOnSearch }),
	},
	{
		Key:     "memory.search-score-floor",
		Kind:    optFloatPtr,
		TOML:    "memory.search_score_floor",
		Type:    "float (0..1)",
		Summary: "Drop memory search hits below this relevance score.",
		Example: "0.25",
		set:     floatPtrSetter(func(c *FileConfig) **float64 { return &c.Memory.SearchScoreFloor }),
	},
	{
		Key:     "memory.cc-index",
		Kind:    optBool,
		TOML:    "memory.cc_index",
		Type:    "bool",
		Summary: "Also index the Claude Code memory directory (read-only).",
		Example: "true",
		set:     boolSetter(func(c *FileConfig) *bool { return &c.Memory.CCIndex }),
	},
	{
		Key:     "checkpoint.thresholds",
		Kind:    optList,
		TOML:    "checkpoint.thresholds",
		Type:    "list (repeatable, percentages)",
		Summary: "Context-window fill levels that trigger compaction.",
		Example: "50%",
		setList: func(c *FileConfig, values []string) { c.Checkpoint.Thresholds = values },
	},
	{
		Key:     "checkpoint.reserved",
		Kind:    optIntOrString,
		TOML:    "checkpoint.reserved",
		Type:    "int or percentage",
		Summary: "Tokens reserved below the window ceiling (an int, or a percentage like 10%).",
		Example: "8192",
		set:     intOrStringSetter(func(c *FileConfig) *IntOrString { return &c.Checkpoint.Reserved }),
	},
	{
		Key:     "checkpoint.push-caps",
		Kind:    optPushCaps,
		TOML:    "checkpoint.push_caps.<section>",
		Type:    "int (per section)",
		Summary: "Per-section injection token budget; use a dotted section, e.g. checkpoint.push-caps.memory.",
		Example: "checkpoint.push-caps.memory 800",
	},
	{
		Key:     "compaction.max-context",
		Kind:    optString,
		TOML:    "compaction.max_context",
		Type:    "string (tokens, K/M suffix, or percentage)",
		Summary: "Lower the auto-compaction trigger point (always clamped by the provider window).",
		Example: "300K",
		set:     stringSetter(func(c *FileConfig) *string { return &c.Compaction.MaxContext }),
	},
	{
		Key:     "dream.enabled",
		Kind:    optBoolPtr,
		TOML:    "dream.enabled",
		Type:    "bool",
		Summary: "Automatic memory consolidation (dream) pass.",
		Example: "false",
		set:     boolPtrSetter(func(c *FileConfig) **bool { return &c.Dream.Enabled }),
	},
	{
		Key:     "dream.interval-days",
		Kind:    optInt,
		TOML:    "dream.interval_days",
		Type:    "int",
		Summary: "Minimum days between dream passes.",
		Example: "7",
		set:     intSetter(func(c *FileConfig) *int { return &c.Dream.IntervalDays }),
	},
	{
		Key:     "dream.recent-sessions",
		Kind:    optInt,
		TOML:    "dream.recent_sessions",
		Type:    "int",
		Summary: "How many recent sessions a dream pass distills.",
		Example: "20",
		set:     intSetter(func(c *FileConfig) *int { return &c.Dream.RecentSessions }),
	},
}

// lookupOption returns the spec for a canonical key.
func lookupOption(key string) (optionSpec, bool) {
	for _, spec := range optionSpecs {
		if spec.Key == key {
			return spec, true
		}
	}
	return optionSpec{}, false
}

// stringSetter builds a scalar setter for a *string field.
func stringSetter(field func(*FileConfig) *string) func(*FileConfig, string) error {
	return func(c *FileConfig, v string) error { *field(c) = v; return nil }
}

// boolSetter builds a setter for a plain bool field.
func boolSetter(field func(*FileConfig) *bool) func(*FileConfig, string) error {
	return func(c *FileConfig, v string) error {
		b, ok := parseShellBool(v)
		if !ok {
			return fmt.Errorf("%q is not a boolean (want true/false/yes/no/1/0/on/off)", v)
		}
		*field(c) = b
		return nil
	}
}

// boolPtrSetter builds a setter for a *bool field (nil means "apply the default").
func boolPtrSetter(field func(*FileConfig) **bool) func(*FileConfig, string) error {
	return func(c *FileConfig, v string) error {
		b, ok := parseShellBool(v)
		if !ok {
			return fmt.Errorf("%q is not a boolean (want true/false/yes/no/1/0/on/off)", v)
		}
		value := b
		*field(c) = &value
		return nil
	}
}

// intSetter builds a setter for an int field.
func intSetter(field func(*FileConfig) *int) func(*FileConfig, string) error {
	return func(c *FileConfig, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%q is not an integer", v)
		}
		*field(c) = n
		return nil
	}
}

// floatPtrSetter builds a setter for a *float64 field.
func floatPtrSetter(field func(*FileConfig) **float64) func(*FileConfig, string) error {
	return func(c *FileConfig, v string) error {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", v)
		}
		*field(c) = &f
		return nil
	}
}

// intOrStringSetter builds a setter for an IntOrString field: an integer literal
// becomes the int form, anything else (e.g. "10%") the string form.
func intOrStringSetter(field func(*FileConfig) *IntOrString) func(*FileConfig, string) error {
	return func(c *FileConfig, v string) error {
		if n, err := strconv.Atoi(v); err == nil {
			*field(c) = IntOrString{IsInt: true, Int: n}
			return nil
		}
		*field(c) = IntOrString{Str: v}
		return nil
	}
}

// applyPigorcOption applies one scalar option to cfg, reporting the spec's error as
// a warning. The dynamic checkpoint.push-caps.<section> key is handled here too.
func (p *Pigorc) applyPigorcOption(cfg *FileConfig, key, value string) {
	value = strings.TrimSpace(value)
	spec, ok := lookupOption(key)
	switch {
	case ok && spec.Kind == optPushCaps:
		p.warnf("option %s needs a section name, e.g. checkpoint.push-caps.memory", key)
		return
	case ok && spec.set != nil:
		if err := spec.set(cfg, value); err != nil {
			p.warnf("option %s: %v", key, err)
		}
		return
	case ok:
		// A list key reaching the scalar path (e.g. from a hand-built Pigorc in a
		// test): apply it as a single-element list.
		if spec.setList != nil {
			spec.setList(cfg, []string{value})
			return
		}
	}
	if section, found := strings.CutPrefix(key, "checkpoint.push-caps."); found && section != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			p.warnf("option %s: %q is not an integer", key, value)
			return
		}
		if cfg.Checkpoint.PushCaps == nil {
			cfg.Checkpoint.PushCaps = make(map[string]int)
		}
		cfg.Checkpoint.PushCaps[section] = n
		return
	}
	p.warnf("unknown option %q", key)
}

// applyPigorcList applies one list option to cfg.
func (p *Pigorc) applyPigorcList(cfg *FileConfig, key string, values []string) {
	if spec, ok := lookupOption(key); ok && spec.setList != nil {
		spec.setList(cfg, values)
		return
	}
	p.warnf("unknown list option %q", key)
}
