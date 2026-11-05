package runtime

// This file implements the tests for the image-degradation policy: a failed turn
// is repaired (images removed, one retry) only when the conversation had no
// image before the request, the request itself added one through a tool call,
// and the model was not explicitly declared vision-capable. Every other shape of
// failure stays terminal, which these tests pin one by one.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// imageTool returns a tool that hands the model a picture, the way read (on an
// image file) and view_image do.
func imageTool(name string) execTool {
	return execTool{
		name: name,
		run: func(_ context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			return agentcore.AgentToolResult{Content: agentcore.ContentList{
				agentcore.NewImageContent("QUJD", "image/png"),
				agentcore.NewTextContent("viewed shot.png"),
			}}, nil
		},
	}
}

// gatewayError stands in for a provider failure whose text says nothing useful
// about the image: the case an error-text classification would miss.
var gatewayError = errors.New("upstream 400 Bad Request")

// rejectingStream wraps a provider and fails every request that carries an
// image, counting them. The provider itself only sees image-free requests, so
// its scripted turns line up with the successful calls.
func rejectingStream(p provider.Provider, refused *int) provider.StreamFn {
	next := provider.StreamFnFromProvider(p)
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		if agentcore.MessagesHaveImage(llm.Messages) {
			*refused++
			return nil, gatewayError
		}
		return next(ctx, model, llm, cfg)
	}
}

// runTrace is what a finished run told the outside world: the event kinds, the
// tool_execution_end events (to assert what the UI was told), the turn_end
// messages (a terminal error turn is never appended to the context, so its
// message only exists here) and the messages the run produced.
type runTrace struct {
	kinds    []string
	ends     []agentcore.ToolExecutionEndEvent
	turnEnds []agentcore.AssistantMessage
	msgs     []agentcore.AgentMessage
}

// lastTurnEnd returns the message of the final turn_end event.
func (d runTrace) lastTurnEnd(t *testing.T) agentcore.AssistantMessage {
	t.Helper()
	if len(d.turnEnds) == 0 {
		t.Fatalf("run emitted no turn_end: kinds %v", d.kinds)
	}
	return d.turnEnds[len(d.turnEnds)-1]
}

// drainRun drives a run to completion and collects its trace.
func drainRun(t *testing.T, agentCtx *agentcore.AgentContext, cfg RunConfig) runTrace {
	t.Helper()
	s := agentLoop(context.Background(), agentCtx, cfg)
	var out runTrace
	for ev := range s.Events() {
		out.kinds = append(out.kinds, ev.EventType())
		switch e := ev.(type) {
		case agentcore.ToolExecutionEndEvent:
			out.ends = append(out.ends, e)
		case agentcore.TurnEndEvent:
			out.turnEnds = append(out.turnEnds, e.Message)
		}
	}
	msgs, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("stream result: %v", err)
	}
	out.msgs = msgs
	return out
}

// toolResultsOf returns the tool result messages in msgs.
func toolResultsOf(msgs agentcore.MessageList) []agentcore.ToolResultMessage {
	var out []agentcore.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			out = append(out, tr)
		}
	}
	return out
}

// TestLoopDegradesImageAddedByThisRequest is the scenario the fallback exists
// for: the conversation starts image-free, this request's view_image makes it
// carry one, and the request fails. The tool call becomes a failure, the picture
// leaves the conversation, and the retried turn answers the question.
func TestLoopDegradesImageAddedByThisRequest(t *testing.T) {
	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		toolCallTurn("call-1", "snap", "{}"),
		textTurn("recovered without the picture"),
	}}
	refused := 0
	cfg := newRunCfg(rejectingStream(p, &refused), imageTool("snap"))
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("what is on the screenshot?")}},
	}}

	res := drainRun(t, agentCtx, cfg)

	if refused != 1 {
		t.Fatalf("image-carrying requests = %d, want exactly 1", refused)
	}
	if got := p.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool turn + retry): kinds %v", got, res.kinds)
	}
	// The failed attempt is retried in place, so the run has three turns, and the
	// refused one leaves nothing behind in the transcript.
	if n := countKind(res.kinds, agentcore.EventTurnStart); n != 3 {
		t.Errorf("turns = %d, want 3 (tool, failed, retry): kinds %v", n, res.kinds)
	}
	if len(res.msgs) != 3 {
		t.Errorf("run produced %d messages, want 3 (tool call, tool result, retried answer): %+v", len(res.msgs), res.msgs)
	}

	final := res.lastTurnEnd(t)
	if final.StopReason != agentcore.StopReasonEndTurn || !strings.Contains(contentText(final.Content), "recovered") {
		t.Fatalf("final assistant = %+v, want the retried end_turn answer", final)
	}

	// The conversation the model sees next carries no image at all, and the failed
	// call records the provider error.
	if agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Errorf("history still carries an image: %+v", agentCtx.Messages)
	}
	results := toolResultsOf(agentCtx.Messages)
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1: %+v", len(results), results)
	}
	tr := results[0]
	if !tr.IsError {
		t.Error("degraded tool result is not marked as an error")
	}
	if text := contentText(tr.Content); !strings.Contains(text, "400 Bad Request") {
		t.Errorf("degraded text = %q, want the provider error quoted", text)
	}
	if !strings.Contains(contentText(tr.Content), "viewed shot.png") {
		t.Errorf("degraded text = %q, want the original descriptor kept", contentText(tr.Content))
	}

	// The retry was actually image-free (not just the repaired history).
	if got := p.requestAt(1); agentcore.MessagesHaveImage(got.Context.Messages) {
		t.Errorf("retried request still carries an image: %+v", got.Context.Messages)
	}

	// The front-end is told the call it already rendered is a failure now.
	var call1Ends []agentcore.ToolExecutionEndEvent
	for _, e := range res.ends {
		if e.ToolCallID == "call-1" {
			call1Ends = append(call1Ends, e)
		}
	}
	if len(call1Ends) != 2 {
		t.Fatalf("tool_execution_end for call-1 = %d, want 2 (original + degraded)", len(call1Ends))
	}
	if call1Ends[0].IsError {
		t.Error("the first tool_execution_end should be the successful call")
	}
	if !call1Ends[1].IsError {
		t.Error("the re-emitted tool_execution_end is not marked as an error")
	}
	if agentcore.ContentHasImage(call1Ends[1].Result.Content) {
		t.Error("the re-emitted result still carries the image")
	}
}

// TestLoopDegradesOnOpaqueError: the trigger never reads the error text, so an
// unrelated-looking 500 is repaired the same way. Under the policy that is the
// point — a missed image rejection is unrecoverable, the extra retry is cheap.
func TestLoopDegradesOnOpaqueError(t *testing.T) {
	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		toolCallTurn("call-1", "snap", "{}"),
		textTurn("answered after the retry"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if attempts == 2 {
			return nil, errors.New("upstream 500: internal server error")
		}
		return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream, imageTool("snap"))
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 3 {
		t.Fatalf("provider attempts = %d, want 3 (tool, 500, retry)", attempts)
	}
	final := res.lastTurnEnd(t)
	if final.StopReason != agentcore.StopReasonEndTurn || !strings.Contains(contentText(final.Content), "answered after the retry") {
		t.Fatalf("final assistant = %+v, want the retried answer", final)
	}
	if agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("history still carries an image after the repair")
	}
	if results := toolResultsOf(agentCtx.Messages); !results[0].IsError {
		t.Error("the image-carrying tool result was not marked as a failure")
	}
}

// TestLoopKeepsUserAttachedImageTerminal: a picture the user attached is not
// added by a view_image call, so it is not degraded — the error stays terminal
// and the user's prompt is left intact.
func TestLoopKeepsUserAttachedImageTerminal(t *testing.T) {
	attempts := 0
	stream := func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		return nil, gatewayError
	}
	cfg := newRunCfg(stream)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewTextContent("what is this?"),
			agentcore.NewImageContent("QUJD", "image/png"),
		}},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 1 {
		t.Fatalf("provider attempts = %d, want 1 (no degradation without a view_image call)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final assistant = %+v, want the terminal error", final)
	}
	user := agentCtx.Messages[0].(agentcore.UserMessage)
	if !agentcore.ContentHasImage(user.Content) {
		t.Error("the user's image was stripped without a view_image call")
	}
}

// TestLoopKeepsFailureTerminalWhenHistoryHadImage: once the conversation already
// carries an image from an earlier request, a later failure is not repaired —
// removing pictures the model has already reasoned about is worse than letting
// the error surface.
func TestLoopKeepsFailureTerminalWhenHistoryHadImage(t *testing.T) {
	// Turn 1's tool result is an image and succeeds; turn 2 adds a second image and
	// the request carrying both fails.
	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		toolCallTurn("call-1", "snap", "{}"),
		toolCallTurn("call-2", "snap", "{}"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		// Only refuse once the conversation holds a tool-made image from an earlier
		// request, i.e. the delta alone would otherwise qualify.
		if attempts >= 3 {
			return nil, gatewayError
		}
		return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream, imageTool("snap"))
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}}

	res := drainRun(t, agentCtx, cfg)

	// tool, tool(call-2), failed request → no retry.
	if attempts != 3 {
		t.Fatalf("provider attempts = %d, want 3 (no degradation when history had an image)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final assistant = %+v, want the terminal error", final)
	}
	if !agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("an earlier image was removed even though the policy says not to")
	}
}

// TestLoopKeepsDeclaredVisionModelTerminal: when the user's config explicitly
// declares the model vision-capable, the declaration wins. The fallback must not
// second-guess it by deleting the image.
func TestLoopKeepsDeclaredVisionModelTerminal(t *testing.T) {
	vision := true
	provider.RegisterDeclaredModels("openai", []provider.DeclaredModel{
		{ID: "declared-vision-test", SupportsImages: &vision},
	})
	t.Cleanup(func() { provider.RegisterDeclaredModels("openai", nil) })

	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		toolCallTurn("call-1", "snap", "{}"),
		textTurn("must not be reached"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if attempts == 2 {
			return nil, gatewayError
		}
		return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream, imageTool("snap"))
	cfg.Provider = "openai"
	cfg.Model = "declared-vision-test"
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 2 {
		t.Fatalf("provider attempts = %d, want 2 (declared vision model → no retry)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final assistant = %+v, want the terminal error", final)
	}
	if !agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("the image was removed for a model the user declared vision-capable")
	}
}

// TestLoopDegradesForUndeclaredModel guards the other side of the declaration
// rule: the same provider and a model with no declaration keeps the fallback,
// because "vision-capable" there is pigo's default, not the user's statement.
func TestLoopDegradesForUndeclaredModel(t *testing.T) {
	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		toolCallTurn("call-1", "snap", "{}"),
		textTurn("recovered"),
	}}
	refused := 0
	cfg := newRunCfg(rejectingStream(p, &refused), imageTool("snap"))
	cfg.Provider = "openai"
	cfg.Model = "undeclared-model"
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}}

	res := drainRun(t, agentCtx, cfg)

	if refused != 1 || p.callCount() != 2 {
		t.Fatalf("refused = %d, provider calls = %d, want 1 and 2", refused, p.callCount())
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("final assistant = %+v, want the retried answer", final)
	}
}

// TestLoopKeepsImageFreeErrorsTerminal: with no image in the conversation there
// is nothing to strip, so a failure must not be retried.
func TestLoopKeepsImageFreeErrorsTerminal(t *testing.T) {
	attempts := 0
	stream := func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		return nil, gatewayError
	}
	cfg := newRunCfg(stream)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 1 {
		t.Fatalf("provider attempts = %d, want 1 (no image → no retry)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final assistant = %+v, want the terminal error", final)
	}
}

// TestLoopRetriesAtMostOnce bounds the repair: even if the provider keeps
// failing, the run terminates after one repair instead of looping.
func TestLoopRetriesAtMostOnce(t *testing.T) {
	attempts := 0
	first := true
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if first {
			first = false
			s := provider.NewAssistantMessageEventStream(0)
			msg := oneToolAssistant("call-1", "snap")
			go func() {
				_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
				s.Close()
			}()
			return s, nil
		}
		return nil, gatewayError
	}
	cfg := newRunCfg(stream, imageTool("snap"))
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}}

	res := drainRun(t, agentCtx, cfg)

	// tool turn, failed turn, one retry that also fails → stop.
	if attempts != 3 {
		t.Fatalf("provider attempts = %d, want 3 (one repair, then stop)", attempts)
	}
	if n := countKind(res.kinds, agentcore.EventTurnStart); n != 3 {
		t.Errorf("turns = %d, want 3: kinds %v", n, res.kinds)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final assistant = %+v, want the terminal error", final)
	}
}

// TestImageFallbackSkipsCancelledRun: a cancelled run must not be retried — the
// retry would fail again, and the image is not the culprit.
func TestImageFallbackSkipsCancelledRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := newImageFallback(false)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("look")}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-1", ToolName: "view_image", Content: agentcore.ContentList{
			agentcore.NewImageContent("QUJD", "image/png"),
		}},
	}}
	emit := func(agentcore.AgentEvent) error { return nil }

	retry := policy.recover(ctx, agentCtx, 1, agentcore.AssistantMessage{
		RoleField:    agentcore.RoleAssistant,
		StopReason:   agentcore.StopReasonError,
		ErrorMessage: "context canceled",
	}, emit)

	if retry {
		t.Fatal("a cancelled run was retried")
	}
	if !agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("a cancelled run stripped the image")
	}
	if policy.used {
		t.Error("a cancelled run consumed its single repair")
	}
}

// TestImageFallbackSkipsShrunkContext: a compaction (or PrepareNextTurn) can
// shrink the context so the request boundary no longer describes the previous
// request. The policy must not guess.
func TestImageFallbackSkipsShrunkContext(t *testing.T) {
	policy := newImageFallback(false)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("look")}},
	}}
	emit := func(agentcore.AgentEvent) error { return nil }

	if policy.recover(context.Background(), agentCtx, 5, agentcore.AssistantMessage{StopReason: agentcore.StopReasonError}, emit) {
		t.Fatal("a boundary past the end of the context was treated as a repair")
	}
	if policy.used {
		t.Error("the policy consumed its single repair on an invalid boundary")
	}
}

// TestStreamResponseStripsImagesOnlyForUndeclaredCapability is the only strip left
// on the request path: the static capability check. A repair does not arm a
// run-wide latch, so a vision-capable model's history keeps its images.
func TestStreamResponseStripsImagesOnlyForUndeclaredCapability(t *testing.T) {
	sawImage := true
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		sawImage = agentcore.MessagesHaveImage(llm.Messages)
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{
				RoleField:  agentcore.RoleAssistant,
				StopReason: agentcore.StopReasonEndTurn,
			}})
			s.Close()
		}()
		return s, nil
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewImageContent("QUJD", "image/png"),
		}},
	}}
	noEmit := func(context.Context, agentcore.AgentEvent) error { return nil }

	// An undeclared model is assumed vision-capable: the image goes out.
	cfg := LoopConfig{Model: "fake", Stream: stream}
	if _, err := streamAssistantResponse(context.Background(), agentCtx, cfg, noEmit); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !sawImage {
		t.Fatal("assumed vision-capable request carried no image")
	}

	// A model declared text-only gets the flattened history.
	text := false
	provider.RegisterDeclaredModels("openai", []provider.DeclaredModel{{ID: "decltest-text-only", SupportsImages: &text}})
	t.Cleanup(func() { provider.RegisterDeclaredModels("openai", nil) })
	cfg.Provider = "openai"
	cfg.Model = "decltest-text-only"
	if _, err := streamAssistantResponse(context.Background(), agentCtx, cfg, noEmit); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if sawImage {
		t.Error("a text-only model's request carried the image")
	}
	if !agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("stripping for the wire must not modify the history")
	}
}

// contentText concatenates the text blocks of list.
func contentText(list agentcore.ContentList) string {
	var b strings.Builder
	for _, c := range list {
		if tc, ok := c.(agentcore.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
