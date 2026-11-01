package prompts

// Tests for the shared /skill and /mcp text commands. The interactive TUI reads
// the same helpers (FindSkill, ApplyMCPCommand) through its pickers, so these
// cover the REPL/headless contract: /skill lists and expands, and /mcp reports
// and switches servers without a front-end holding state.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/mcp"
	"github.com/smallnest/pigo/internal/runtime"
)

// skillCommandFixture builds a registry holding two skills with $ARGUMENTS.
func skillCommandFixture(t *testing.T) *runtime.SlashRegistry {
	t.Helper()
	skills := []*runtime.Skill{
		{Frontmatter: runtime.SkillFrontmatter{Name: "commit", Description: "write a commit message"}, Body: "Write a commit for: $ARGUMENTS"},
		{Frontmatter: runtime.SkillFrontmatter{Name: "review-it", Description: "review the diff"}, Body: "Review the diff."},
	}
	reg, err := BuildSlashRegistry(&cli.LiveConfig{Model: "test", ProviderName: "test"}, nil, skills, nil, PromptTemplateSources{})
	if err != nil {
		t.Fatalf("BuildSlashRegistry: %v", err)
	}
	return reg
}

// TestSkillCommandListsSkills verifies a bare /skill reports every loaded skill
// instead of starting a turn (the TUI opens its picker for the same invocation).
func TestSkillCommandListsSkills(t *testing.T) {
	reg := skillCommandFixture(t)
	out, err := reg.ResolveOutcome("/skill")
	if err != nil {
		t.Fatalf("ResolveOutcome: %v", err)
	}
	if out.Prompt != "" {
		t.Errorf("bare /skill should not start a turn, got prompt %q", out.Prompt)
	}
	for _, want := range []string{"commit", "review-it", "write a commit message", "($@)"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("listing missing %q:\n%s", want, out.Message)
		}
	}
}

// TestSkillCommandExpandsNamedSkill verifies "/skill <name> [args]" expands the
// skill body (with $ARGUMENTS) as the next turn and echoes the invocation.
func TestSkillCommandExpandsNamedSkill(t *testing.T) {
	reg := skillCommandFixture(t)
	out, err := reg.ResolveOutcome("/skill commit add tests")
	if err != nil {
		t.Fatalf("ResolveOutcome: %v", err)
	}
	if out.Kind != runtime.SlashPrompt {
		t.Errorf("Kind = %v, want SlashPrompt", out.Kind)
	}
	if out.Prompt != "Write a commit for: add tests" {
		t.Errorf("Prompt = %q, want the skill body with $ARGUMENTS", out.Prompt)
	}
}

// TestSkillCommandUnknownName verifies an unknown skill is reported (with the
// available names) rather than silently starting a turn.
func TestSkillCommandUnknownName(t *testing.T) {
	reg := skillCommandFixture(t)
	out, err := reg.ResolveOutcome("/skill nope")
	if err != nil {
		t.Fatalf("ResolveOutcome: %v", err)
	}
	if out.Prompt != "" {
		t.Errorf("unknown skill must not produce a prompt, got %q", out.Prompt)
	}
	if !strings.Contains(out.Message, "unknown skill") || !strings.Contains(out.Message, "commit") {
		t.Errorf("message = %q, want an unknown-skill report listing the skills", out.Message)
	}
}

// TestMCPCommandWithoutManager verifies /mcp degrades cleanly when no MCP
// manager was assembled (RegisterMCPCommand with nil).
func TestMCPCommandWithoutManager(t *testing.T) {
	reg := runtime.NewSlashRegistry()
	RegisterMCPCommand(reg, nil)
	out, err := reg.ResolveOutcome("/mcp")
	if err != nil {
		t.Fatalf("ResolveOutcome: %v", err)
	}
	if !strings.Contains(out.Message, "no MCP servers configured") {
		t.Errorf("message = %q, want the not-configured note", out.Message)
	}
	if msg := ApplyMCPCommand(nil, "github on"); !strings.Contains(msg, "no MCP servers configured") {
		t.Errorf("ApplyMCPCommand(nil) = %q", msg)
	}
}

// TestMCPCommandListsAndSwitches verifies /mcp lists configured servers with
// their state and that an unknown name is rejected without touching the manager.
func TestMCPCommandListsAndSwitches(t *testing.T) {
	mgr := mcp.Connect(context.Background(), []mcp.ServerConfig{
		{Name: "github", Command: "x", Disabled: true},
	}, io.Discard)
	t.Cleanup(func() { _ = mgr.Close() })

	reg := runtime.NewSlashRegistry()
	RegisterMCPCommand(reg, mgr)

	out, err := reg.ResolveOutcome("/mcp")
	if err != nil {
		t.Fatalf("ResolveOutcome: %v", err)
	}
	if !strings.Contains(out.Message, "github  off") {
		t.Errorf("listing = %q, want github off", out.Message)
	}

	out, err = reg.ResolveOutcome("/mcp missing on")
	if err != nil {
		t.Fatalf("ResolveOutcome(/mcp missing on): %v", err)
	}
	if !strings.Contains(out.Message, "unknown server") {
		t.Errorf("message = %q, want an unknown-server report", out.Message)
	}
	if got := mgr.Statuses(); len(got) != 1 || got[0].Active {
		t.Errorf("manager state changed on an unknown name: %+v", got)
	}
}
