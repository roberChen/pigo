package run

// Tests for the pigorc hook merge: script-defined hooks join the layered hook set
// after the JSON layers, so they share the same trust gate and dispatch path.

import (
	"testing"

	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/hooks"
)

// TestResolveHookSetAppendsPigorcHooks verifies a pigorc hook reaches the resolved
// set alongside (and after) the file layers, and that no pigorc means an empty set.
func TestResolveHookSetAppendsPigorcHooks(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	cwd := t.TempDir()

	base, err := ResolveHookSet(cwd, false)
	if err != nil {
		t.Fatalf("ResolveHookSet: %v", err)
	}
	if len(base) != 0 {
		t.Fatalf("hook set without pigorc = %v, want empty", base)
	}

	config.SetCurrentPigorc(&config.Pigorc{Hooks: hooks.HookSet{
		"PreToolUse": []hooks.HookMatcherConfig{{
			Matcher: "^bash$",
			Hooks:   []hooks.HookConfig{{Type: hooks.CommandType, Command: "echo pigorc"}},
		}},
		"Notification": []hooks.HookMatcherConfig{{
			Hooks: []hooks.HookConfig{{Type: hooks.CommandType, Command: "echo notify"}},
		}},
	}})
	t.Cleanup(func() { config.SetCurrentPigorc(nil) })

	set, err := ResolveHookSet(cwd, false)
	if err != nil {
		t.Fatalf("ResolveHookSet: %v", err)
	}
	pre := set["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "^bash$" || pre[0].Hooks[0].Command != "echo pigorc" {
		t.Fatalf("PreToolUse = %+v, want the pigorc hook", pre)
	}
	if len(set["Notification"]) != 1 {
		t.Errorf("Notification = %+v, want the pigorc hook", set["Notification"])
	}
}
