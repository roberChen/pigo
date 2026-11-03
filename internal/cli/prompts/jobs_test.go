// This file implements tests for /jobs: the shared formatter both front-ends
// render, and its registration against live background registries.
package prompts

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agenttool"
	pi_runtime "github.com/smallnest/pigo/internal/runtime"
)

// With no registries (or no jobs) /jobs explains how to start one instead of
// printing an empty table.
func TestFormatJobsEmpty(t *testing.T) {
	out := FormatJobs(false, nil, nil)
	if !strings.Contains(out, "no background jobs have been started") {
		t.Errorf("output = %q, want an empty notice", out)
	}
	if strings.Contains(out, "running") {
		t.Errorf("empty listing should not claim anything is running: %q", out)
	}

	filtered := FormatJobs(true, nil, nil)
	if !strings.Contains(filtered, "no background jobs are still running") {
		t.Errorf("running filter output = %q", filtered)
	}
}

// A running shell job appears with its id, command and follow-up calls.
func TestFormatJobsListsBashJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	store := agenttool.NewBashJobStore()
	bash := &agenttool.BashTool{Jobs: store}
	raw, _ := json.Marshal(map[string]any{"command": "sleep 30", "run_in_background": true})
	res, err := bash.Execute(context.Background(), "call-1", raw, nil)
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)
	t.Cleanup(store.KillAll)

	out := FormatJobs(false, store, nil)
	for _, want := range []string{id, "running", "sleep 30", "bash_output", "kill_bash"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// A running filter keeps it; the running count in the header is 1.
	if got := FormatJobs(true, store, nil); !strings.Contains(got, id) || !strings.Contains(got, "1 running") {
		t.Errorf("running view = %q", got)
	}
}

// Background sub-agents are listed separately from shell jobs.
func TestFormatJobsListsSubAgents(t *testing.T) {
	jobs := pi_runtime.NewSubAgentJobs()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	jobs.Start("task", "investigate the loop", "prompt", context.Background(),
		func(ctx context.Context, jobID string) (string, string, error) {
			<-release
			return "done", "sess-1", nil
		})

	out := FormatJobs(false, nil, jobs)
	for _, want := range []string{"job-1", "task", "running", "investigate the loop", "task_output"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "shell commands") {
		t.Errorf("no bash store should mean no shell section:\n%s", out)
	}
}

// /jobs is registered even when no background tool is wired, so /help is stable,
// and its action reflects the live registries it was registered with.
func TestRegisterJobsCommand(t *testing.T) {
	reg := pi_runtime.NewSlashRegistry()
	RegisterJobsCommand(reg, nil, nil)
	cmd, ok := reg.Lookup("jobs")
	if !ok {
		t.Fatal("/jobs was not registered")
	}
	if !strings.Contains(cmd.Action(""), "no background jobs") {
		t.Errorf("action = %q", cmd.Action(""))
	}
	if !strings.Contains(cmd.Action("running"), "still running") {
		t.Errorf("action with 'running' = %q", cmd.Action("running"))
	}
}

// The command's description mentions both families so it is discoverable from
// /help without invoking it.
func TestRegisterJobsCommandDescription(t *testing.T) {
	reg := pi_runtime.NewSlashRegistry()
	RegisterJobsCommand(reg, nil, nil)
	cmd, _ := reg.Lookup("jobs")
	if !strings.Contains(cmd.Description, "sub-agent") || !strings.Contains(cmd.Description, "shell") {
		t.Errorf("description = %q, want both families named", cmd.Description)
	}
	if cmd.ArgumentHint != "[running]" {
		t.Errorf("argument hint = %q", cmd.ArgumentHint)
	}
}
