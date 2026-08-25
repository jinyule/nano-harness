// Package approval owns fail-closed, durable, one-shot tool decisions.
package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidRequest identifies an approval operation that cannot be evaluated safely.
	ErrInvalidRequest = errors.New("invalid approval request")
	// ErrNotRunning indicates the approval service has not started or has stopped.
	ErrNotRunning = errors.New("approval service is not running")
)

// Question is the secret-free prompt shown to a local operator.
type Question struct {
	ID       string
	Session  string
	ToolName string
	CallID   string
	Reason   string
}

// Broker presents one question and returns a stable one-shot outcome.
type Broker interface {
	Ask(context.Context, Question) session.ApprovalOutcome
}

// Service owns the active UI broker and per-session policies.
type Service struct {
	mu       sync.RWMutex
	started  bool
	active   bool
	broker   Broker
	policies map[string]session.ApprovalPolicy
	nextID   atomic.Uint64
}

// New constructs an inactive approval service.
func New() *Service { return &Service{policies: map[string]session.ApprovalPolicy{}} }

// ID returns the stable plugin identity.
func (*Service) ID() string { return "approval" }

// Start activates the service and owns every published decision surface.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidRequest
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		service.broker = nil
		service.policies = map[string]session.ApprovalPolicy{}
		service.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active = true, true
	return nil
}

// RegisterBroker publishes a local decision surface until the caller's scope closes.
func (service *Service) RegisterBroker(broker Broker, scope *plugin.Scope) error {
	if broker == nil || scope == nil {
		return ErrInvalidRequest
	}
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return ErrNotRunning
	}
	if service.broker != nil {
		service.mu.Unlock()
		return ErrInvalidRequest
	}
	service.broker = broker
	service.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		if service.broker == broker {
			service.broker = nil
		}
		service.mu.Unlock()
		return nil
	}); err != nil {
		service.mu.Lock()
		service.broker = nil
		service.mu.Unlock()
		return err
	}
	return nil
}

// Restore derives the current policy from durable metadata during resume.
func (service *Service) Restore(sessionID string, events []session.Event) error {
	policy := session.ApprovalAsk
	for _, event := range events {
		if event.Record.Type == session.RecordApprovalPolicy {
			policy = event.Record.Approval.Policy
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return ErrNotRunning
	}
	service.policies[sessionID] = policy
	return nil
}

// SetPolicy commits a new root-session policy before publishing it.
func (service *Service) SetPolicy(ctx context.Context, sessionID string, journal tool.Journal, policy session.ApprovalPolicy) error {
	if sessionID == "" || journal == nil || policy != session.ApprovalAsk && policy != session.ApprovalNever {
		return ErrInvalidRequest
	}
	service.mu.RLock()
	active := service.active
	service.mu.RUnlock()
	if !active {
		return ErrNotRunning
	}
	if _, err := journal.Append(ctx, session.Record{Type: session.RecordApprovalPolicy, Approval: &session.ApprovalData{Policy: policy}}); err != nil {
		return err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.policies[sessionID] = policy
	return nil
}

// Decide records a paired question and outcome. Delegated sessions can never elevate.
func (service *Service) Decide(ctx context.Context, request tool.ApprovalRequest) (session.ApprovalOutcome, error) {
	if request.SessionID == "" || request.Turn == 0 || request.Step == 0 || request.Call.ID == "" || request.Call.Name == "" || request.Reason == "" || request.Journal == nil {
		return "", ErrInvalidRequest
	}
	service.mu.RLock()
	if !service.active {
		service.mu.RUnlock()
		return "", ErrNotRunning
	}
	policy := service.policies[request.SessionID]
	if policy == "" {
		policy = session.ApprovalAsk
	}
	broker := service.broker
	service.mu.RUnlock()
	id := fmt.Sprintf("approval-%d", service.nextID.Add(1))
	asked := session.Record{Type: session.RecordApprovalAsked, Turn: request.Turn, Step: request.Step, Approval: &session.ApprovalData{
		ID: id, ToolName: request.Call.Name, CallID: request.Call.ID, Reason: request.Reason,
	}}
	if _, err := request.Journal.Append(ctx, asked); err != nil {
		return "", err
	}
	outcome := session.ApprovalUnavailable
	source := "unavailable"
	switch {
	case request.Delegated:
		source = "delegation"
	case policy == session.ApprovalNever:
		outcome, source = session.ApprovalRejected, "policy"
	case ctx.Err() != nil:
		outcome, source = session.ApprovalCancelled, "cancellation"
	case broker == nil:
	default:
		outcome, source = broker.Ask(ctx, Question{ID: id, Session: request.SessionID, ToolName: request.Call.Name, CallID: request.Call.ID, Reason: request.Reason}), "operator"
		if outcome != session.ApprovalAllowedOnce && outcome != session.ApprovalRejected && outcome != session.ApprovalCancelled {
			outcome, source = session.ApprovalUnavailable, "unavailable"
		}
	}
	commitContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := request.Journal.Append(commitContext, session.Record{Type: session.RecordApprovalDecided, Turn: request.Turn, Step: request.Step, Approval: &session.ApprovalData{ID: id, Outcome: outcome, Source: source}})
	return outcome, err
}
