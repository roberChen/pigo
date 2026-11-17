package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskBatchResolvesNamedJobIDs(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	if err := configureProfile(Config{Tools: "task_output_batch", AvgToolCalls: 3, CallsPerStage: 1}); err != nil {
		t.Fatalf("configureProfile: %v", err)
	}
	calls := sequence("parent-token")
	if len(calls) != 3 || calls[0].Name != "task" || calls[1].Name != "task" || calls[2].Name != "task_output" {
		t.Fatalf("unexpected batch plan: %+v", calls)
	}

	call1 := wireToolCall{ID: "call-1", Type: "function"}
	call1.Function.Name = "task"
	call1.Function.Arguments = calls[0].Arguments
	call2 := wireToolCall{ID: "call-2", Type: "function"}
	call2.Function.Name = "task"
	call2.Function.Arguments = calls[1].Arguments
	result1 := `Started sub-agent "task" in the background (mode: async) as job-7.`
	result2 := `Started sub-agent "task" in the background (mode: async) as job-8.`
	req := chatRequest{Messages: []wireMessage{
		{Role: "user", Content: mustJSON("prompt [[longstay:parent-token]]")},
		{Role: "assistant", Content: mustJSON("working"), ToolCalls: []wireToolCall{call1}},
		{Role: "tool", ToolCallID: "call-1", Content: mustJSON(result1)},
		{Role: "user", Content: mustJSON("<system-reminder>state</system-reminder>")},
		{Role: "assistant", Content: mustJSON("working"), ToolCalls: []wireToolCall{call2}},
		{Role: "tool", ToolCallID: "call-2", Content: mustJSON(result2)},
	}}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	m := &mockLLM{}
	resolved, err := m.resolveDynamicArguments(replyPlan{Text: "batch", Stage: 2, Calls: []plannedCall{calls[2]}}, body)
	if err != nil {
		t.Fatalf("resolveDynamicArguments: %v", err)
	}
	var args struct {
		JobIDs []string `json:"job_ids"`
		Wait   bool     `json:"wait"`
	}
	if err := json.Unmarshal([]byte(resolved.Calls[0].Arguments), &args); err != nil {
		t.Fatalf("decode resolved arguments: %v", err)
	}
	if len(args.JobIDs) != 2 || args.JobIDs[0] != "job-7" || args.JobIDs[1] != "job-8" || !args.Wait {
		t.Fatalf("resolved batch arguments = %+v", args)
	}
}

func TestTaskChildPlanAndToolBoundary(t *testing.T) {
	var taskArgs struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(taskArguments("parent-token", "sync", "sync")), &taskArgs); err != nil {
		t.Fatalf("decode task arguments: %v", err)
	}
	req := chatRequest{Messages: []wireMessage{
		{Role: "system", Content: mustJSON("You are a focused sub-agent working on one delegated task.")},
		{Role: "user", Content: mustJSON(taskArgs.Prompt)},
	}}
	if !isTaskChildRequest(req) {
		t.Fatal("child request was not detected")
	}
	token, stage := taskTokenAndStage(req)
	if token != "parent-token/sync" || stage != 0 {
		t.Fatalf("taskTokenAndStage = %q, %d", token, stage)
	}
	plan := taskPlanFor(token, stage)
	if len(plan.Calls) != 1 || plan.Calls[0].Name != "read" {
		t.Fatalf("child stage 0 plan = %+v", plan)
	}
	finalRequest := req
	finalRequest.Messages = append(finalRequest.Messages,
		wireMessage{Role: "assistant", Content: mustJSON(plan.Text), ToolCalls: nil},
		wireMessage{Role: "tool", Content: mustJSON(notesMarker)},
	)
	_, stage = taskTokenAndStage(finalRequest)
	if stage != 1 || !strings.Contains(taskTurnToolText(finalRequest), notesMarker) {
		t.Fatalf("child final stage/read evidence missing: stage=%d", stage)
	}
	if got := taskPlanFor(token, stage).Text; !strings.Contains(got, taskReportMarker) {
		t.Fatalf("child final report = %q", got)
	}

	validTools := []byte(`{"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]}`)
	if err := validateTaskChildTools(validTools); err != nil {
		t.Fatalf("valid child tools rejected: %v", err)
	}
	nestedTools := []byte(`{"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}},{"type":"function","function":{"name":"task","parameters":{"type":"object"}}}]}`)
	if err := validateTaskChildTools(nestedTools); err == nil {
		t.Fatal("nested task tool was accepted in a child")
	}
}

func TestAutoTaskGroupRunsOnceAndAddsFollowUpStage(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	if err := configureProfile(Config{Tools: "task_auto", AvgToolCalls: 8, CallsPerStage: 1, ThinkingBlocks: 1}); err != nil {
		t.Fatalf("configureProfile: %v", err)
	}
	for n := range 20 {
		token := "auto-token-" + string(rune(n+1))
		calls := sequence(token)
		if len(calls) != 1 || !calls[0].FollowUp {
			t.Fatalf("auto plan must contain one follow-up task: %+v", calls)
		}
		if got := planRounds(token); got != 2 {
			t.Fatalf("planRounds = %d, want 2", got)
		}
		if got := len(planFor(token, 0, true).Calls); got != 1 {
			t.Fatalf("stage 0 calls = %d, want 1", got)
		}
		if got := len(planFor(token, 1, true).Calls); got != 0 {
			t.Fatalf("interim stage calls = %d, want 0", got)
		}
		if got := planFor(token, 2, true).Text; got != expectedFinalText(token, true) {
			t.Fatalf("final text = %q", got)
		}
	}
}

func TestWaitAllSuppressesEarlierAutomaticReport(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	if err := configureProfile(Config{Tools: "task_auto,task_wait_all", AvgToolCalls: 4, CallsPerStage: 1}); err != nil {
		t.Fatalf("configureProfile: %v", err)
	}
	for n := range 40 {
		token := "mixed-task-token-" + string(rune(n+1))
		calls := sequence(token)
		autoIndex, waitAllIndex := -1, -1
		for i, call := range calls {
			if call.FollowUp {
				autoIndex = i
			}
			if strings.Contains(call.Arguments, `"wait_all":true`) {
				waitAllIndex = i
			}
		}
		if autoIndex < 0 || waitAllIndex < 0 {
			t.Fatalf("combined plan lacks auto or wait_all: %+v", calls)
		}
		want := autoIndex > waitAllIndex
		if got := taskAutoFollowUp(token); got != want {
			t.Fatalf("taskAutoFollowUp = %v, want %v (auto=%d waitAll=%d)", got, want, autoIndex, waitAllIndex)
		}
	}
}

func TestSuccessfulTaskSessionMatchesIgnoreFailedJobs(t *testing.T) {
	result := "batch of 2 sub-agent jobs: 1 done, 1 failed:\n\n" +
		"job-1 (agent task) failed after 1s — longstay auto\n" +
		"error: sub-agent failed\n\n[sub-agent session: failed-child]\n\n" +
		"job-2 (agent task) done after 1s — longstay single\n" +
		"report:\n" + taskReportMarker + "\n\n[sub-agent session: successful-child]"
	matches := successfulTaskSessionMatches(result)
	if len(matches) != 1 || matches[0][1] != "successful-child" {
		t.Fatalf("successful sessions = %+v", matches)
	}
}
