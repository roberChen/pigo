package agentcore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message roles, matching pi's wire format.
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"
	// RoleCompaction marks a compaction checkpoint persisted inline in the
	// message list: it replaces the history summarized before it (pi's
	// "compactionSummary"). It is not sent to the model verbatim; the LLM
	// conversion turns it into a user text block.
	RoleCompaction = "compaction"
)

// Message is the sealed interface implemented by the three message roles.
// AgentMessage (the loop's message abstraction) is simply Message: custom
// message kinds implement the same interface and convertToLlm filters out any
// that are not LLM-bound. This deliberately replaces pi's declaration merging,
// which has no Go equivalent.
type Message interface {
	isMessage()
	// Role returns the discriminant ("user" | "assistant" | "toolResult").
	Role() string
}

// AgentMessage is the loop-level message type. It is the same as Message; the
// alias documents intent at call sites that deal with the loop rather than raw
// LLM messages.
type AgentMessage = Message

// Usage reports token accounting for an assistant response.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// UserMessage is input from the user. Content is restricted at construction to
// text/image blocks (runtime constraint, not a separate interface).
//
// Origin distinguishes a turn the human typed from one the harness injected
// (see UserMessageOrigin*). It is persisted so a resumed session renders an
// injected turn as what it is instead of replaying it behind a "you" gutter: a
// finished background sub-agent's report is delivered as a user-role turn so the
// model sees it as input, but it is not something the human said.
type UserMessage struct {
	RoleField string      `json:"role"`
	Content   ContentList `json:"content"`
	Timestamp int64       `json:"timestamp"`
	Origin    string      `json:"origin,omitempty"`
}

func (UserMessage) isMessage()     {}
func (m UserMessage) Role() string { return RoleUser }

// UserMessageOrigin values for UserMessage.Origin. The empty value means the
// turn came from the human.
const (
	// UserMessageOriginSubAgentReport marks a finished background sub-agent's
	// report injected onto the follow-up seam.
	UserMessageOriginSubAgentReport = "subagent_report"
	// UserMessageOriginHookGuidance marks the guidance a Stop hook feeds back when
	// it blocks a run from ending. The model treats it as input, but it is the
	// harness's words, not the human's.
	UserMessageOriginHookGuidance = "hook_guidance"
	// UserMessageOriginGoalContinuation marks the autonomous-mode nudge injected
	// each turn to keep a /goal run going.
	UserMessageOriginGoalContinuation = "goal_continuation"
)

// IsSynthetic reports whether the harness rather than the human produced this
// user turn. Callers use it to render the turn as a note instead of a prompt.
func (m UserMessage) IsSynthetic() bool { return m.Origin != "" }

// AssistantMessage is a model response. Content may hold text/thinking/toolCall
// blocks. StopReason follows pi's set (end_turn/tool_use/length/error/aborted).
type AssistantMessage struct {
	RoleField    string      `json:"role"`
	Content      ContentList `json:"content"`
	API          string      `json:"api,omitempty"`
	Provider     string      `json:"provider,omitempty"`
	Model        string      `json:"model,omitempty"`
	Usage        *Usage      `json:"usage,omitempty"`
	StopReason   string      `json:"stopReason,omitempty"`
	ErrorMessage string      `json:"errorMessage,omitempty"`
	Timestamp    int64       `json:"timestamp"`

	// Optional diagnostics, kept for cross-provider replay/observability.
	ResponseModel string `json:"responseModel,omitempty"`
	ResponseID    string `json:"responseId,omitempty"`
}

func (AssistantMessage) isMessage()     {}
func (m AssistantMessage) Role() string { return RoleAssistant }

// ToolCalls returns the tool call blocks in this assistant message, in order.
func (m AssistantMessage) ToolCalls() []ToolCallContent {
	var calls []ToolCallContent
	for _, c := range m.Content {
		if tc, ok := c.(ToolCallContent); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// ToolResultMessage carries the outcome of executing a single tool call.
// Content is restricted to text/image blocks at construction.
type ToolResultMessage struct {
	RoleField  string      `json:"role"`
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Content    ContentList `json:"content"`
	Details    any         `json:"details,omitempty"`
	IsError    bool        `json:"isError"`
	Timestamp  int64       `json:"timestamp"`
}

func (ToolResultMessage) isMessage()     {}
func (m ToolResultMessage) Role() string { return RoleToolResult }

// CompactionMessage is a summarization checkpoint persisted inline in the
// message list. It stands in for the history compacted before it: Summary is
// the structured checkpoint text and TokensBefore records the estimated context
// size at compaction time (for observability). Details optionally holds the
// file operations extracted from the compacted range. Mirrors pi's
// CompactionSummaryMessage + CompactionEntry.
type CompactionMessage struct {
	RoleField    string `json:"role"`
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore,omitempty"`
	// CoveredMessages is how many messages of the context this checkpoint
	// summarizes (the compaction cut point). Persistence uses it to attach the
	// checkpoint to the session tree at the last summarized entry instead of
	// rewriting the session, so the conversation it summarizes stays on disk.
	// Zero means "not recorded" (an older file, or a checkpoint built without a
	// cut point) and makes persistence fall back to a linear rewrite.
	CoveredMessages int `json:"coveredMessages,omitempty"`
	// Details is opaque at this layer (the compaction package owns its shape);
	// kept as raw JSON so agentcore stays free of a compaction dependency.
	Details json.RawMessage `json:"details,omitempty"`
	// UserInstructions is the rolling ledger of what the human asked for. It is
	// the anti-drift half of a checkpoint: Summary is a model's account of the
	// work, which degrades turn over turn, while this section keeps a cumulative
	// digest plus the verbatim requests of the window the checkpoint covers.
	// Nil on a checkpoint produced before the section existed, or when a
	// checkpoint carried no user instructions. See UserInstructionBlock.
	UserInstructions *UserInstructionBlock `json:"userInstructions,omitempty"`
	Timestamp        int64                 `json:"timestamp"`
}

// UserInstructionBlock is the user-instruction section of a compaction
// checkpoint: a rolling summary of every instruction given so far plus the
// verbatim instructions of the compaction window that produced this checkpoint.
//
// The two halves age differently on purpose. Summary is rewritten by the
// summarization call each time (it absorbs the previous Summary and the previous
// verbatim list), so it is cumulative but paraphrased. Verbatim is replaced
// wholesale with the current window's requests and never passes through the
// model, so the most recent round of instructions is always exactly what the
// human typed. That split is what keeps a long session from slowly drifting away
// from the original requirements.
type UserInstructionBlock struct {
	// Summary is the rolling digest: everything the user asked for across the
	// whole session, including windows this checkpoint no longer quotes verbatim.
	Summary string `json:"summary,omitempty"`
	// Verbatim holds the user's own words from the compacted window, in
	// chronological order, each already budget-trimmed.
	Verbatim []string `json:"verbatim,omitempty"`
}

// IsEmpty reports whether the block carries nothing worth rendering.
func (b *UserInstructionBlock) IsEmpty() bool {
	return b == nil || (strings.TrimSpace(b.Summary) == "" && len(b.Verbatim) == 0)
}

// Section headings of the rendered user-instruction section. They are exported
// as constants because tests and the summary prompt refer to them literally, and
// because the model must be able to recognize its own previous output.
const (
	// UserInstructionsHeading leads the section inside a rendered summary.
	UserInstructionsHeading = "## User Instructions"
	// UserInstructionsSummaryLabel heads the rolling digest half.
	UserInstructionsSummaryLabel = "### Standing instructions"
	// UserInstructionsVerbatimLabel heads the verbatim half.
	UserInstructionsVerbatimLabel = "### Verbatim requests (this window)"
)

// Render returns the user-instruction section as Markdown: a heading, the
// rolling digest, and the verbatim list. It returns "" for an empty block so
// callers can append it unconditionally.
func (b *UserInstructionBlock) Render() string {
	if b.IsEmpty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(UserInstructionsHeading)
	sb.WriteString("\n")
	if s := strings.TrimSpace(b.Summary); s != "" {
		sb.WriteString(UserInstructionsSummaryLabel)
		sb.WriteString("\n")
		sb.WriteString(s)
		sb.WriteString("\n")
	}
	if len(b.Verbatim) > 0 {
		sb.WriteString(UserInstructionsVerbatimLabel)
		sb.WriteString("\n")
		for _, v := range b.Verbatim {
			sb.WriteString("- ")
			sb.WriteString(v)
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Note is a one-line, human-facing description of the section, or "" when there
// is none. The transcript views print it beside the compaction marker so a
// resumed session says out loud that the user's requirements survived the
// collapse — a reader otherwise has no way to tell a plain summary from a
// summary that still carries the ledger.
func (b *UserInstructionBlock) Note() string {
	if b.IsEmpty() {
		return ""
	}
	var parts []string
	if strings.TrimSpace(b.Summary) != "" {
		parts = append(parts, "standing instructions")
	}
	switch n := len(b.Verbatim); n {
	case 0:
	case 1:
		parts = append(parts, "1 verbatim request")
	default:
		parts = append(parts, fmt.Sprintf("%d verbatim requests", n))
	}
	return "user instructions preserved (" + strings.Join(parts, " + ") + ")"
}

// Text renders the checkpoint the way the model sees it: the technical summary
// followed by the user-instruction section. It is the single rendering used for
// the LLM request, the checkpoint file and the transcript views, so no consumer
// can accidentally drop a half of the checkpoint.
func (m CompactionMessage) Text() string {
	section := m.UserInstructions.Render()
	if section == "" {
		return m.Summary
	}
	if strings.TrimSpace(m.Summary) == "" {
		return section
	}
	return m.Summary + "\n\n" + section
}

// ExtractUserInstructionsSection is the inverse of Render: it splits a rendered
// checkpoint body into its technical summary and the parsed instruction block.
//
// It is what makes the section survive the on-disk round trip through a plain
// Markdown checkpoint: the loader hands the whole body here instead of trusting
// frontmatter, so the section stays greppable and editable by hand. A body with
// no (or an unrecognizable) section yields the body unchanged and a nil block,
// which is exactly how checkpoints written before the section existed behave.
func ExtractUserInstructionsSection(body string) (string, *UserInstructionBlock) {
	idx := strings.LastIndex(body, UserInstructionsHeading)
	if idx < 0 || (idx > 0 && body[idx-1] != '\n') {
		return body, nil
	}
	block, ok := parseUserInstructionsSection(body[idx+len(UserInstructionsHeading):])
	if !ok {
		return body, nil
	}
	return strings.TrimRight(body[:idx], "\n"), block
}

// parseUserInstructionsSection parses the text after the heading. ok is false
// when neither label is present, which means the heading was prose rather than a
// real section and the caller must treat the body as summary-only.
func parseUserInstructionsSection(rest string) (*UserInstructionBlock, bool) {
	block := &UserInstructionBlock{}
	mode := ""
	started := false
	for _, raw := range strings.Split(rest, "\n") {
		line := strings.TrimSpace(raw)
		switch line {
		case UserInstructionsSummaryLabel:
			mode, started = "summary", true
			continue
		case UserInstructionsVerbatimLabel:
			mode, started = "verbatim", true
			continue
		}
		if !started || line == "" {
			continue
		}
		if mode == "verbatim" && strings.HasPrefix(line, "- ") {
			block.Verbatim = append(block.Verbatim, strings.TrimSpace(line[2:]))
			continue
		}
		if mode == "summary" {
			if block.Summary != "" {
				block.Summary += "\n"
			}
			block.Summary += line
			continue
		}
		// A wrapped continuation line of the last verbatim entry.
		if n := len(block.Verbatim); n > 0 {
			block.Verbatim[n-1] += " " + line
		}
	}
	if !started {
		return nil, false
	}
	return block, true
}

func (CompactionMessage) isMessage()     {}
func (m CompactionMessage) Role() string { return RoleCompaction }

// compactionSummaryPrefix / compactionSummarySuffix wrap a compaction summary
// when it is rendered into an LLM user message, matching pi's
// COMPACTION_SUMMARY_PREFIX / COMPACTION_SUMMARY_SUFFIX.
const (
	compactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
)

// AsUserMessage renders a compaction checkpoint as the user text message that
// stands in for the compacted history when building the LLM request. The
// provider encoders call this so a persisted compaction line replays as
// context rather than being dropped. Text() includes the user-instruction
// section, so the model receives the anti-drift ledger alongside the summary.
func (m CompactionMessage) AsUserMessage() UserMessage {
	return UserMessage{
		RoleField: RoleUser,
		Content:   ContentList{NewTextContent(compactionSummaryPrefix + m.Text() + compactionSummarySuffix)},
		Timestamp: m.Timestamp,
	}
}

// StopReason values, matching pi.
const (
	StopReasonEndTurn = "end_turn"
	StopReasonToolUse = "tool_use"
	StopReasonLength  = "length"
	StopReasonError   = "error"
	StopReasonAborted = "aborted"
)

// MessageList is a slice of Message with discriminated JSON (un)marshalling,
// dispatching on the "role" field. Used by AgentContext and session persistence.
type MessageList []Message

// UnmarshalJSON decodes a JSON array of messages, dispatching each element on
// its "role" discriminant.
func (ml *MessageList) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(MessageList, 0, len(raws))
	for i, raw := range raws {
		m, err := decodeMessage(raw)
		if err != nil {
			return fmt.Errorf("message[%d]: %w", i, err)
		}
		out = append(out, m)
	}
	*ml = out
	return nil
}

// decodeMessage peeks at the "role" field and decodes into the matching
// concrete message struct.
func decodeMessage(raw json.RawMessage) (Message, error) {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("peek role: %w", err)
	}
	switch probe.Role {
	case RoleUser:
		var m UserMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case RoleAssistant:
		var m AssistantMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case RoleToolResult:
		var m ToolResultMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case RoleCompaction:
		var m CompactionMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "":
		return nil, fmt.Errorf("missing role discriminant")
	default:
		return nil, fmt.Errorf("unknown role %q", probe.Role)
	}
}
