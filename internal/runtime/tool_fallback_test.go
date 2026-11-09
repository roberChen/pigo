package runtime

// This file implements the tests for the failed-request policy
// (runtime/tool_fallback.go): a rejected turn is repaired and retried once —
// either by removing the images (the provider named one, or the conversation
// shape isolates the one this request added) or by replacing the tool output the
// request just added with an abnormal placeholder. Every other shape of failure
// stays terminal, which these tests pin one by one.

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

// TestLoopKeepsUserAttachedImageTerminal: an *opaque* failure of a request the
// user attached a picture to is not repaired — no tool call added the image, and
// the error says nothing about it, so the error stays terminal and the user's
// prompt is left intact. (An image-naming error is repaired even here; see
// TestLoopRepairsResumedSessionImageError.)
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
// carries an image from an earlier request, an opaque later failure is not
// repaired — removing pictures the model has already reasoned about is worse than
// letting the error surface. The structural trigger deliberately stays narrow;
// the explicit one covers the case where the provider names the image.
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
// declares the model vision-capable, an opaque failure of a request that added an
// image does not second-guess the declaration — the error is terminal. The
// declaration does not survive the provider naming the image, though:
// TestLoopRepairsDeclaredVisionOnExplicitImageError covers that, because a
// payload the endpoint refuses cannot be sent no matter what the config says.
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

// TestToolFallbackSkipsCancelledRun: a cancelled run must not be retried — the
// retry would fail again, and the image is not the culprit.
func TestToolFallbackSkipsCancelledRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := newToolFallback(false)
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

// TestToolFallbackSkipsShrunkContext: a compaction (or PrepareNextTurn) can
// shrink the context so the request boundary no longer describes the previous
// request. The policy must not guess.
func TestToolFallbackSkipsShrunkContext(t *testing.T) {
	policy := newToolFallback(false)
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

// upstreamImageError is the failure this policy was widened for: the provider
// itself names the image, in the shape OpenAI-compatible gateways use (and the
// media-type list Anthropic uses). Real text, so the classifier is exercised
// against the message a user actually sees.
var upstreamImageError = errors.New(`transport: upstream 400: {"error":{"message":"input[207].image[0]: You have uploaded an unsupported image. Please make sure your image is valid and has one of the following formats: webp, png, jpeg, and gif.","type":"invalid_request_error"}}`)

// TestLoopRepairsResumedSessionImageError is the case that used to be
// unrepairable: the offending picture is already in the conversation (a resumed
// session, or an earlier turn), so no request ever "adds" it. The provider names
// the image, the history is repaired, and the retried turn answers.
func TestLoopRepairsResumedSessionImageError(t *testing.T) {
	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		textTurn("answering after the picture was dropped"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if agentcore.MessagesHaveImage(llm.Messages) {
			return nil, upstreamImageError
		}
		return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream)
	// The session was recorded earlier: a tool produced a picture, and the user
	// came back with a follow-up question.
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("what is on the screen?")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{
			agentcore.NewToolCallContent("call-1", "view_image", json.RawMessage(`{}`)),
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-1", ToolName: "view_image", Content: agentcore.ContentList{
			agentcore.NewImageContent("QUJD", "image/png"),
			agentcore.NewTextContent("viewed shot.png"),
		}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn, Content: agentcore.ContentList{agentcore.NewTextContent("It is a login form.")}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("and the button color?")}},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 2 {
		t.Fatalf("provider attempts = %d, want 2 (failed request + one retry)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonEndTurn ||
		!strings.Contains(contentText(final.Content), "answering after") {
		t.Fatalf("final assistant = %+v, want the retried answer", final)
	}
	if agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("the resumed history still carries the image that failed the request")
	}
	// The tool result keeps its text and is flagged as a failure, so the model
	// knows the picture was dropped rather than silently missing.
	results := toolResultsOf(agentCtx.Messages)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("tool results = %+v, want the image call marked failed", results)
	}
	if text := contentText(results[0].Content); !strings.Contains(text, "viewed shot.png") {
		t.Errorf("repaired tool result = %q, want the original descriptor kept", text)
	}
}

// TestLoopRepairsDeclaredVisionOnExplicitImageError: the explicit trigger beats
// the declaration. A user can declare a gateway model vision-capable and still
// have it refuse one payload; refusing to repair would strand that session
// forever, because the picture cannot be sent at all.
func TestLoopRepairsDeclaredVisionOnExplicitImageError(t *testing.T) {
	vision := true
	provider.RegisterDeclaredModels("openai", []provider.DeclaredModel{
		{ID: "declared-vision-repair-test", SupportsImages: &vision},
	})
	t.Cleanup(func() { provider.RegisterDeclaredModels("openai", nil) })

	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		textTurn("recovered despite the declaration"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if agentcore.MessagesHaveImage(llm.Messages) {
			return nil, upstreamImageError
		}
		return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream)
	cfg.Provider = "openai"
	cfg.Model = "declared-vision-repair-test"
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewTextContent("what is this?"),
			agentcore.NewImageContent("QUJD", "image/png"),
		}},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 2 {
		t.Fatalf("provider attempts = %d, want 2 (explicit image error → one repair)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("final assistant = %+v, want the retried answer", final)
	}
	if agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("the declared-vision run kept the picture the provider refused")
	}
	// The user's prompt survives; only the picture is replaced by a notice.
	user := agentCtx.Messages[0].(agentcore.UserMessage)
	if !strings.Contains(contentText(user.Content), "what is this?") {
		t.Errorf("user message = %q, want the prompt kept", contentText(user.Content))
	}
	if !strings.Contains(contentText(user.Content), "image was removed") {
		t.Errorf("user message = %q, want an omission notice", contentText(user.Content))
	}
}

// TestLoopDoesNotRepairWhenTheErrorIsNotAboutImages: widening the trigger must
// not turn every failure of a vision session into a picture purge. An opaque 500
// on a conversation that already had images stays terminal.
func TestLoopDoesNotRepairWhenTheErrorIsNotAboutImages(t *testing.T) {
	attempts := 0
	stream := func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		return nil, gatewayError
	}
	cfg := newRunCfg(stream)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("look")}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-0", ToolName: "view_image", Content: agentcore.ContentList{
			agentcore.NewImageContent("QUJD", "image/png"),
		}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("continue")}},
	}}

	res := drainRun(t, agentCtx, cfg)

	if attempts != 1 {
		t.Fatalf("provider attempts = %d, want 1 (no repair without an image-naming error)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final assistant = %+v, want the terminal error", final)
	}
	if !agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("an unrelated failure stripped the conversation's images")
	}
}

// TestStreamResponseDropsUnsendableImages: the request path itself must never
// re-send a picture the wire cannot carry, even from a session recorded before
// ingestion enforced the rule. The conversation keeps it; only the LLM copy gets
// a notice.
func TestStreamResponseDropsUnsendableImages(t *testing.T) {
	var sent agentcore.MessageList
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		sent = llm.Messages
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
			agentcore.NewImageContent("AAAAIGZ0eXBhdmlm", "image/avif"),
		}},
	}}
	noEmit := func(context.Context, agentcore.AgentEvent) error { return nil }
	cfg := LoopConfig{Model: "fake", Stream: stream}

	if _, err := streamAssistantResponse(context.Background(), agentCtx, cfg, noEmit); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if agentcore.MessagesHaveImage(sent) {
		t.Error("the request still carries an AVIF image")
	}
	if !strings.Contains(agentcore.ContentToText(sent[0].(agentcore.UserMessage).Content), "image omitted") {
		t.Error("the dropped image left no notice for the model")
	}
	if !agentcore.MessagesHaveImage(agentCtx.Messages) {
		t.Error("sanitizing the request must not modify the history")
	}
}

// TestRepairedSessionContinuesInLaterTurns is the requirement the repair exists
// for: after one image-shaped failure, the conversation — not just the failed
// turn — keeps working. The follow-up message is sent as its own run, exactly as
// an interactive turn after the error, and must reach the model with no image and
// no second failure.
func TestRepairedSessionContinuesInLaterTurns(t *testing.T) {
	first := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		textTurn("answered without the picture"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if agentcore.MessagesHaveImage(llm.Messages) {
			return nil, upstreamImageError
		}
		return provider.StreamFnFromProvider(first)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream)
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewTextContent("what is this?"),
			agentcore.NewImageContent("QUJD", "image/png"),
		}},
	}}
	if res := drainRun(t, agentCtx, cfg); attempts != 2 {
		t.Fatalf("first run attempts = %d, want 2 (failure + repaired retry)", attempts)
	} else if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("first run end = %+v, want an answer", final)
	}

	// The user reacts to that answer: a fresh run over the same history.
	agentCtx.Messages = append(agentCtx.Messages,
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("thanks, and what about the button?")}})
	second := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		textTurn("second answer"),
	}}
	attempts = 0
	cfg2 := newRunCfg(func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		if agentcore.MessagesHaveImage(llm.Messages) {
			return nil, upstreamImageError
		}
		return provider.StreamFnFromProvider(second)(ctx, model, llm, sc)
	})

	res := drainRun(t, agentCtx, cfg2)

	if attempts != 1 {
		t.Fatalf("second run attempts = %d, want 1 (the history no longer carries the image)", attempts)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("second run end = %+v, want an answer", final)
	}
}

// textTool returns a tool whose output is plain text, so the general
// tool-output trigger (not either image trigger) is the one under test.
func textTool(name, output string) execTool {
	return execTool{
		name: name,
		run: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(output)}}, nil
		},
	}
}

// TestLoopDegradesToolOutputOnRejection is the general case: the request the
// provider rejected carried no image, but it added a tool result — so that result
// is the suspect. It becomes an abnormal placeholder holding the call's arguments
// and the provider's error verbatim, and the turn is retried.
func TestLoopDegradesToolOutputOnRejection(t *testing.T) {
	p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
		toolCallTurn("call-1", "lookup", `{"query":"release notes"}`),
		textTurn("answered without the lookup output"),
	}}
	attempts := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		attempts++
		// Attempt 1 is the turn that calls the tool; the request that replays its
		// output is attempt 2, and that is the one the endpoint rejects.
		if attempts == 2 {
			return nil, errors.New("upstream 500: internal server error")
		}
		return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
	}
	cfg := newRunCfg(stream, textTool("lookup", "a very long result the endpoint refused"))
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser},
	}}

	res := drainRun(t, agentCtx, cfg)

	// tool turn, failed request, retried request that answers.
	if attempts != 3 {
		t.Fatalf("provider attempts = %d, want 3 (tool, failed, retry): kinds %v", attempts, res.kinds)
	}
	if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonEndTurn ||
		!strings.Contains(contentText(final.Content), "answered without") {
		t.Fatalf("final assistant = %+v, want the retried answer", final)
	}
	results := toolResultsOf(agentCtx.Messages)
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1: %+v", len(results), results)
	}
	tr := results[0]
	if !agentcore.IsAbnormalToolResult(tr) {
		t.Fatalf("result is not an abnormal placeholder: %+v", tr)
	}
	detail, ok := agentcore.AbnormalDetailsOf(tr)
	if !ok {
		t.Fatalf("placeholder carries no details: %+v", tr)
	}
	if detail.Arguments != `{"query":"release notes"}` {
		t.Errorf("arguments = %q, want the model's verbatim arguments", detail.Arguments)
	}
	if !strings.Contains(detail.ProviderError, "internal server error") {
		t.Errorf("provider error = %q, want it quoted verbatim", detail.ProviderError)
	}
	text := contentText(tr.Content)
	for _, want := range []string{agentcore.AbnormalToolResultMarker, "a very long result"} {
		if !strings.Contains(text, want) {
			t.Errorf("placeholder text = %q, want it to contain %q", text, want)
		}
	}
}

// TestLoopKeepsAuthFailuresTerminal: a credential or rate-limit rejection answers
// the same way on every retry, so degrading the tool output cannot help and the
// error stays terminal.
func TestLoopKeepsAuthFailuresTerminal(t *testing.T) {
	for _, msg := range []string{"401 Unauthorized: invalid api key", "429 Too Many Requests", "insufficient_quota"} {
		t.Run(msg, func(t *testing.T) {
			p := &fauxProvider{name: "faux", models: []provider.Model{{ID: "fake"}}, turns: []fauxTurn{
				toolCallTurn("call-1", "lookup", `{}`),
			}}
			attempts := 0
			stream := func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
				attempts++
				if attempts == 2 {
					return nil, errors.New(msg)
				}
				return provider.StreamFnFromProvider(p)(ctx, model, llm, sc)
			}
			cfg := newRunCfg(stream, textTool("lookup", "output"))
			agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
				agentcore.UserMessage{RoleField: agentcore.RoleUser},
			}}

			res := drainRun(t, agentCtx, cfg)

			if attempts != 2 {
				t.Fatalf("provider attempts = %d, want 2 (no retry after an unrepairable failure)", attempts)
			}
			if final := res.lastTurnEnd(t); final.StopReason != agentcore.StopReasonError {
				t.Fatalf("final assistant = %+v, want the terminal error", final)
			}
			results := toolResultsOf(agentCtx.Messages)
			if len(results) != 1 || results[0].IsError || agentcore.IsAbnormalToolResult(results[0]) {
				t.Errorf("tool output was degraded for an unrepairable failure: %+v", results)
			}
		})
	}
}

// TestAbnormalizeToolResultsIgnoresUnknownAndExisting: the helper only rewrites
// the ids it was given, and never rewraps a placeholder (which would nest
// reasons and lose the original arguments).
func TestAbnormalizeToolResultsIgnoresUnknownAndExisting(t *testing.T) {
	msgs := agentcore.MessageList{
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Content: agentcore.ContentList{
			agentcore.NewToolCallContent("call-1", "lookup", json.RawMessage(`{"q":"x"}`)),
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-1", ToolName: "lookup",
			Content: agentcore.ContentList{agentcore.NewTextContent("first output")}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "call-2", ToolName: "other",
			Content: agentcore.ContentList{agentcore.NewTextContent("keep me")}},
	}

	first := agentcore.AbnormalizeToolResults(msgs, []string{"call-1", "missing"}, "boom", "replaced")
	if len(first) != 1 || first[0].ToolCallID != "call-1" {
		t.Fatalf("rewrote %+v, want only call-1", first)
	}
	if again := agentcore.AbnormalizeToolResults(msgs, []string{"call-1"}, "boom2", "replaced again"); len(again) != 0 {
		t.Errorf("a placeholder was rewritten again: %+v", again)
	}
	if got := contentText(msgs[1].(agentcore.ToolResultMessage).Content); !strings.Contains(got, "first output") {
		t.Errorf("original text lost: %q", got)
	}
	if got := contentText(msgs[2].(agentcore.ToolResultMessage).Content); got != "keep me" {
		t.Errorf("unlisted result changed: %q", got)
	}
}
