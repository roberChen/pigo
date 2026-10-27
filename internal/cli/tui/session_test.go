package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// newTestStore opens a session store rooted at a temp dir so persistence/resume
// can be exercised without touching ~/.pigo.
func newTestStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// saveSession writes a linear session with the given messages and returns its id.
func saveSession(t *testing.T, store *session.Store, msgs agentcore.MessageList) string {
	t.Helper()
	now := time.Now().UTC()
	header := session.SessionHeader{
		ID:        session.NewID(now),
		CreatedAt: now,
		UpdatedAt: now,
		Model:     "test-model",
		Provider:  "test-provider",
	}
	if err := store.Save(header, msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return header.ID
}

// TestResumeSeedsTranscript constructs a session with a few messages, resumes it
// through newRunSessionWithStore, seeds a transcript with the returned history,
// and asserts the initial transcript blocks carry those messages (FR-16 resume).
func TestResumeSeedsTranscript(t *testing.T) {
	store := newTestStore(t)
	id := saveSession(t, store, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hello, world")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("hello back")}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("second question")}},
	})

	s, history, err := newRunSessionWithStore(store, Options{ResumeID: id})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history len = %d, want 3", len(history))
	}
	// The persisted cursor must cover the full resumed history so the first new
	// turn appends only fresh messages, not a re-save of history.
	if s.persisted != 3 {
		t.Errorf("persisted = %d, want 3", s.persisted)
	}
	if s.curLeaf == "" {
		t.Error("curLeaf should be the resumed leaf, got empty")
	}

	tr := newTranscript(DefaultTheme())
	seedTranscript(&tr, history)

	wantTexts := []string{"hello, world", "hello back", "second question"}
	if len(tr.blocks) != len(wantTexts) {
		t.Fatalf("transcript blocks = %d, want %d", len(tr.blocks), len(wantTexts))
	}
	for i, want := range wantTexts {
		if tr.blocks[i].text != want {
			t.Errorf("block[%d] = %q, want %q", i, tr.blocks[i].text, want)
		}
	}
}

// TestBuildConfigAssembly asserts the run-config assembly maps the live config
// onto RunConfig without a live provider: the model/provider/thinking/window
// fields flow through, compaction is enabled, and the tool registry is wired.
func TestBuildConfigAssembly(t *testing.T) {
	store := newTestStore(t)
	s, _, err := newRunSessionWithStore(store, Options{
		Model:         "opus-test",
		ProviderName:  "anthropic",
		ThinkingLevel: agentcore.ThinkingLevel("high"),
	})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}

	cfg := s.buildConfig()
	if cfg.Model != "opus-test" {
		t.Errorf("cfg.Model = %q, want opus-test", cfg.Model)
	}
	if cfg.Provider != "anthropic" {
		t.Errorf("cfg.Provider = %q, want anthropic", cfg.Provider)
	}
	if cfg.ThinkingLevel != agentcore.ThinkingLevel("high") {
		t.Errorf("cfg.ThinkingLevel = %q, want high", cfg.ThinkingLevel)
	}
	if cfg.ContextWindow <= 0 {
		t.Errorf("cfg.ContextWindow = %d, want a positive default", cfg.ContextWindow)
	}
	if !cfg.Compaction.Enabled {
		t.Error("cfg.Compaction.Enabled = false, want true (DefaultCompactionSettings)")
	}
	if cfg.Batch.Registry == nil {
		t.Error("cfg.Batch.Registry is nil, want the assembled tool registry")
	}
	if cfg.Stream == nil {
		t.Error("cfg.Stream is nil, want a stream fn derived from the provider")
	}
}

// TestFreshSessionPersists starts a fresh session, appends a turn to the context,
// persists it, and confirms it round-trips back through the store (FR-16 persist).
func TestFreshSessionPersists(t *testing.T) {
	store := newTestStore(t)
	s, history, err := newRunSessionWithStore(store, Options{Model: "m", ProviderName: "p"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}
	if history != nil {
		t.Fatalf("fresh session history = %v, want nil", history)
	}

	s.agentCtx.Messages = append(s.agentCtx.Messages,
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("yo")}},
	)
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if s.persisted != 2 {
		t.Errorf("persisted = %d, want 2", s.persisted)
	}

	_, msgs, err := store.Load(s.header.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("persisted messages = %d, want 2", len(msgs))
	}

	// A second persist with no new messages is a no-op.
	before := s.curLeaf
	if err := s.persist(); err != nil {
		t.Fatalf("persist (no-op): %v", err)
	}
	if s.curLeaf != before {
		t.Errorf("curLeaf changed on no-op persist: %q -> %q", before, s.curLeaf)
	}
}

// TestPersistAfterCompaction reproduces the crash where an automatic compaction
// shrinks agentCtx.Messages below the persisted cursor: an incremental
// Messages[persisted:] would panic with a slice-bounds error. persist() must
// instead re-save the flattened context and reset the cursor to the new length.
func TestPersistAfterCompaction(t *testing.T) {
	store := newTestStore(t)
	s, _, err := newRunSessionWithStore(store, Options{Model: "m", ProviderName: "p"})
	if err != nil {
		t.Fatalf("newRunSessionWithStore: %v", err)
	}

	// Persist a few turns so the cursor advances past what compaction will keep.
	for i := 0; i < 4; i++ {
		s.agentCtx.Messages = append(s.agentCtx.Messages,
			agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("q")}},
			agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("a")}},
		)
	}
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if s.persisted != 8 {
		t.Fatalf("persisted = %d, want 8 before compaction", s.persisted)
	}

	// Simulate the run loop compacting: Messages is rewritten to a shorter
	// summary + tail (here just a 2-message tail), and the loop signalled it via
	// compactionMsg (which sets s.compacted).
	s.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("recent q")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("recent a")}},
	}
	s.compacted = true

	if err := s.persist(); err != nil {
		t.Fatalf("persist after compaction: %v", err)
	}
	if s.compacted {
		t.Error("compacted flag should be cleared after persist")
	}
	if s.persisted != 2 {
		t.Errorf("persisted = %d, want 2 (the compacted length)", s.persisted)
	}

	_, msgs, err := store.Load(s.header.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("persisted messages = %d, want 2 (flattened compacted context)", len(msgs))
	}
}

// TestResumeSeedsToolCards verifies a resumed session rebuilds the same rich tool
// cards a live run shows, instead of the bare "· name" lines it used to draw: the
// card carries the call's decoded arguments, the matching result's response text
// and status, and the reported diff as its own section. A call whose result never
// reached the disk keeps its running look, and a result whose call is missing
// from the replayed history still gets a card so the work stays visible.
func TestResumeSeedsToolCards(t *testing.T) {
	store := newTestStore(t)
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
	id := saveSession(t, store, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("inspect the repo")}},
		agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("Reading it."), agentcore.NewToolCallContent("call-1", "read", []byte(`{"path":"main.go"}`))},
			StopReason: agentcore.StopReasonToolUse,
		},
		agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: "call-1",
			ToolName:   "read",
			Content:    agentcore.ContentList{agentcore.NewTextContent("package main\n\nline two")},
		},
		agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewToolCallContent("call-2", "edit", []byte(`{"path":"main.go"}`))},
			StopReason: agentcore.StopReasonToolUse,
		},
		agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: "call-2",
			ToolName:   "edit",
			Content:    agentcore.ContentList{agentcore.NewTextContent("updated main.go\n" + diff)},
			Details:    map[string]any{"diff": diff},
		},
		agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewToolCallContent("call-3", "bash", []byte(`{"command":"go test ./..."}`))},
			StopReason: agentcore.StopReasonToolUse,
		},
		agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: "call-4",
			ToolName:   "bash",
			IsError:    true,
			Content:    agentcore.ContentList{agentcore.NewTextContent("command failed")},
		},
	})

	_, history, err := store.LoadEntries(id)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	msgs := make(agentcore.MessageList, len(history))
	for i, e := range history {
		msgs[i] = e.Message
	}

	tr := newTranscript(DefaultTheme())
	last := seedTranscript(&tr, msgs)

	cards := map[string]*toolCard{}
	for _, b := range tr.blocks {
		if b.role == roleSystem {
			t.Errorf("seedTranscript should not emit plain tool lines, got system block %q", b.text)
		}
		if b.role == roleTool && b.card != nil {
			cards[b.card.id] = b.card
		}
	}
	if len(cards) != 4 {
		t.Fatalf("seeded %d tool cards, want 4", len(cards))
	}

	read := cards["call-1"]
	if read == nil || read.state != cardSuccess {
		t.Fatalf("call-1 card = %+v, want a successful card", read)
	}
	if got := read.primaryArg(); got != "main.go" {
		t.Errorf("call-1 argument = %q, want main.go", got)
	}
	if len(read.response) != 3 || !strings.Contains(read.response[0].text, "package main") {
		t.Errorf("call-1 response = %+v, want the read output", read.response)
	}

	edit := cards["call-2"]
	if edit == nil || edit.diff != diff {
		t.Fatalf("call-2 diff = %q, want the reported diff", edit.diff)
	}
	if joined := responseText(edit); strings.Contains(joined, "--- a/") {
		t.Errorf("diff should be shown only in its own section, response = %q", joined)
	}

	running := cards["call-3"]
	if running == nil || running.state != cardRunning || len(running.response) != 0 {
		t.Errorf("call-3 card = %+v, want a running card with no response", running)
	}

	orphan := cards["call-4"]
	if orphan == nil || orphan.state != cardWarn {
		t.Fatalf("orphan result card = %+v, want a warn card", orphan)
	}
	if last == nil || last.id != "call-4" {
		t.Errorf("returned last card = %+v, want call-4", last)
	}
}

// responseText joins a card's parsed response nodes back into text.
func responseText(c *toolCard) string {
	var b strings.Builder
	for _, n := range c.response {
		b.WriteString(n.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestResumeSeedsUnparsedToolArguments verifies a persisted malformed tool call
// survives the session round-trip and is restored as a card showing what the
// model actually sent. The session file stores such arguments as a JSON string
// (so persistence cannot fail); decoding must give the raw bytes back, or the
// resumed card (and the next request) would carry a quoted blob.
func TestResumeSeedsUnparsedToolArguments(t *testing.T) {
	store := newTestStore(t)
	const badArgs = `{"todos": []{}}`
	id := saveSession(t, store, agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("plan the work")}},
		agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewToolCallContent("call-1", "todo", json.RawMessage(badArgs))},
			StopReason: agentcore.StopReasonToolUse,
		},
		agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: "call-1",
			ToolName:   "todo",
			IsError:    true,
			Content:    agentcore.ContentList{agentcore.NewTextContent(`Invalid arguments for tool "todo": arguments are not valid JSON`)},
		},
	})

	_, history, err := store.LoadEntries(id)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	msgs := make(agentcore.MessageList, len(history))
	for i, e := range history {
		msgs[i] = e.Message
	}

	tr := newTranscript(DefaultTheme())
	seedTranscript(&tr, msgs)

	var card *toolCard
	for _, b := range tr.blocks {
		if b.role == roleTool && b.card != nil {
			card = b.card
		}
	}
	if card == nil {
		t.Fatal("no tool card was restored")
	}
	if card.rawArgs != badArgs {
		t.Errorf("rawArgs = %q, want the original malformed arguments", card.rawArgs)
	}
	if card.state != cardWarn {
		t.Errorf("state = %v, want warn (the call errored)", card.state)
	}
	out := card.render(DefaultTheme(), 60)
	if !strings.Contains(out, "Input arguments (unparsed)") || !strings.Contains(out, badArgs) {
		t.Errorf("restored card does not show the malformed payload:\n%s", out)
	}
}
