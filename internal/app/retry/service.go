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

	mu      sync.RWMutex
	started bool
	active  bool
}

// New constructs an inactive retry service.
func New(configuration *settings.Service) (*Service, error) {
	if configuration == nil {
		return nil, ErrInvalidConfig
	}
	return &Service{settings: configuration, sleep: sleep, jitter: rand.Float64}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "retry" }

// Start activates decisions until scope cleanup.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidConfig
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active = false
		service.mu.Unlock()
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
		policy, policyErr := service.policy()
		if policyErr != nil {
			return llm.Completion{}, errors.Join(err, policyErr)
		}
		code, retryAfter, retryable := classify(err, policy.Mode)
		if emitted || !retryable || retryNumber >= policy.MaxRetries {
			return llm.Completion{}, err
		}
		number := retryNumber + 1
		delay := retryDelay(policy, number, retryAfter, service.jitter())
		id := fmt.Sprintf("retry-%d-%d-%d", turn, step, number)
		data := &session.RetryData{
			ID: id, Provider: provider, PolicyKey: policyKey, Attempt: number,
			MaxRetries: policy.MaxRetries, DelayMS: delay.Milliseconds(), Failure: string(code),
		}
		if _, appendErr := journal.Append(ctx, session.Record{Type: session.RecordRetry, Turn: turn, Step: step, Retry: data}); appendErr != nil {
			return llm.Completion{}, errors.Join(err, appendErr)
		}
		if sleepErr := service.sleep(ctx, delay); sleepErr != nil {
			return llm.Completion{}, sleepErr
		}
		if _, appendErr := journal.Append(ctx, session.Record{Type: session.RecordRetryStarted, Turn: turn, Step: step, Retry: &session.RetryData{ID: id, Attempt: number}}); appendErr != nil {
			return llm.Completion{}, appendErr
		}
	}
}

func (service *Service) policy() (settings.Retry, error) {
	service.mu.RLock()
	active := service.active
	service.mu.RUnlock()
	if !active {
		return settings.Retry{}, ErrNotRunning
	}
	document, _, err := service.settings.Snapshot()
	return document.Retry, err
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
