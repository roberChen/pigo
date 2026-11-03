// Tests for background bash execution: launching a detached job, draining its
// output incrementally via bash_output, and terminating a long-running job with
// kill_bash. These exercise the shared BashJobStore wiring end to end.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

func runBGTool(t *testing.T, tool agentcore.AgentTool, args map[string]any) (agentcore.AgentToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), "call-1", raw, nil)
}

// A background command returns immediately with a bash_id, then bash_output
// drains its output and reports it exited.
func TestBashBackgroundRunAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}

	res, gerr := runBGTool(t, bash, map[string]any{"command": "echo bg-hello", "run_in_background": true})
	if gerr != nil {
		t.Fatalf("unexpected error: %v", gerr)
	}
	details, ok := res.Details.(map[string]any)
	if !ok || details["background"] != true {
		t.Fatalf("expected background details, got %+v", res.Details)
	}
	id, _ := details["bash_id"].(string)
	if id == "" {
		t.Fatalf("no bash_id returned")
	}

	// Poll bash_output until the job exits and produced its line.
	var text string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := runBGTool(t, out, map[string]any{"bash_id": id})
		if err != nil {
			t.Fatalf("bash_output: %v", err)
		}
		text += resultText(r)
		d, _ := r.Details.(map[string]any)
		if d["status"] == string(BashExited) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(text, "bg-hello") {
		t.Errorf("output = %q, want to contain bg-hello", text)
	}
	if !strings.Contains(text, "exited") {
		t.Errorf("status never reported exited: %q", text)
	}
}

// kill_bash terminates a long-running background job and it stops running.
func TestBashBackgroundKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	kill := &BashKillTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}

	res, gerr := runBGTool(t, bash, map[string]any{"command": "sleep 30", "run_in_background": true})
	if gerr != nil {
		t.Fatalf("unexpected error: %v", gerr)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)

	kr, err := runBGTool(t, kill, map[string]any{"bash_id": id})
	if err != nil {
		t.Fatalf("kill_bash: %v", err)
	}
	if killed, _ := kr.Details.(map[string]any)["killed"].(bool); !killed {
		t.Errorf("expected killed=true, got %+v", kr.Details)
	}

	// After the kill, the job should report exited within a short window.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := runBGTool(t, out, map[string]any{"bash_id": id})
		if r.Details.(map[string]any)["status"] == string(BashExited) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("job still running after kill")
}

// bash_output and kill_bash report a clear error for an unknown id.
func TestBashControlUnknownID(t *testing.T) {
	jobs := NewBashJobStore()
	out := &BashOutputTool{Jobs: jobs}
	kill := &BashKillTool{Jobs: jobs}

	r, _ := runBGTool(t, out, map[string]any{"bash_id": "bash_99"})
	if !strings.Contains(resultText(r), "no background command") {
		t.Errorf("bash_output on unknown id should error, got %q", resultText(r))
	}
	r, _ = runBGTool(t, kill, map[string]any{"bash_id": "bash_99"})
	if !strings.Contains(resultText(r), "no background command") {
		t.Errorf("kill_bash on unknown id should error, got %q", resultText(r))
	}
}

// run_in_background without a store wired reports it is unavailable.
func TestBashBackgroundNoStore(t *testing.T) {
	bash := &BashTool{}
	r, err := runBGTool(t, bash, map[string]any{"command": "echo x", "run_in_background": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resultText(r), "not available") {
		t.Errorf("expected unavailable message when no store is wired, got %q", resultText(r))
	}
}

// A foreground command that exceeds its timeout is moved to the background
// instead of being killed: it returns promptly with a bash_id, stays running,
// and kill_bash can still stop it.
func TestBashToolTimeoutMovesToBackground(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	start := time.Now()
	res, gerr := runBGTool(t, bash, map[string]any{"command": "sleep 30", "timeout_ms": 100})
	if gerr != nil {
		t.Fatalf("unexpected error: %v", gerr)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout detach took too long: %s", elapsed)
	}
	details := res.Details.(map[string]any)
	if details["background"] != true || details["timedOut"] != true {
		t.Fatalf("expected timed-out background details, got %+v", details)
	}
	id, _ := details["bash_id"].(string)
	if id == "" {
		t.Fatal("no bash_id returned")
	}
	// The notice is the model's only hint that the job survived: it must say the
	// timeout happened, and show how to read/wait/kill by id.
	text := resultText(res)
	for _, want := range []string{"TIMEOUT", "moved to the background", "bash_id: " + id, "bash_output", "wait_for_exit", "kill_bash"} {
		if !strings.Contains(text, want) {
			t.Errorf("timeout notice missing %q, got:\n%s", want, text)
		}
	}

	out := &BashOutputTool{Jobs: jobs}
	r, err := runBGTool(t, out, map[string]any{"bash_id": id})
	if err != nil {
		t.Fatalf("bash_output: %v", err)
	}
	if got := r.Details.(map[string]any)["status"]; got != string(BashRunning) {
		t.Errorf("status = %v, want running (job was killed instead of detached?)", got)
	}

	kill := &BashKillTool{Jobs: jobs}
	kr, err := runBGTool(t, kill, map[string]any{"bash_id": id})
	if err != nil {
		t.Fatalf("kill_bash: %v", err)
	}
	if killed, _ := kr.Details.(map[string]any)["killed"].(bool); !killed {
		t.Errorf("expected killed=true for detached job, got %+v", kr.Details)
	}
	waitForStatus(t, out, id, BashExited, 2*time.Second)
}

// bash_output with wait_ms blocks until the command produces output instead of
// returning an empty "running" snapshot.
func TestBashOutputWaitsForOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}

	res, err := runBGTool(t, bash, map[string]any{"command": "sleep 0.3; echo waited-hello", "run_in_background": true})
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)

	start := time.Now()
	r, err := runBGTool(t, out, map[string]any{"bash_id": id, "wait_ms": 3000})
	if err != nil {
		t.Fatalf("bash_output: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned before output appeared (%s); wait_ms was ignored", elapsed)
	}
	if !strings.Contains(resultText(r), "waited-hello") {
		t.Errorf("output = %q, want waited-hello", resultText(r))
	}
	waitForStatus(t, out, id, BashExited, 2*time.Second)
}

// wait_for_exit blocks through intermediate output until the job finishes.
func TestBashOutputWaitsForExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}

	res, err := runBGTool(t, bash, map[string]any{"command": "echo first; sleep 0.3; echo last", "run_in_background": true})
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)

	r, err := runBGTool(t, out, map[string]any{"bash_id": id, "wait_for_exit": true, "wait_ms": 5000})
	if err != nil {
		t.Fatalf("bash_output: %v", err)
	}
	if got := r.Details.(map[string]any)["status"]; got != string(BashExited) {
		t.Errorf("status = %v, want exited", got)
	}
	text := resultText(r)
	if !strings.Contains(text, "first") || !strings.Contains(text, "last") {
		t.Errorf("output = %q, want both lines", text)
	}
}

// A wait that reaches wait_ms returns the current status rather than blocking
// for the whole job lifetime.
func TestBashOutputWaitTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}
	kill := &BashKillTool{Jobs: jobs}

	res, err := runBGTool(t, bash, map[string]any{"command": "sleep 30", "run_in_background": true})
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)
	t.Cleanup(func() { runBGTool(t, kill, map[string]any{"bash_id": id}) })

	start := time.Now()
	r, err := runBGTool(t, out, map[string]any{"bash_id": id, "wait_ms": 150})
	if err != nil {
		t.Fatalf("bash_output: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("wait_ms not honored: %s", elapsed)
	}
	if got := r.Details.(map[string]any)["status"]; got != string(BashRunning) {
		t.Errorf("status = %v, want running", got)
	}
}

// Canceling the turn context interrupts a bash_output wait promptly.
func TestBashOutputWaitCanceled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}
	kill := &BashKillTool{Jobs: jobs}

	res, err := runBGTool(t, bash, map[string]any{"command": "sleep 30", "run_in_background": true})
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)
	t.Cleanup(func() { runBGTool(t, kill, map[string]any{"bash_id": id}) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	raw, _ := json.Marshal(map[string]any{"bash_id": id, "wait_for_exit": true, "wait_ms": 5000})
	start := time.Now()
	r, err := out.Execute(ctx, "call-1", raw, nil)
	if err != nil {
		t.Fatalf("bash_output: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancel did not interrupt wait: %s", elapsed)
	}
	if got := r.Details.(map[string]any)["status"]; got != string(BashRunning) {
		t.Errorf("status = %v, want running", got)
	}
}

// Canceling a foreground command kills its whole process group, not just the
// shell: a helper the shell spawned must not keep running after Ctrl-C.
func TestBashToolCancelKillsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	marker := filepath.Join(t.TempDir(), "survived")
	tool := &BashTool{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	raw, _ := json.Marshal(map[string]any{
		"command": fmt.Sprintf("sleep 1 && touch %q & wait", marker),
	})
	if _, gerr := tool.Execute(ctx, "call-1", raw, nil); gerr == nil {
		t.Fatal("expected cancellation error")
	}
	// The helper would touch the marker at ~1s if it survived the cancellation.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("child process survived cancellation: process group was not killed")
	}
}

// waitForStatus polls bash_output until the job reaches want or timeout passes.
func waitForStatus(t *testing.T, out *BashOutputTool, id string, want BashJobStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := runBGTool(t, out, map[string]any{"bash_id": id})
		if err != nil {
			t.Fatalf("bash_output: %v", err)
		}
		if r.Details.(map[string]any)["status"] == string(want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s within %s", id, want, timeout)
}

// A background job is rooted at context.Background(), not the turn context:
// canceling the turn (Esc/Ctrl-C) must not stop a detached dev server.
func TestBashBackgroundSurvivesTurnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash not available on windows")
	}
	jobs := NewBashJobStore()
	bash := &BashTool{Jobs: jobs}
	out := &BashOutputTool{Jobs: jobs}

	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(map[string]any{"command": "sleep 0.3; echo survived", "run_in_background": true})
	res, err := bash.Execute(ctx, "call-1", raw, nil)
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := res.Details.(map[string]any)["bash_id"].(string)
	cancel()

	r, err := runBGTool(t, out, map[string]any{"bash_id": id, "wait_for_exit": true, "wait_ms": 3000})
	if err != nil {
		t.Fatalf("bash_output: %v", err)
	}
	if got := r.Details.(map[string]any)["status"]; got != string(BashExited) {
		t.Errorf("status = %v, want exited (job was canceled with the turn)", got)
	}
	if !strings.Contains(resultText(r), "survived") {
		t.Errorf("output = %q, want survived", resultText(r))
	}
}
