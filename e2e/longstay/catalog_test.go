package main

import (
	"errors"
	"strings"
	"testing"
)

func TestSequenceAndStagesPreserveDependencies(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	currentProfile = profile{
		AvgToolCalls:  12,
		CallsPerStage: 3,
		Enabled: map[string]bool{
			"bash": true, "read": true,
			"schedule_create": true, "schedule_list": true, "schedule_delete": true,
		},
	}

	for n := 0; n < 40; n++ {
		token := "plan-token-" + string(rune(n+1))
		calls := sequence(token)
		if len(calls) != 12 {
			t.Fatalf("sequence(%q) has %d calls, want 12", token, len(calls))
		}
		for i, call := range calls {
			switch {
			case call.Name == "schedule_create":
				if i+2 >= len(calls) || calls[i+1].Name != "schedule_list" || calls[i+2].Name != "schedule_delete" {
					t.Fatalf("schedule group at %d is not atomic: %+v", i, calls)
				}
			case call.Name == "bash" && strings.Contains(call.Arguments, `"run_in_background":true`):
				if i+2 >= len(calls) || calls[i+1].Name != "kill_bash" || calls[i+2].Name != "bash_output" {
					t.Fatalf("background group at %d is not atomic: %+v", i, calls)
				}
			}
		}

		stages := planStages(token)
		if planRounds(token) != len(stages) {
			t.Fatalf("planRounds = %d, stages = %d", planRounds(token), len(stages))
		}
		var flattened []plannedCall
		for stageIndex, stage := range stages {
			if len(stage) == 0 || len(stage) > 3 {
				t.Fatalf("stage %d has %d calls", stageIndex, len(stage))
			}
			serial := false
			for _, call := range stage {
				if call.StageBreak {
					serial = true
				}
			}
			if serial && len(stage) != 1 {
				t.Fatalf("serial stage %d contains %d calls", stageIndex, len(stage))
			}
			planned := planFor(token, stageIndex, true)
			if len(planned.Calls) != len(stage) {
				t.Fatalf("planFor stage %d has %d calls, want %d", stageIndex, len(planned.Calls), len(stage))
			}
			for i := range stage {
				if planned.Calls[i].Name != stage[i].Name || planned.Calls[i].Arguments != stage[i].Arguments {
					t.Fatalf("planFor stage %d call %d differs from sequence", stageIndex, i)
				}
			}
			flattened = append(flattened, stage...)
		}
		if len(flattened) != len(calls) {
			t.Fatalf("flattened stages have %d calls, want %d", len(flattened), len(calls))
		}
		final := planFor(token, len(stages), true)
		if len(final.Calls) != 0 || final.Text != expectedFinalText(token, true) {
			t.Fatalf("unexpected final plan: %+v", final)
		}
	}
}

func TestConfigureProfileExpandsDependencyCompanions(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	cfg := Config{Tools: "kill_bash,bash_output,schedule_delete", AvgToolCalls: 6, CallsPerStage: 1}
	if err := configureProfile(cfg); err != nil {
		t.Fatalf("configureProfile: %v", err)
	}
	calls := sequence("companions")
	if len(calls) != 6 {
		t.Fatalf("sequence has %d calls, want 6", len(calls))
	}
	want := map[string]bool{
		"bash": false, "kill_bash": false, "bash_output": false,
		"schedule_create": false, "schedule_list": false, "schedule_delete": false,
	}
	for _, call := range calls {
		if _, ok := want[call.Name]; ok {
			want[call.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("dependency companion %q was not planned", name)
		}
	}
}

func TestBackgroundCommandForShell(t *testing.T) {
	lookPath := func(available ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, candidate := range available {
				if name == candidate {
					return name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	tests := []struct {
		name      string
		goos      string
		available []string
		want      string
	}{
		{name: "windows bash", goos: "windows", available: []string{"bash", "powershell"}, want: "echo JOB-MARKER && sleep 5"},
		{name: "windows powershell", goos: "windows", available: []string{"powershell"}, want: "Write-Output JOB-MARKER; Start-Sleep -Seconds 5"},
		{name: "windows cmd", goos: "windows", want: "echo JOB-MARKER && ping -n 6 127.0.0.1 >NUL"},
		{name: "linux", goos: "linux", want: "echo JOB-MARKER && sleep 5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := backgroundCommandFor(test.goos, lookPath(test.available...)); got != test.want {
				t.Fatalf("backgroundCommandFor = %q, want %q", got, test.want)
			}
		})
	}
}
