package session

import (
	"slices"
	"strings"
)

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

// FinalAssistantText returns the closing answer within events: the text of
// the last assistant message that has any, or, when none does, the streamed
// text deltas joined in order, so an answer cut off by cancellation or a
// failed request still surfaces. It returns "" when no text was produced.
func FinalAssistantText(events []Event) string {
	final := ""
	var partial strings.Builder
	for _, event := range events {
		record := event.Record
		if record.Type == RecordAssistantMessage && Text(*record.Message) != "" {
			final = Text(*record.Message)
		}
		if record.Type == RecordAssistantChunk && record.Chunk.Kind == ChunkText {
			partial.WriteString(record.Chunk.Text)
		}
	}
	if final != "" {
		return final
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
