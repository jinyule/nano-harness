package jsonl

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/approval"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

// TestApproval_IDsStayUniqueAcrossRestart reopens the real log with a new
// approval service, as a process restart does, and asks again: the second
// question must commit beside the first instead of reusing its ID.
func TestApproval_IDsStayUniqueAcrossRestart(t *testing.T) {
	ctx := context.Background()
	manager, managerScope := startManager(t)
	t.Cleanup(func() { _ = managerScope.Close(ctx) })
	cwd := t.TempDir()
	for turn := uint64(1); turn <= 2; turn++ {
		log, err := manager.Open(ctx, OpenOptions{SessionID: "approval-restart", Create: turn == 1, Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		service := approval.New()
		scope := &plugin.Scope{}
		if err := service.Start(ctx, scope); err != nil {
			t.Fatal(err)
		}
		events, err := log.Events(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Restore(log.Header().SessionID, events); err != nil {
			t.Fatal(err)
		}
		appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: turn})
		appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: turn, Message: userMessage("write")})
		appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: turn, Step: 1})
		appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: turn, Step: 1, Message: assistantMessage("writing")})
		call := coresession.ToolCall{ID: "call", Name: "write", Arguments: json.RawMessage(`{}`)}
		appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: turn, Step: 1, Call: &call})
		outcome, err := service.Decide(ctx, appTool.ApprovalRequest{SessionID: log.Header().SessionID, Turn: turn, Step: 1, Call: call, Reason: "write", Journal: log})
		if err != nil || outcome != coresession.ApprovalUnavailable {
			t.Fatalf("question in turn %d = %q, %v", turn, outcome, err)
		}
		appendRecord(t, log, coresession.Record{Type: coresession.RecordToolResult, Turn: turn, Step: 1, Result: &coresession.ToolResult{CallID: call.ID, Output: "Error: approval unavailable", IsError: true}})
		appendRecord(t, log, coresession.Record{Type: coresession.RecordStepEnd, Turn: turn, Step: 1})
		appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnEnd, Turn: turn, Outcome: coresession.OutcomeCompleted})
		if err := scope.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	log, err := manager.Open(ctx, OpenOptions{SessionID: "approval-restart", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(ctx) })
	events, err := log.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	for _, event := range events {
		if event.Record.Type == coresession.RecordApprovalAsked {
			asked = append(asked, event.Record.Approval.ID)
		}
	}
	if len(asked) != 2 || asked[0] == asked[1] {
		t.Fatalf("approval IDs across restart = %q", asked)
	}
}
