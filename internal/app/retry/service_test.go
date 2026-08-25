package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type retryJournal struct {
	records []session.Record
	err     error
	failAt  int
}

func (journal *retryJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	if journal.err != nil && (journal.failAt == 0 || journal.failAt == len(journal.records)+1) {
		return session.Event{}, journal.err
	}
	journal.records = append(journal.records, record)
	return session.Event{Sequence: uint64(len(journal.records)), Record: record}, nil
}

func retrySettings(t *testing.T) (*settings.Service, *plugin.Scope) {
	t.Helper()
	service := settings.New()
	scope := &plugin.Scope{}
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, scope
}

func startRetry(t *testing.T) *Service {
	t.Helper()
	configuration, _ := retrySettings(t)
	service, err := New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	service.sleep = func(context.Context, time.Duration) error { return nil }
	service.jitter = func() float64 { return .5 }
	if service.ID() != "retry" || service.Start(context.Background(), scope) != nil {
		t.Fatal("start")
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service
}

func TestServiceRetriesAndStops(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil settings")
	}
	service := startRetry(t)
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	configuration, _ := retrySettings(t)
	closedService, _ := New(configuration)
	if err := closedService.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
	if service.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start")
	}
	journal := &retryJournal{}
	attempts := 0
	completion, err := service.Do(context.Background(), journal, 1, 1, "openai", "route", func() (llm.Completion, bool, error) {
		attempts++
		if attempts == 1 {
			return llm.Completion{}, false, &llm.Error{Code: llm.ErrorServer, Provider: "openai"}
		}
		return llm.Completion{Stop: "done"}, false, nil
	})
	if err != nil || completion.Stop != "done" || attempts != 2 || len(journal.records) != 2 || journal.records[0].Type != session.RecordRetry || journal.records[1].Type != session.RecordRetryStarted {
		t.Fatalf("completion=%#v attempts=%d records=%#v err=%v", completion, attempts, journal.records, err)
	}
	for _, test := range []struct {
		name    string
		emitted bool
		err     error
	}{
		{"emitted", true, &llm.Error{Code: llm.ErrorServer, Provider: "p"}},
		{"invalid", false, &llm.Error{Code: llm.ErrorInvalid, Provider: "p"}},
		{"plain", false, errors.New("plain")},
	} {
		t.Run(test.name, func(t *testing.T) {
			count := 0
			_, gotErr := service.Do(context.Background(), &retryJournal{}, 1, 1, "p", "k", func() (llm.Completion, bool, error) { count++; return llm.Completion{}, test.emitted, test.err })
			if gotErr == nil || count != 1 {
				t.Fatalf("err=%v count=%d", gotErr, count)
			}
		})
	}
	if _, err := service.Do(context.Background(), nil, 0, 0, "", "", nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid=%v", err)
	}
	journal.err = errors.New("append")
	_, err = service.Do(context.Background(), journal, 1, 1, "p", "k", func() (llm.Completion, bool, error) {
		return llm.Completion{}, false, &llm.Error{Code: llm.ErrorServer, Provider: "p"}
	})
	if err == nil {
		t.Fatal("append error lost")
	}
	journal = &retryJournal{err: errors.New("append started"), failAt: 2}
	_, err = service.Do(context.Background(), journal, 1, 1, "p", "k", func() (llm.Completion, bool, error) {
		return llm.Completion{}, false, &llm.Error{Code: llm.ErrorServer, Provider: "p"}
	})
	if err == nil || len(journal.records) != 1 {
		t.Fatalf("retry-started append error=%v records=%#v", err, journal.records)
	}
	inactive, _ := New(configuration)
	_, err = inactive.Do(context.Background(), &retryJournal{}, 1, 1, "p", "k", func() (llm.Completion, bool, error) {
		return llm.Completion{}, false, &llm.Error{Code: llm.ErrorServer, Provider: "p"}
	})
	if err == nil {
		t.Fatal("inactive policy error missing")
	}
}

func TestRetryClassificationDelayAndSleep(t *testing.T) {
	for _, code := range []llm.ErrorCode{llm.ErrorEmptyResponse, llm.ErrorRateLimit, llm.ErrorServer, llm.ErrorTimeout, llm.ErrorTransport} {
		got, _, retryable := classify(&llm.Error{Code: code, RetryAfterMS: 10}, "normal")
		if got != code || !retryable {
			t.Fatalf("code=%s retryable=%t", got, retryable)
		}
	}
	if _, _, retryable := classify(&llm.Error{Code: llm.ErrorProtocol}, "always"); !retryable {
		t.Fatal("always did not retry protocol")
	}
	if _, _, retryable := classify(&llm.Error{Code: llm.ErrorUnauthorized}, "always"); retryable {
		t.Fatal("always retried auth")
	}
	if _, _, retryable := classify(errors.New("x"), "always"); !retryable {
		t.Fatal("always did not retry plain")
	}
	policy := settings.Retry{InitialDelayMS: 500, MaxDelayMS: 1000, JitterRatio: .1}
	if got := retryDelay(policy, 3, 900, 0); got <= 0 || got > time.Second {
		t.Fatalf("delay=%s", got)
	}
	if got := retryDelay(settings.Retry{InitialDelayMS: 900, MaxDelayMS: 1000}, 2, 0, .5); got != time.Second {
		t.Fatalf("exponential cap=%s", got)
	}
	if got := retryDelay(policy, 1, 5000, 1); got != 1100*time.Millisecond && got > time.Second {
		t.Fatalf("capped delay=%s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(sleep(ctx, time.Hour), context.Canceled) {
		t.Fatal("sleep cancellation")
	}
	if err := sleep(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

func TestServiceAfterCloseAndSleepFailure(t *testing.T) {
	configuration, _ := retrySettings(t)
	service, _ := New(configuration)
	scope := &plugin.Scope{}
	service.sleep = func(context.Context, time.Duration) error { return errors.New("sleep") }
	service.jitter = func() float64 { return .5 }
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	_, err := service.Do(context.Background(), &retryJournal{}, 1, 1, "p", "k", func() (llm.Completion, bool, error) {
		return llm.Completion{}, false, &llm.Error{Code: llm.ErrorServer, Provider: "p"}
	})
	if err == nil {
		t.Fatal("sleep error lost")
	}
	_ = scope.Close(context.Background())
	if _, err := service.policy(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("closed policy=%v", err)
	}
}
