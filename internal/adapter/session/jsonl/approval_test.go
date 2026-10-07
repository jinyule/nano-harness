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

// startApproval starts a fresh approval service, as a new process does, and
// restores it from log.
func startApproval(t *testing.T, log *Log) (*approval.Service, *plugin.Scope) {
	t.Helper()
	service := approval.New()
	scope := &plugin.Scope{}
	if err := service.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	events, err := log.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Restore(log.Header().SessionID, events); err != nil {
		t.Fatal(err)
	}
	return service, scope
}

// approvalTurn commits one closed turn whose single tool call asks service
// for approval, failing the test if the question cannot be recorded.
func approvalTurn(t *testing.T, log *Log, service *approval.Service, turn uint64, delegated bool) {
	t.Helper()
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: turn})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: turn, Message: userMessage("write")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: turn, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: turn, Step: 1, Message: assistantMessage("writing")})
	call := coresession.ToolCall{ID: "call", Name: "write", Arguments: json.RawMessage(`{}`)}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: turn, Step: 1, Call: &call})
	outcome, err := service.Decide(t.Context(), appTool.ApprovalRequest{SessionID: log.Header().SessionID, Turn: turn, Step: 1, Call: call, Reason: "write", Delegated: delegated, Journal: log})
	if err != nil || outcome != coresession.ApprovalUnavailable {
		t.Fatalf("question in %s turn %d = %q, %v", log.Header().SessionID, turn, outcome, err)
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolResult, Turn: turn, Step: 1, Result: &coresession.ToolResult{CallID: call.ID, Output: "Error: approval unavailable", IsError: true}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepEnd, Turn: turn, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnEnd, Turn: turn, Outcome: coresession.OutcomeCompleted})
}

// askedIDs returns the approval IDs of every question in events.
func askedIDs(events []coresession.Event) []string {
	var ids []string
	for _, event := range events {
		if event.Record.Type == coresession.RecordApprovalAsked {
			ids = append(ids, event.Record.Approval.ID)
		}
	}
	return ids
}

// TestApproval_IDsStayUniqueAcrossRestart reopens the real log with a new
// approval service, as a process restart does, and asks again: the second
// question must commit beside the first instead of reusing its ID.
func TestApproval_IDsStayUniqueAcrossRestart(t *testing.T) {
	manager, managerScope := startManager(t)
	t.Cleanup(func() { _ = managerScope.Close(context.Background()) })
	cwd := t.TempDir()
	for turn := uint64(1); turn <= 2; turn++ {
		log, err := manager.Open(t.Context(), OpenOptions{SessionID: "approval-restart", Create: turn == 1, Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		service, scope := startApproval(t, log)
		approvalTurn(t, log, service, turn, false)
		if err := scope.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	_, events, err := manager.Inspect(t.Context(), "approval-restart")
	if err != nil {
		t.Fatal(err)
	}
	if asked := askedIDs(events); len(asked) != 2 || asked[0] == asked[1] {
		t.Fatalf("approval IDs across restart = %q", asked)
	}
}

// TestApproval_ForkChildOfARestartedParentAsksUniquely forks a child from a
// parent resumed by a new process: the child's seed carries the parent's
// question, and the child's own delegated question must not reuse its ID.
func TestApproval_ForkChildOfARestartedParentAsksUniquely(t *testing.T) {
	manager, managerScope := startManager(t)
	t.Cleanup(func() { _ = managerScope.Close(context.Background()) })
	cwd := t.TempDir()
	parent, err := manager.Open(t.Context(), OpenOptions{SessionID: "parent", Create: true, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	earlier, earlierScope := startApproval(t, parent)
	approvalTurn(t, parent, earlier, 1, false)
	if err := earlierScope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	parent, err = manager.Open(t.Context(), OpenOptions{SessionID: "parent", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close(context.Background()) })
	service, _ := startApproval(t, parent)
	seed, err := parent.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Open(t.Context(), OpenOptions{SessionID: "child", Create: true, Cwd: cwd, ParentSessionID: "parent", DelegationDepth: 1, Seed: seed})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close(context.Background()) })
	appendRecord(t, child, coresession.Record{Type: coresession.RecordSubagentDescriptor, Subagent: &coresession.SubagentDescriptor{Version: 3, Route: coresession.SubagentRoute{Provider: "openai", Model: "model", Effort: coresession.EffortMax}, Provider: coresession.SubagentFork, Mode: coresession.SubagentOneShot, Label: "fork", Inherited: uint64(len(seed))}})
	events, err := child.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Restore("child", events); err != nil {
		t.Fatal(err)
	}
	approvalTurn(t, child, service, 2, true)
	events, err = child.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if asked := askedIDs(events); len(asked) != 2 || asked[0] == asked[1] {
		t.Fatalf("inherited and own approval IDs = %q", asked)
	}
}
