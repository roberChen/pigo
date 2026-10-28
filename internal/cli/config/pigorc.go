// This file implements pigorc, pigo's Bash-script configuration layer, in the
// spirit of a shell rc file: a real Bash script (not a data format) that builds
// config by calling tiny builtins, so includes, conditionals, variables, command
// substitution and secrets handling come for free.
//
// Discovery, lowest precedence first (a later file wins on conflicts):
//
//	$PIGO_HOME/pigorc        (default ~/.pigo/pigorc) — always evaluated
//	<cwd>/.pigorc            — evaluated only when the directory is trusted
//
// Both files are sourced in that order by one Bash process, so a project file can
// override or extend the global one, `source` helpers, and read the environment.
// The DSL is deliberately small:
//
//	option <key> <value>     set a config option (config.toml keys; last wins)
//	option reset <key>       clear a list option
//	hook add <event> [--matcher RE] [--timeout N] --command CMD
//	provider add <name> [--type openai|anthropic] [--base-url URL]
//	             [--env-var VAR] [--auth bearer|x-api-key] [--api-key KEY]
//	             [--model ID ...]      define a third-party provider
//	provider remove <name>              drop a definition from a lower layer
//	<Event>() { ...; }       a shell function named after a hook event becomes a
//	                         hook; it receives the same stdin JSON/env as a
//	                         command hook and its exit code decides the outcome
//	export NAME=value        exported variables are applied to pigo's
//	                         environment, so provider keys and PIGO_* knobs work
//
// Options are returned as a FileConfig overlay (so the whole config.toml surface,
// including the nested memory/checkpoint/compaction/dream tables, is reachable),
// hooks are returned as a hooks.HookSet that run.ResolveHookSet appends, and
// exported variables are diffed against the launching environment. Evaluation is
// best-effort: a missing `bash`, a failing script or an unknown option produces a
// warning, never a failed launch.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/smallnest/pigo/internal/hooks"
)

// pigorcTimeout bounds the whole evaluation so a script waiting on input cannot
// hang startup. It is a var so tests can shorten it.
var pigorcTimeout = 15 * time.Second

// pigorcFileName is the project-local script; the global one lives in
// $PIGO_HOME/pigorc.
const pigorcFileName = ".pigorc"

// Pigorc is the result of evaluating the user's pigorc scripts.
type Pigorc struct {
	// Options holds scalar settings by canonical (lowercase, dash-separated) key.
	// A repeated option overwrites, matching shell semantics.
	Options map[string]string
	// Lists holds list-valued settings (allowed-tools, disallowed-tools, prompts,
	// checkpoint.thresholds) in the order they were added.
	Lists map[string][]string
	// Hooks are the hooks the scripts defined, keyed by canonical event name.
	Hooks hooks.HookSet
	// Providers holds the third-party provider definitions the scripts added,
	// in the order they were declared (a repeated name replaces the earlier
	// definition). RemovedProviders names definitions to drop from a lower layer.
	Providers        []ProviderConfig
	RemovedProviders []string
	// Env holds the exported variables whose value differs from the launching
	// environment; Unset holds the names the script removed.
	Env   map[string]string
	Unset []string
	// Sources lists the files that were actually evaluated, in order.
	Sources []string
	// Warnings collects non-fatal problems (unknown option key, invalid value,
	// non-zero script exit) for the caller to print.
	Warnings []string
	// Output is whatever the scripts wrote to stdout/stderr, for the caller to
	// print when it is not empty.
	Output string
}

// defaultPigorcPath returns $PIGO_HOME/pigorc (default ~/.pigo/pigorc), or "" when
// the home directory cannot be resolved and PIGO_HOME is unset.
func defaultPigorcPath() string {
	dir := os.Getenv("PIGO_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".pigo")
	}
	return filepath.Join(dir, "pigorc")
}

// ProjectPigorcPath returns the project-local pigorc path for cwd.
func ProjectPigorcPath(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, pigorcFileName)
}

// PigorcPaths returns the pigorc files to evaluate for cwd, lowest precedence
// first: the global file, then the project file when the directory is trusted
// (a project script executes arbitrary code, so it gets the same fail-closed
// trust gate as project hooks). Non-existent paths are dropped.
func PigorcPaths(cwd string, trusted bool) []string {
	var paths []string
	if p := defaultPigorcPath(); p != "" && fileExists(p) {
		paths = append(paths, p)
	}
	if trusted {
		if p := ProjectPigorcPath(cwd); p != "" && fileExists(p) {
			paths = append(paths, p)
		}
	}
	return paths
}

// LoadPigorc discovers and evaluates the pigorc scripts for cwd. version is
// exported to the script as PIGO_VERSION so it can feature-detect. No script
// present yields an empty (non-nil) Pigorc. Evaluation problems are reported as
// Warnings rather than errors: configuration must never block a launch.
func LoadPigorc(cwd string, trusted bool, version string) *Pigorc {
	paths := PigorcPaths(cwd, trusted)
	if len(paths) == 0 {
		return &Pigorc{}
	}
	p, err := EvaluatePigorc(cwd, paths, version)
	if err != nil {
		return &Pigorc{Sources: paths, Warnings: []string{err.Error()}}
	}
	return p
}

// EvaluatePigorc sources the given scripts, in order, in one Bash process and
// returns what they configured. cwd is the working directory the script sees.
func EvaluatePigorc(cwd string, paths []string, version string) (*Pigorc, error) {
	if len(paths) == 0 {
		return &Pigorc{}, nil
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		return nil, fmt.Errorf("pigorc: bash not found, skipping %s", strings.Join(paths, ", "))
	}

	dir, err := os.MkdirTemp("", "pigo-pigorc-")
	if err != nil {
		return nil, fmt.Errorf("pigorc: temp dir: %w", err)
	}
	preludePath := filepath.Join(dir, "prelude.sh")
	if err := os.WriteFile(preludePath, []byte(pigorcPrelude), 0o600); err != nil {
		return nil, fmt.Errorf("pigorc: write prelude: %w", err)
	}
	recordsPath := filepath.Join(dir, "records")
	hooksDir := filepath.Join(dir, "hooks")
	envBefore := filepath.Join(dir, "env.before")
	envAfter := filepath.Join(dir, "env.after")
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		return nil, fmt.Errorf("pigorc: create hook dir: %w", err)
	}

	script := buildPigorcScript(preludePath, paths)
	ctx, cancel := context.WithTimeout(context.Background(), pigorcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", script)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(),
		"PIGO_VERSION="+version,
		"PIGO_RC_RECORDS="+recordsPath,
		"PIGO_RC_HOOKS="+hooksDir,
		"PIGO_RC_ENV_BEFORE="+envBefore,
		"PIGO_RC_ENV_AFTER="+envAfter,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// A killed script can leave a grandchild holding the output pipe open; without
	// this Wait would block until that child exits, defeating the timeout.
	cmd.WaitDelay = time.Second

	p := &Pigorc{Sources: paths}
	err = cmd.Run()
	if out.Len() > 0 {
		p.Output = strings.TrimRight(out.String(), "\n")
	}
	if ctx.Err() != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("pigorc: evaluation timed out after %s", pigorcTimeout))
		return p, nil
	}
	if err != nil {
		// A failing script does not abort the launch: whatever it emitted before
		// failing is still applied, matching how a sourcing shell behaves.
		p.Warnings = append(p.Warnings, fmt.Sprintf("pigorc: %v", err))
	}

	records, readErr := os.ReadFile(recordsPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		p.Warnings = append(p.Warnings, fmt.Sprintf("pigorc: read records: %v", readErr))
	}
	p.parseRecords(string(records))
	p.parseEnv(envBefore, envAfter)
	return p, nil
}

// buildPigorcScript assembles the Bash program that sources the prelude and then
// each rc file. Paths are single-quoted for the shell; the prelude opens the
// record channel itself.
func buildPigorcScript(prelude string, paths []string) string {
	var b strings.Builder
	// errtrace off, but exit-on-error on: a failing statement aborts the load
	// (crushrc semantics), so a broken helper cannot silently leave half a
	// configuration applied and look like it succeeded.
	b.WriteString("set -eu\n")
	b.WriteString("exec 3>\"$PIGO_RC_RECORDS\"\n")
	fmt.Fprintf(&b, "source %s\n", shellQuote(prelude))
	for _, p := range paths {
		fmt.Fprintf(&b, "source %s\n", shellQuote(p))
	}
	b.WriteString("__pigo_emit_hook_functions\n")
	b.WriteString("__pigo_env > \"$PIGO_RC_ENV_AFTER\"\n")
	b.WriteString("exec 3>&-\n")
	return b.String()
}

// shellQuote renders s as a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseRecords folds the emitted command records into p.
func (p *Pigorc) parseRecords(records string) {
	for _, line := range strings.Split(records, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x1f")
		switch fields[0] {
		case "option":
			if len(fields) != 3 {
				p.warnf("option: malformed record %q", line)
				continue
			}
			key := canonicalOptionKey(fields[1])
			if key == "" {
				continue
			}
			if isListOption(key) {
				p.addList(key, fields[2])
				continue
			}
			if p.Options == nil {
				p.Options = make(map[string]string)
			}
			p.Options[key] = fields[2]
		case "option-reset":
			if len(fields) != 2 {
				p.warnf("option reset: malformed record %q", line)
				continue
			}
			key := canonicalOptionKey(fields[1])
			if p.Lists != nil {
				delete(p.Lists, key)
			}
		case "hook":
			if len(fields) != 5 {
				p.warnf("hook: malformed record %q", line)
				continue
			}
			p.addHook(fields[1], fields[2], fields[3], fields[4])
		case "provider":
			p.parseProviderRecord(fields)
		case "providermodel":
			if len(fields) != 3 {
				p.warnf("provider model: malformed record %q", line)
				continue
			}
			p.addProviderModel(fields[1], fields[2])
		case "hookfn":
			if len(fields) != 4 {
				p.warnf("hook function: malformed record %q", line)
				continue
			}
			// The generated wrapper defines the function and calls it, so the
			// existing command-hook pipeline (stdin JSON, exit codes, timeout)
			// handles a shell-function hook unchanged.
			p.addHook(fields[1], "", "", "bash "+shellQuote(fields[3]))
		default:
			p.warnf("unknown record %q", fields[0])
		}
	}
}

// parseProviderRecord folds one `provider add|remove` record into p.
func (p *Pigorc) parseProviderRecord(fields []string) {
	if len(fields) < 3 {
		p.warnf("provider: malformed record %v", fields)
		return
	}
	switch fields[1] {
	case "add":
		if len(fields) != 8 {
			p.warnf("provider add: malformed record %v", fields)
			return
		}
		def := ProviderConfig{
			Name:     strings.TrimSpace(fields[2]),
			Protocol: strings.TrimSpace(fields[3]),
			BaseURL:  strings.TrimSpace(fields[4]),
			EnvVar:   strings.TrimSpace(fields[5]),
			Auth:     strings.TrimSpace(fields[6]),
			APIKey:   strings.TrimSpace(fields[7]),
		}
		if def.Name == "" {
			p.warnf("provider add: the provider name must not be empty")
			return
		}
		// A repeated definition replaces the earlier one but keeps any models it
		// already accumulated, so `provider add x ...` can be refined per flag.
		if old := p.providerIndex(def.Name); old >= 0 {
			def.Models = p.Providers[old].Models
			p.Providers[old] = def
			return
		}
		p.Providers = append(p.Providers, def)
	case "remove":
		name := strings.TrimSpace(fields[2])
		if name == "" {
			p.warnf("provider remove: the provider name must not be empty")
			return
		}
		for i, def := range p.Providers {
			if def.Name == name {
				p.Providers = append(p.Providers[:i], p.Providers[i+1:]...)
				break
			}
		}
		p.RemovedProviders = append(p.RemovedProviders, name)
	default:
		p.warnf("provider: unknown action %q", fields[1])
	}
}

// addProviderModel appends one model id to a provider definition, creating the
// definition when the model was declared before it.
func (p *Pigorc) addProviderModel(name, id string) {
	name = strings.TrimSpace(name)
	id = strings.TrimSpace(id)
	if name == "" || id == "" {
		return
	}
	i := p.providerIndex(name)
	if i < 0 {
		p.Providers = append(p.Providers, ProviderConfig{Name: name})
		i = len(p.Providers) - 1
	}
	for _, existing := range p.Providers[i].Models {
		if existing == id {
			return
		}
	}
	p.Providers[i].Models = append(p.Providers[i].Models, id)
}

// providerIndex returns the position of a provider definition by name, or -1.
func (p *Pigorc) providerIndex(name string) int {
	for i, def := range p.Providers {
		if def.Name == name {
			return i
		}
	}
	return -1
}

// addHook registers one command hook on an event, validating the event name.
func (p *Pigorc) addHook(event, matcher, timeout, command string) {
	canonical, ok := hooks.CanonicalEvent(event)
	if !ok {
		p.warnf("hook: unknown event %q (want one of %s)", event, strings.Join(hooks.AllEvents, ", "))
		return
	}
	if strings.TrimSpace(command) == "" {
		p.warnf("hook %s: empty command", canonical)
		return
	}
	var secs *int
	if timeout != "" {
		n, err := strconv.Atoi(strings.TrimSpace(timeout))
		if err != nil || n <= 0 {
			p.warnf("hook %s: invalid timeout %q", canonical, timeout)
		} else {
			secs = &n
		}
	}
	if p.Hooks == nil {
		p.Hooks = make(hooks.HookSet)
	}
	p.Hooks[canonical] = append(p.Hooks[canonical], hooks.HookMatcherConfig{
		Matcher: matcher,
		Hooks:   []hooks.HookConfig{{Type: hooks.CommandType, Command: command, Timeout: secs}},
	})
}

// addList appends one value to a list option.
func (p *Pigorc) addList(key, value string) {
	if p.Lists == nil {
		p.Lists = make(map[string][]string)
	}
	p.Lists[key] = append(p.Lists[key], value)
}

// warnf records a non-fatal configuration problem.
func (p *Pigorc) warnf(format string, args ...any) {
	p.Warnings = append(p.Warnings, "pigorc: "+fmt.Sprintf(format, args...))
}

// parseEnv diffs the exported environment before and after the scripts ran and
// records the changes. PIGO_RC_* scaffolding is filtered out so a script cannot
// corrupt the harness.
func (p *Pigorc) parseEnv(beforePath, afterPath string) {
	before := readEnvDump(beforePath)
	after := readEnvDump(afterPath)
	for k, v := range after {
		if strings.HasPrefix(k, "PIGO_RC_") {
			continue
		}
		if old, ok := before[k]; ok && old == v {
			continue
		}
		if p.Env == nil {
			p.Env = make(map[string]string)
		}
		p.Env[k] = v
	}
	for k := range before {
		if strings.HasPrefix(k, "PIGO_RC_") {
			continue
		}
		if _, ok := after[k]; !ok {
			p.Unset = append(p.Unset, k)
		}
	}
	sort.Strings(p.Unset)
}

// readEnvDump parses a NUL-separated NAME/VALUE stream produced by the prelude.
func readEnvDump(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	out := make(map[string]string, len(parts)/2)
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "" {
			continue
		}
		out[parts[i]] = parts[i+1]
	}
	return out
}

// ApplyToFileConfig overlays the script's options onto cfg, giving pigorc
// precedence over the config.toml file (explicit CLI flags still win, because the
// caller applies this before overlaying flags). Unknown keys and invalid values
// are recorded as warnings instead of failing the launch. It returns all
// warnings collected so far.
func (p *Pigorc) ApplyToFileConfig(cfg *FileConfig) []string {
	if p == nil || cfg == nil {
		return nil
	}
	// Only report what this overlay adds: evaluation warnings were already
	// surfaced when the script was loaded.
	firstNew := len(p.Warnings)
	keys := make([]string, 0, len(p.Options))
	for k := range p.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p.applyOption(cfg, key, p.Options[key])
	}

	listKeys := make([]string, 0, len(p.Lists))
	for k := range p.Lists {
		listKeys = append(listKeys, k)
	}
	sort.Strings(listKeys)
	for _, key := range listKeys {
		values := p.Lists[key]
		switch key {
		case "allowed-tools":
			cfg.AllowedTools = values
		case "disallowed-tools":
			cfg.DisallowedTools = values
		case "prompts":
			cfg.Prompts = values
		case "checkpoint.thresholds":
			cfg.Checkpoint.Thresholds = values
		default:
			p.warnf("unknown list option %q", key)
		}
	}

	// Third-party provider definitions: script definitions win over the file's,
	// and `provider remove` drops a lower-layer definition.
	for _, name := range p.RemovedProviders {
		cfg.Providers = removeProviderConfig(cfg.Providers, name)
	}
	for _, def := range p.Providers {
		replaced := false
		for i, existing := range cfg.Providers {
			if existing.Name == def.Name {
				cfg.Providers[i] = def
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Providers = append(cfg.Providers, def)
		}
	}
	return p.Warnings[firstNew:]
}

// removeProviderConfig drops the definition named name from defs.
func removeProviderConfig(defs []ProviderConfig, name string) []ProviderConfig {
	out := defs[:0]
	for _, def := range defs {
		if def.Name != name {
			out = append(out, def)
		}
	}
	return out
}

// applyOption applies one scalar option to cfg.
func (p *Pigorc) applyOption(cfg *FileConfig, key, value string) {
	value = strings.TrimSpace(value)
	switch key {
	case "model":
		cfg.Model = value
	case "base-url":
		cfg.BaseURL = value
	case "api-key":
		cfg.APIKey = value
	case "credential":
		cfg.Credential = value
	case "protocol":
		cfg.Protocol = value
	case "provider":
		cfg.Provider = value
	case "thinking-level":
		cfg.ThinkingLevel = value
	case "output-format":
		cfg.OutputFormat = value
	case "system-prompt":
		cfg.SystemPrompt = value
	case "no-tools":
		p.applyBool(key, value, &cfg.NoTools)
	case "no-skills":
		p.applyBool(key, value, &cfg.NoSkills)
	case "approve":
		p.applyBool(key, value, &cfg.Approve)
	case "memory.enabled":
		p.applyBoolPtr(key, value, &cfg.Memory.Enabled)
	case "memory.reconcile-on-search":
		p.applyBoolPtr(key, value, &cfg.Memory.ReconcileOnSearch)
	case "memory.cc-index":
		p.applyBool(key, value, &cfg.Memory.CCIndex)
	case "memory.search-score-floor":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			cfg.Memory.SearchScoreFloor = &f
		} else {
			p.warnf("option %s: %q is not a number", key, value)
		}
	case "checkpoint.reserved":
		if n, err := strconv.Atoi(value); err == nil {
			cfg.Checkpoint.Reserved = IntOrString{IsInt: true, Int: n}
		} else {
			cfg.Checkpoint.Reserved = IntOrString{Str: value}
		}
	case "checkpoint.push-caps":
		p.warnf("option %s needs a section name, e.g. checkpoint.push-caps.memory", key)
	case "compaction.max-context":
		cfg.Compaction.MaxContext = value
	case "dream.enabled":
		p.applyBoolPtr(key, value, &cfg.Dream.Enabled)
	case "dream.interval-days":
		if n, err := strconv.Atoi(value); err == nil {
			cfg.Dream.IntervalDays = n
		} else {
			p.warnf("option %s: %q is not an integer", key, value)
		}
	case "dream.recent-sessions":
		if n, err := strconv.Atoi(value); err == nil {
			cfg.Dream.RecentSessions = n
		} else {
			p.warnf("option %s: %q is not an integer", key, value)
		}
	default:
		// checkpoint.push-caps is a nested table keyed by section.
		if section, ok := strings.CutPrefix(key, "checkpoint.push-caps."); ok && section != "" {
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
}

// applyBool parses a shell-style boolean; an unparseable value is a warning.
func (p *Pigorc) applyBool(key, value string, dst *bool) {
	b, ok := parseShellBool(value)
	if !ok {
		p.warnf("option %s: %q is not a boolean (want true/false/yes/no/1/0/on/off)", key, value)
		return
	}
	*dst = b
}

// applyBoolPtr is applyBool for a *bool that distinguishes "unset" from false.
func (p *Pigorc) applyBoolPtr(key, value string, dst **bool) {
	b, ok := parseShellBool(value)
	if !ok {
		p.warnf("option %s: %q is not a boolean (want true/false/yes/no/1/0/on/off)", key, value)
		return
	}
	v := b
	*dst = &v
}

// parseShellBool accepts the boolean spellings a shell user expects.
func parseShellBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true, true
	case "0", "false", "no", "n", "off":
		return false, true
	default:
		return false, false
	}
}

// canonicalOptionKey lowercases a key and normalizes underscores to dashes, so
// `option thinking_level high` and `option thinking-level high` are the same key.
func canonicalOptionKey(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.ReplaceAll(k, "_", "-")
	return k
}

// isListOption reports whether key accumulates values instead of overwriting.
func isListOption(key string) bool {
	switch key {
	case "allowed-tools", "disallowed-tools", "prompts", "checkpoint.thresholds":
		return true
	}
	return false
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// currentPigorc holds the process's evaluated pigorc so the config readers that
// run later (hook resolution) see the same values without re-running the script.
var currentPigorc atomic.Pointer[Pigorc]

// SetCurrentPigorc records the evaluated pigorc for the rest of the process.
func SetCurrentPigorc(p *Pigorc) { currentPigorc.Store(p) }

// CurrentPigorc returns the evaluated pigorc, or nil when none was loaded (tests,
// library use). Callers must treat nil as "no script config".
func CurrentPigorc() *Pigorc { return currentPigorc.Load() }

// HookSet returns the script-defined hook set, or nil.
func (p *Pigorc) HookSet() hooks.HookSet {
	if p == nil {
		return nil
	}
	return p.Hooks
}
