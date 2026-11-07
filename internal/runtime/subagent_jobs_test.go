package runtime

// Tests for background sub-agent jobs: the tool returns before the child finishes,
// the report arrives through the follow-up seam exactly once, a run does not settle
// while a background child is running, and task_output can list/await jobs.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/provider"
)

// gatedProvider is a child provider that signals when a run starts and blocks until
// released, so a test can observe "the tool returned while the child still ran".
type gatedProvider struct {
	name    string
	started chan struct{}
	release chan struct{}
	reply   string
	once    sync.Once
}

func newGatedProvider(reply string) *gatedProvider {
	return &gatedProvider{name: "gated", started: make(chan struct{}), release: make(chan struct{}), reply: reply}
}

func (p *gatedProvider) Name() string { return p.name }
func (p *gatedProvider) Models() []provider.Model {
	return []provider.Model{{Provider: p.name, ID: "child"}}
}

func (p *gatedProvider) StreamCompletion(ctx context.Context, _ provider.CompletionRequest) (*provider.AssistantMessageEventStream, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
	final := partial
	final.Content = agentcore.ContentList{agentcore.NewTextContent(p.reply)}
	final.StopReason = agentcore.StopReasonEndTurn
	s := provider.NewAssistantMessageEventStream(0)
	go func() {
		_ = s.Emit(ctx, provider.StreamStartEvent{Partial: partial})
		_ = s.Emit(ctx, provider.StreamTextEvent{Partial: final})
		_ = s.Emit(ctx, provider.StreamDoneEvent{Message: final})
		s.Close()
	}()
	return s, nil
}

// backgroundTool builds a task tool wired to a job registry and a gated child.
func backgroundTool(t *testing.T, child *gatedProvider) (*SubAgentTool, *SubAgentJobs, context.Context) {
	t.Helper()
	t.Setenv("PIGO_HOME", t.TempDir())
	jobs := NewSubAgentJobs()
	factory := func() RunConfig {
		return RunConfig{
			LoopConfig: LoopConfig{Model: "child", Stream: provider.StreamFnFromProvider(child)},
			Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
		}
	}
	tool := NewTaskTool(factory, nil, jobs)
	ctx := agentcore.WithSessionID(context.Background(), "parent-session-bg")
	t.Cleanup(func() { closeReleased(child) })
	return tool, jobs, ctx
}

// closeReleased releases a gated provider once (idempotent via recover).
func closeReleased(p *gatedProvider) {
	defer func() { _ = recover() }()
	close(p.release)
}

// TestBackgroundJobReturnsImmediatelyAndDeliversOnce verifies `mode: "async"`
// returns a job id while the child is still running, and that the finished report is
// delivered as a follow-up user message exactly once.
func TestBackgroundJobReturnsImmediatelyAndDeliversOnce(t *testing.T) {
	child := newGatedProvider("background report")
	tool, jobs, ctx := backgroundTool(t, child)

	res, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"do it","mode":"async"}`), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := agentcore.ContentToText(res.Content)
	if !strings.Contains(text, "Started sub-agent") || !strings.Contains(text, "mode: async") || !strings.Contains(text, "job-1") {
		t.Fatalf("result should hand back the job id:\n%s", text)
	}
	// The child has started but must still be blocked: the tool returned anyway.
	select {
	case <-child.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background child never started")
	}
	if _, _, result, _, _ := mustJob(t, jobs, "job-1").Snapshot(); result != "" {
		t.Fatalf("child finished before the tool returned: %q", result)
	}

	// No follow-up while it is still running and nothing else changed: the seam
	// blocks, so use a cancelled context to assert it does not report early.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if msgs := jobs.FollowUpMessages(cancelled, nil); len(msgs) != 0 {
		t.Fatalf("cancelled follow-up delivered %d messages", len(msgs))
	}

	close(child.release)
	job := mustJob(t, jobs, "job-1")
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not finish after release")
	}

	msgs := jobs.FollowUpMessages(context.Background(), nil)
	if len(msgs) != 1 {
		t.Fatalf("follow-up messages = %d, want 1", len(msgs))
	}
	msg, ok := msgs[0].(agentcore.UserMessage)
	if !ok {
		t.Fatalf("follow-up = %T, want a user message", msgs[0])
	}
	content := agentcore.ContentToText(msg.Content)
	for _, want := range []string{"background sub-agent you started has finished", "job-1", "background report"} {
		if !strings.Contains(content, want) {
			t.Errorf("follow-up missing %q:\n%s", want, content)
		}
	}
	if again := jobs.FollowUpMessages(context.Background(), nil); len(again) != 0 {
		t.Errorf("a delivered report must not be delivered twice, got %d messages", len(again))
	}
}

// TestFollowUpWaitsForRunningJob verifies the seam waits for the next completion
// instead of letting the run settle while a background child is still running.
func TestFollowUpWaitsForRunningJob(t *testing.T) {
	child := newGatedProvider("late report")
	tool, jobs, ctx := backgroundTool(t, child)
	if _, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"slow","mode":"async"}`), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	<-child.started

	done := make(chan []agentcore.AgentMessage, 1)
	go func() { done <- jobs.FollowUpMessages(context.Background(), nil) }()
	select {
	case msgs := <-done:
		t.Fatalf("follow-up returned %d messages before the job finished", len(msgs))
	case <-time.After(100 * time.Millisecond):
	}
	close(child.release)
	select {
	case msgs := <-done:
		if len(msgs) != 1 {
			t.Fatalf("follow-up messages = %d, want 1", len(msgs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up did not wake when the job finished")
	}
}

// TestBackgroundJobFailureDelivered verifies a failed background child is reported
// as a failed job (the model learns the delegation failed).
func TestBackgroundJobFailureDelivered(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	jobs := NewSubAgentJobs()
	errTurn := func(text string) fauxTurn {
		partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
		withText := partial
		withText.Content = agentcore.ContentList{agentcore.NewTextContent(text)}
		final := withText
		final.StopReason = agentcore.StopReasonError
		return fauxTurn{
			provider.StreamStartEvent{Partial: partial},
			provider.StreamTextEvent{Partial: withText},
			provider.StreamDoneEvent{Message: final},
		}
	}
	child := &fauxProvider{
		name:   "faux-child",
		models: []provider.Model{{Provider: "faux-child", ID: "child"}},
		turns:  []fauxTurn{errTurn("child exploded")},
	}
	tool := NewTaskTool(func() RunConfig {
		return RunConfig{
			LoopConfig: LoopConfig{Model: "child", Stream: provider.StreamFnFromProvider(child)},
			Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
		}
	}, nil, jobs)
	ctx := agentcore.WithSessionID(context.Background(), "parent-session-bg")

	if _, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"boom","mode":"async"}`), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	job := mustJob(t, jobs, "job-1")
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
		t.Fatal("failing child never finished")
	}
	msgs := jobs.FollowUpMessages(context.Background(), nil)
	if len(msgs) != 1 {
		t.Fatalf("follow-up messages = %d, want 1", len(msgs))
	}
	content := agentcore.ContentToText(msgs[0].(agentcore.UserMessage).Content)
	if !strings.Contains(content, "status: failed") || !strings.Contains(content, "child exploded") {
		t.Errorf("failure not reported:\n%s", content)
	}
}

// TestBackgroundRejectedWithoutRegistry verifies a run without a job registry
// refuses async calls with an explanatory error instead of silently blocking.
func TestBackgroundRejectedWithoutRegistry(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	tool := NewTaskTool(func() RunConfig { return RunConfig{} }, nil, nil)
	_, err := tool.Execute(context.Background(), "c", json.RawMessage(`{"prompt":"x","mode":"async"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "is not configured for this run") {
		t.Fatalf("err = %v, want an async-not-configured error", err)
	}
}

// TestTaskOutputTool verifies the model's handle on jobs: listing, status while
// running, waiting, and reading the final report.
func TestTaskOutputTool(t *testing.T) {
	child := newGatedProvider("output report")
	tool, jobs, ctx := backgroundTool(t, child)
	out := &TaskOutputTool{Jobs: jobs}

	if _, err := out.Execute(ctx, "c", json.RawMessage(`{}`), nil); err == nil {
		t.Error("listing with no jobs should report that none were started")
	}
	if _, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"bg","mode":"async"}`), nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	<-child.started

	// Listing shows the running job.
	res, err := out.Execute(ctx, "c", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "job-1") || !strings.Contains(got, "running") {
		t.Errorf("list output = %q", got)
	}

	// A status read while running says so and does not block.
	res, err = out.Execute(ctx, "c", json.RawMessage(`{"job_id":"job-1"}`), nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "still running") {
		t.Errorf("status output = %q", got)
	}

	// wait blocks until the child finishes and returns the report.
	close(child.release)
	res, err = out.Execute(ctx, "c", json.RawMessage(`{"job_id":"job-1","wait":true}`), nil)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := agentcore.ContentToText(res.Content); !strings.Contains(got, "output report") {
		t.Errorf("wait output = %q", got)
	}

	// Unknown job is a clear error.
	if _, err := out.Execute(ctx, "c", json.RawMessage(`{"job_id":"job-404"}`), nil); err == nil {
		t.Error("unknown job should error")
	}
}

// TestLoopDeliversBackgroundReport drives the real loop: the parent starts a
// background child, ends its turn, and the follow-up seam feeds the child's report
// back as a new user turn that the parent then answers.
func TestLoopDeliversBackgroundReport(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	child := &fauxProvider{
		name:   "faux-child",
		models: []provider.Model{{Provider: "faux-child", ID: "child"}},
		turns:  []fauxTurn{textTurn("child says hi")},
	}
	jobs := NewSubAgentJobs()
	taskTool := NewTaskTool(func() RunConfig {
		return RunConfig{
			LoopConfig: LoopConfig{Model: "child", Stream: provider.StreamFnFromProvider(child)},
			Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
		}
	}, nil, jobs)

	parent := &fauxProvider{
		name:   "faux-parent",
		models: []provider.Model{{Provider: "faux-parent", ID: "parent"}},
		turns: []fauxTurn{
			toolCallTurn("call-1", "task", `{"prompt":"delegate this","mode":"async"}`),
			textTurn("all done"),
		},
	}
	cfg := newFauxRunCfg(parent, taskTool)
	cfg.GetFollowUpMessages = jobs.FollowUpMessages
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("start")}},
	}}
	cfg.SessionID = "parent-session-loop"

	_, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	// The run must have continued past the background launch: a follow-up user turn
	// carries the child's report.
	var followUp string
	for _, m := range msgs {
		if u, ok := m.(agentcore.UserMessage); ok {
			text := agentcore.ContentToText(u.Content)
			if strings.Contains(text, "background sub-agent you started has finished") {
				followUp = text
			}
		}
	}
	if followUp == "" {
		t.Fatalf("no follow-up message delivered; messages: %+v", msgs)
	}
	if !strings.Contains(followUp, "child says hi") {
		t.Errorf("follow-up missing the child's report:\n%s", followUp)
	}
	if parent.callCount() < 3 {
		t.Errorf("parent provider called %d times, want >= 3 (launch, follow-up turn, final)", parent.callCount())
	}
}

// mustJob fails the test when the job is missing.
func mustJob(t *testing.T, jobs *SubAgentJobs, id string) *subAgentJob {
	t.Helper()
	job, ok := jobs.Get(id)
	if !ok {
		t.Fatalf("job %q not found", id)
	}
	return job
}

// TestTaskOutputWaitsForBatch verifies task_output job_ids + wait is a fan-in
// barrier: it blocks until every job in the batch settles, then returns all their
// reports in one result instead of one follow-up turn per child.
func TestTaskOutputWaitsForBatch(t *testing.T) {
	child := newGatedProvider("batch report")
	tool, jobs, ctx := backgroundTool(t, child)
	out := &TaskOutputTool{Jobs: jobs}
	for _, callID := range []string{"call-1", "call-2"} {
		if _, err := tool.Execute(ctx, callID, json.RawMessage(`{"prompt":"work","mode":"async"}`), nil); err != nil {
			t.Fatalf("launch %s: %v", callID, err)
		}
	}
	<-child.started

	type outcome struct {
		res agentcore.AgentToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := out.Execute(ctx, "c", json.RawMessage(`{"job_ids":["job-1","job-2"],"wait":true}`), nil)
		done <- outcome{res: res, err: err}
	}()
	select {
	case got := <-done:
		t.Fatalf("batch wait returned before the children finished: err=%v text=%q", got.err, agentcore.ContentToText(got.res.Content))
	case <-time.After(100 * time.Millisecond):
	}

	close(child.release)
	var got outcome
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("batch wait did not return after the children finished")
	}
	if got.err != nil {
		t.Fatalf("batch wait: %v", got.err)
	}
	text := agentcore.ContentToText(got.res.Content)
	for _, want := range []string{"batch of 2", "2 done", "job-1", "job-2", "batch report"} {
		if !strings.Contains(text, want) {
			t.Errorf("batch result missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "still running") {
		t.Errorf("a settled batch must not report a running job:\n%s", text)
	}
	// The model already has the reports in this tool result, so the follow-up seam
	// must not inject them again as user turns.
	if msgs := jobs.FollowUpMessages(context.Background(), nil); len(msgs) != 0 {
		t.Errorf("collected batch was re-delivered as follow-ups: %+v", msgs)
	}
}

// TestLoopBatchBarrierNoDuplicateDelivery drives the real loop: the parent fans out
// two async children, awaits them with one task_output batch, and answers. The
// reports must reach the model once (in the tool result), not again as follow-up
// turns - that duplication is exactly what the batch barrier exists to avoid.
func TestLoopBatchBarrierNoDuplicateDelivery(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	child := &fauxProvider{
		name:   "faux-child",
		models: []provider.Model{{Provider: "faux-child", ID: "child"}},
		turns:  []fauxTurn{textTurn("report one"), textTurn("report two")},
	}
	jobs := NewSubAgentJobs()
	taskTool := jobToolOver(child, jobs)
	parent := &fauxProvider{
		name:   "faux-parent",
		models: []provider.Model{{Provider: "faux-parent", ID: "parent"}},
		turns: []fauxTurn{
			{
				provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}},
				provider.StreamToolCallEvent{Partial: agentcore.AssistantMessage{
					RoleField: agentcore.RoleAssistant,
					Content: agentcore.ContentList{
						agentcore.NewToolCallContent("call-1", "task", json.RawMessage(`{"prompt":"a","mode":"async"}`)),
						agentcore.NewToolCallContent("call-2", "task", json.RawMessage(`{"prompt":"b","mode":"async"}`)),
						agentcore.NewToolCallContent("call-3", "task_output", json.RawMessage(`{"job_ids":["job-1","job-2"],"wait":true}`)),
					},
				}},
				provider.StreamDoneEvent{Message: agentcore.AssistantMessage{
					RoleField:  agentcore.RoleAssistant,
					StopReason: agentcore.StopReasonToolUse,
					Content: agentcore.ContentList{
						agentcore.NewToolCallContent("call-1", "task", json.RawMessage(`{"prompt":"a","mode":"async"}`)),
						agentcore.NewToolCallContent("call-2", "task", json.RawMessage(`{"prompt":"b","mode":"async"}`)),
						agentcore.NewToolCallContent("call-3", "task_output", json.RawMessage(`{"job_ids":["job-1","job-2"],"wait":true}`)),
					},
				}},
			},
			textTurn("both reports incorporated"),
		},
	}
	cfg := newFauxRunCfg(parent, taskTool, &TaskOutputTool{Jobs: jobs})
	cfg.GetFollowUpMessages = jobs.FollowUpMessages
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("fan out")}},
	}}
	cfg.SessionID = "parent-session-barrier"

	_, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	batchSeen := false
	for _, m := range msgs {
		u, ok := m.(agentcore.UserMessage)
		if !ok {
			continue
		}
		text := agentcore.ContentToText(u.Content)
		if strings.Contains(text, "background sub-agent you started has finished") {
			t.Errorf("collected reports were re-delivered as a follow-up:\n%s", text)
		}
	}
	for _, m := range msgs {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			text := agentcore.ContentToText(tr.Content)
			if strings.Contains(text, "batch of 2") && strings.Contains(text, "report one") && strings.Contains(text, "report two") {
				batchSeen = true
			}
		}
	}
	if !batchSeen {
		t.Fatalf("no tool result carried the collected batch; messages: %+v", msgs)
	}
	if got := parent.callCount(); got != 2 {
		t.Errorf("parent provider called %d times, want 2 (fan-out, final): the barrier must not cost extra turns", got)
	}
}

// TestTaskOutputWaitAll verifies wait_all waits for every job running at the time
// of the call and returns them all - the barrier a model uses after a fan-out when
// it does not want to track individual job ids.
func TestTaskOutputWaitAll(t *testing.T) {
	child := newGatedProvider("all report")
	tool, jobs, ctx := backgroundTool(t, child)
	out := &TaskOutputTool{Jobs: jobs}
	for _, callID := range []string{"call-1", "call-2"} {
		if _, err := tool.Execute(ctx, callID, json.RawMessage(`{"prompt":"work","mode":"async"}`), nil); err != nil {
			t.Fatalf("launch %s: %v", callID, err)
		}
	}
	<-child.started

	done := make(chan agentcore.AgentToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := out.Execute(ctx, "c", json.RawMessage(`{"wait_all":true}`), nil)
		if err != nil {
			errCh <- err
			return
		}
		done <- res
	}()
	select {
	case res := <-done:
		t.Fatalf("wait_all returned before the children finished: %q", agentcore.ContentToText(res.Content))
	case err := <-errCh:
		t.Fatalf("wait_all: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(child.release)
	select {
	case res := <-done:
		text := agentcore.ContentToText(res.Content)
		if !strings.Contains(text, "batch of 2") || !strings.Contains(text, "2 done") || !strings.Contains(text, "all report") {
			t.Errorf("wait_all result = %q", text)
		}
	case err := <-errCh:
		t.Fatalf("wait_all: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wait_all did not return after the children finished")
	}
}

// TestTaskOutputBatchReportsFailureInline verifies one failed job in a batch does
// not turn the whole call into an error: the model gets the successful reports and
// the failure side by side.
func TestTaskOutputBatchReportsFailureInline(t *testing.T) {
	t.Setenv("PIGO_HOME", t.TempDir())
	jobs := NewSubAgentJobs()
	okTool := jobToolOver(&fauxProvider{
		name:   "faux-ok",
		models: []provider.Model{{Provider: "faux-ok", ID: "child"}},
		turns:  []fauxTurn{textTurn("good report")},
	}, jobs)
	errPartial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
	errWithText := errPartial
	errWithText.Content = agentcore.ContentList{agentcore.NewTextContent("child exploded")}
	errFinal := errWithText
	errFinal.StopReason = agentcore.StopReasonError
	failTool := jobToolOver(&fauxProvider{
		name:   "faux-fail",
		models: []provider.Model{{Provider: "faux-fail", ID: "child"}},
		turns: []fauxTurn{{
			provider.StreamStartEvent{Partial: errPartial},
			provider.StreamTextEvent{Partial: errWithText},
			provider.StreamDoneEvent{Message: errFinal},
		}},
	}, jobs)
	ctx := agentcore.WithSessionID(context.Background(), "parent-session-batch")
	if _, err := okTool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"good","mode":"async"}`), nil); err != nil {
		t.Fatalf("launch ok: %v", err)
	}
	if _, err := failTool.Execute(ctx, "call-2", json.RawMessage(`{"prompt":"bad","mode":"async"}`), nil); err != nil {
		t.Fatalf("launch fail: %v", err)
	}
	out := &TaskOutputTool{Jobs: jobs}
	res, err := out.Execute(ctx, "c", json.RawMessage(`{"job_ids":["job-1","job-2"],"wait":true}`), nil)
	if err != nil {
		t.Fatalf("batch with one failure must not be a tool error: %v", err)
	}
	text := agentcore.ContentToText(res.Content)
	for _, want := range []string{"1 done", "1 failed", "good report", "error:", "child exploded"} {
		if !strings.Contains(text, want) {
			t.Errorf("batch result missing %q:\n%s", want, text)
		}
	}
}

// TestTaskOutputBatchSelectorConflicts verifies the batch selectors are mutually
// exclusive and that an unknown id in a batch is named.
func TestTaskOutputBatchSelectorConflicts(t *testing.T) {
	child := newGatedProvider("report")
	tool, jobs, ctx := backgroundTool(t, child)
	out := &TaskOutputTool{Jobs: jobs}
	if _, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"work","mode":"async"}`), nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	cases := []struct{ args, want string }{
		{`{"job_id":"job-1","job_ids":["job-1"]}`, "job_id or job_ids"},
		{`{"wait_all":true,"job_ids":["job-1"]}`, "wait_all"},
		{`{"job_ids":["job-404"],"wait":true}`, "unknown job"},
		{`{"wait":true}`, "wait needs"},
		{`{"job_id":"job-404"}`, "unknown job"},
	}
	for _, c := range cases {
		if _, err := out.Execute(ctx, "c", json.RawMessage(c.args), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Execute(%s) err = %v, want one containing %q", c.args, err, c.want)
		}
	}
}

// TestTaskOutputBatchWaitCancelled verifies a cancelled run stops a batch wait
// instead of hanging on children that were cancelled with it.
func TestTaskOutputBatchWaitCancelled(t *testing.T) {
	child := newGatedProvider("report")
	tool, jobs, ctx := backgroundTool(t, child)
	out := &TaskOutputTool{Jobs: jobs}
	if _, err := tool.Execute(ctx, "call-1", json.RawMessage(`{"prompt":"work","mode":"async"}`), nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	<-child.started

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := out.Execute(cancelled, "c", json.RawMessage(`{"job_ids":["job-1"],"wait":true}`), nil)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want a cancelled-while-waiting error", err)
	}
}

// jobToolOver builds a task tool over a provider, sharing the job registry, so a
// test can mix a successful and a failing async child in one batch.
func jobToolOver(p provider.Provider, jobs *SubAgentJobs) *SubAgentTool {
	return NewTaskTool(func() RunConfig {
		return RunConfig{
			LoopConfig: LoopConfig{Model: "child", Stream: provider.StreamFnFromProvider(p)},
			Batch:      agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: agenttool.NewToolRegistry()}},
		}
	}, nil, jobs)
}
