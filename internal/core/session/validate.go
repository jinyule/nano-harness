package session

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalidRecord identifies a session fact that violates the versioned schema.
var ErrInvalidRecord = errors.New("invalid session record")

// Validate checks the complete shape and size of a record.
func (record Record) Validate() error {
	if record.Type == "" {
		return invalid("type is required")
	}
	if record.Turn == 0 && !allowsZeroTurn(record.Type) {
		return invalid("turn must be positive")
	}
	switch record.Type {
	case RecordTurnStart:
		return record.requireBare(false, false)
	case RecordUserMessage:
		return record.requireMessage(RoleUser)
	case RecordStepStart:
		return record.requireBare(true, false)
	case RecordRequestHeader:
		return record.requireHeader()
	case RecordAssistantChunk:
		return record.requireChunk()
	case RecordAssistantMessage:
		return record.requireMessage(RoleAssistant)
	case RecordToolCall:
		return record.requireCall()
	case RecordToolResult:
		return record.requireResult()
	case RecordApprovalAsked, RecordApprovalDecided, RecordApprovalPolicy:
		return record.requireApproval()
	case RecordRetry, RecordRetryStarted:
		return record.requireRetry()
	case RecordCompactionStart, RecordCompactionSummary, RecordCompactionEnd:
		return record.requireCompaction()
	case RecordSubagentDescriptor:
		return record.requireSubagent()
	case RecordStepEnd:
		return record.requireBare(true, true)
	case RecordTurnEnd:
		return record.requireTurnEnd()
	default:
		return invalid("unknown type %q", record.Type)
	}
}

func allowsZeroTurn(recordType RecordType) bool {
	return recordType == RecordApprovalPolicy || recordType == RecordCompactionStart || recordType == RecordCompactionSummary || recordType == RecordCompactionEnd || recordType == RecordSubagentDescriptor
}

func invalid(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRecord, fmt.Sprintf(format, values...))
}

func (record Record) requireBare(step, usage bool) error {
	if step != (record.Step > 0) {
		return invalid("%s has invalid step", record.Type)
	}
	if record.Message != nil || record.Chunk != nil || record.Call != nil || record.Result != nil || record.Header != nil || record.Retry != nil || record.Approval != nil || record.Compaction != nil || record.Subagent != nil || record.Outcome != "" || !usage && record.Usage != nil {
		return invalid("%s has unrelated fields", record.Type)
	}
	return validateUsage(record.Usage)
}

func (record Record) requireMessage(role MessageRole) error {
	requireStep := role == RoleAssistant
	if record.Message == nil || record.Message.Role != role || requireStep && record.Step == 0 || record.hasExtras("message") {
		return invalid("%s shape is invalid", record.Type)
	}
	return validateMessage(*record.Message, role == RoleUser)
}

func (record Record) hasExtras(keep string) bool {
	return keep != "message" && record.Message != nil || keep != "chunk" && record.Chunk != nil || keep != "call" && record.Call != nil || keep != "result" && record.Result != nil || keep != "header" && record.Header != nil || keep != "usage" && record.Usage != nil || keep != "retry" && record.Retry != nil || keep != "approval" && record.Approval != nil || keep != "compaction" && record.Compaction != nil || keep != "subagent" && record.Subagent != nil || keep != "outcome" && record.Outcome != ""
}

func validateMessage(message Message, requireContent bool) error {
	if len(message.Content) > MaxContentBlocks || requireContent && len(message.Content) == 0 {
		return invalid("message content count is invalid")
	}
	if message.Source.Kind == "" || len(message.Source.Kind) > 64 || len(message.Source.Plugin) > 128 {
		return invalid("message source is invalid")
	}
	for index, block := range message.Content {
		if err := validateContent(block); err != nil {
			return invalid("content %d: %v", index, err)
		}
	}
	return nil
}

func validateContent(block ContentBlock) error {
	switch block.Type {
	case ContentText:
		if block.Image != nil || len(block.Text) > MaxTextBytes {
			return errors.New("text block is invalid")
		}
		return nil
	case ContentImage:
		if block.Text != "" || block.Image == nil {
			return errors.New("image block is invalid")
		}
		return validateImage(*block.Image)
	default:
		return fmt.Errorf("unknown content type %q", block.Type)
	}
}

func validateImage(image Image) error {
	if err := validateIdentifier("image ID", image.ID, 128); err != nil {
		return err
	}
	if image.Name == "" || len(image.Name) > 255 || strings.ContainsAny(image.Name, "\r\n") {
		return errors.New("image name is invalid")
	}
	if image.MediaType != "image/jpeg" && image.MediaType != "image/png" {
		return errors.New("image media type is unsupported")
	}
	if image.Width < 1 || image.Width > 4096 || image.Height < 1 || image.Height > 4096 || len(image.SHA256) != 64 {
		return errors.New("image dimensions or digest are invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil || len(decoded) == 0 || len(decoded) > MaxImageBytes {
		return errors.New("image data is invalid")
	}
	digest := sha256.Sum256(decoded)
	if !strings.EqualFold(image.SHA256, hex.EncodeToString(digest[:])) {
		return errors.New("image digest does not match data")
	}
	return nil
}

func (record Record) requireHeader() error {
	if record.Step == 0 || record.Header == nil || record.hasExtras("header") {
		return invalid("request/header shape is invalid")
	}
	header := record.Header
	if err := validateIdentifier("provider", header.Provider, 64); err != nil {
		return err
	}
	if err := validateIdentifier("model", header.Model, 256); err != nil {
		return err
	}
	if len(header.System) > MaxTextBytes || header.ContextWindow < 0 || len(header.Tools) > 64 {
		return invalid("request/header fields exceed limits")
	}
	seen := map[string]struct{}{}
	for _, tool := range header.Tools {
		if _, ok := seen[tool.Name]; ok {
			return invalid("duplicate request tool %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		if err := validateToolDefinition(tool); err != nil {
			return err
		}
	}
	return nil
}

func validateToolDefinition(tool ToolDefinition) error {
	if err := validateIdentifier("tool name", tool.Name, 64); err != nil {
		return err
	}
	if tool.Description == "" || len(tool.Description) > 2048 {
		return invalid("tool description is invalid")
	}
	if len(tool.Parameters) == 0 || len(tool.Parameters) > MaxArgumentsBytes || !json.Valid(tool.Parameters) {
		return invalid("tool parameters are invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(tool.Parameters, &object); err != nil || object == nil {
		return invalid("tool parameters must be an object")
	}
	return nil
}

func (record Record) requireChunk() error {
	if record.Step == 0 || record.Chunk == nil || record.hasExtras("chunk") {
		return invalid("assistant/chunk shape is invalid")
	}
	chunk := record.Chunk
	if len(chunk.Text)+len(chunk.Arguments) > MaxArgumentsBytes || chunk.Index < 0 {
		return invalid("assistant/chunk exceeds limits")
	}
	switch chunk.Kind {
	case ChunkText, ChunkReasoning:
		if chunk.Text == "" || chunk.CallID != "" || chunk.Name != "" || chunk.Arguments != "" {
			return invalid("assistant text chunk is invalid")
		}
	case ChunkTool:
		if chunk.Text != "" {
			return invalid("assistant tool chunk is invalid")
		}
	default:
		return invalid("unknown chunk kind %q", chunk.Kind)
	}
	return nil
}

func (record Record) requireCall() error {
	if record.Step == 0 || record.Call == nil || record.hasExtras("call") {
		return invalid("tool/call shape is invalid")
	}
	if err := validateIdentifier("call ID", record.Call.ID, 128); err != nil {
		return err
	}
	if err := validateIdentifier("tool name", record.Call.Name, 64); err != nil {
		return err
	}
	if len(record.Call.Arguments) == 0 || len(record.Call.Arguments) > MaxArgumentsBytes || !json.Valid(record.Call.Arguments) {
		return invalid("tool arguments are invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(record.Call.Arguments, &object); err != nil || object == nil {
		return invalid("tool arguments must be an object")
	}
	return nil
}

func (record Record) requireResult() error {
	if record.Step == 0 || record.Result == nil || record.hasExtras("result") {
		return invalid("tool/result shape is invalid")
	}
	if err := validateIdentifier("call ID", record.Result.CallID, 128); err != nil {
		return err
	}
	if len(record.Result.Output) > MaxTextBytes {
		return invalid("tool output exceeds %d bytes", MaxTextBytes)
	}
	return nil
}

func (record Record) requireApproval() error {
	if record.Approval == nil || record.hasExtras("approval") {
		return invalid("%s shape is invalid", record.Type)
	}
	data := record.Approval
	switch record.Type {
	case RecordApprovalAsked:
		if record.Step == 0 || validateIdentifier("approval ID", data.ID, 128) != nil || validateIdentifier("tool name", data.ToolName, 64) != nil || validateIdentifier("call ID", data.CallID, 128) != nil || data.Reason == "" || len(data.Reason) > 2048 || data.Outcome != "" || data.Policy != "" {
			return invalid("approval/asked fields are invalid")
		}
	case RecordApprovalDecided:
		if record.Step == 0 || validateIdentifier("approval ID", data.ID, 128) != nil || !validApprovalOutcome(data.Outcome) || data.ToolName != "" || data.Reason != "" || data.Policy != "" {
			return invalid("approval/decided fields are invalid")
		}
	case RecordApprovalPolicy:
		if record.Step != 0 || data.ID != "" || data.ToolName != "" || data.CallID != "" || data.Reason != "" || data.Outcome != "" || !validApprovalPolicy(data.Policy) || data.Source != "" && data.Source != "delegation" {
			return invalid("approval/policy fields are invalid")
		}
	case RecordTurnStart, RecordUserMessage, RecordStepStart, RecordRequestHeader,
		RecordAssistantChunk, RecordAssistantMessage, RecordToolCall, RecordToolResult,
		RecordRetry, RecordRetryStarted, RecordCompactionStart, RecordCompactionSummary,
		RecordCompactionEnd, RecordSubagentDescriptor, RecordStepEnd, RecordTurnEnd:
		// Validate dispatches only approval record types to this shape-specific helper.
	}
	return nil
}

func validApprovalOutcome(outcome ApprovalOutcome) bool {
	return outcome == ApprovalAllowedOnce || outcome == ApprovalRejected || outcome == ApprovalCancelled || outcome == ApprovalUnavailable
}

func validApprovalPolicy(policy ApprovalPolicy) bool {
	return policy == ApprovalAsk || policy == ApprovalNever
}

func (record Record) requireRetry() error {
	if record.Step == 0 || record.Retry == nil || record.hasExtras("retry") {
		return invalid("%s shape is invalid", record.Type)
	}
	data := record.Retry
	if validateIdentifier("retry ID", data.ID, 128) != nil || data.Attempt < 1 {
		return invalid("retry identity is invalid")
	}
	if record.Type == RecordRetryStarted {
		if data.Provider != "" || data.PolicyKey != "" || data.MaxRetries != 0 || data.DelayMS != 0 || data.Failure != "" {
			return invalid("llm/retry-started has unrelated fields")
		}
		return nil
	}
	if validateIdentifier("provider", data.Provider, 64) != nil || data.PolicyKey == "" || len(data.PolicyKey) > 512 || data.DelayMS < 0 || data.MaxRetries < 0 || data.Failure == "" || len(data.Failure) > 64 {
		return invalid("llm/retry fields are invalid")
	}
	return nil
}

func (record Record) requireCompaction() error {
	if record.Compaction == nil || record.Step != 0 || record.hasExtras("compaction") {
		return invalid("%s shape is invalid", record.Type)
	}
	data := record.Compaction
	if validateIdentifier("compaction ID", data.ID, 128) != nil {
		return invalid("compaction ID is invalid")
	}
	switch record.Type {
	case RecordCompactionStart:
		if len(data.ShadowedSeqs) != 0 || len(data.Summary) != 0 || data.Provider != "" || data.Model != "" || data.Error != "" || data.ShadowedTokenCount != 0 {
			return invalid("compaction/start has unrelated fields")
		}
	case RecordCompactionSummary:
		if len(data.ShadowedSeqs) == 0 || len(data.Summary) == 0 || len(data.Summary) > MaxContentBlocks || data.ShadowedTokenCount <= 0 || validateIdentifier("provider", data.Provider, 64) != nil || validateIdentifier("model", data.Model, 256) != nil || data.Error != "" {
			return invalid("compaction/summary fields are invalid")
		}
		if !slices.IsSorted(data.ShadowedSeqs) || slices.ContainsFunc(data.ShadowedSeqs, func(seq uint64) bool { return seq == 0 }) {
			return invalid("compaction shadow seqs are invalid")
		}
		for _, block := range data.Summary {
			if err := validateContent(block); err != nil {
				return invalid("compaction summary: %v", err)
			}
		}
	case RecordCompactionEnd:
		if len(data.ShadowedSeqs) != 0 || len(data.Summary) != 0 || data.Provider != "" || data.Model != "" || data.ShadowedTokenCount != 0 || len(data.Error) > 2048 {
			return invalid("compaction/end fields are invalid")
		}
	case RecordTurnStart, RecordUserMessage, RecordStepStart, RecordRequestHeader,
		RecordAssistantChunk, RecordAssistantMessage, RecordToolCall, RecordApprovalAsked,
		RecordApprovalDecided, RecordApprovalPolicy, RecordToolResult, RecordRetry,
		RecordRetryStarted, RecordSubagentDescriptor, RecordStepEnd, RecordTurnEnd:
		// Validate dispatches only compaction record types to this shape-specific helper.
	}
	return nil
}

func (record Record) requireSubagent() error {
	if record.Step != 0 || record.Subagent == nil || record.hasExtras("subagent") {
		return invalid("subagent/descriptor shape is invalid")
	}
	data := record.Subagent
	if data.Version != 1 || validateIdentifier("subagent provider", data.Provider, 64) != nil || data.Mode != "one-shot" && data.Mode != "continuable" || data.Label == "" || len(data.Label) > 128 || len(data.Persona) > 4096 || len(data.Tools) > 32 {
		return invalid("subagent descriptor fields are invalid")
	}
	for _, tool := range data.Tools {
		if err := validateIdentifier("subagent tool", tool, 64); err != nil {
			return err
		}
	}
	return nil
}

func (record Record) requireTurnEnd() error {
	if record.Step != 0 || record.hasExtras("outcome") {
		return invalid("turn/end has unrelated fields")
	}
	switch record.Outcome {
	case OutcomeCompleted, OutcomeCanceled, OutcomeError, OutcomeStepLimit, OutcomeInterrupted:
		return nil
	default:
		return invalid("invalid turn outcome %q", record.Outcome)
	}
}

func validateUsage(usage *TokenUsage) error {
	if usage != nil && (usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0) {
		return invalid("token usage cannot be negative")
	}
	return nil
}

func validateIdentifier(subject, value string, limit int) error {
	if value == "" || len(value) > limit || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		return invalid("%s must be 1-%d trimmed bytes", subject, limit)
	}
	return nil
}
