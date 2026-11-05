package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// fakeStream builds a StreamFn that replays a fixed sequence of events, pushing
// each onto an AssistantMessageEventStream from a producer goroutine.
func fakeStream(events []provider.AssistantMessageEvent) provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			for _, ev := range events {
				if err := s.Emit(ctx, ev); err != nil {
					s.SetError(err)
					s.Close()
					return
				}
			}
			s.Close()
		}()
		return s, nil
	}
}

// drives streamAssistantResponse with a synchronous emit that records events.
func runStream(t *testing.T, agentCtx *agentcore.AgentContext, cfg LoopConfig) (agentcore.AssistantMessage, []agentcore.AgentEvent) {
	t.Helper()
	var got []agentcore.AgentEvent
	emit := func(ctx context.Context, ev agentcore.AgentEvent) error {
		got = append(got, ev)
		return nil
	}
	msg, err := streamAssistantResponse(context.Background(), agentCtx, cfg, emit)
	if err != nil {
		t.Fatalf("streamAssistantResponse: %v", err)
	}
	return msg, got
}

func TestStreamResponseBackfillAndEvents(t *testing.T) {
	partial0 := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
	partial1 := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("hel")}}
	final := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("hello")}, StopReason: agentcore.StopReasonEndTurn}

	cfg := LoopConfig{
		Model: "fake",
		Stream: fakeStream([]provider.AssistantMessageEvent{
			provider.StreamStartEvent{Partial: partial0},
			provider.StreamTextEvent{Partial: partial1},
			provider.StreamDoneEvent{Message: final},
		}),
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	msg, events := runStream(t, agentCtx, cfg)

	if msg.StopReason != agentcore.StopReasonEndTurn {
		t.Errorf("final stopReason = %q, want end_turn", msg.StopReason)
	}
	// Context should hold the user message + the final assistant message (the
	// placeholder was replaced, not appended twice).
	if len(agentCtx.Messages) != 2 {
		t.Fatalf("context messages = %d, want 2: %+v", len(agentCtx.Messages), agentCtx.Messages)
	}
	last, ok := agentCtx.Messages[1].(agentcore.AssistantMessage)
	if !ok || len(last.Content) != 1 {
		t.Fatalf("last message not final assistant: %+v", agentCtx.Messages[1])
	}
	// Event order: message_start, message_update, message_end.
	wantKinds := []string{agentcore.EventMessageStart, agentcore.EventMessageUpdate, agentcore.EventMessageEnd}
	if len(events) != len(wantKinds) {
		t.Fatalf("event count = %d, want %d: %+v", len(events), len(wantKinds), events)
	}
	for i, w := range wantKinds {
		if events[i].EventType() != w {
			t.Errorf("event[%d] = %q, want %q", i, events[i].EventType(), w)
		}
	}
}

func TestStreamResponseErrorEvent(t *testing.T) {
	errMsg := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonError, ErrorMessage: "boom"}
	cfg := LoopConfig{
		Model:  "fake",
		Stream: fakeStream([]provider.AssistantMessageEvent{provider.StreamErrorEvent{Message: errMsg}}),
	}
	agentCtx := &agentcore.AgentContext{}
	msg, events := runStream(t, agentCtx, cfg)
	if msg.StopReason != agentcore.StopReasonError || msg.ErrorMessage != "boom" {
		t.Errorf("want error terminal message, got %+v", msg)
	}
	// No start event was sent; error should still append the terminal message.
	if len(agentCtx.Messages) != 1 {
		t.Fatalf("context messages = %d, want 1", len(agentCtx.Messages))
	}
	if events[len(events)-1].EventType() != agentcore.EventMessageEnd {
		t.Errorf("last event = %q, want message_end", events[len(events)-1].EventType())
	}
}

func TestStreamResponseDynamicAPIKey(t *testing.T) {
	var seenKey string
	streamFn := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		seenKey = cfg.APIKey
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn}})
			s.Close()
		}()
		return s, nil
	}
	cfg := LoopConfig{
		Model:     "fake",
		APIKey:    "static-key",
		Provider:  "test",
		Stream:    streamFn,
		GetAPIKey: func(ctx context.Context, provider string) string { return "dynamic-key" },
	}
	runStream(t, &agentcore.AgentContext{}, cfg)
	if seenKey != "dynamic-key" {
		t.Errorf("dynamic key not used: got %q", seenKey)
	}

	// Empty dynamic key falls back to static.
	cfg.GetAPIKey = func(ctx context.Context, provider string) string { return "" }
	runStream(t, &agentcore.AgentContext{}, cfg)
	if seenKey != "static-key" {
		t.Errorf("fallback to static key failed: got %q", seenKey)
	}
}

func TestStreamResponseTransformAndConvertOrder(t *testing.T) {
	var order []string
	cfg := LoopConfig{
		Model: "fake",
		TransformContext: func(ctx context.Context, msgs agentcore.MessageList) agentcore.MessageList {
			order = append(order, "transform")
			return msgs
		},
		ConvertToLlm: func(msgs agentcore.MessageList) agentcore.MessageList {
			order = append(order, "convert")
			return msgs
		},
		Stream: func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			order = append(order, "stream")
			s := provider.NewAssistantMessageEventStream(0)
			go func() {
				_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
				s.Close()
			}()
			return s, nil
		},
	}
	runStream(t, &agentcore.AgentContext{}, cfg)
	if len(order) != 3 || order[0] != "transform" || order[1] != "convert" || order[2] != "stream" {
		t.Errorf("call order wrong: %v", order)
	}
}

func TestStreamResponseEarlyBuildFailure(t *testing.T) {
	cfg := LoopConfig{
		Model: "fake",
		Stream: func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			return nil, context.DeadlineExceeded
		},
	}
	msg, _ := runStream(t, &agentcore.AgentContext{}, cfg)
	if msg.StopReason != agentcore.StopReasonError {
		t.Errorf("early build failure should yield error message, got %+v", msg)
	}
}

// TestStreamResponseStripsImagesForTextOnlyModel covers Issue 3c: a resumed
// session (or a mid-session /model switch) may hold images the current model
// cannot accept. They must be flattened on the way to the provider — otherwise
// the whole request fails at the wire — while the session's own message list
// keeps them, so switching back to a vision model still sees the picture.
func TestStreamResponseStripsImagesForTextOnlyModel(t *testing.T) {
	const name = "runtime-image-test"
	if err := provider.RegisterCustomProvider(provider.ProviderSpec{
		Name:           name,
		Protocol:       provider.ProtocolOpenAI,
		DefaultBaseURL: "https://example.invalid/v1",
		// SupportsImages deliberately left false: a custom endpoint is text-only
		// unless it opts in.
	}, nil, ""); err != nil {
		t.Fatalf("register custom provider: %v", err)
	}
	t.Cleanup(func() { provider.UnregisterCustomProvider(name) })

	img := agentcore.NewImageContent("aGk=", "image/png")
	agentCtx := &agentcore.AgentContext{
		Messages: agentcore.MessageList{
			agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
				agentcore.NewTextContent("what is this?"),
				img,
			}},
			agentcore.ToolResultMessage{Content: agentcore.ContentList{img}},
		},
	}

	var sent provider.LlmContext
	cfg := LoopConfig{
		Model:    "text-only",
		Provider: name,
		Stream: func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			sent = llm
			s := provider.NewAssistantMessageEventStream(0)
			go func() {
				_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn}})
				s.Close()
			}()
			return s, nil
		},
	}

	runStream(t, agentCtx, cfg)

	if sent.Messages == nil {
		t.Fatal("provider was never called")
	}
	for _, m := range sent.Messages {
		switch v := m.(type) {
		case agentcore.UserMessage:
			if agentcore.ContentHasImage(v.Content) {
				t.Error("user message still carried an image for a text-only model")
			}
		case agentcore.ToolResultMessage:
			if agentcore.ContentHasImage(v.Content) {
				t.Error("tool result still carried an image for a text-only model")
			}
		}
	}
	// The notice must be present so the model knows something was omitted.
	found := false
	if u, ok := sent.Messages[0].(agentcore.UserMessage); ok {
		for _, c := range u.Content {
			if tc, ok := c.(agentcore.TextContent); ok && strings.Contains(tc.Text, "image omitted") {
				found = true
			}
		}
	}
	if !found {
		t.Error("stripped message lacks the omission notice")
	}

	// The session keeps its images: only the LLM-bound copy was flattened.
	if !agentcore.ContentHasImage(agentCtx.Messages[0].(agentcore.UserMessage).Content) {
		t.Error("session history lost its image; switching back to a vision model would not recover it")
	}
}

// TestStreamResponseKeepsImagesForVisionModel is the contrast case: a model that
// accepts images receives them untouched.
func TestStreamResponseKeepsImagesForVisionModel(t *testing.T) {
	img := agentcore.NewImageContent("aGk=", "image/png")
	agentCtx := &agentcore.AgentContext{
		Messages: agentcore.MessageList{
			agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
				agentcore.NewTextContent("what is this?"),
				img,
			}},
		},
	}
	var sent provider.LlmContext
	cfg := LoopConfig{
		// An unresolvable provider/model pair keeps the permissive default, which
		// is what every built-in model does.
		Model: "fake",
		Stream: func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			sent = llm
			s := provider.NewAssistantMessageEventStream(0)
			go func() {
				_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonEndTurn}})
				s.Close()
			}()
			return s, nil
		},
	}
	runStream(t, agentCtx, cfg)
	if !agentcore.ContentHasImage(sent.Messages[0].(agentcore.UserMessage).Content) {
		t.Error("vision model did not receive the image")
	}
}
