package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// toolSpec describes one planned call. Arguments are deterministic except for
// runtime values such as bash ids, which the mock substitutes from real results.
type toolSpec struct {
	Name        string
	Arguments   func(token string) string
	ExpectAny   []string
	AllowError  bool
	Effect      *workspaceEffect
	StageBreak  bool
	FollowUp    bool
	CheckResult func(string) error
}

// workspaceEffect is a file-system assertion checked after the tool result.
type workspaceEffect struct {
	Path     string
	Contains string
}

// catalog is the offline tool set exercised by normal scenarios. It deliberately
// excludes external-provider and configuration-changing tools: websearch needs a
// real search service and pigo_config changes user configuration. task plans run
// nested agents against the same offline mock. question is included as a negative
// case for a host with no interactive asker, not as a claim that a human answered.
var catalog = map[string]toolSpec{
	"read": {
		Name: "read",
		Arguments: func(string) string {
			return `{"path":"notes.txt"}`
		},
		ExpectAny: []string{"NOTES-MARKER-LINE"},
	},
	"read_window": {
		Name: "read",
		Arguments: func(string) string {
			return `{"path":"log.txt","offset":2,"limit":2}`
		},
		ExpectAny: []string{"LOG-MARKER-LINE"},
	},
	"grep": {
		Name: "grep",
		Arguments: func(string) string {
			return `{"pattern":"DATA-MARKER"}`
		},
		ExpectAny: []string{"data.txt"},
	},
	"find": {
		Name: "find",
		Arguments: func(string) string {
			return `{"glob":"*.txt"}`
		},
		ExpectAny: []string{"notes.txt"},
	},
	"bash": {
		Name: "bash",
		Arguments: func(string) string {
			return `{"command":"echo BASH-MARKER","timeout_ms":5000}`
		},
		ExpectAny: []string{"BASH-MARKER"},
	},
	"write": {
		Name: "write",
		Arguments: func(token string) string {
			return jsonArgs(map[string]any{"path": "created-" + artifactName(token) + ".txt", "content": "WRITE-MARKER\n"})
		},
		ExpectAny: []string{"Created", "Overwrote"},
		Effect:    &workspaceEffect{Path: "created.txt", Contains: "WRITE-MARKER"},
	},
	"edit": {
		Name: "edit",
		Arguments: func(token string) string {
			return jsonArgs(map[string]any{"path": "editable-" + artifactName(token) + ".txt", "old_string": "WRITE-MARKER", "new_string": "EDIT-MARKER"})
		},
		ExpectAny: []string{"Edited", "replacement"},
		Effect:    &workspaceEffect{Path: "editable.txt", Contains: "EDIT-MARKER"},
	},
	"view_image": {
		Name: "view_image",
		Arguments: func(string) string {
			return `{"path":"pixel.png"}`
		},
		ExpectAny: []string{"viewed"},
	},
	"webfetch": {
		Name: "webfetch",
		Arguments: func(string) string {
			return jsonArgs(map[string]any{"url": currentProfile.FixtureURL})
		},
		ExpectAny: []string{"WEBFETCH-MARKER"},
	},
	"todo": {
		Name: "todo",
		Arguments: func(string) string {
			return `{"todos":[{"content":"TODO-MARKER","status":"in_progress"}]}`
		},
		ExpectAny: []string{"TODO-MARKER"},
	},
	"memory_search": {
		Name: "memory_search",
		Arguments: func(string) string {
			return `{"query":"MEMORY-MARKER","limit":3}`
		},
		ExpectAny: []string{"MEMORY-MARKER"},
	},
	"question": {
		Name: "question",
		Arguments: func(string) string {
			return `{"questions":[{"question":"LONGSTAY-QUESTION","options":["yes","no"]}]}`
		},
		ExpectAny:  []string{"no one can answer here"},
		AllowError: true,
	},
	"bash_jobs": {
		Name: "bash_jobs",
		Arguments: func(string) string {
			return `{}`
		},
		ExpectAny: []string{"bash_", "no background"},
	},
	"schedule_create": {
		Name: "schedule_create",
		Arguments: func(string) string {
			return `{"prompt":"SCHEDULE-MARKER","every_seconds":300}`
		},
		ExpectAny: []string{"scheduled sched-"},
	},
	"schedule_list": {
		Name: "schedule_list",
		Arguments: func(string) string {
			return `{}`
		},
		ExpectAny: []string{"SCHEDULE-MARKER"},
	},
	"schedule_delete": {
		Name: "schedule_delete",
		Arguments: func(string) string {
			return `{"id":"{{LAST_SCHEDULE_ID}}"}`
		},
		ExpectAny: []string{"deleted sched-"},
	},
	"kill_bash": {
		Name: "kill_bash",
		Arguments: func(string) string {
			return `{"bash_id":"{{LAST_BASH_ID}}"}`
		},
		ExpectAny:  []string{"killed background command", "was not running"},
		StageBreak: true,
	},
	"bash_output": {
		Name: "bash_output",
		Arguments: func(string) string {
			return `{"bash_id":"{{LAST_BASH_ID}}","wait_for_exit":true,"wait_ms":5000}`
		},
		ExpectAny:  []string{"JOB-MARKER", "exited code"},
		StageBreak: true,
	},
	"task_sync":         taskSyncSpec("sync", false),
	"task_error":        taskSyncSpec("error", true),
	"task_async":        taskAsyncSpec("single", false),
	"task_output":       taskOutputSpec("single"),
	"task_output_list":  taskOutputListSpec(),
	"task_batch":        taskAsyncSpec("batch-a", false),
	"task_output_batch": taskBatchOutputSpec(),
	"task_wait_all":     taskWaitAllOutputSpec(),
	"task_auto":         taskAsyncSpec("auto", true),
}

// backgroundSpec returns the background bash plan. Its id is filled by the mock
// from the real startBackground result, not guessed from a counter.
func backgroundSpec() []toolSpec {
	return []toolSpec{
		{
			Name: "bash",
			Arguments: func(string) string {
				return jsonArgs(map[string]any{"command": backgroundCommand(), "run_in_background": true})
			},
			ExpectAny:  []string{"bash_id"},
			StageBreak: true,
		},
		catalog["kill_bash"],
		catalog["bash_output"],
	}
}

// scheduleSpec returns a complete create/list/delete cycle. The delete id is
// resolved from this turn's real create result because counters differ by session.
func scheduleSpec() []toolSpec {
	create := catalog["schedule_create"]
	create.StageBreak = true
	list := catalog["schedule_list"]
	list.StageBreak = true
	remove := catalog["schedule_delete"]
	remove.StageBreak = true
	return []toolSpec{catalog["schedule_create"], list, remove}
}

// toolGroup is one dependency-safe unit in a turn's sequence.
type toolGroup struct {
	specs []toolSpec
	once  bool
}

// profile is the configured intensity and enabled tool set for one run.
type profile struct {
	AvgToolCalls   float64
	CallsPerStage  int
	ThinkingBlocks int
	Enabled        map[string]bool
	FixtureURL     string
}

// currentProfile is set once by the runner before planning starts. There is one
// harness per process, so the mock and all clients share this read-only profile.
var currentProfile profile

// enabledTools is the default complete offline set.
var enabledTools = []string{
	"read", "read_window", "grep", "find", "bash", "write", "edit", "view_image",
	"webfetch", "todo", "memory_search", "question", "bash_jobs", "task_sync", "task_error",
}

// configureProfile parses -tools and validates the intensity knobs.
func configureProfile(cfg Config) error {
	enabled := make(map[string]bool)
	for _, name := range strings.Split(cfg.Tools, ",") {
		name = strings.TrimSpace(name)
		if name == "task" {
			name = "task_sync"
		}
		if name == "" {
			continue
		}
		if _, ok := catalog[name]; !ok {
			return fmt.Errorf("unknown planned tool %q", name)
		}
		enabled[name] = true
	}
	if cfg.RequireTaskCoverage && (!taskPlansEnabled(profile{Enabled: enabled}) || cfg.AvgToolCalls <= 0) {
		return fmt.Errorf("require-task-coverage needs at least one enabled task plan and positive avg-tool-calls")
	}
	currentProfile = profile{
		AvgToolCalls:   cfg.AvgToolCalls,
		CallsPerStage:  cfg.CallsPerStage,
		ThinkingBlocks: cfg.ThinkingBlocks,
		Enabled:        enabled,
	}
	return nil
}

// enabled reports whether a named tool or dependency group is selected.
func (p profile) enabled(name string) bool { return p.Enabled[name] }

// callCount applies stochastic rounding to the average, then caps a turn at a
// sane size so a configuration typo cannot create an unbounded response.
func (p profile) callCount(token string) int {
	if p.AvgToolCalls <= 0 {
		return 0
	}
	base := int(p.AvgToolCalls)
	fraction := p.AvgToolCalls - float64(base)
	if fraction > 0 && float64(hashToken(token, 0)%1000)/1000 < fraction {
		base++
	}
	if base > 32 {
		base = 32
	}
	return base
}

// sequence returns the complete ordered plan for this token. Dependency groups
// are appended atomically; partial groups are never allowed to leave a live job
// or an invalid edit/schedule behind.
func sequence(token string) []plannedCall {
	groups := make([]toolGroup, 0, len(catalog))
	for _, name := range enabledTools {
		if currentProfile.enabled(name) {
			groups = append(groups, toolGroup{specs: []toolSpec{catalog[name]}})
		}
	}
	if currentProfile.enabled("bash") || currentProfile.enabled("kill_bash") || currentProfile.enabled("bash_output") {
		groups = append(groups, toolGroup{specs: backgroundSpec()})
	}
	if currentProfile.enabled("schedule_create") || currentProfile.enabled("schedule_list") || currentProfile.enabled("schedule_delete") {
		groups = append(groups, toolGroup{specs: scheduleSpec()})
	}
	if currentProfile.enabled("task_async") || currentProfile.enabled("task_output") || currentProfile.enabled("task_output_list") {
		groups = append(groups, toolGroup{specs: asyncTaskGroup()})
	}
	if currentProfile.enabled("task_batch") || currentProfile.enabled("task_output_batch") {
		groups = append(groups, toolGroup{specs: batchTaskGroup()})
	}
	if currentProfile.enabled("task_wait_all") {
		groups = append(groups, toolGroup{specs: waitAllTaskGroup()})
	}
	if currentProfile.enabled("task_auto") {
		groups = append(groups, toolGroup{specs: autoTaskGroup(), once: true})
	}
	if len(groups) == 0 {
		return nil
	}

	remaining := currentProfile.callCount(token)
	out := make([]plannedCall, 0, remaining)
	usedOnce := make(map[int]bool)
	for i := 0; remaining > 0; i++ {
		selected := -1
		for j := range groups {
			index := (hashToken(token, -1) + uint64(i+j)) % uint64(len(groups))
			if (groups[index].once && usedOnce[int(index)]) || len(groups[index].specs) > remaining {
				continue
			}
			selected = int(index)
			break
		}
		if selected < 0 {
			break
		}
		group := groups[selected]
		if group.once {
			usedOnce[selected] = true
		}
		for _, spec := range group.specs {
			out = append(out, plannedCall{
				Name: spec.Name, Arguments: spec.Arguments(token),
				ExpectAny: spec.ExpectAny, AllowError: spec.AllowError,
				Effect: effectForToken(spec, token), StageBreak: spec.StageBreak,
				FollowUp: spec.FollowUp, CheckResult: spec.CheckResult,
			})
		}
		remaining -= len(group.specs)
	}
	return out
}

// effectForToken makes file assertions unique per turn while keeping the
// plannedCall's Arguments as the source of truth for the tool itself.
func effectForToken(spec toolSpec, token string) *workspaceEffect {
	if spec.Effect == nil {
		return nil
	}
	effect := *spec.Effect
	if spec.Name == "write" {
		effect.Path = "created-" + artifactName(token) + ".txt"
	} else if spec.Name == "edit" {
		effect.Path = "editable-" + artifactName(token) + ".txt"
	}
	return &effect
}

// artifactName is a stable short token used in paths, not in model arguments.
func artifactName(token string) string {
	return fmt.Sprintf("%08x", hashToken(token, -1))
}

// jsonArgs is used for dynamic arguments whose shape must stay valid JSON.
func jsonArgs(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// backgroundCommand uses the same shell preference as BashTool, so its syntax
// works with Git Bash, PowerShell, cmd, and POSIX bash.
func backgroundCommand() string {
	return backgroundCommandFor(runtime.GOOS, exec.LookPath)
}

func backgroundCommandFor(goos string, lookPath func(string) (string, error)) string {
	if goos == "windows" {
		if _, err := lookPath("bash"); err == nil {
			return "echo JOB-MARKER && sleep 5"
		}
		if _, err := lookPath("powershell"); err == nil {
			return "Write-Output JOB-MARKER; Start-Sleep -Seconds 5"
		}
		return "echo JOB-MARKER && ping -n 6 127.0.0.1 >NUL"
	}
	return "echo JOB-MARKER && sleep 5"
}
