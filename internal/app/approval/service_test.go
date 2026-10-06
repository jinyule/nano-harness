package approval

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

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

// lockedJournal records appends made on another goroutine. hold, when set,
// runs inside every Append of the given type before it records.
type lockedJournal struct {
	mu      sync.Mutex
	records []session.Record
	holdFor session.RecordType
	hold    func(context.Context) error
}

func (journal *lockedJournal) Append(ctx context.Context, record session.Record) (session.Event, error) {
	if journal.hold != nil && record.Type == journal.holdFor {
		if err := journal.hold(ctx); err != nil {
			return session.Event{}, err
		}
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.records = append(journal.records, record)
	return session.Event{Sequence: uint64(len(journal.records)), Record: record}, nil
}

func (journal *lockedJournal) snapshot() []session.Record {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return slices.Clone(journal.records)
}

// heldBroker answers outcome only after its context ends or release closes,
// like an operator who approves just as shutdown begins.
type heldBroker struct {
	entered chan struct{}
	release chan struct{}
	outcome session.ApprovalOutcome
}

func (broker heldBroker) Ask(ctx context.Context, _ Question) session.ApprovalOutcome {
	close(broker.entered)
	select {
	case <-ctx.Done():
	case <-broker.release:
	}
	return broker.outcome
}

func (service *Service) policyCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return len(service.policies)
}

type testBroker struct{ outcome session.ApprovalOutcome }

func (broker testBroker) Ask(context.Context, Question) session.ApprovalOutcome {
	return broker.outcome
}

type contextBroker struct{ contexts chan context.Context }

func (broker contextBroker) Ask(ctx context.Context, _ Question) session.ApprovalOutcome {
	broker.contexts <- ctx
	return session.ApprovalAllowedOnce
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

func approvalRequest(journal appTool.Journal) appTool.ApprovalRequest {
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

func types(records []session.Record) []session.RecordType {
	kinds := make([]session.RecordType, len(records))
	for index, record := range records {
		kinds[index] = record.Type
	}
	return kinds
}

func TestService_CleanupCancelsInFlightAppends(t *testing.T) {
	for _, test := range []struct {
		name string
		held session.RecordType
		call func(*Service, *lockedJournal) error
	}{
		{"policy", session.RecordApprovalPolicy, func(service *Service, journal *lockedJournal) error {
			return service.SetPolicy(context.Background(), "s", journal, session.ApprovalNever)
		}},
		// A question that never committed needs no outcome.
		{"question", session.RecordApprovalAsked, func(service *Service, journal *lockedJournal) error {
			_, err := service.Decide(context.Background(), approvalRequest(journal))
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, scope := startApproval(t)
			entered, release := make(chan struct{}), make(chan struct{})
			journal := &lockedJournal{holdFor: test.held, hold: func(ctx context.Context) error {
				close(entered)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			}}
			result := make(chan error, 1)
			go func() { result <- test.call(service, journal) }()
			<-entered
			// Close runs while the append is held; nothing else releases it.
			if err := scope.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			close(release)
			err := <-result
			if got := journal.snapshot(); len(got) != 0 || service.policyCount() != 0 {
				t.Fatalf("a call committed after cleanup returned: records %v, policies %d", types(got), service.policyCount())
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("in-flight call = %v", err)
			}
		})
	}
}

func TestService_CleanupWaitsForInFlightPolicyChange(t *testing.T) {
	service, scope := startApproval(t)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// The append observes cancellation but, like a write already past its
	// last check, still commits once released.
	journal := &lockedJournal{holdFor: session.RecordApprovalPolicy, hold: func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil
	}}
	result := make(chan error, 1)
	go func() { result <- service.SetPolicy(context.Background(), "s", journal, session.ApprovalNever) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not cancel the in-flight policy change")
	}
	if err := service.SetPolicy(t.Context(), "other", &lockedJournal{}, session.ApprovalNever); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("policy change during cleanup = %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("cleanup returned with a policy change in flight: %v", err)
	default:
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("in-flight policy change = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	// The committed change landed before cleanup forgot every policy.
	if got := journal.snapshot(); len(got) != 1 || service.policyCount() != 0 {
		t.Fatalf("records %v, policies %d", types(got), service.policyCount())
	}
}

func TestService_CleanupSettlesPendingDecisions(t *testing.T) {
	service, scope := startApproval(t)
	broker := heldBroker{entered: make(chan struct{}), release: make(chan struct{}), outcome: session.ApprovalAllowedOnce}
	t.Cleanup(func() { close(broker.release) })
	if err := service.RegisterBroker(broker, &plugin.Scope{}); err != nil {
		t.Fatal(err)
	}
	journal := &lockedJournal{}
	type decision struct {
		outcome session.ApprovalOutcome
		err     error
	}
	result := make(chan decision, 1)
	go func() {
		outcome, err := service.Decide(context.Background(), approvalRequest(journal))
		result <- decision{outcome, err}
	}()
	<-broker.entered
	// Close runs while the operator has not answered; only cleanup's
	// cancellation ends the question.
	if err := scope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := journal.snapshot()
	if len(got) != 2 || got[0].Type != session.RecordApprovalAsked || got[1].Type != session.RecordApprovalDecided {
		t.Fatalf("cleanup returned before the decision was paired: %v", types(got))
	}
	if data := got[1].Approval; data.ID != got[0].Approval.ID || data.Outcome != session.ApprovalCancelled || data.Source != "cancellation" {
		t.Fatalf("decision = %+v", data)
	}
	if settled := <-result; settled.err != nil || settled.outcome != session.ApprovalCancelled {
		t.Fatalf("pending decision = %s, %v", settled.outcome, settled.err)
	}
}

func TestService_CleanupDuringDecisionCommit(t *testing.T) {
	service, scope := startApproval(t)
	cleanupEntered := make(chan struct{})
	if err := scope.Defer(func(context.Context) error {
		close(cleanupEntered)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	broker := contextBroker{contexts: make(chan context.Context, 1)}
	brokerScope := &plugin.Scope{}
	t.Cleanup(func() { _ = brokerScope.Close(context.Background()) })
	if err := service.RegisterBroker(broker, brokerScope); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var callContext context.Context
	var stoppedDuringCommit bool
	journal := &lockedJournal{holdFor: session.RecordApprovalDecided, hold: func(context.Context) error {
		close(entered)
		<-release
		stoppedDuringCommit = callContext.Err() != nil
		return nil
	}}
	type decision struct {
		outcome session.ApprovalOutcome
		err     error
	}
	result := make(chan decision, 1)
	go func() {
		outcome, err := service.Decide(t.Context(), approvalRequest(journal))
		result <- decision{outcome, err}
	}()
	<-entered
	callContext = <-broker.contexts
	// Only Decide can own the mutex here. If it leaves the commit unlocked,
	// let real cleanup cancel the call before releasing the append. Otherwise
	// the commit precedes stopping, and cleanup must join that same outcome.
	commitLocked := !service.mu.TryLock()
	if !commitLocked {
		service.mu.Unlock()
	}
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	<-cleanupEntered
	if !commitLocked {
		<-callContext.Done()
	}
	select {
	case err := <-closed:
		t.Errorf("cleanup returned during decided append: %v", err)
		closed <- err
	default:
	}
	close(release)
	settled := <-result
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	got := journal.snapshot()
	if len(got) != 2 || got[0].Type != session.RecordApprovalAsked || got[1].Type != session.RecordApprovalDecided {
		t.Fatalf("cleanup returned before the decision was paired: %v", types(got))
	}
	data := got[1].Approval
	want, source := session.ApprovalAllowedOnce, "operator"
	if stoppedDuringCommit {
		want, source = session.ApprovalCancelled, "cancellation"
	}
	if settled.err != nil || settled.outcome != want || data.Outcome != settled.outcome || data.Source != source || data.ID != got[0].Approval.ID {
		t.Fatalf("cleanup during decided append: commitLocked=%t outcome=%s err=%v durable=%+v; want %s/%s", commitLocked, settled.outcome, settled.err, data, want, source)
	}
	if t.Context().Err() != nil {
		t.Fatal("approval cleanup cancelled the caller")
	}
	if _, err := service.Decide(t.Context(), approvalRequest(journal)); !errors.Is(err, ErrNotRunning) || len(journal.snapshot()) != 2 {
		t.Fatalf("stopped service admitted a decision: %v", err)
	}
}
