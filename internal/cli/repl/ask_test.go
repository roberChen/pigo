package repl

// Tests for the REPL's side of the question tool: the model's question is printed
// on the terminal, the operator's answer is read from the same shared input as the
// prompt, and the tool result the model receives carries that answer.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/provider"
)

// askProvider replays one scripted turn per call: the first asks the operator a
// question through the question tool, the second is a plain final answer. It keeps
// every request so a test can assert what the model actually received.
type askProvider struct {
	calls    int
	requests []provider.CompletionRequest
}

func (p *askProvider) Name() string { return "faux" }
func (p *askProvider) Models() []provider.Model {
	return []provider.Model{{Provider: "faux", ID: "faux"}}
}

func (p *askProvider) StreamCompletion(ctx context.Context, req provider.CompletionRequest) (*provider.AssistantMessageEventStream, error) {
	p.calls++
	p.requests = append(p.requests, req)

	var msg agentcore.AssistantMessage
	if p.calls == 1 {
		msg = agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content: agentcore.ContentList{
				agentcore.NewToolCallContent("call-1", "question",
					json.RawMessage(`{"question":"Which branch?","options":["main","release"]}`)),
			},
			StopReason: agentcore.StopReasonToolUse,
		}
	} else {
		msg = agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("done")},
			StopReason: agentcore.StopReasonEndTurn,
		}
	}

	s := provider.NewAssistantMessageEventStream(0)
	go func() {
		_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
		if p.calls == 1 {
			_ = s.Emit(ctx, provider.StreamToolCallEvent{Partial: msg})
		} else {
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
		}
		_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		s.Close()
	}()
	return s, nil
}

// TestREPLAnswersQuestionTool drives a run whose first turn asks a question: the
// question is printed with its options, the numbered reply selects an option, and
// the model's next request carries that answer as the tool result.
func TestREPLAnswersQuestionTool(t *testing.T) {
	p := &askProvider{}
	deps, _ := newTestDeps(t, p)
	deps.reg = agenttool.NewToolRegistry()
	if err := deps.reg.Register(&agenttool.QuestionTool{}); err != nil {
		t.Fatalf("register question tool: %v", err)
	}

	var out bytes.Buffer
	// prompt, the numbered answer to the question, then exit.
	if err := runREPL(strings.NewReader("hello\n2\n/exit\n"), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	for _, want := range []string{"? Which branch?", "1) main", "2) release", "answer (number or text):"} {
		if !strings.Contains(got, want) {
			t.Fatalf("question prompt missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "The user answered: release") {
		t.Errorf("the inline tool result did not show the selected answer:\n%s", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("the run did not continue to the final turn:\n%s", got)
	}

	if p.calls != 2 {
		t.Fatalf("provider called %d times, want 2 (question turn, then final)", p.calls)
	}
	answer := toolResultText(p.requests[1].Context.Messages)
	if !strings.Contains(answer, "release") {
		t.Errorf("the model's second request did not carry the answer; tool results:\n%s", answer)
	}
}

// TestREPLAnswersQuestionWithFreeText verifies a non-numeric reply is passed
// through verbatim instead of being rejected as an unknown option.
func TestREPLAnswersQuestionWithFreeText(t *testing.T) {
	p := &askProvider{}
	deps, _ := newTestDeps(t, p)
	deps.reg = agenttool.NewToolRegistry()
	if err := deps.reg.Register(&agenttool.QuestionTool{}); err != nil {
		t.Fatalf("register question tool: %v", err)
	}

	var out bytes.Buffer
	if err := runREPL(strings.NewReader("hello\nfeature/x\n/exit\n"), &out, deps); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider called %d times, want 2", p.calls)
	}
	if answer := toolResultText(p.requests[1].Context.Messages); !strings.Contains(answer, "feature/x") {
		t.Errorf("free-text answer not delivered to the model:\n%s", answer)
	}
}

// toolResultText joins the text of every tool result message in msgs.
func toolResultText(msgs agentcore.MessageList) string {
	var b strings.Builder
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			b.WriteString(agentcore.ContentToText(tr.Content))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestAskUserREPLClosedInput verifies the asker reports EOF so the question tool
// can give the model an actionable result instead of hanging on a dead terminal.
func TestAskUserREPLClosedInput(t *testing.T) {
	deps, _ := newTestDeps(t, &askProvider{})
	deps.in = bufio.NewReader(strings.NewReader(""))
	var out bytes.Buffer
	if _, err := askUserREPL(&out, deps, agentcore.AskRequest{Question: "anyone?"}); err == nil {
		t.Fatal("asker should report a closed input as an error")
	}
}
