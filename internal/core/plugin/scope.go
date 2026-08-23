// Package plugin owns the lifecycle contract shared by every runtime component.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

var (
	// ErrScopeClosed reports a registration attempted after cleanup began.
	ErrScopeClosed = errors.New("plugin scope is closed")
	// ErrNilCleanup reports an invalid nil cleanup registration.
	ErrNilCleanup = errors.New("plugin cleanup is nil")
)

// Cleanup releases one plugin-owned effect and waits for it to become quiet.
type Cleanup func(context.Context) error

// Scope owns the reversible effects registered by one plugin.
//
// The zero value is ready for use. Close runs cleanups in reverse registration
// order, attempts every cleanup, and is idempotent.
type Scope struct {
	mu       sync.Mutex
	cleanups []Cleanup
	closed   bool
}

// Defer registers one cleanup before the scope starts closing.
func (scope *Scope) Defer(cleanup Cleanup) error {
	if cleanup == nil {
		return ErrNilCleanup
	}

	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return ErrScopeClosed
	}
	scope.cleanups = append(scope.cleanups, cleanup)
	return nil
}

// Close runs all registered cleanups in reverse order and joins their errors.
func (scope *Scope) Close(ctx context.Context) error {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return nil
	}
	scope.closed = true
	cleanups := scope.cleanups
	scope.cleanups = nil
	scope.mu.Unlock()

	var cleanupErrors []error
	for index, cleanup := range slices.Backward(cleanups) {
		if err := cleanup(ctx); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup %d: %w", index, err))
		}
	}
	return errors.Join(cleanupErrors...)
}
