// This file implements deterministic task sub-agent plans and evidence checks
// for the long-stay host/client reliability harness.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	taskMarkerPrefix    = "[[longstay-task:"
	taskJobMarkerPrefix = "[[longstay-job:"
	taskReportMarker    = "TASK-REPORT-MARKER: NOTES-MARKER-LINE"
)

var (
	taskJobResultPattern = regexp.MustCompile(`as (job-[0-9]+)\.`)
	taskSessionPattern   = regexp.MustCompile(`\[sub-agent session: ([^\]]+)\]`)
)

func taskSyncSpec(key string, allowError bool) toolSpec {
	expect := taskReportMarker
	if allowError {
		expect = `sub-agent "task" failed (error)`
	}
	return toolSpec{
		Name: "task",
		Arguments: func(token string) string {
			return taskArguments(token, key, "sync")
		},
		ExpectAny:  []string{expect},
		AllowError: allowError,
	}
}

func taskAsyncSpec(key string, followUp bool) toolSpec {
	return toolSpec{
		Name: "task",
		Arguments: func(token string) string {
			return taskArguments(token, key, "async")
		},
		ExpectAny:  []string{"Started sub-agent"},
		StageBreak: true,
		FollowUp:   followUp,
	}
}

func taskOutputSpec(key string) toolSpec {
	return toolSpec{
		Name: "task_output",
		Arguments: func(string) string {
			return jsonArgs(map[string]any{"job_id": taskJobPlaceholder(key), "wait": true})
		},
		ExpectAny:  []string{taskReportMarker},
		StageBreak: true,
	}
}

func taskOutputListSpec() toolSpec {
	return toolSpec{
		Name: "task_output",
		Arguments: func(string) string {
			return `{}`
		},
		ExpectAny:  []string{"background sub-agents:"},
		StageBreak: true,
	}
}

func taskBatchOutputSpec() toolSpec {
	return toolSpec{
		Name: "task_output",
		Arguments: func(string) string {
			return jsonArgs(map[string]any{"job_ids": []string{taskJobPlaceholder("batch-a"), taskJobPlaceholder("batch-b")}, "wait": true})
		},
		ExpectAny:  []string{"batch of 2 sub-agent jobs: 2 done"},
		StageBreak: true,
		CheckResult: func(result string) error {
			return requireTaskReports(result, 2)
		},
	}
}

func taskWaitAllOutputSpec() toolSpec {
	return toolSpec{
		Name: "task_output",
		Arguments: func(string) string {
			return `{"wait_all":true}`
		},
		ExpectAny:  []string{"sub-agent jobs:"},
		StageBreak: true,
		CheckResult: func(result string) error {
			return requireTaskReports(result, 2)
		},
	}
}

func asyncTaskGroup() []toolSpec {
	return []toolSpec{taskAsyncSpec("single", false), taskOutputListSpec(), taskOutputSpec("single")}
}

func batchTaskGroup() []toolSpec {
	return []toolSpec{taskAsyncSpec("batch-a", false), taskAsyncSpec("batch-b", false), taskBatchOutputSpec()}
}

func waitAllTaskGroup() []toolSpec {
	return []toolSpec{taskAsyncSpec("all-a", false), taskAsyncSpec("all-b", false), taskWaitAllOutputSpec()}
}

func autoTaskGroup() []toolSpec {
	return []toolSpec{taskAsyncSpec("auto", true)}
}

func taskArguments(token, key, mode string) string {
	prompt := "Use the read tool on notes.txt, then return its marker. " +
		taskMarkerPrefix + token + "/" + key + markerSuffix + " " + taskJobMarkerPrefix + key + markerSuffix
	return jsonArgs(map[string]any{
		"description": "longstay " + key,
		"prompt":      prompt,
		"mode":        mode,
	})
}

func taskJobPlaceholder(key string) string { return "{{TASK_JOB:" + key + "}}" }

func taskKeyFromArguments(arguments string) string {
	var args struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return ""
	}
	return markedToken(args.Prompt, taskJobMarkerPrefix)
}

func recordTaskDynamicID(ids map[string]string, arguments, result string) {
	matches := taskJobResultPattern.FindAllStringSubmatch(result, -1)
	if len(matches) == 0 {
		return
	}
	id := matches[len(matches)-1][1]
	ids["{{LAST_TASK_JOB_ID}}"] = id
	if key := taskKeyFromArguments(arguments); key != "" {
		ids[taskJobPlaceholder(key)] = id
	}
}

func requireTaskReports(result string, minimum int) error {
	if count := strings.Count(result, taskReportMarker); count < minimum {
		return fmt.Errorf("result contains %d child reports, want at least %d", count, minimum)
	}
	return nil
}
func successfulTaskSessionMatches(result string) [][]string {
	var matches [][]string
	for _, section := range strings.Split(result, "\n\njob-") {
		if !strings.Contains(section, taskReportMarker) {
			continue
		}
		matches = append(matches, taskSessionPattern.FindAllStringSubmatch(section, -1)...)
	}
	return matches
}

func isTaskChildRequest(req chatRequest) bool {
	for _, message := range req.Messages {
		if message.Role == "system" {
			return strings.Contains(contentText(message.Content), "focused sub-agent")
		}
	}
	return false
}

func taskTokenAndStage(req chatRequest) (string, int) {
	stage := 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		switch message.Role {
		case "assistant":
			stage++
		case "user":
			if token := markedToken(contentText(message.Content), taskMarkerPrefix); token != "" {
				return token, stage
			}
		}
	}
	return "", 0
}

func taskPlanFor(token string, stage int) replyPlan {
	if stage == 0 {
		return replyPlan{
			Stage:    0,
			Thinking: "child checking the fixture.",
			Text:     "child working " + token,
			Calls: []plannedCall{{
				Name:      "read",
				Arguments: `{"path":"notes.txt"}`,
				ExpectAny: []string{notesMarker},
			}},
		}
	}
	return replyPlan{
		Stage: stage,
		Text:  taskReportMarker + " " + token,
	}
}

func taskTurnToolText(req chatRequest) string {
	start := 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && markedToken(contentText(req.Messages[i].Content), taskMarkerPrefix) != "" {
			start = i + 1
			break
		}
	}
	var results strings.Builder
	for _, message := range req.Messages[start:] {
		if message.Role == "tool" {
			results.WriteString(contentText(message.Content))
			results.WriteByte('\n')
		}
	}
	return results.String()
}

func validateTaskChildTools(body []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return err
	}
	names, err := validateToolSet(top["tools"])
	if err != nil {
		return err
	}
	if !names["read"] {
		return fmt.Errorf("task child does not advertise read")
	}
	if names["task"] || names["question"] {
		return fmt.Errorf("task child illegally advertises task or question")
	}
	return nil
}

func autoTaskFollowUpSeen(req chatRequest) bool {
	for _, message := range req.Messages {
		if message.Role != "user" {
			continue
		}
		text := contentText(message.Content)
		if strings.Contains(text, "A background sub-agent you started has finished.") &&
			strings.Contains(text, "task: longstay auto") && strings.Contains(text, taskReportMarker) {
			return true
		}
	}
	return false
}

func markedToken(text, prefix string) string {
	start := strings.Index(text, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(text[start:], markerSuffix)
	if end < 0 {
		return ""
	}
	return text[start : start+end]
}

func taskPlansEnabled(p profile) bool {
	for name := range p.Enabled {
		if strings.HasPrefix(name, "task_") {
			return true
		}
	}
	return false
}

func taskSessionCoverageExpected(p profile) bool {
	for _, name := range []string{"task_sync", "task_async", "task_output", "task_output_list", "task_batch", "task_output_batch", "task_wait_all"} {
		if p.enabled(name) {
			return true
		}
	}
	return false
}
func (m *mockLLM) recordTaskChildKey(token string, failed bool) {
	key := token
	if index := strings.LastIndex(key, "/"); index >= 0 {
		key = key[index+1:]
	}
	if key == "" {
		return
	}
	m.taskChildMu.Lock()
	defer m.taskChildMu.Unlock()
	if m.taskChildByKey == nil {
		m.taskChildByKey = make(map[string]int64)
	}
	if m.taskChildFailedByKey == nil {
		m.taskChildFailedByKey = make(map[string]int64)
	}
	if failed {
		m.taskChildFailedByKey[key]++
	} else {
		m.taskChildByKey[key]++
	}
}

func (m *mockLLM) taskChildKeyCount(key string, failed bool) int64 {
	m.taskChildMu.Lock()
	defer m.taskChildMu.Unlock()
	if failed {
		return m.taskChildFailedByKey[key]
	}
	return m.taskChildByKey[key]
}

func (m *mockLLM) taskChildKeySnapshot(failed bool) map[string]int64 {
	m.taskChildMu.Lock()
	defer m.taskChildMu.Unlock()
	source := m.taskChildByKey
	if failed {
		source = m.taskChildFailedByKey
	}
	out := make(map[string]int64, len(source))
	for key, count := range source {
		out[key] = count
	}
	return out
}

func requiredTaskChildKeys(p profile) []string {
	var keys []string
	if p.enabled("task_sync") {
		keys = append(keys, "sync")
	}
	if p.enabled("task_async") || p.enabled("task_output") || p.enabled("task_output_list") {
		keys = append(keys, "single")
	}
	if p.enabled("task_batch") || p.enabled("task_output_batch") {
		keys = append(keys, "batch-a", "batch-b")
	}
	if p.enabled("task_wait_all") {
		keys = append(keys, "all-a", "all-b")
	}
	if p.enabled("task_auto") {
		keys = append(keys, "auto")
	}
	return keys
}
