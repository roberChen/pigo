package run

// Tests for custom sub-agent definitions wired into the tool set: each definition
// becomes its own tool with its own prompt, tool allow list, hooks (independent by
// default, optionally inherited) and validation, and it never shadows a builtin.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/runtime"
)

// agentTestContext builds the parent-side build context over a small tool set.
func agentTestContext(t *testing.T, tools ...string) agentBuildContext {
	t.Helper()
	taken := make(map[string]bool, len(tools))
	for _, name := range tools {
		taken[name] = true
	}
	return agentBuildContext{
		cwd:        t.TempDir(),
		takenNames: taken,
	}
}

// TestBuildAgentToolDefinition verifies a definition's fields reach the built
// tool: its name, prompt, tool allow list and timeout.
func TestBuildAgentToolDefinition(t *testing.T) {
	bc := agentTestContext(t, "task", "read", "grep", "bash")
	tool, err := buildAgentTool(config.AgentConfig{
		Name:           "researcher",
		Description:    "Read-only researcher",
		SystemPrompt:   "Research the repo.",
		Tools:          []string{"read", "grep"},
		TimeoutSeconds: 30,
	}, "researcher", bc)
	if err != nil {
		t.Fatalf("buildAgentTool: %v", err)
	}
	if tool.Name() != "researcher" {
		t.Errorf("Name() = %q", tool.Name())
	}
	if tool.Description() != "Read-only researcher" {
		t.Errorf("Description() = %q", tool.Description())
	}
	spec := tool.Spec()
	if spec.SystemPrompt != "Research the repo." {
		t.Errorf("SystemPrompt = %q", spec.SystemPrompt)
	}
	if len(spec.Tools) != 2 {
		t.Fatalf("child tools = %v, want read+grep", toolNames(spec.Tools))
	}
	if got := spec.Timeout.Seconds(); got != 30 {
		t.Errorf("Timeout = %v, want 30s", spec.Timeout)
	}
	// The child tool set never includes the delegation or question tools.
	for _, name := range toolNames(spec.Tools) {
		if name == "task" || name == "question" {
			t.Errorf("child tool set contains %q, which sub-agents must not have", name)
		}
	}
}

// TestBuildAgentToolUnknownToolWarns verifies an unavailable tool name is dropped
// (the child set is the builtin set plus task/question exclusions, so an arbitrary
// name cannot be granted) rather than silently widening the set.
func TestBuildAgentToolUnknownToolWarns(t *testing.T) {
	bc := agentTestContext(t, "task", "read")
	tool, err := buildAgentTool(config.AgentConfig{Name: "a", Tools: []string{"read", "nosuchtool"}}, "a", bc)
	if err != nil {
		t.Fatalf("buildAgentTool: %v", err)
	}
	if got := toolNames(tool.Spec().Tools); len(got) != 1 || got[0] != "read" {
		t.Fatalf("child tools = %v, want just read", got)
	}
}

// TestBuildAgentToolValidation verifies the definitions that must be rejected: an
// unknown isolation mode and an unknown thinking level.
func TestBuildAgentToolValidation(t *testing.T) {
	bc := agentTestContext(t, "task", "read")
	if _, err := buildAgentTool(config.AgentConfig{Name: "a", Isolation: "vm"}, "a", bc); err == nil {
		t.Error("unknown isolation must be rejected")
	}
	if _, err := buildAgentTool(config.AgentConfig{Name: "a", ThinkingLevel: "turbo"}, "a", bc); err == nil {
		t.Error("unknown thinking level must be rejected")
	}
}

// TestBuildAgentToolTimeoutOnlyWhenSet verifies an unset timeout leaves the child
// unbounded rather than expiring immediately.
func TestBuildAgentToolTimeoutOnlyWhenSet(t *testing.T) {
	bc := agentTestContext(t, "task")
	tool, err := buildAgentTool(config.AgentConfig{Name: "a"}, "a", bc)
	if err != nil {
		t.Fatalf("buildAgentTool: %v", err)
	}
	if tool.Spec().Timeout != 0 {
		t.Errorf("Timeout = %v, want none", tool.Spec().Timeout)
	}
}

// TestAgentHookSetIndependentByDefault verifies an agent's hooks are its own unless
// inherit_hooks is set.
func TestAgentHookSetIndependentByDefault(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	cwd := t.TempDir()
	set := agentHookSet(config.AgentConfig{
		Name: "a",
		Hooks: []config.AgentHookConfig{
			{Event: "PreToolUse", Matcher: "^bash$", Command: "./no-bash.sh", Timeout: 5},
		},
	}, cwd)
	if len(set) != 1 || len(set["PreToolUse"]) != 1 {
		t.Fatalf("hook set = %+v, want only the agent's own hook", set)
	}
	if got := set["PreToolUse"][0].Hooks[0]; got.Command != "./no-bash.sh" || got.TimeoutSeconds() != 5 {
		t.Errorf("hook = %+v", got)
	}
	// An unknown event is dropped, not registered.
	set = agentHookSet(config.AgentConfig{Name: "a", Hooks: []config.AgentHookConfig{{Event: "NotAnEvent", Command: "x"}}}, cwd)
	if len(set) != 0 {
		t.Errorf("hook set = %+v, want the unknown event dropped", set)
	}
}

// TestAgentHookSetInherit verifies inherit_hooks merges the resolved global hook
// set after the agent's own hooks.
func TestAgentHookSetInherit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	cwd := t.TempDir()
	// A global config.json hook, which ResolveHookSet picks up.
	writeJSON(t, home, `{"hooks":{"PreToolUse":[{"matcher":"^write$","hooks":[{"type":"command","command":"global-hook.sh"}]}]}}`)

	set := agentHookSet(config.AgentConfig{
		Name:         "a",
		InheritHooks: true,
		Hooks:        []config.AgentHookConfig{{Event: "PostToolUse", Command: "./own.sh"}},
	}, cwd)
	pre := set["PreToolUse"]
	if len(pre) != 1 || pre[0].Hooks[0].Command != "global-hook.sh" {
		t.Errorf("PreToolUse = %+v, want the inherited global hook", pre)
	}
	if post := set["PostToolUse"]; len(post) != 1 || post[0].Hooks[0].Command != "./own.sh" {
		t.Errorf("PostToolUse = %+v, want the agent's own hook", post)
	}
}

// TestSetupEnvRegistersAgentTools verifies the definitions published at startup are
// registered into the assembled tool set.
func TestSetupEnvRegistersAgentTools(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	config.SetCurrentAgents([]config.AgentConfig{{
		Name:         "researcher",
		Description:  "Read-only researcher",
		SystemPrompt: "Research.",
		Tools:        []string{"read"},
	}})
	t.Cleanup(func() { config.SetCurrentAgents(nil) })

	env, err := SetupEnv("openrouter/free", "", "", "", "", false /*noTools*/, true /*noSkills*/, "", nil, false /*memEnabled*/, ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	var found bool
	for _, tool := range env.Tools {
		if tool.Name() == "researcher" {
			found = true
			if !strings.Contains(tool.Description(), "Read-only") {
				t.Errorf("description = %q", tool.Description())
			}
		}
	}
	if !found {
		t.Fatalf("configured agent tool missing from the tool set: %v", toolNames(env.Tools))
	}

	// A definition that collides with a builtin is skipped, not registered twice.
	config.SetCurrentAgents([]config.AgentConfig{{Name: "read"}, {Name: "task"}})
	env, err = SetupEnv("openrouter/free", "", "", "", "", false, true, "", nil, false, ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	seen := map[string]int{}
	for _, tool := range env.Tools {
		seen[tool.Name()]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("tool %q registered %d times", name, count)
		}
	}
}

// toolNames maps tools to their names.
func toolNames(tools []agentcore.AgentTool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name()
	}
	return out
}

// writeJSON writes content to <home>/config.json (the global config layer
// ResolveHookSet reads).
func writeJSON(t *testing.T, home, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// TestFollowUpsComposition verifies the follow-up seam composition: nil when
// neither provider exists, and background job reports take precedence over due
// schedule reminders.
func TestFollowUpsComposition(t *testing.T) {
	if fn := FollowUps(nil, nil); fn != nil {
		t.Error("FollowUps(nil, nil) should leave the seam unwired")
	}

	jobs := runtime.NewSubAgentJobs()
	sched := agenttool.NewSchedule()
	fn := FollowUps(jobs, sched)
	if fn == nil {
		t.Fatal("FollowUps with providers should return a seam")
	}
	// No jobs, no due reminders: nothing to deliver.
	if msgs := fn(context.Background(), &agentcore.AgentContext{}); len(msgs) != 0 {
		t.Errorf("follow-ups = %d, want 0", len(msgs))
	}

	// A finished job's report is delivered before any reminder.
	done := make(chan struct{})
	jobs.Start("task", "bg", "p", context.Background(), func(context.Context, string) (string, string, error) {
		close(done)
		return "job report", "", nil
	})
	<-done
	// Wait for the registry to record the completion (the job finishes just after
	// its runner returns).
	deadline := time.Now().Add(2 * time.Second)
	var msgs []agentcore.AgentMessage
	for time.Now().Before(deadline) {
		if msgs = fn(context.Background(), &agentcore.AgentContext{}); len(msgs) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(msgs) != 1 {
		t.Fatalf("follow-ups = %d, want the job report", len(msgs))
	}
	if got := agentcore.ContentToText(msgs[0].(agentcore.UserMessage).Content); !strings.Contains(got, "job report") {
		t.Errorf("follow-up = %q, want the job report", got)
	}
}

// TestSetupEnvWiresBackgroundJobs verifies the assembled tool set carries a shared
// background-job registry, the task_output handle for it, and that the registry is
// reachable from the tools for the follow-up seam.
func TestSetupEnvWiresBackgroundJobs(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	config.SetCurrentAgents(nil)
	env, err := SetupEnv("openrouter/free", "", "", "", "", false /*noTools*/, true /*noSkills*/, "", nil, false /*memEnabled*/, ToolPolicy{})
	if err != nil {
		t.Fatalf("SetupEnv: %v", err)
	}
	if env.SubAgentJobs == nil {
		t.Fatal("Env.SubAgentJobs should be wired")
	}
	var hasOutput bool
	for _, tool := range env.Tools {
		if tool.Name() == "task_output" {
			hasOutput = true
		}
	}
	if !hasOutput {
		t.Fatalf("task_output missing from the tool set: %v", toolNames(env.Tools))
	}
	if got := runtime.SubAgentJobsFromTools(env.Tools); got != env.SubAgentJobs {
		t.Error("the tools and Env should share one registry")
	}
}
