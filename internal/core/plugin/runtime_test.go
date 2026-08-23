package plugin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type testPlugin struct {
	id    string
	start func(context.Context, *Scope) error
}

func (candidate testPlugin) ID() string {
	return candidate.id
}

func (candidate testPlugin) Start(ctx context.Context, scope *Scope) error {
	return candidate.start(ctx, scope)
}

func TestNewRejectsInvalidPlugins(t *testing.T) {
	t.Parallel()

	validStart := func(context.Context, *Scope) error { return nil }
	var typedNil *testPlugin
	tests := []struct {
		name    string
		plugins []Plugin
	}{
		{name: "nil", plugins: []Plugin{nil}},
		{name: "typed nil", plugins: []Plugin{typedNil}},
		{name: "empty ID", plugins: []Plugin{testPlugin{start: validStart}}},
		{name: "untrimmed ID", plugins: []Plugin{testPlugin{id: " session ", start: validStart}}},
		{
			name: "duplicate ID",
			plugins: []Plugin{
				testPlugin{id: "session", start: validStart},
				testPlugin{id: "session", start: validStart},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(test.plugins...); !errors.Is(err, ErrInvalidPlugin) {
				t.Fatalf("New() error = %v, want ErrInvalidPlugin", err)
			}
		})
	}
}

func TestRuntimeLifecycle(t *testing.T) {
	t.Parallel()

	var events []string
	makePlugin := func(id string) Plugin {
		return testPlugin{id: id, start: func(_ context.Context, scope *Scope) error {
			events = append(events, "start "+id)
			return scope.Defer(func(context.Context) error {
				events = append(events, "stop "+id)
				return nil
			})
		}}
	}
	runtime, err := New(makePlugin("session"), makePlugin("loop"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := runtime.State(); got != StateNew {
		t.Fatalf("initial State() = %q, want %q", got, StateNew)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := runtime.State(); got != StateRunning {
		t.Fatalf("running State() = %q, want %q", got, StateRunning)
	}
	if err := runtime.Start(context.Background()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second Start() error = %v, want ErrInvalidState", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if got := runtime.State(); got != StateStopped {
		t.Fatalf("stopped State() = %q, want %q", got, StateStopped)
	}
	want := []string{"start session", "start loop", "stop loop", "stop session"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRuntimeStartupFailureRollsBack(t *testing.T) {
	t.Parallel()

	startError := errors.New("provider unavailable")
	failedCleanupError := errors.New("failed plugin cleanup")
	mountedCleanupError := errors.New("mounted plugin cleanup")
	var events []string
	first := testPlugin{id: "session", start: func(_ context.Context, scope *Scope) error {
		events = append(events, "start session")
		return scope.Defer(func(context.Context) error {
			events = append(events, "stop session")
			return mountedCleanupError
		})
	}}
	second := testPlugin{id: "provider", start: func(_ context.Context, scope *Scope) error {
		events = append(events, "start provider")
		if err := scope.Defer(func(context.Context) error {
			events = append(events, "stop provider")
			return failedCleanupError
		}); err != nil {
			return err
		}
		return startError
	}}
	runtime, err := New(first, second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	err = runtime.Start(context.Background())
	for _, want := range []error{startError, failedCleanupError, mountedCleanupError} {
		if !errors.Is(err, want) {
			t.Errorf("Start() error = %v, want joined %v", err, want)
		}
	}
	if !strings.Contains(err.Error(), `start plugin "provider"`) || !strings.Contains(err.Error(), `stop plugin "session"`) {
		t.Fatalf("Start() error lacks plugin identity: %v", err)
	}
	if got := runtime.State(); got != StateStopped {
		t.Fatalf("State() = %q, want %q", got, StateStopped)
	}
	wantEvents := []string{"start session", "start provider", "stop provider", "stop session"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestRuntimeShutdownBeforeStart(t *testing.T) {
	t.Parallel()

	runtime, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := runtime.Start(context.Background()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Start() after shutdown error = %v, want ErrInvalidState", err)
	}
}

func TestRuntimeShutdownRejectsTransitionalState(t *testing.T) {
	t.Parallel()

	for _, state := range []State{StateStarting, StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			runtime, err := New()
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			runtime.setState(state)
			if err := runtime.Shutdown(context.Background()); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("Shutdown() error = %v, want ErrInvalidState", err)
			}
		})
	}
}
