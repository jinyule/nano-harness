package plugin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

var (
	// ErrInvalidPlugin reports a nil, unnamed, or duplicate plugin.
	ErrInvalidPlugin = errors.New("invalid plugin")
	// ErrInvalidState reports a lifecycle operation attempted in the wrong state.
	ErrInvalidState = errors.New("invalid plugin runtime state")
)

// Plugin is the lifecycle unit implemented by every runtime component.
// Start must register every side effect with scope before returning success.
type Plugin interface {
	ID() string
	Start(ctx context.Context, scope *Scope) error
}

// State is the observable lifecycle state of a Runtime.
type State string

const (
	// StateNew means no plugin has started.
	StateNew State = "new"
	// StateStarting means plugins are starting in declaration order.
	StateStarting State = "starting"
	// StateRunning means every plugin started successfully.
	StateRunning State = "running"
	// StateStopping means plugin effects are closing in reverse order.
	StateStopping State = "stopping"
	// StateStopped means cleanup finished or startup rolled back.
	StateStopped State = "stopped"
)

type mountedPlugin struct {
	id    string
	scope *Scope
}

// Runtime starts an ordered plugin composition and owns its cleanup.
// Construct a Runtime with New; its zero value is not a valid composition.
type Runtime struct {
	lifecycle sync.Mutex
	stateMu   sync.RWMutex
	state     State
	plugins   []Plugin
	mounted   []mountedPlugin
}

// New validates a complete, ordered plugin composition.
func New(plugins ...Plugin) (*Runtime, error) {
	seen := make(map[string]struct{}, len(plugins))
	for index, candidate := range plugins {
		if isNilPlugin(candidate) {
			return nil, fmt.Errorf("%w at index %d: nil", ErrInvalidPlugin, index)
		}
		id := candidate.ID()
		if id == "" || id != strings.TrimSpace(id) {
			return nil, fmt.Errorf("%w at index %d: ID must be non-empty and trimmed", ErrInvalidPlugin, index)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate ID %q", ErrInvalidPlugin, id)
		}
		seen[id] = struct{}{}
	}

	return &Runtime{state: StateNew, plugins: append([]Plugin(nil), plugins...)}, nil
}

func isNilPlugin(candidate Plugin) bool {
	if candidate == nil {
		return true
	}
	value := reflect.ValueOf(candidate)
	kind := value.Kind()
	canBeNil := kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface ||
		kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice
	return canBeNil && value.IsNil()
}

// State returns the current lifecycle state.
func (runtime *Runtime) State() State {
	runtime.stateMu.RLock()
	defer runtime.stateMu.RUnlock()
	return runtime.state
}

// Start starts plugins in declaration order and rolls back on the first error.
func (runtime *Runtime) Start(ctx context.Context) error {
	runtime.lifecycle.Lock()
	defer runtime.lifecycle.Unlock()

	if state := runtime.State(); state != StateNew {
		return fmt.Errorf("%w: start from %s", ErrInvalidState, state)
	}
	runtime.setState(StateStarting)

	mounted := make([]mountedPlugin, 0, len(runtime.plugins))
	for _, candidate := range runtime.plugins {
		scope := &Scope{}
		if err := candidate.Start(ctx, scope); err != nil {
			startError := fmt.Errorf("start plugin %q: %w", candidate.ID(), err)
			rollbackContext := context.WithoutCancel(ctx)
			failedCleanup := scope.Close(rollbackContext)
			mountedCleanup := closeMounted(rollbackContext, mounted)
			runtime.setState(StateStopped)
			return errors.Join(startError, failedCleanup, mountedCleanup)
		}
		mounted = append(mounted, mountedPlugin{id: candidate.ID(), scope: scope})
	}

	runtime.mounted = mounted
	runtime.setState(StateRunning)
	return nil
}

// Shutdown closes every mounted plugin in reverse order and waits for cleanup.
func (runtime *Runtime) Shutdown(ctx context.Context) error {
	runtime.lifecycle.Lock()
	defer runtime.lifecycle.Unlock()

	switch runtime.State() {
	case StateStopped:
		return nil
	case StateNew:
		runtime.setState(StateStopped)
		return nil
	case StateRunning:
		runtime.setState(StateStopping)
	case StateStarting, StateStopping:
		return fmt.Errorf("%w: shutdown from %s", ErrInvalidState, runtime.State())
	}

	err := closeMounted(ctx, runtime.mounted)
	runtime.mounted = nil
	runtime.setState(StateStopped)
	return err
}

func (runtime *Runtime) setState(state State) {
	runtime.stateMu.Lock()
	runtime.state = state
	runtime.stateMu.Unlock()
}

func closeMounted(ctx context.Context, mounted []mountedPlugin) error {
	var cleanupErrors []error
	for _, candidate := range slices.Backward(mounted) {
		if err := candidate.scope.Close(ctx); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("stop plugin %q: %w", candidate.id, err))
		}
	}
	return errors.Join(cleanupErrors...)
}
