package main

import "testing"

func TestRequireTaskCoverageValidation(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	if err := configureProfile(Config{RequireTaskCoverage: true, Tools: "read", AvgToolCalls: 1, CallsPerStage: 1}); err == nil {
		t.Fatal("non-task profile passed require-task-coverage")
	}
	if err := configureProfile(Config{RequireTaskCoverage: true, Tools: "task_error", AvgToolCalls: 1, CallsPerStage: 1}); err != nil {
		t.Fatalf("task profile rejected: %v", err)
	}
}

func TestHealthyRequiresSelectedTaskOutputs(t *testing.T) {
	old := currentProfile
	t.Cleanup(func() { currentProfile = old })
	r := &runner{cfg: Config{RequireTaskCoverage: true}, mock: &mockLLM{}}
	r.mock.requests.Store(1)
	r.mock.taskChildRequests.Store(1)
	r.metrics.prompts.Store(1)
	r.metrics.completed.Store(1)
	r.metrics.reconnects.Store(1)
	r.metrics.peakClients.Store(2)
	r.metrics.subAgentProgress.Store(1)
	r.metrics.subAgentDone.Store(1)
	r.mock.taskChildCompleted.Store(1)
	r.mock.taskChildReadChecks.Store(1)
	r.metrics.taskSessionsVerified.Store(1)
	r.mock.recordTaskChildKey("parent/single", false)

	currentProfile = profile{Enabled: map[string]bool{"task_async": true}}
	if r.healthy() {
		t.Fatal("async task coverage passed without list and single output")
	}
	r.metrics.taskOutputList.Store(1)
	r.metrics.taskOutputSingle.Store(1)
	if !r.healthy() {
		t.Fatal("complete async task coverage was rejected")
	}

	currentProfile = profile{Enabled: map[string]bool{"task_auto": true}}
	r.mock.recordTaskChildKey("parent/auto", false)
	r.mock.taskFollowUps.Store(1)
	r.metrics.taskFollowUpsVerified.Store(1)
	if !r.healthy() {
		t.Fatal("automatic follow-up coverage was rejected")
	}
}
