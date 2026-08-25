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
			result := *record.Result
			nodes = append(nodes, SurfaceNode{Sequence: event.Sequence, Result: &result})
		case RecordCompactionSummary:
			var err error
			nodes, err = replaceWithSummary(nodes, event)
			if err != nil {
				return nil, err
			}
		case RecordTurnStart, RecordStepStart, RecordRequestHeader, RecordAssistantChunk,
			RecordApprovalAsked, RecordApprovalDecided, RecordApprovalPolicy, RecordRetry,
			RecordRetryStarted, RecordCompactionStart, RecordCompactionEnd,
			RecordSubagentDescriptor, RecordStepEnd, RecordTurnEnd:
			// Metadata and lifecycle facts do not directly contribute a model-visible node.
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
		if node.Result != nil {
			result := *node.Result
			cloned[index].Result = &result
		}
	}
	return cloned
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
	if event.Record.Result != nil {
		result := *event.Record.Result
		cloned.Record.Result = &result
	}
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
