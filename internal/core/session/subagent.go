package session

import (
	"slices"
	"strings"
)

// MaxSubagentLabelBytes leaves room for the job notification envelope within
// one text block, independently of the tool argument object's budget.
const MaxSubagentLabelBytes = 128 << 10

// OwnEvents returns the suffix of a session's events that the session wrote
// itself. A forked child begins with events copied from its parent; its own
// descriptor, the last one in the log, records how many. Events of a session
// without a descriptor are all its own. The result aliases events.
func OwnEvents(events []Event) []Event {
	for _, event := range slices.Backward(events) {
		if descriptor := event.Record.Subagent; descriptor != nil {
			return events[descriptor.Inherited:]
		}
	}
	return events
}

// Children projects the catalog entries a session recorded for the children
// it created, in creation order. Entries inherited from a forked parent
// belong to that parent and are excluded.
func Children(events []Event) []SubagentCatalog {
	var children []SubagentCatalog
	for _, event := range OwnEvents(events) {
		if event.Record.Type == RecordSubagentCatalog {
			children = append(children, *event.Record.Catalog)
		}
	}
	return children
}

// FinalAssistantText selects the last assistant message with content,
// including a tool proposal whose calls are separate records, then extracts
// its text. Empty usage-only messages do not replace it. Streamed text is
// the fallback only when no message with content exists.
func FinalAssistantText(events []Event) string {
	hasCalls := false
	for _, event := range slices.Backward(events) {
		record := event.Record
		if record.Type == RecordToolCall {
			hasCalls = true
		}
		if record.Type == RecordAssistantMessage {
			if len(record.Message.Content) > 0 || hasCalls {
				return Text(*record.Message)
			}
		}
	}
	var partial strings.Builder
	for _, event := range events {
		if record := event.Record; record.Type == RecordAssistantChunk && record.Chunk.Kind == ChunkText {
			partial.WriteString(record.Chunk.Text)
		}
	}
	return partial.String()
}

// LastOutcome returns the outcome of the last turn that closed within
// events, and false when none closed.
func LastOutcome(events []Event) (TurnOutcome, bool) {
	for _, event := range slices.Backward(events) {
		if event.Record.Type == RecordTurnEnd {
			return event.Record.Outcome, true
		}
	}
	return "", false
}
