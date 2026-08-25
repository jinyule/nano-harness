// Package session defines the durable event vocabulary and replay surface.
package session

import "encoding/json"

// Header is the immutable identity and delegation metadata of one session.
type Header struct {
	SessionID       string `json:"session_id"`
	CompositionID   string `json:"composition_id"`
	CreatedAtUnixMS int64  `json:"created_at_unix_ms"`
	Cwd             string `json:"cwd"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	DelegationDepth int    `json:"delegation_depth,omitempty"`
}

const (
	// FormatVersion is the only JSONL session format understood by this build.
	FormatVersion = 2
	// MaxTextBytes bounds one text block or tool result.
	MaxTextBytes = 256 << 10
	// MaxArgumentsBytes bounds one serialized tool argument object.
	MaxArgumentsBytes = 128 << 10
	// MaxImageBytes bounds one normalized decoded image.
	MaxImageBytes = 4 << 20
	// MaxContentBlocks bounds one message or summary.
	MaxContentBlocks = 32
)

// RecordType identifies one durable harness fact.
type RecordType string

const (
	// RecordTurnStart opens one durable agent turn.
	RecordTurnStart RecordType = "turn/start"
	// RecordUserMessage commits model-visible user input within an active turn.
	RecordUserMessage RecordType = "user/message"
	// RecordStepStart opens one model request and tool-execution step.
	RecordStepStart RecordType = "step/start"
	// RecordRequestHeader freezes the provider-neutral request identity and schemas.
	RecordRequestHeader RecordType = "request/header"
	// RecordAssistantChunk commits one ordered provider stream delta.
	RecordAssistantChunk RecordType = "assistant/chunk"
	// RecordAssistantMessage commits the assembled assistant completion.
	RecordAssistantMessage RecordType = "assistant/message"
	// RecordToolCall commits a provider-requested tool invocation before execution.
	RecordToolCall RecordType = "tool/call"
	// RecordApprovalAsked commits the reason for one privileged tool decision.
	RecordApprovalAsked RecordType = "approval/asked"
	// RecordApprovalDecided commits the stable outcome of an approval question.
	RecordApprovalDecided RecordType = "approval/decided"
	// RecordApprovalPolicy commits the current per-session prompt policy.
	RecordApprovalPolicy RecordType = "approval/policy"
	// RecordToolResult commits the unique model-visible result for a tool call.
	RecordToolResult RecordType = "tool/result"
	// RecordRetry commits a provider retry decision before its delay.
	RecordRetry RecordType = "llm/retry"
	// RecordRetryStarted commits the resumption of a previously delayed attempt.
	RecordRetryStarted RecordType = "llm/retry-started"
	// RecordCompactionStart opens an append-only surface replacement transaction.
	RecordCompactionStart RecordType = "compaction/start"
	// RecordCompactionSummary commits the replacement summary and shadowed sequences.
	RecordCompactionSummary RecordType = "compaction/summary"
	// RecordCompactionEnd closes a successful or failed compaction transaction.
	RecordCompactionEnd RecordType = "compaction/end"
	// RecordSubagentDescriptor commits cold-resume metadata for a delegated agent.
	RecordSubagentDescriptor RecordType = "subagent/descriptor"
	// RecordStepEnd closes an active step after all calls and approvals settle.
	RecordStepEnd RecordType = "step/end"
	// RecordTurnEnd closes an active turn with a stable outcome.
	RecordTurnEnd RecordType = "turn/end"
)

// TurnOutcome is the stable reason recorded when a turn closes.
type TurnOutcome string

const (
	// OutcomeCompleted identifies a turn that reached a provider completion without tool calls.
	OutcomeCompleted TurnOutcome = "completed"
	// OutcomeCanceled identifies a turn stopped through context cancellation.
	OutcomeCanceled TurnOutcome = "canceled"
	// OutcomeError identifies a turn stopped by a non-cancellation failure.
	OutcomeError TurnOutcome = "error"
	// OutcomeStepLimit identifies a turn that consumed its configured step bound.
	OutcomeStepLimit TurnOutcome = "step_limit"
	// OutcomeInterrupted identifies an incomplete turn repaired during resume.
	OutcomeInterrupted TurnOutcome = "interrupted"
)

// MessageRole is a provider-neutral conversation role.
type MessageRole string

const (
	// RoleUser identifies model input attributed to a user-side source.
	RoleUser MessageRole = "user"
	// RoleAssistant identifies model output attributed to a provider.
	RoleAssistant MessageRole = "assistant"
)

// ContentType identifies one model-visible block.
type ContentType string

const (
	// ContentText identifies one textual message block.
	ContentText ContentType = "text"
	// ContentImage identifies one normalized replayable image block.
	ContentImage ContentType = "image"
)

// Image is a normalized, replayable image attachment. Data is standard base64.
type Image struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	SHA256    string `json:"sha256"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// ContentBlock is either text or an inline normalized image.
type ContentBlock struct {
	Type  ContentType `json:"type"`
	Text  string      `json:"text,omitempty"`
	Image *Image      `json:"image,omitempty"`
}

// MessageSource records provenance without granting authority.
type MessageSource struct {
	Kind   string `json:"kind"`
	Plugin string `json:"plugin,omitempty"`
}

// Message is one replayable user or assistant message.
type Message struct {
	Role    MessageRole    `json:"role"`
	Content []ContentBlock `json:"content"`
	Source  MessageSource  `json:"source"`
}

// ToolCall is the provider-neutral function call committed before execution.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the single model-visible outcome of a committed tool call.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

// ToolDefinition is the exact schema frozen into one request header.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// RequestHeader makes a provider call reconstructable from the log.
type RequestHeader struct {
	Provider      string           `json:"provider"`
	Model         string           `json:"model"`
	System        string           `json:"system"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
	ContextWindow int              `json:"context_window,omitempty"`
}

// ChunkKind distinguishes streamed assistant content.
type ChunkKind string

const (
	// ChunkText identifies streamed assistant-visible text.
	ChunkText ChunkKind = "text"
	// ChunkReasoning identifies streamed reasoning text presented separately by the UI.
	ChunkReasoning ChunkKind = "reasoning"
	// ChunkTool identifies streamed tool identity or argument deltas.
	ChunkTool ChunkKind = "tool"
)

// AssistantChunk is a durable provider-neutral stream delta.
type AssistantChunk struct {
	Kind      ChunkKind `json:"kind"`
	Text      string    `json:"text,omitempty"`
	Index     int       `json:"index,omitempty"`
	CallID    string    `json:"call_id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Arguments string    `json:"arguments,omitempty"`
}

// TokenUsage is provider-reported usage for one completed request.
type TokenUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// RetryData records one provider-routed retry decision.
type RetryData struct {
	ID         string `json:"id"`
	Provider   string `json:"provider,omitempty"`
	PolicyKey  string `json:"policy_key,omitempty"`
	Attempt    int    `json:"attempt"`
	MaxRetries int    `json:"max_retries,omitempty"`
	DelayMS    int64  `json:"delay_ms,omitempty"`
	Failure    string `json:"failure,omitempty"`
}

// ApprovalOutcome is a fail-closed one-shot decision.
type ApprovalOutcome string

const (
	// ApprovalAllowedOnce authorizes exactly the pending tool execution.
	ApprovalAllowedOnce ApprovalOutcome = "allowed-once"
	// ApprovalRejected records an explicit operator or policy denial.
	ApprovalRejected ApprovalOutcome = "rejected"
	// ApprovalCancelled records cancellation before an authorization decision.
	ApprovalCancelled ApprovalOutcome = "cancelled"
	// ApprovalUnavailable records the absence of a valid local decision surface.
	ApprovalUnavailable ApprovalOutcome = "unavailable"
)

// ApprovalPolicy is the per-session prompt policy.
type ApprovalPolicy string

const (
	// ApprovalAsk requires an available local broker for privileged tools.
	ApprovalAsk ApprovalPolicy = "ask"
	// ApprovalNever rejects every privileged tool without presenting a broker question.
	ApprovalNever ApprovalPolicy = "never"
)

// ApprovalData pairs a question and its decision.
type ApprovalData struct {
	ID       string          `json:"id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Outcome  ApprovalOutcome `json:"outcome,omitempty"`
	Policy   ApprovalPolicy  `json:"policy,omitempty"`
	Source   string          `json:"source,omitempty"`
}

// CompactionData describes one log-preserving surface replacement.
type CompactionData struct {
	ID                 string         `json:"id"`
	ShadowedSeqs       []uint64       `json:"shadowed_seqs,omitempty"`
	ShadowedTokenCount int            `json:"shadowed_token_count,omitempty"`
	Summary            []ContentBlock `json:"summary,omitempty"`
	Provider           string         `json:"provider,omitempty"`
	Model              string         `json:"model,omitempty"`
	Error              string         `json:"error,omitempty"`
}

// SubagentDescriptor is the durable identity needed for cold resume.
type SubagentDescriptor struct {
	Version  int      `json:"version"`
	Provider string   `json:"provider"`
	Mode     string   `json:"mode"`
	Label    string   `json:"label"`
	Persona  string   `json:"persona,omitempty"`
	Tools    []string `json:"tools,omitempty"`
}

// Record is an unsequenced fact. A store assigns Sequence at commit.
type Record struct {
	Type       RecordType          `json:"type"`
	Turn       uint64              `json:"turn,omitempty"`
	Step       uint64              `json:"step,omitempty"`
	Message    *Message            `json:"message,omitempty"`
	Chunk      *AssistantChunk     `json:"chunk,omitempty"`
	Call       *ToolCall           `json:"call,omitempty"`
	Result     *ToolResult         `json:"result,omitempty"`
	Header     *RequestHeader      `json:"header,omitempty"`
	Usage      *TokenUsage         `json:"usage,omitempty"`
	Retry      *RetryData          `json:"retry,omitempty"`
	Approval   *ApprovalData       `json:"approval,omitempty"`
	Compaction *CompactionData     `json:"compaction,omitempty"`
	Subagent   *SubagentDescriptor `json:"subagent,omitempty"`
	Outcome    TurnOutcome         `json:"outcome,omitempty"`
}

// Event is one committed fact with a contiguous sequence.
type Event struct {
	Sequence uint64 `json:"seq"`
	Record   Record `json:"record"`
}

// SurfaceNode is one current model-visible node after replacements are folded.
type SurfaceNode struct {
	Sequence uint64      `json:"seq"`
	Message  *Message    `json:"message,omitempty"`
	Call     *ToolCall   `json:"call,omitempty"`
	Result   *ToolResult `json:"result,omitempty"`
}
