package session

import (
	"fmt"
	"slices"
	"strings"
)

// Surface folds append-only events into the current model-visible transcript.
func Surface(events []Event) ([]SurfaceNode, error) {
	nodes := make([]SurfaceNode, 0, len(events))
	for _, event := range events {
		record := event.Record
		switch record.Type {
		case RecordUserMessage, RecordAssistantMessage:
			nodes = append(nodes, SurfaceNode{Sequence: event.Sequence, Message: cloneMessage(record.Message)})
		case RecordToolCall:
			call := *record.Call
			call.Arguments = slices.Clone(call.Arguments)
			nodes = append(nodes, SurfaceNode{Sequence: event.Sequence, Call: &call})
		case RecordToolResult:
			nodes = append(nodes, SurfaceNode{Sequence: event.Sequence, Result: cloneResult(record.Result)})
		case RecordCompactionSummary:
			var err error
			nodes, err = replaceWithSummary(nodes, event)
			if err != nil {
				return nil, err
			}
		case RecordCompactionPrune:
			if err := applyPrune(nodes, event); err != nil {
				return nil, err
			}
		case RecordTurnStart, RecordStepStart, RecordRequestHeader, RecordAssistantChunk,
			RecordApprovalAsked, RecordApprovalDecided, RecordApprovalPolicy, RecordRetry,
			RecordRetryStarted, RecordCompactionStart, RecordCompactionEnd,
			RecordSubagentDescriptor, RecordSubagentCatalog, RecordTodoWrite, RecordWebSearchRequest, RecordPlanMode, RecordGoalChange, RecordNoticeQueued, RecordStepEnd, RecordTurnEnd:
			// Metadata, UI state, and lifecycle facts do not directly contribute a model-visible node.
		}
	}
	return cloneSurface(nodes), nil
}

func replaceWithSummary(nodes []SurfaceNode, event Event) ([]SurfaceNode, error) {
	data := event.Record.Compaction
	shadowed := make(map[uint64]struct{}, len(data.ShadowedSeqs))
	for _, seq := range data.ShadowedSeqs {
		shadowed[seq] = struct{}{}
	}
	position := -1
	kept := nodes[:0]
	for _, node := range nodes {
		if _, ok := shadowed[node.Sequence]; ok {
			if position == -1 {
				position = len(kept)
			}
			continue
		}
		kept = append(kept, node)
	}
	if position == -1 {
		return nil, fmt.Errorf("%w: compaction %q shadows no visible nodes", ErrInvalidRecord, data.ID)
	}
	summary := &Message{
		Role:    RoleUser,
		Content: cloneContent(data.Summary),
		Source:  MessageSource{Kind: "compaction"},
	}
	kept = append(kept, SurfaceNode{})
	copy(kept[position+1:], kept[position:])
	kept[position] = SurfaceNode{Sequence: event.Sequence, Message: summary}
	return kept, nil
}

func cloneMessage(message *Message) *Message {
	if message == nil {
		return nil
	}
	copyMessage := *message
	copyMessage.Content = cloneContent(message.Content)
	return &copyMessage
}

func cloneContent(content []ContentBlock) []ContentBlock {
	cloned := slices.Clone(content)
	for index := range cloned {
		if cloned[index].Image != nil {
			imageCopy := *cloned[index].Image
			cloned[index].Image = &imageCopy
		}
	}
	return cloned
}

func cloneSurface(nodes []SurfaceNode) []SurfaceNode {
	cloned := make([]SurfaceNode, len(nodes))
	for index, node := range nodes {
		cloned[index] = node
		cloned[index].Message = cloneMessage(node.Message)
		if node.Call != nil {
			call := *node.Call
			call.Arguments = slices.Clone(call.Arguments)
			cloned[index].Call = &call
		}
		cloned[index].Result = cloneResult(node.Result)
	}
	return cloned
}

// cloneResult detaches a tool result, including its image, error, and metadata.
func cloneResult(result *ToolResult) *ToolResult {
	if result == nil {
		return nil
	}
	copyResult := *result
	if result.Image != nil {
		image := *result.Image
		copyResult.Image = &image
	}
	if result.Error != nil {
		failure := *result.Error
		copyResult.Error = &failure
	}
	if result.Meta != nil {
		meta := result.Meta.clone(func(text string) string { return text })
		copyResult.Meta = &meta
	}
	return &copyResult
}

// CloneEvent detaches all mutable slices and pointers in one event.
func CloneEvent(event Event) Event {
	cloned := event
	cloned.Record.Message = cloneMessage(event.Record.Message)
	if event.Record.Chunk != nil {
		chunk := *event.Record.Chunk
		cloned.Record.Chunk = &chunk
	}
	if event.Record.Call != nil {
		call := *event.Record.Call
		call.Arguments = slices.Clone(call.Arguments)
		cloned.Record.Call = &call
	}
	cloned.Record.Result = cloneResult(event.Record.Result)
	if event.Record.Header != nil {
		header := *event.Record.Header
		header.Tools = slices.Clone(header.Tools)
		for index := range header.Tools {
			header.Tools[index].Parameters = slices.Clone(header.Tools[index].Parameters)
		}
		cloned.Record.Header = &header
	}
	if event.Record.Usage != nil {
		usage := *event.Record.Usage
		cloned.Record.Usage = &usage
	}
	if event.Record.Retry != nil {
		retry := *event.Record.Retry
		cloned.Record.Retry = &retry
	}
	if event.Record.Approval != nil {
		approval := *event.Record.Approval
		cloned.Record.Approval = &approval
	}
	if event.Record.Compaction != nil {
		compaction := *event.Record.Compaction
		compaction.ShadowedSeqs = slices.Clone(compaction.ShadowedSeqs)
		compaction.Summary = cloneContent(compaction.Summary)
		cloned.Record.Compaction = &compaction
	}
	if event.Record.Subagent != nil {
		descriptor := *event.Record.Subagent
		descriptor.Tools = slices.Clone(descriptor.Tools)
		cloned.Record.Subagent = &descriptor
	}
	if event.Record.Catalog != nil {
		catalog := *event.Record.Catalog
		cloned.Record.Catalog = &catalog
	}
	if event.Record.Todo != nil {
		todo := *event.Record.Todo
		todo.Items = slices.Clone(todo.Items)
		cloned.Record.Todo = &todo
	}
	if event.Record.Plan != nil {
		mode := *event.Record.Plan
		cloned.Record.Plan = &mode
	}
	if event.Record.Search != nil {
		search := *event.Record.Search
		cloned.Record.Search = &search
	}
	if event.Record.Goal != nil {
		change := cloneGoalChange(*event.Record.Goal)
		cloned.Record.Goal = &change
	}
	if event.Record.Prune != nil {
		prune := *event.Record.Prune
		cloned.Record.Prune = &prune
	}
	return cloned
}

// Text returns the concatenated text blocks of a message.
func Text(message Message) string {
	var output strings.Builder
	for _, block := range message.Content {
		if block.Type == ContentText {
			output.WriteString(block.Text)
		}
	}
	return output.String()
}
