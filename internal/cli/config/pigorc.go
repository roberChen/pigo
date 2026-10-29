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
//	agent add <name> [--description T] [--system-prompt T | --prompt-file F]
//	          [--tools a,b] [--model M] [--thinking L] [--isolation I]
//	          [--timeout N] [--inherit-hooks]   define a custom sub-agent
//	agent hook <name> <event> [--matcher RE] [--timeout N] --command CMD
//	agent remove <name>                 drop an agent defined by an earlier file
//	mcp add <name> [--command CMD] [--arg A ...] [--env K V ...]
//	               [--url URL] [--header K V ...] [--timeout N]
//	               [--transport http|sse] [--oauth] [--disabled]
//	mcp remove <name>                   define an MCP server (stdio or http)
//
// Custom sub-agents are defined here only: the pigorc is their single source of
// truth (config.toml has no agent table). The evaluated definitions are published
// with SetCurrentAgents for the tool assembly.
//
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
	"github.com/smallnest/pigo/internal/mcp"
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
	// Agents holds the custom sub-agent definitions the scripts added, in
	// declaration order. A repeated name replaces the earlier definition while
	// keeping its hooks, and `agent remove` drops a definition gathered from an
	// earlier file (e.g. the global pigorc cancelled by the project one).
	Agents []AgentConfig
	// MCP holds the MCP server definitions the scripts added (a repeated name
	// replaces the earlier one). RemovedMCP names definitions to drop from a lower
	// layer.
	MCP        []mcp.ServerConfig
	RemovedMCP []string
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

// DefaultPigorcPath returns $PIGO_HOME/pigorc (default ~/.pigo/pigorc), or "" when
// the home directory cannot be resolved and PIGO_HOME is unset. It is exported so
// diagnostics (the pigo_config report) can name the file that was looked for even
// when it does not exist.
func DefaultPigorcPath() string {
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
	if p := DefaultPigorcPath(); p != "" && fileExists(p) {
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
		case "agent":
			p.parseAgentRecord(fields)
		case "mcp":
			p.parseMCPRecord(fields)
		case "mcparg":
			if len(fields) == 3 {
				p.addMCPValue(fields[1], func(c *mcp.ServerConfig) { c.Args = append(c.Args, fields[2]) })
			}
		case "mcpenv":
			if len(fields) == 4 {
				p.addMCPValue(fields[1], func(c *mcp.ServerConfig) {
					if c.Env == nil {
						c.Env = make(map[string]string)
					}
					c.Env[fields[2]] = fields[3]
				})
			}
		case "mcphdr":
			if len(fields) == 4 {
				p.addMCPValue(fields[1], func(c *mcp.ServerConfig) {
					if c.Headers == nil {
						c.Headers = make(map[string]string)
					}
					c.Headers[fields[2]] = fields[3]
				})
			}
		case "agenthook":
			if len(fields) != 6 {
				p.warnf("agent hook: malformed record %q", line)
				continue
			}
			p.addAgentHook(fields[1], AgentHookConfig{Event: fields[2], Matcher: fields[3], Timeout: atoiOrZero(fields[4]), Command: fields[5]})
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

// parseAgentRecord folds one `agent add|remove` record into p.
func (p *Pigorc) parseAgentRecord(fields []string) {
	if len(fields) < 3 {
		p.warnf("agent: malformed record %v", fields)
		return
	}
	switch fields[1] {
	case "add":
		// agent, add, name, description, tools, model, thinking, isolation,
		// timeout, inherit, systemPrompt
		if len(fields) != 11 {
			p.warnf("agent add: malformed record %v", fields)
			return
		}
		def := AgentConfig{
			Name:           strings.TrimSpace(fields[2]),
			Description:    strings.TrimSpace(fields[3]),
			Tools:          splitCSV(fields[4]),
			Model:          strings.TrimSpace(fields[5]),
			ThinkingLevel:  strings.TrimSpace(fields[6]),
			Isolation:      strings.TrimSpace(fields[7]),
			TimeoutSeconds: atoiOrZero(fields[8]),
			InheritHooks:   strings.EqualFold(strings.TrimSpace(fields[9]), "true"),
			SystemPrompt:   fields[10],
		}
		if def.Name == "" {
			p.warnf("agent add: the agent name must not be empty")
			return
		}
		if i := p.agentIndex(def.Name); i >= 0 {
			def.Hooks = p.Agents[i].Hooks
			p.Agents[i] = def
			return
		}
		p.Agents = append(p.Agents, def)
	case "remove":
		name := strings.TrimSpace(fields[2])
		if name == "" {
			p.warnf("agent remove: the agent name must not be empty")
			return
		}
		// Only definitions gathered by this evaluation can be removed (there is no
		// other agent-definition source), and only already-sourced ones: a later
		// `agent add` wins over an earlier `agent remove`.
		if i := p.agentIndex(name); i >= 0 {
			p.Agents = append(p.Agents[:i], p.Agents[i+1:]...)
		}
	default:
		p.warnf("agent: unknown action %q", fields[1])
	}
}

// addAgentHook attaches one hook to an agent definition, creating the definition
// when the hook was declared before it.
func (p *Pigorc) addAgentHook(name string, hook AgentHookConfig) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	i := p.agentIndex(name)
	if i < 0 {
		p.Agents = append(p.Agents, AgentConfig{Name: name})
		i = len(p.Agents) - 1
	}
	p.Agents[i].Hooks = append(p.Agents[i].Hooks, hook)
}

// agentIndex returns the position of an agent definition by name, or -1.
func (p *Pigorc) agentIndex(name string) int {
	for i, def := range p.Agents {
		if def.Name == name {
			return i
		}
	}
	return -1
}

// splitCSV splits a comma-separated flag value, dropping blanks.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// atoiOrZero parses an optional integer flag value.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// parseMCPRecord folds one `mcp add|remove` record into p.
func (p *Pigorc) parseMCPRecord(fields []string) {
	if len(fields) < 3 {
		p.warnf("mcp: malformed record %v", fields)
		return
	}
	switch fields[1] {
	case "add":
		// mcp, add, name, command, url, timeout, disabled, transport, oauth
		if len(fields) != 9 {
			p.warnf("mcp add: malformed record %v", fields)
			return
		}
		def := mcp.ServerConfig{
			Name:           strings.TrimSpace(fields[2]),
			Command:        strings.TrimSpace(fields[3]),
			URL:            strings.TrimSpace(fields[4]),
			TimeoutSeconds: atoiOrZero(fields[5]),
			Disabled:       strings.EqualFold(strings.TrimSpace(fields[6]), "true"),
			Transport:      strings.TrimSpace(fields[7]),
			OAuth:          strings.EqualFold(strings.TrimSpace(fields[8]), "true"),
		}
		if def.Name == "" {
			p.warnf("mcp add: the server name must not be empty")
			return
		}
		// A repeated definition layers onto the earlier one so a project file can
		// refine a global server (add args/env/headers, disable it) without repeating
		// the command. Empty scalars keep the previous value; disabled is last-wins so
		// a later `mcp add` re-enables a disabled server.
		if i := p.mcpIndex(def.Name); i >= 0 {
			existing := p.MCP[i]
			if def.Command == "" {
				def.Command = existing.Command
			}
			if def.URL == "" {
				def.URL = existing.URL
			}
			if def.TimeoutSeconds == 0 {
				def.TimeoutSeconds = existing.TimeoutSeconds
			}
			if def.Transport == "" {
				def.Transport = existing.Transport
			}
			if !def.OAuth {
				def.OAuth = existing.OAuth
			}
			def.Args = append(append([]string(nil), existing.Args...), def.Args...)
			def.Env = mergeStringMap(existing.Env, def.Env)
			def.Headers = mergeStringMap(existing.Headers, def.Headers)
			p.MCP[i] = def
			return
		}
		p.MCP = append(p.MCP, def)
	case "remove":
		name := strings.TrimSpace(fields[2])
		if name == "" {
			p.warnf("mcp remove: the server name must not be empty")
			return
		}
		for i, def := range p.MCP {
			if def.Name == name {
				p.MCP = append(p.MCP[:i], p.MCP[i+1:]...)
				break
			}
		}
		p.RemovedMCP = append(p.RemovedMCP, name)
	default:
		p.warnf("mcp: unknown action %q", fields[1])
	}
}

// mergeStringMap layers add over base (nil-safe), returning a fresh map.
func mergeStringMap(base, add map[string]string) map[string]string {
	if len(base) == 0 && len(add) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(add))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

// addMCPValue mutates the named server definition, creating it when a value was
// declared before its `mcp add`.
func (p *Pigorc) addMCPValue(name string, mutate func(*mcp.ServerConfig)) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	i := p.mcpIndex(name)
	if i < 0 {
		p.MCP = append(p.MCP, mcp.ServerConfig{Name: name})
		i = len(p.MCP) - 1
	}
	mutate(&p.MCP[i])
}

// mcpIndex returns the position of an MCP definition by name, or -1.
func (p *Pigorc) mcpIndex(name string) int {
	for i, def := range p.MCP {
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
		p.applyPigorcOption(cfg, key, p.Options[key])
	}

	listKeys := make([]string, 0, len(p.Lists))
	for k := range p.Lists {
		listKeys = append(listKeys, k)
	}
	sort.Strings(listKeys)
	for _, key := range listKeys {
		p.applyPigorcList(cfg, key, p.Lists[key])
	}

	// MCP server definitions: script definitions win over the file's, and
	// `mcp remove` drops a lower-layer definition.
	for _, name := range p.RemovedMCP {
		cfg.MCP = removeMCPConfig(cfg.MCP, name)
	}
	for _, def := range p.MCP {
		replaced := false
		for i, existing := range cfg.MCP {
			if existing.Name == def.Name {
				cfg.MCP[i] = def
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.MCP = append(cfg.MCP, def)
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

// removeMCPConfig drops the MCP server named name from defs.
func removeMCPConfig(defs []mcp.ServerConfig, name string) []mcp.ServerConfig {
	out := defs[:0]
	for _, def := range defs {
		if def.Name != name {
			out = append(out, def)
		}
	}
	return out
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

// isListOption reports whether key accumulates values instead of overwriting. It
// is derived from the option registry so a new list key needs no second edit.
func isListOption(key string) bool {
	spec, ok := lookupOption(key)
	return ok && spec.Kind == optList
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

// currentAgents holds the custom agent definitions resolved at startup so the tool
// assembly (run.SetupEnv) can register them without threading the config through
// every driver.
var currentAgents []AgentConfig

// SetCurrentAgents records the custom agent definitions for this process.
func SetCurrentAgents(agents []AgentConfig) { currentAgents = agents }

// CurrentAgents returns the custom agent definitions (nil when none were
// configured, or in library use where no config was loaded).
func CurrentAgents() []AgentConfig { return currentAgents }

// currentMCP holds the MCP server definitions resolved at startup so the tool
// assembly (run.SetupEnv) can connect them without threading config through every
// driver.
var currentMCP []mcp.ServerConfig

// SetCurrentMCP records the MCP server definitions for this process.
func SetCurrentMCP(servers []mcp.ServerConfig) { currentMCP = servers }

// CurrentMCP returns the MCP server definitions (nil when none were configured or
// no config was loaded, e.g. in library use).
func CurrentMCP() []mcp.ServerConfig { return currentMCP }
