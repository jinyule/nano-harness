// Package retry owns provider-aware model request recovery.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidConfig identifies retry configuration or input that cannot be evaluated.
	ErrInvalidConfig = errors.New("invalid retry configuration")
	// ErrNotRunning indicates the retry service has not started or has stopped.
	ErrNotRunning = errors.New("retry service is not running")
)

// Journal is the durable retry fact boundary.
type Journal interface {
	Append(context.Context, session.Record) (session.Event, error)
}

// Attempt returns whether any model content was committed before failure.
type Attempt func() (llm.Completion, bool, error)

// Service evaluates the current hot policy for each retry decision.
type Service struct {
	settings *settings.Service
	sleep    func(context.Context, time.Duration) error
	jitter   func() float64

	mu      sync.Mutex
	started bool
	active  bool
	// waits cancels and joins each retry decision in flight.
	waits *plugin.Calls
}

// New constructs an inactive retry service.
func New(configuration *settings.Service) (*Service, error) {
	if configuration == nil {
		return nil, ErrInvalidConfig
	}
	service := &Service{settings: configuration, sleep: sleep, jitter: rand.Float64}
	service.waits = plugin.NewCalls(&service.mu)
	return service, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "retry" }

// Start activates decisions until scope cleanup. Cleanup rejects new
// decisions, cancels each one in flight, and returns only after each has
// returned, so no retry record is appended once it returns. A model attempt
// belongs to the caller and is not waited for; its failure after cleanup
// ends Do with ErrNotRunning.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidConfig
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		service.waits.Cancel(nil)
		service.mu.Unlock()
		// A decision may be inside a journal append; the wait is bounded by
		// the journal's cancellation latency.
		service.waits.Wait()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active = true, true
	return nil
}

// Do retries an empty failed stream and records each decision before sleeping.
func (service *Service) Do(ctx context.Context, journal Journal, turn, step uint64, provider, policyKey string, attempt Attempt) (llm.Completion, error) {
	if journal == nil || turn == 0 || step == 0 || provider == "" || policyKey == "" || attempt == nil {
		return llm.Completion{}, ErrInvalidConfig
	}
	for retryNumber := 0; ; retryNumber++ {
		completion, emitted, err := attempt()
		if err == nil {
			return completion, nil
		}
		if err := service.wait(ctx, journal, turn, step, provider, policyKey, retryNumber, emitted, err); err != nil {
			return llm.Completion{}, err
		}
	}
}

// wait decides whether failure, the outcome of attempt retryNumber, is
// retried. If so it records the decision, sleeps the delay, and records the
// retry's start before returning nil; otherwise it returns the error Do
// reports. It runs as one call that cleanup cancels and waits for.
func (service *Service) wait(ctx context.Context, journal Journal, turn, step uint64, provider, policyKey string, retryNumber int, emitted bool, failure error) error {
	ctx, done, err := service.begin(ctx)
	if err != nil {
		return errors.Join(failure, err)
	}
	defer done()
	document, _, err := service.settings.Snapshot()
	if err != nil {
		return errors.Join(failure, err)
	}
	policy := document.Retry
	code, retryAfter, retryable := classify(failure, policy.Mode)
	if emitted || !retryable || retryNumber >= policy.MaxRetries {
		return failure
	}
	number := retryNumber + 1
	delay := retryDelay(policy, number, retryAfter, service.jitter())
	id := fmt.Sprintf("retry-%d-%d-%d", turn, step, number)
	data := &session.RetryData{
		ID: id, Provider: provider, PolicyKey: policyKey, Attempt: number,
		MaxRetries: policy.MaxRetries, DelayMS: delay.Milliseconds(), Failure: string(code),
	}
	if _, err := journal.Append(ctx, session.Record{Type: session.RecordRetry, Turn: turn, Step: step, Retry: data}); err != nil {
		return errors.Join(failure, err)
	}
	if err := service.sleep(ctx, delay); err != nil {
		return err
	}
	_, err = journal.Append(ctx, session.Record{Type: session.RecordRetryStarted, Turn: turn, Step: step, Retry: &session.RetryData{ID: id, Attempt: number}})
	return err
}

// begin admits one retry decision while the service runs and returns its
// context, which cleanup cancels; done must run once the decision returns.
func (service *Service) begin(ctx context.Context) (context.Context, func(), error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return nil, nil, ErrNotRunning
	}
	call, done := service.waits.Admit(ctx)
	return call, done, nil
}

func classify(err error, mode string) (llm.ErrorCode, int64, bool) {
	var failure *llm.Error
	if !errors.As(err, &failure) {
		return llm.ErrorTransport, 0, mode == "always"
	}
	retryable := failure.Code == llm.ErrorEmptyResponse || failure.Code == llm.ErrorRateLimit || failure.Code == llm.ErrorServer || failure.Code == llm.ErrorTimeout || failure.Code == llm.ErrorTransport
	if mode == "always" && failure.Code != llm.ErrorUnauthorized && failure.Code != llm.ErrorInvalid && failure.Code != llm.ErrorContextWindow {
		retryable = true
	}
	return failure.Code, failure.RetryAfterMS, retryable
}

func retryDelay(policy settings.Retry, attempt int, retryAfterMS int64, random float64) time.Duration {
	delay := time.Duration(policy.InitialDelayMS) * time.Millisecond
	for current := 1; current < attempt && delay < time.Duration(policy.MaxDelayMS)*time.Millisecond; current++ {
		delay *= 2
	}
	maximum := time.Duration(policy.MaxDelayMS) * time.Millisecond
	if delay > maximum {
		delay = maximum
	}
	if retryAfter := time.Duration(retryAfterMS) * time.Millisecond; retryAfter > delay {
		delay = min(retryAfter, maximum)
	}
	factor := 1 + (random*2-1)*policy.JitterRatio
	return max(time.Duration(float64(delay)*factor), time.Millisecond)
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
