package approval

import (
	"context"
	"errors"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type testJournal struct {
	records []session.Record
	err     error
}

func (journal *testJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	if journal.err != nil {
		return session.Event{}, journal.err
	}
	journal.records = append(journal.records, record)
	return session.Event{Sequence: uint64(len(journal.records)), Record: record}, nil
}

type testBroker struct{ outcome session.ApprovalOutcome }

func (broker testBroker) Ask(context.Context, Question) session.ApprovalOutcome {
	return broker.outcome
}

func startApproval(t *testing.T) (*Service, *plugin.Scope) {
	t.Helper()
	service := New()
	scope := &plugin.Scope{}
	if service.ID() != "approval" || service.Start(context.Background(), scope) != nil {
		t.Fatal("start")
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, scope
}

func approvalRequest(journal *testJournal) appTool.ApprovalRequest {
	return appTool.ApprovalRequest{SessionID: "s", Turn: 1, Step: 1, Call: session.ToolCall{ID: "c", Name: "tool"}, Reason: "risk", Journal: journal}
}

func TestServiceDecisionsAndPolicy(t *testing.T) {
	service, serviceScope := startApproval(t)
	if service.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start")
	}
	journal := &testJournal{}
	if err := service.Restore("s", []session.Event{{Record: session.Record{Type: session.RecordApprovalPolicy, Approval: &session.ApprovalData{Policy: session.ApprovalNever}}}}); err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Decide(context.Background(), approvalRequest(journal))
	if err != nil || outcome != session.ApprovalRejected || len(journal.records) != 2 {
		t.Fatalf("outcome=%s records=%#v err=%v", outcome, journal.records, err)
	}
	if err := service.SetPolicy(context.Background(), "s", journal, session.ApprovalAsk); err != nil {
		t.Fatal(err)
	}
	brokerScope := &plugin.Scope{}
	if err := service.RegisterBroker(testBroker{outcome: session.ApprovalAllowedOnce}, brokerScope); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterBroker(testBroker{}, &plugin.Scope{}); err == nil {
		t.Fatal("duplicate broker")
	}
	outcome, err = service.Decide(context.Background(), approvalRequest(journal))
	if err != nil || outcome != session.ApprovalAllowedOnce {
		t.Fatalf("operator outcome=%s err=%v", outcome, err)
	}
	request := approvalRequest(journal)
	request.Delegated = true
	if outcome, _ = service.Decide(context.Background(), request); outcome != session.ApprovalUnavailable {
		t.Fatalf("delegated=%s", outcome)
	}
	if err := brokerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if outcome, _ = service.Decide(context.Background(), approvalRequest(journal)); outcome != session.ApprovalUnavailable {
		t.Fatalf("no broker=%s", outcome)
	}
	if err := serviceScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Restore("s", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("restore closed=%v", err)
	}
	if err := service.SetPolicy(context.Background(), "s", journal, session.ApprovalAsk); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("policy closed=%v", err)
	}
}

func TestServiceFailureAndCancellation(t *testing.T) {
	closedServiceScope := &plugin.Scope{}
	_ = closedServiceScope.Close(context.Background())
	if err := New().Start(context.Background(), closedServiceScope); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed service scope=%v", err)
	}
	inactive := New()
	if err := inactive.RegisterBroker(testBroker{}, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive broker=%v", err)
	}
	if _, err := inactive.Decide(context.Background(), approvalRequest(&testJournal{})); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive decision=%v", err)
	}
	service, _ := startApproval(t)
	if err := service.RegisterBroker(nil, &plugin.Scope{}); err == nil {
		t.Fatal("nil broker")
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := service.RegisterBroker(testBroker{}, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
	journal := &testJournal{err: errors.New("append")}
	if _, err := service.Decide(context.Background(), approvalRequest(journal)); err == nil {
		t.Fatal("append error lost")
	}
	if _, err := service.Decide(context.Background(), appTool.ApprovalRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid=%v", err)
	}
	journal.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := approvalRequest(journal)
	outcome, err := service.Decide(ctx, request)
	if err != nil || outcome != session.ApprovalCancelled {
		t.Fatalf("cancel=%s err=%v", outcome, err)
	}
	if err := service.SetPolicy(context.Background(), "", journal, session.ApprovalAsk); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid policy=%v", err)
	}
	journal.err = errors.New("policy append")
	if err := service.SetPolicy(context.Background(), "s", journal, session.ApprovalAsk); err == nil {
		t.Fatal("policy append error lost")
	}
	journal.err = nil
	brokerScope := &plugin.Scope{}
	if err := service.RegisterBroker(testBroker{outcome: session.ApprovalUnavailable}, brokerScope); err != nil {
		t.Fatal(err)
	}
	if outcome, err := service.Decide(context.Background(), approvalRequest(journal)); err != nil || outcome != session.ApprovalUnavailable {
		t.Fatalf("invalid broker outcome=%s err=%v", outcome, err)
	}
	_ = brokerScope.Close(context.Background())
}
