package jsonl

import (
	"encoding/json"
	"errors"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

func orderedEvent(record coresession.Record) coresession.Event {
	return coresession.Event{Record: record}
}

func addOrder(events []coresession.Event, record coresession.Record) []coresession.Event {
	return append(append([]coresession.Event(nil), events...), orderedEvent(record))
}

func orderPrefix() []coresession.Event {
	return []coresession.Event{
		orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 1}),
		orderedEvent(coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")}),
		orderedEvent(coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1}),
	}
}

func withAssistant(events []coresession.Event) []coresession.Event {
	return addOrder(events, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("answer")})
}

func withCall(events []coresession.Event) []coresession.Event {
	return addOrder(events, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}})
}

func withApproval(events []coresession.Event) []coresession.Event {
	return addOrder(events, coresession.Record{Type: coresession.RecordApprovalAsked, Turn: 1, Step: 1, Approval: &coresession.ApprovalData{ID: "approval", CallID: "call", ToolName: "tool", Reason: "reason"}})
}

func TestValidateOrderRejectsEveryInvalidTransition(t *testing.T) {
	prefix := orderPrefix()
	assistant := withAssistant(orderPrefix())
	call := withCall(withAssistant(orderPrefix()))
	approval := withApproval(withCall(withAssistant(orderPrefix())))
	cases := map[string][]coresession.Event{
		"turn start": {
			orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 2}),
		},
		"user outside": {
			orderedEvent(coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("x")}),
		},
		"step start":                addOrder([]coresession.Event{orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})}, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 2}),
		"header outside":            addOrder(prefix, coresession.Record{Type: coresession.RecordRequestHeader, Turn: 2, Step: 1, Header: &coresession.RequestHeader{Provider: "p", Model: "m"}}),
		"assistant duplicate":       addOrder(assistant, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("again")}),
		"call before assistant":     addOrder(prefix, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}}),
		"duplicate call":            addOrder(call, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}}),
		"approval missing call":     addOrder(call, coresession.Record{Type: coresession.RecordApprovalAsked, Turn: 1, Step: 1, Approval: &coresession.ApprovalData{ID: "a", CallID: "missing", ToolName: "tool", Reason: "r"}}),
		"duplicate approval":        addOrder(approval, coresession.Record{Type: coresession.RecordApprovalAsked, Turn: 1, Step: 1, Approval: &coresession.ApprovalData{ID: "approval", CallID: "call", ToolName: "tool", Reason: "r"}}),
		"decision outside":          addOrder(approval, coresession.Record{Type: coresession.RecordApprovalDecided, Turn: 2, Step: 1, Approval: &coresession.ApprovalData{ID: "approval", Outcome: coresession.ApprovalRejected}}),
		"decision missing question": addOrder(call, coresession.Record{Type: coresession.RecordApprovalDecided, Turn: 1, Step: 1, Approval: &coresession.ApprovalData{ID: "missing", Outcome: coresession.ApprovalRejected}}),
		"result outside":            addOrder(call, coresession.Record{Type: coresession.RecordToolResult, Turn: 2, Step: 1, Result: &coresession.ToolResult{CallID: "call"}}),
		"result missing call":       addOrder(assistant, coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "missing"}}),
		"result before decision":    addOrder(approval, coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call"}}),
		"retry after assistant":     addOrder(assistant, coresession.Record{Type: coresession.RecordRetry, Turn: 1, Step: 1, Retry: &coresession.RetryData{ID: "r", Provider: "p", PolicyKey: "k", Attempt: 1, Failure: "server"}}),
		"nested compaction": {
			orderedEvent(coresession.Record{Type: coresession.RecordCompactionStart, Compaction: &coresession.CompactionData{ID: "one"}}),
			orderedEvent(coresession.Record{Type: coresession.RecordCompactionStart, Compaction: &coresession.CompactionData{ID: "two"}}),
		},
		"summary without start": {
			orderedEvent(coresession.Record{Type: coresession.RecordCompactionSummary, Compaction: &coresession.CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "s"}}, Provider: "p", Model: "m"}}),
		},
		"end without start": {
			orderedEvent(coresession.Record{Type: coresession.RecordCompactionEnd, Compaction: &coresession.CompactionData{ID: "compact"}}),
		},
		"step unfinished": addOrder(call, coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 1}),
		"turn unfinished": addOrder(prefix, coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted}),
		"surface compaction": {
			orderedEvent(coresession.Record{Type: coresession.RecordCompactionStart, Compaction: &coresession.CompactionData{ID: "compact"}}),
			orderedEvent(coresession.Record{Type: coresession.RecordCompactionSummary, Compaction: &coresession.CompactionData{ID: "compact", ShadowedSeqs: []uint64{99}, ShadowedTokenCount: 1, Summary: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "s"}}, Provider: "p", Model: "m"}}),
		},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateOrder(events, false); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if _, err := validateOrder(orderPrefix(), true); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("open tail accepted: %v", err)
	}
}

func TestValidateOrderValidMetadataCompactionAndRemoval(t *testing.T) {
	events := []coresession.Event{
		orderedEvent(coresession.Record{Type: coresession.RecordApprovalPolicy, Approval: &coresession.ApprovalData{Policy: coresession.ApprovalAsk}}),
		orderedEvent(coresession.Record{Type: coresession.RecordSubagentDescriptor, Subagent: &coresession.SubagentDescriptor{Version: 1, Provider: "in-process", Mode: "continuable", Label: "worker"}}),
		orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 1}),
		{Sequence: 1, Record: coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")}},
		orderedEvent(coresession.Record{Type: coresession.RecordCompactionStart, Turn: 1, Compaction: &coresession.CompactionData{ID: "compact"}}),
		{Sequence: 2, Record: coresession.Record{Type: coresession.RecordCompactionSummary, Turn: 1, Compaction: &coresession.CompactionData{ID: "compact", ShadowedSeqs: []uint64{1}, ShadowedTokenCount: 1, Summary: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "summary"}}, Provider: "p", Model: "m"}}},
		orderedEvent(coresession.Record{Type: coresession.RecordCompactionEnd, Turn: 1, Compaction: &coresession.CompactionData{ID: "compact"}}),
		orderedEvent(coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1}),
		orderedEvent(coresession.Record{Type: coresession.RecordRetryStarted, Turn: 1, Step: 1, Retry: &coresession.RetryData{ID: "retry", Attempt: 1}}),
		orderedEvent(coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("done")}),
		orderedEvent(coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 1}),
		orderedEvent(coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted}),
	}
	if _, err := validateOrder(events, true); err != nil {
		t.Fatal(err)
	}
	values := []string{"a", "b"}
	if got := remove(values, "a"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("remove=%#v", got)
	}
	if got := remove(values, "missing"); len(got) != 2 {
		t.Fatalf("missing remove=%#v", got)
	}
}
