package retry

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
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

// lockedJournal records appends made on another goroutine. hold, when set,
// runs inside every Append before it records and can fail it.
type lockedJournal struct {
	mu      sync.Mutex
	records []session.RecordType
	hold    func(context.Context) error
}

func (journal *lockedJournal) Append(ctx context.Context, record session.Record) (session.Event, error) {
	if journal.hold != nil {
		if err := journal.hold(ctx); err != nil {
			return session.Event{}, err
		}
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.records = append(journal.records, record.Type)
	return session.Event{Sequence: uint64(len(journal.records)), Record: record}, nil
}

func (journal *lockedJournal) types() []session.RecordType {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return slices.Clone(journal.records)
}

func failServer() (llm.Completion, bool, error) {
	return llm.Completion{}, false, &llm.Error{Code: llm.ErrorServer, Provider: "p"}
}

func startRetry(t *testing.T) (*Service, *plugin.Scope) {
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
	return service, scope
}

func TestServiceRetriesAndStops(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil settings")
	}
	service, _ := startRetry(t)
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
	configuration, settingsScope := retrySettings(t)
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
	_ = settingsScope.Close(context.Background())
	journal := &retryJournal{}
	_, err = service.Do(context.Background(), journal, 1, 1, "p", "k", failServer)
	if !errors.Is(err, settings.ErrNotRunning) || len(journal.records) != 0 {
		t.Fatalf("stopped settings=%v records=%#v", err, journal.records)
	}
	_ = scope.Close(context.Background())
	if _, _, err := service.begin(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("closed decision=%v", err)
	}
}

func TestService_CleanupCancelsRetryWait(t *testing.T) {
	service, scope := startRetry(t)
	entered, release := make(chan struct{}), make(chan struct{})
	service.sleep = func(ctx context.Context, _ time.Duration) error {
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	journal := &lockedJournal{}
	var attempts atomic.Int32
	result := make(chan error, 1)
	go func() {
		_, err := service.Do(context.Background(), journal, 1, 1, "p", "k", func() (llm.Completion, bool, error) {
			attempts.Add(1)
			return failServer()
		})
		result <- err
	}()
	<-entered
	// Close runs while Do waits out its delay; nothing else ends that wait.
	if err := scope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(release)
	err := <-result
	if got := journal.types(); !slices.Equal(got, []session.RecordType{session.RecordRetry}) || attempts.Load() != 1 {
		t.Fatalf("a retry continued after cleanup returned: records %v, attempts %d", got, attempts.Load())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight retry = %v", err)
	}
}

func TestService_CleanupWaitsForRetryRecords(t *testing.T) {
	service, scope := startRetry(t)
	service.sleep = sleep
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	journal := &lockedJournal{}
	// The append observes cancellation but, like a write already past its
	// last check, still commits once released.
	journal.hold = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := service.Do(context.Background(), journal, 1, 1, "p", "k", failServer)
		result <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not cancel the in-flight retry record")
	}
	select {
	case err := <-closed:
		t.Fatalf("cleanup returned with a retry in flight: %v", err)
	default:
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight retry = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if got := journal.types(); !slices.Equal(got, []session.RecordType{session.RecordRetry}) {
		t.Fatalf("records = %v", got)
	}
}

func TestService_CleanupLeavesModelAttemptsToTheCaller(t *testing.T) {
	service, scope := startRetry(t)
	entered, release := make(chan struct{}), make(chan struct{})
	journal := &lockedJournal{}
	result := make(chan error, 1)
	go func() {
		_, err := service.Do(context.Background(), journal, 1, 1, "p", "k", func() (llm.Completion, bool, error) {
			close(entered)
			<-release
			return failServer()
		})
		result <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("cleanup waited for a model attempt the caller owns")
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrNotRunning) {
		t.Fatalf("attempt failing after cleanup = %v", err)
	}
	if got := journal.types(); len(got) != 0 {
		t.Fatalf("records after cleanup = %v", got)
	}
}
