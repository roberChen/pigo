// This file implements the deterministic reply plan shared by the mock LLM and
// the clients. Both sides derive it from a hash of the prompt's marker token, so
// the harness can prove that what a client received is exactly what a turn was
// supposed to produce, without any shared mutable state between the processes.
package main

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strconv"
	"strings"
)

const (
	// markerPrefix and markerSuffix delimit the token embedded in every prompt.
	markerPrefix = "[[longstay:"
	markerSuffix = "]]"
)

// plannedCall is one tool call a stage emits, together with the result and
// workspace assertions its real execution must satisfy.
type plannedCall struct {
	Name      string
	Arguments string
	// ExpectAny lists acceptable result markers. AllowError marks a deliberate
	// negative case whose error is itself the expected outcome.
	ExpectAny   []string
	AllowError  bool
	Effect      *workspaceEffect
	StageBreak  bool
	FollowUp    bool
	CheckResult func(string) error
}

// replyPlan is what one assistant message is supposed to carry: its thinking,
// its text and (for a non-final stage) its tool calls.
type replyPlan struct {
	Stage    int
	Thinking string
	Text     string
	Calls    []plannedCall
}

// planRounds returns how many tool round-trips this token's turn performs. It
// is derived from the dependency-safe sequence so every stage agrees on shape.
func planRounds(token string) int {
	rounds := len(planStages(token))
	if taskAutoFollowUp(token) {
		rounds++
	}
	return rounds
}

func taskAutoFollowUp(token string) bool {
	followUp := false
	for _, call := range sequence(token) {
		if call.FollowUp {
			followUp = true
		}
		if call.Name == "task_output" && strings.Contains(call.Arguments, `"wait_all":true`) {
			followUp = false
		}
	}
	return followUp
}

func planStages(token string) [][]plannedCall {
	calls := sequence(token)
	stages := make([][]plannedCall, 0, len(calls))
	for _, call := range calls {
		startStage := len(stages) == 0
		if !startStage {
			current := stages[len(stages)-1]
			startStage = call.StageBreak || current[0].StageBreak || len(current) >= currentProfile.CallsPerStage
		}
		if startStage {
			stages = append(stages, nil)
		}
		last := len(stages) - 1
		stages[last] = append(stages[last], call)
	}
	return stages
}

// planFor returns the deterministic plan for one assistant message. stage 0 is
// the first response to the prompt; each stage that emits tool calls is followed
// by the next stage, which answers the tool results. tools=false (the stress
// scenario runs the host with --no-tools) forces a single final stage, because a
// tool call to a tool that is not registered would only produce an error result.
func planFor(token string, stage int, tools bool) replyPlan {
	if !tools {
		return replyPlan{Stage: stage, Thinking: phrase(hashToken(token, stage)>>7, thinkingWords, 0, 2), Text: finalText(token, stage)}
	}
	if stage < planRounds(token) {
		h := hashToken(token, stage)
		plan := replyPlan{
			Stage:    stage,
			Thinking: phrase(h>>7, thinkingWords, 0, 3),
			Text:     "working on " + token + " (stage " + strconv.Itoa(stage) + ")",
		}
		stages := planStages(token)
		if stage < len(stages) {
			plan.Calls = append(plan.Calls, stages[stage]...)
		}
		if stage >= currentProfile.ThinkingBlocks {
			plan.Thinking = ""
		}
		return plan
	}
	thinking := ""
	if stage < currentProfile.ThinkingBlocks {
		thinking = phrase(hashToken(token, stage)>>7, thinkingWords, 0, 2)
	}
	return replyPlan{
		Stage:    stage,
		Thinking: thinking,
		Text:     finalText(token, stage),
	}
}

// finalText is the text of the stage that answers the prompt (rounds+1 stages).
func finalText(token string, stage int) string {
	return "final " + token + " done (" + strconv.Itoa(stage+1) + " stages)"
}

// expectedFinalText is the prompt result a client must receive for token.
func expectedFinalText(token string, tools bool) string {
	stage := 0
	if tools {
		stage = planRounds(token)
	}
	return finalText(token, stage)
}

// hashToken hashes a marker token with a stage discriminator. -1 addresses the
// token-wide plan shape.
func hashToken(token string, stage int) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(token))
	_, _ = h.Write([]byte{byte(stage + 1)})
	return h.Sum64()
}

// promptFor builds the natural-language prompt a client sends: some random
// filler words plus the marker token the plan is derived from. The filler is
// generated from the client's own RNG so a seed reproduces the exact prompts.
func promptFor(rng *rand.Rand, token string) string {
	return phrase(uint64(rng.Int63()), promptWords, 4, 10) + " " + markerPrefix + token + markerSuffix
}

// promptToken extracts the marker token from a prompt, or "" when it carries
// none (a summarization prompt, for example).
func promptToken(prompt string) string {
	start := strings.Index(prompt, markerPrefix)
	if start < 0 {
		return ""
	}
	start += len(markerPrefix)
	end := strings.Index(prompt[start:], markerSuffix)
	if end < 0 {
		return ""
	}
	return prompt[start : start+end]
}

// phrase builds a deterministic sentence of min..max words picked by hash. The
// trailing period keeps the text from looking truncated to the model.
func phrase(hash uint64, words []string, min, max int) string {
	n := min
	if max > min {
		n += int(hash % uint64(max-min+1))
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, words[int((hash>>(uint(i)*7+3))%uint64(len(words)))])
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + "."
}

// thinkingWords and promptWords are deliberately tiny vocabularies: they only
// need to produce variable-length, distinct text so delta accumulation and
// sequence handling are exercised.
var (
	thinkingWords = []string{"considering", "the", "workspace", "layout", "then", "checking", "each", "note", "file", "carefully"}
	promptWords   = []string{"please", "inspect", "the", "workspace", "notes", "and", "report", "back", "with", "a", "short", "summary"}
)

// planSummary renders a plan for logs and failure messages.
func planSummary(p replyPlan) string {
	calls := make([]string, 0, len(p.Calls))
	for _, c := range p.Calls {
		calls = append(calls, fmt.Sprintf("%s%s", c.Name, c.Arguments))
	}
	return fmt.Sprintf("stage=%d text=%q thinking=%q calls=[%s]", p.Stage, p.Text, p.Thinking, strings.Join(calls, " "))
}
