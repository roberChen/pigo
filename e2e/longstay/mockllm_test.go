package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveDynamicArgumentsUsesCurrentTurn(t *testing.T) {
	m := &mockLLM{}
	body := []byte(`{"messages":[
		{"role":"user","content":"old turn [[longstay:old-token]]"},
		{"role":"tool","content":"scheduled sched-1 (session-local)\nbash_id: bash_1"},
		{"role":"user","content":"current turn [[longstay:current-token]]"},
		{"role":"tool","content":"scheduled sched-7 (session-local)"},
		{"role":"tool","content":[{"type":"text","text":"bash_id: bash_9"}]},
		{"role":"user","content":"<system-reminder>background</system-reminder>"}
	]}`)
	plan := replyPlan{Text: "current", Calls: []plannedCall{
		{Name: "schedule_delete", Arguments: `{"id":"{{LAST_SCHEDULE_ID}}"}`},
		{Name: "kill_bash", Arguments: `{"bash_id":"{{LAST_BASH_ID}}"}`},
	}}
	resolved, err := m.resolveDynamicArguments(plan, body)
	if err != nil {
		t.Fatalf("resolveDynamicArguments: %v", err)
	}
	if got, want := resolved.Calls[0].Arguments, `{"id":"sched-7"}`; got != want {
		t.Errorf("schedule arguments = %s, want %s", got, want)
	}
	if got, want := resolved.Calls[1].Arguments, `{"bash_id":"bash_9"}`; got != want {
		t.Errorf("bash arguments = %s, want %s", got, want)
	}
}

func TestResolveDynamicArgumentsRejectsStaleID(t *testing.T) {
	m := &mockLLM{}
	body := []byte(`{"messages":[
		{"role":"user","content":"old turn [[longstay:old-token]]"},
		{"role":"tool","content":"scheduled sched-1 (session-local)"},
		{"role":"user","content":"current turn [[longstay:current-token]]"},
		{"role":"tool","content":"unrelated result"}
	]}`)
	plan := replyPlan{Text: "current", Stage: 2, Calls: []plannedCall{
		{Name: "schedule_delete", Arguments: `{"id":"{{LAST_SCHEDULE_ID}}"}`},
	}}
	_, err := m.resolveDynamicArguments(plan, body)
	if err == nil || !strings.Contains(err.Error(), "current-turn schedule_create") {
		t.Fatalf("resolveDynamicArguments error = %v", err)
	}
}

func TestRecordDynamicIDUsesLatestResult(t *testing.T) {
	ids := make(map[string]string)
	recordDynamicID(ids, "schedule_create", "scheduled sched-2\nscheduled sched-11")
	recordDynamicID(ids, "bash", "bash_id: bash_3\nbash_id: bash_8")
	arguments, err := resolvePlannedArguments(`{"id":"{{LAST_SCHEDULE_ID}}","bash_id":"{{LAST_BASH_ID}}"}`, ids)
	if err != nil {
		t.Fatalf("resolvePlannedArguments: %v", err)
	}
	if want := `{"id":"sched-11","bash_id":"bash_8"}`; arguments != want {
		t.Fatalf("arguments = %s, want %s", arguments, want)
	}
}

func TestPlanTokenAndStageSkipsSystemReminders(t *testing.T) {
	var req chatRequest
	body := []byte(`{"messages":[
		{"role":"system","content":"pigo"},
		{"role":"user","content":"prompt [[longstay:client-1/turn-2/attempt-3]]"},
		{"role":"assistant","content":"working stage 0"},
		{"role":"tool","content":"result"},
		{"role":"user","content":"<system-reminder>background state</system-reminder>"},
		{"role":"assistant","content":"working stage 1"},
		{"role":"tool","content":"result"},
		{"role":"user","content":[{"type":"text","text":"<system-reminder>new state</system-reminder>"}]}
	]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	token, stage := planTokenAndStage(req)
	if token != "client-1/turn-2/attempt-3" || stage != 2 {
		t.Fatalf("planTokenAndStage = %q, %d", token, stage)
	}
	if isSummarizationRequest(req) {
		t.Fatal("ordinary pigo request detected as summarization")
	}
}

func TestSummarizationRequestDetectedDespiteTranscriptMarker(t *testing.T) {
	var req chatRequest
	body := []byte(`{"messages":[
		{"role":"system","content":"You are a context summarization assistant. Summarize only."},
		{"role":"user","content":"<conversation>[User]: prompt [[longstay:client-1/turn-2/attempt-3]]</conversation>"}
	]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if !isSummarizationRequest(req) {
		t.Fatal("summarization request was not detected")
	}
	token, _ := planTokenAndStage(req)
	if token == "" {
		t.Fatal("summary metadata token should remain available for diagnostics")
	}
}
