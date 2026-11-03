// Tests for the bash_jobs tool: listing every background job with its status,
// runtime and unread output, and filtering to the running ones.
package agenttool

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newBashJobsTool(t *testing.T) (*BashJobStore, *BashTool, *BashOutputTool, *BashJobsTool) {
	t.Helper()
	jobs := NewBashJobStore()
	return jobs, &BashTool{Jobs: jobs}, &BashOutputTool{Jobs: jobs}, &BashJobsTool{Jobs: jobs}
}

// An empty registry is reported as such rather than as an error.
func TestBashJobsToolEmpty(t *testing.T) {
	_, _, _, listing := newBashJobsTool(t)
	res, err := runBGTool(t, listing, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resultText(res), "no background bash jobs") {
		t.Errorf("output = %q, want an empty-registry notice", resultText(res))
	}
	if details := res.Details.(map[string]any); details["count"] != 0 {
		t.Errorf("count = %v, want 0", details["count"])
	}
}

// A running and an exited job are both listed, with the id, status and command.
func TestBashJobsToolListsStatuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	_, bash, out, listing := newBashJobsTool(t)

	done, err := runBGTool(t, bash, map[string]any{"command": "echo quick", "run_in_background": true})
	if err != nil {
		t.Fatalf("start quick job: %v", err)
	}
	doneID := done.Details.(map[string]any)["bash_id"].(string)
	waitForStatus(t, out, doneID, BashExited, 3*time.Second)

	long, err := runBGTool(t, bash, map[string]any{"command": "sleep 30", "run_in_background": true})
	if err != nil {
		t.Fatalf("start long job: %v", err)
	}
	longID := long.Details.(map[string]any)["bash_id"].(string)
	t.Cleanup(func() { runBGTool(t, &BashKillTool{Jobs: listing.Jobs}, map[string]any{"bash_id": longID}) })

	res, err := runBGTool(t, listing, map[string]any{})
	if err != nil {
		t.Fatalf("bash_jobs: %v", err)
	}
	text := resultText(res)
	for _, want := range []string{doneID, longID, "running", "exited code 0", "echo quick", "sleep 30"} {
		if !strings.Contains(text, want) {
			t.Errorf("listing missing %q:\n%s", want, text)
		}
	}
	details := res.Details.(map[string]any)
	if details["count"] != 2 || details["running"] != 1 {
		t.Errorf("details = %+v, want count 2 running 1", details)
	}
}

// running_only hides jobs that already exited.
func TestBashJobsToolRunningOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	_, bash, out, listing := newBashJobsTool(t)

	done, err := runBGTool(t, bash, map[string]any{"command": "echo quick", "run_in_background": true})
	if err != nil {
		t.Fatalf("start quick job: %v", err)
	}
	doneID := done.Details.(map[string]any)["bash_id"].(string)
	waitForStatus(t, out, doneID, BashExited, 3*time.Second)

	long, err := runBGTool(t, bash, map[string]any{"command": "sleep 30", "run_in_background": true})
	if err != nil {
		t.Fatalf("start long job: %v", err)
	}
	longID := long.Details.(map[string]any)["bash_id"].(string)
	t.Cleanup(func() { runBGTool(t, &BashKillTool{Jobs: listing.Jobs}, map[string]any{"bash_id": longID}) })

	res, err := runBGTool(t, listing, map[string]any{"running_only": true})
	if err != nil {
		t.Fatalf("bash_jobs: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, longID) || strings.Contains(text, doneID) {
		t.Errorf("running_only listing = %q, want only %s", text, longID)
	}
}

// The listing points at the calls that consume it, so it is actionable on its own.
func TestBashJobsToolPointsAtFollowUps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	_, bash, _, listing := newBashJobsTool(t)
	res, err := runBGTool(t, bash, map[string]any{"command": "sleep 30", "run_in_background": true})
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)
	t.Cleanup(func() { runBGTool(t, &BashKillTool{Jobs: listing.Jobs}, map[string]any{"bash_id": id}) })

	out, err := runBGTool(t, listing, map[string]any{})
	if err != nil {
		t.Fatalf("bash_jobs: %v", err)
	}
	for _, want := range []string{"bash_output", "kill_bash", "wait_for_exit"} {
		if !strings.Contains(resultText(out), want) {
			t.Errorf("listing missing follow-up hint %q:\n%s", want, resultText(out))
		}
	}
}

// A canceled context does not affect listing: it is a pure read.
func TestBashJobsToolIgnoresCanceledContext(t *testing.T) {
	_, _, _, listing := newBashJobsTool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := listing.Execute(ctx, "call-1", nil, nil); err != nil {
		t.Fatalf("bash_jobs with canceled ctx: %v", err)
	}
}
