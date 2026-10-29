package config

// Tests for pigorc, the Bash-script config layer: discovery and trust gating, the
// option overlay onto config.toml, hook extraction (command hooks and shell
// function hooks), exported-variable capture, and the best-effort failure modes.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/mcp"
	"time"
)

// requireBash skips a test when no bash is available (the layer degrades to a
// warning in that case, which has its own test).
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

// writeFile writes path with mode 0600, creating parent dirs.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestPigorcPathsToggleProjectFile verifies the project script is trust-gated
// (a checked-out repo must not run code on launch) while the global one always
// applies.
func TestPigorcPathsToggleProjectFile(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	writeFile(t, filepath.Join(home, "pigorc"), "option model home\n")
	writeFile(t, filepath.Join(cwd, ".pigorc"), "option model project\n")

	untrusted := PigorcPaths(cwd, false)
	if len(untrusted) != 1 || untrusted[0] != filepath.Join(home, "pigorc") {
		t.Fatalf("untrusted paths = %v, want only the global pigorc", untrusted)
	}
	trusted := PigorcPaths(cwd, true)
	if len(trusted) != 2 || trusted[1] != filepath.Join(cwd, ".pigorc") {
		t.Fatalf("trusted paths = %v, want global then project", trusted)
	}
}

// TestLoadPigorcWithoutBash verifies a missing bash degrades to a warning rather
// than a failed launch.
func TestLoadPigorcWithoutBash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	writeFile(t, filepath.Join(home, "pigorc"), "option model m\n")
	t.Setenv("PATH", "") // exec.LookPath("bash") now fails

	p := LoadPigorc(t.TempDir(), false, "test")
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "bash not found") {
		t.Fatalf("warnings = %v, want a bash-not-found warning", p.Warnings)
	}
	if len(p.Options) != 0 {
		t.Errorf("options = %v, want none without bash", p.Options)
	}
}

// TestEvaluatePigorc reads a script that exercises the whole DSL: options (flat,
// nested, list), a command hook, a shell-function hook, and exported/unset
// variables.
func TestEvaluatePigorc(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
option model "zai/glm-4.6"
option thinking_level high
option no_tools yes
option system-prompt "be brief"
option allowed-tools read
option allowed-tools grep
option allowed-tools read
option prompts "~/.pigo/prompts"
option memory.enabled false
option memory.search_score_floor 0.25
option checkpoint.thresholds "40%"
option checkpoint.thresholds "85%"
option checkpoint.push-caps.memory 900
option compaction.max_context "200K"
option dream.interval_days 3
option bogus.key nope
hook add PreToolUse --matcher '^bash$' --command 'echo blocked; exit 2' --timeout 5
PreToolUse() { printf '{"decision":"block","reason":"from function"}\n'; }
post_tool_use() { :; }
export PIGO_TEST_PIGORC=hello
export PIGO_TEST_EMPTY=
unset PIGO_TEST_UNSET
`)
	t.Setenv("PIGO_TEST_UNSET", "1")
	t.Setenv("PIGO_TEST_KEPT", "keep")

	p, err := EvaluatePigorc(dir, []string{rc}, "9.9.9")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if p.Sources[0] != rc {
		t.Errorf("sources = %v, want %s", p.Sources, rc)
	}

	wantOptions := map[string]string{
		"model":                       "zai/glm-4.6",
		"thinking-level":              "high",
		"no-tools":                    "yes",
		"system-prompt":               "be brief",
		"memory.enabled":              "false",
		"memory.search-score-floor":   "0.25",
		"checkpoint.push-caps.memory": "900",
		"compaction.max-context":      "200K",
		"dream.interval-days":         "3",
		"bogus.key":                   "nope",
	}
	for k, want := range wantOptions {
		if got := p.Options[k]; got != want {
			t.Errorf("option %q = %q, want %q", k, got, want)
		}
	}
	if got := p.Lists["allowed-tools"]; len(got) != 3 || got[0] != "read" || got[1] != "grep" {
		t.Errorf("allowed-tools = %v, want read,greep,read in order", got)
	}
	if got := p.Lists["checkpoint.thresholds"]; len(got) != 2 || got[1] != "85%" {
		t.Errorf("thresholds = %v, want 40%%,85%%", got)
	}
	if got := p.Lists["prompts"]; len(got) != 1 || got[0] != "~/.pigo/prompts" {
		t.Errorf("prompts = %v", got)
	}

	// Hooks: the command hook (with matcher + timeout) then the function hook.
	matchers := p.Hooks["PreToolUse"]
	if len(matchers) != 2 {
		t.Fatalf("PreToolUse matchers = %d, want 2 (command + function hook): %+v", len(matchers), matchers)
	}
	if matchers[0].Matcher != "^bash$" || matchers[0].Hooks[0].Command != "echo blocked; exit 2" {
		t.Errorf("command hook = %+v", matchers[0])
	}
	if got := matchers[0].Hooks[0].TimeoutSeconds(); got != 5 {
		t.Errorf("timeout = %d, want 5", got)
	}
	fnCmd := matchers[1].Hooks[0].Command
	if !strings.HasPrefix(fnCmd, "bash '") {
		t.Fatalf("function hook command = %q, want a bash wrapper", fnCmd)
	}
	wrapper := strings.TrimSuffix(strings.TrimPrefix(fnCmd, "bash '"), "'")
	body, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatalf("generated hook wrapper not readable: %v", err)
	}
	if !strings.Contains(string(body), "PreToolUse ()") || !strings.Contains(string(body), `"from function"`) {
		t.Errorf("wrapper does not carry the function:\n%s", body)
	}

	// The wrapper runs like any command hook: stdout JSON, exit code decides.
	out, err := exec.Command("bash", wrapper).CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"decision":"block"`) {
		t.Errorf("wrapper output = %q, want the function's JSON", out)
	}

	// The snake_case function name resolves to the canonical event.
	if _, ok := p.Hooks["PostToolUse"]; !ok {
		t.Errorf("post_tool_use() was not registered as a PostToolUse hook: %v", p.Hooks)
	}

	// Environment: only the changes are captured, and unrelated vars are kept.
	if got := p.Env["PIGO_TEST_PIGORC"]; got != "hello" {
		t.Errorf("exported var = %q, want hello", got)
	}
	if got, ok := p.Env["PIGO_TEST_EMPTY"]; !ok || got != "" {
		t.Errorf("empty export missing: %q ok=%v", got, ok)
	}
	if _, ok := p.Env["PIGO_TEST_KEPT"]; ok {
		t.Error("unchanged variables must not be reported as changes")
	}
	if len(p.Unset) != 1 || p.Unset[0] != "PIGO_TEST_UNSET" {
		t.Errorf("unset = %v, want [PIGO_TEST_UNSET]", p.Unset)
	}
	// Option keys are validated when the overlay is applied, not while parsing.
	var cfg FileConfig
	if warnings := strings.Join(p.ApplyToFileConfig(&cfg), "\n"); !strings.Contains(warnings, `unknown option "bogus.key"`) {
		t.Errorf("warnings = %q, want an unknown-option warning", warnings)
	}
}

// TestPigorcOptionsOverlayFileConfig verifies the option overlay fills every
// supported config.toml field, including the nested tables.
func TestPigorcOptionsOverlayFileConfig(t *testing.T) {
	p := &Pigorc{
		Options: map[string]string{
			"model":                       "m1",
			"provider":                    "zai",
			"base-url":                    "https://example.test/v1",
			"api-key":                     "sk-test",
			"credential":                  "work",
			"protocol":                    "anthropic",
			"thinking-level":              "high",
			"output-format":               "stream-json",
			"system-prompt":               "terse",
			"no-tools":                    "off",
			"no-skills":                   "1",
			"approve":                     "true",
			"memory.enabled":              "false",
			"memory.reconcile-on-search":  "no",
			"memory.cc-index":             "on",
			"memory.search-score-floor":   "0.4",
			"checkpoint.reserved":         "8192",
			"checkpoint.push-caps.memory": "700",
			"compaction.max-context":      "300K",
			"dream.enabled":               "false",
			"dream.interval-days":         "14",
			"dream.recent-sessions":       "5",
		},
		Lists: map[string][]string{
			"allowed-tools":         {"read"},
			"disallowed-tools":      {"bash"},
			"prompts":               {"~/prompts"},
			"checkpoint.thresholds": {"50%", "90%"},
		},
	}
	var cfg FileConfig
	warnings := p.ApplyToFileConfig(&cfg)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if cfg.Model != "m1" || cfg.Provider != "zai" || cfg.BaseURL != "https://example.test/v1" ||
		cfg.APIKey != "sk-test" || cfg.Credential != "work" || cfg.Protocol != "anthropic" ||
		cfg.ThinkingLevel != "high" || cfg.OutputFormat != "stream-json" || cfg.SystemPrompt != "terse" {
		t.Errorf("flat options not applied: %+v", cfg)
	}
	if cfg.NoTools || !cfg.NoSkills || !cfg.Approve {
		t.Errorf("booleans not applied: no_tools=%v no_skills=%v approve=%v", cfg.NoTools, cfg.NoSkills, cfg.Approve)
	}
	if cfg.Memory.Enabled == nil || *cfg.Memory.Enabled {
		t.Errorf("memory.enabled should be false")
	}
	if cfg.Memory.ReconcileOnSearch == nil || *cfg.Memory.ReconcileOnSearch {
		t.Errorf("memory.reconcile-on-search should be false")
	}
	if !cfg.Memory.CCIndex {
		t.Error("memory.cc-index should be true")
	}
	if cfg.Memory.SearchScoreFloor == nil || *cfg.Memory.SearchScoreFloor != 0.4 {
		t.Errorf("search score floor = %v", cfg.Memory.SearchScoreFloor)
	}
	if !cfg.Checkpoint.Reserved.IsInt || cfg.Checkpoint.Reserved.Int != 8192 {
		t.Errorf("checkpoint.reserved = %+v", cfg.Checkpoint.Reserved)
	}
	if got := cfg.Checkpoint.PushCaps["memory"]; got != 700 {
		t.Errorf("push-caps memory = %d", got)
	}
	if len(cfg.Checkpoint.Thresholds) != 2 {
		t.Errorf("thresholds = %v", cfg.Checkpoint.Thresholds)
	}
	if cfg.Compaction.MaxContext != "300K" {
		t.Errorf("max_context = %q", cfg.Compaction.MaxContext)
	}
	if cfg.Dream.Enabled == nil || *cfg.Dream.Enabled {
		t.Errorf("dream.enabled should be false")
	}
	if cfg.Dream.IntervalDays != 14 || cfg.Dream.RecentSessions != 5 {
		t.Errorf("dream = %+v", cfg.Dream)
	}
	if len(cfg.AllowedTools) != 1 || cfg.AllowedTools[0] != "read" || cfg.DisallowedTools[0] != "bash" {
		t.Errorf("tool boundary not applied: %v / %v", cfg.AllowedTools, cfg.DisallowedTools)
	}
	if len(cfg.Prompts) != 1 || cfg.Prompts[0] != "~/prompts" {
		t.Errorf("prompts = %v", cfg.Prompts)
	}
}

// TestPigorcInvalidOptionWarns verifies a bad value or unknown key warns and
// leaves the rest of the overlay intact (a typo must not brick startup).
func TestPigorcInvalidOptionWarns(t *testing.T) {
	p := &Pigorc{
		Options: map[string]string{
			"no-tools":                  "maybe",
			"memory.search-score-floor": "high",
			"dream.interval-days":       "weekly",
			"totally.unknown":           "x",
			"checkpoint.push-caps":      "9",
			"model":                     "m",
		},
		Lists: map[string][]string{"bogus-list": {"x"}},
	}
	var cfg FileConfig
	warnings := strings.Join(p.ApplyToFileConfig(&cfg), "\n")
	if cfg.Model != "m" {
		t.Errorf("valid options must still apply, model = %q", cfg.Model)
	}
	for _, want := range []string{
		`option no-tools: "maybe" is not a boolean`,
		`option memory.search-score-floor: "high" is not a number`,
		`option dream.interval-days: "weekly" is not an integer`,
		`unknown option "totally.unknown"`,
		`checkpoint.push-caps needs a section name`,
		`unknown list option "bogus-list"`,
	} {
		if !strings.Contains(warnings, want) {
			t.Errorf("warnings missing %q:\n%s", want, warnings)
		}
	}
}

// TestPigorcOptionResetClearsLists verifies `option reset` wipes a list gathered
// earlier (including one pulled in via a sourced helper file).
func TestPigorcOptionResetClearsLists(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
option allowed-tools read
option allowed-tools grep
option reset allowed-tools
option allowed-tools find
`)
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if got := p.Lists["allowed-tools"]; len(got) != 1 || got[0] != "find" {
		t.Fatalf("allowed-tools = %v, want [find]", got)
	}
}

// TestPigorcScriptFailureKeepsEarlierOptions verifies a script that fails midway
// keeps what it already emitted and reports a warning instead of aborting.
func TestPigorcScriptFailureKeepsEarlierOptions(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, "option model before\nfalse\noption model after\n")

	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if got := p.Options["model"]; got != "before" {
		t.Errorf("model = %q, want the option emitted before the failure", got)
	}
	if len(p.Warnings) == 0 {
		t.Error("a failing script should be reported as a warning")
	}
}

// TestPigorcTimeout verifies a hanging script is cut off with a warning instead of
// blocking startup.
func TestPigorcTimeout(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, "sleep 5\n")

	old := pigorcTimeout
	pigorcTimeout = 150 * time.Millisecond
	t.Cleanup(func() { pigorcTimeout = old })

	start := time.Now()
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("evaluation took %v, want it cut off near the timeout", elapsed)
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "timed out") {
		t.Errorf("warnings = %v, want a timeout warning", p.Warnings)
	}
}

// TestPigorcPreludeRegistersEveryEvent verifies the generated prelude knows every
// hook event, so a function hook for any of them is recognized.
func TestPigorcPreludeRegistersEveryEvent(t *testing.T) {
	for _, ev := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "Stop", "SubagentStop", "SessionStart", "SessionEnd", "PreCompact", "Notification"} {
		if !strings.Contains(pigorcPrelude, ev) {
			t.Errorf("prelude is missing event %q", ev)
		}
	}
}

// TestLoadPigorcProjectOverridesGlobal verifies both files are sourced in order,
// so the project script wins on conflicts.
func TestLoadPigorcProjectOverridesGlobal(t *testing.T) {
	requireBash(t)
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	writeFile(t, filepath.Join(home, "pigorc"), "option model global-model\noption provider global-provider\n")
	writeFile(t, filepath.Join(cwd, ".pigorc"), "option model project-model\n")

	p := LoadPigorc(cwd, true, "test")
	if got := p.Options["model"]; got != "project-model" {
		t.Errorf("model = %q, want the project override", got)
	}
	if got := p.Options["provider"]; got != "global-provider" {
		t.Errorf("provider = %q, want the global value to survive", got)
	}
	if len(p.Sources) != 2 {
		t.Errorf("sources = %v, want both files", p.Sources)
	}

	untrusted := LoadPigorc(cwd, false, "test")
	if got := untrusted.Options["model"]; got != "global-model" {
		t.Errorf("untrusted model = %q, want the global value only", got)
	}
}

// TestPigorcProviderDefinitions verifies the `provider add` / `provider remove`
// DSL: flags become a ProviderConfig, repeated --model accumulates, and applying
// the script to a FileConfig replaces same-name definitions and drops removed ones.
func TestPigorcProviderDefinitions(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
provider add mycorp --type openai --base-url "https://llm.mycorp.test/v1" \
  --env-var MYCORP_KEY --auth bearer --model mycorp-large --model mycorp-small
provider add othercorp --type anthropic --model claude-custom
provider remove legacy
`)
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if len(p.Providers) != 2 {
		t.Fatalf("providers = %+v, want 2", p.Providers)
	}
	my := p.Providers[0]
	if my.Name != "mycorp" || my.Protocol != "openai" || my.BaseURL != "https://llm.mycorp.test/v1" ||
		my.EnvVar != "MYCORP_KEY" || my.Auth != "bearer" {
		t.Errorf("mycorp = %+v", my)
	}
	if len(my.Models) != 2 || my.Models[0] != "mycorp-large" || my.Models[1] != "mycorp-small" {
		t.Errorf("mycorp models = %v", my.Models)
	}
	if got := p.Providers[1]; got.Name != "othercorp" || got.Protocol != "anthropic" || len(got.Models) != 1 {
		t.Errorf("othercorp = %+v", got)
	}
	if len(p.RemovedProviders) != 1 || p.RemovedProviders[0] != "legacy" {
		t.Errorf("removed = %v, want [legacy]", p.RemovedProviders)
	}

	// Applying to a file config: the script's definition wins by name, and the
	// removed one is dropped.
	cfg := FileConfig{Providers: []ProviderConfig{
		{Name: "legacy", BaseURL: "https://old.test"},
		{Name: "mycorp", BaseURL: "https://file.test", Protocol: "openai"},
		{Name: "keepme", BaseURL: "https://keep.test"},
	}}
	if warnings := p.ApplyToFileConfig(&cfg); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("providers after overlay = %+v, want legacy dropped and 3 kept", cfg.Providers)
	}
	byName := map[string]ProviderConfig{}
	for _, def := range cfg.Providers {
		byName[def.Name] = def
	}
	if _, ok := byName["legacy"]; ok {
		t.Error("provider remove did not drop the lower-layer definition")
	}
	if got := byName["mycorp"]; got.BaseURL != "https://llm.mycorp.test/v1" || len(got.Models) != 2 {
		t.Errorf("script definition did not win: %+v", got)
	}
	if _, ok := byName["keepme"]; !ok {
		t.Error("unrelated file definitions must survive")
	}
}

// TestPigorcAgentDefinitions verifies the `agent add` / `agent hook` /
// `agent remove` DSL: flags become an AgentConfig, hooks attach to their agent (and
// survive a re-declaration), an agent's hooks are independent by default, and
// `agent remove` cancels a definition gathered from an earlier file.
func TestPigorcAgentDefinitions(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "researcher.md")
	writeFile(t, promptFile, "Research the repository.\nCite file:line.\n")

	// Global file: defines two agents and hooks one of them.
	global := filepath.Join(dir, "pigorc")
	writeFile(t, global, `
agent add researcher --description "Read-only researcher" --prompt-file "`+promptFile+`" \
  --tools read,grep,find --model claude-haiku-4 --thinking off --timeout 120
agent hook researcher PreToolUse --matcher '^bash$' --command './hooks/no-bash.sh'
agent add writer --description "Edits files" --inherit-hooks
`)
	// Project file: refines the researcher (keeping its hooks) and removes writer.
	project := filepath.Join(dir, ".pigorc")
	writeFile(t, project, `
agent add researcher --description "Refined researcher" --tools read
agent remove writer
`)

	p, err := EvaluatePigorc(dir, []string{global, project}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if len(p.Agents) != 1 {
		t.Fatalf("agents = %+v, want only researcher after the remove", p.Agents)
	}
	researcher := p.Agents[0]
	if researcher.Name != "researcher" || researcher.Description != "Refined researcher" {
		t.Errorf("researcher = %+v, want the project file's refinement", researcher)
	}
	if got := researcher.Tools; len(got) != 1 || got[0] != "read" {
		t.Errorf("researcher tools = %v, want the refined allow list", got)
	}
	if len(researcher.Hooks) != 1 || researcher.Hooks[0].Event != "PreToolUse" || researcher.Hooks[0].Matcher != "^bash$" {
		t.Errorf("researcher hooks = %+v, want the global hook kept across the re-declaration", researcher.Hooks)
	}
	if researcher.InheritHooks {
		t.Error("hooks must be independent by default")
	}

	// A single-file definition carries every flag through.
	p, err = EvaluatePigorc(dir, []string{global, project, writeNamed(t, dir, `
agent add solitary --system-prompt "Do one thing." --isolation process --timeout 42 --inherit-hooks
`)}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	var solitary *AgentConfig
	for i := range p.Agents {
		if p.Agents[i].Name == "solitary" {
			solitary = &p.Agents[i]
		}
	}
	if solitary == nil {
		t.Fatalf("solitary agent missing: %+v", p.Agents)
	}
	if solitary.SystemPrompt != "Do one thing." || solitary.Isolation != "process" ||
		solitary.TimeoutSeconds != 42 || !solitary.InheritHooks {
		t.Errorf("solitary = %+v", *solitary)
	}
}

// writeNamed writes content to a fresh file under dir and returns its path.
func writeNamed(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "extra-"+time.Now().Format("150405.000000000")+".pigorc")
	writeFile(t, path, content)
	return path
}

// TestPigorcMCPDefinitions verifies the `mcp add` / `mcp remove` DSL: flags become an
// mcp.ServerConfig (stdio or http), repeated --arg/--env/--header accumulate, a
// re-declaration keeps them, and applying the script replaces same-name definitions
// and drops removed ones.
func TestPigorcMCPDefinitions(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	// The script expands $GH_PAT; with `set -u` an unset variable aborts the load.
	t.Setenv("GH_PAT", "test-pat")

	global := filepath.Join(dir, "pigorc")
	writeFile(t, global, `
mcp add local --command npx --arg -y --arg server-filesystem --arg /tmp \
  --env FOO bar --timeout 30
mcp add github --url "https://api.githubcopilot.com/mcp/" --header Authorization "Bearer $GH_PAT"
mcp add legacy --command old-server
mcp add old --url "https://legacy.test/sse" --transport sse --oauth
`)
	project := filepath.Join(dir, ".pigorc")
	writeFile(t, project, `
mcp add local --disabled
mcp remove legacy
`)
	p, err := EvaluatePigorc(dir, []string{global, project}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if len(p.MCP) != 3 {
		t.Fatalf("mcp servers = %+v, want local + github + old", p.MCP)
	}
	local := p.MCP[0]
	// The project file only refined it (disable): command/timeout/args/env survive.
	if local.Name != "local" || local.Command != "npx" || local.TimeoutSeconds != 30 || !local.Disabled {
		t.Errorf("local = %+v", local)
	}
	if got := local.Args; len(got) != 3 || got[0] != "-y" || got[2] != "/tmp" {
		t.Errorf("local args = %v", got)
	}
	if local.Env["FOO"] != "bar" || local.Headers != nil {
		t.Errorf("local env/headers = %v / %v", local.Env, local.Headers)
	}
	github := p.MCP[1]
	if github.Name != "github" || github.URL != "https://api.githubcopilot.com/mcp/" {
		t.Errorf("github = %+v", github)
	}
	if got := github.Headers["Authorization"]; got != "Bearer test-pat" {
		t.Errorf("github headers = %v, want the expanded Authorization value", got)
	}
	if old := p.MCP[2]; old.Name != "old" || old.Transport != "sse" || !old.OAuth {
		t.Errorf("old = %+v, want the sse transport with oauth", old)
	}
	if len(p.RemovedMCP) != 1 || p.RemovedMCP[0] != "legacy" {
		t.Errorf("removed = %v, want [legacy]", p.RemovedMCP)
	}

	// Applying to a file config: script definitions win by name, removed ones drop.
	cfg := FileConfig{MCP: []mcp.ServerConfig{
		{Name: "legacy", Command: "old-server"},
		{Name: "local", Command: "from-file"},
		{Name: "keepme", URL: "https://keep.test/mcp"},
	}}
	if warnings := p.ApplyToFileConfig(&cfg); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	byName := map[string]mcp.ServerConfig{}
	for _, def := range cfg.MCP {
		byName[def.Name] = def
	}
	if _, ok := byName["legacy"]; ok {
		t.Error("mcp remove did not drop the lower-layer definition")
	}
	if got := byName["local"]; got.Command != "npx" || !got.Disabled {
		t.Errorf("script definition did not win: %+v", got)
	}
	if _, ok := byName["keepme"]; !ok {
		t.Error("unrelated file definitions must survive")
	}
}
