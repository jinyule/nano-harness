package agent

import (
	"context"
	"sync"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// Bootstrap creates one configured root agent inside the plugin composition.
type Bootstrap struct {
	registry *Registry
	request  CreateRequest

	mu      sync.RWMutex
	started bool
	active  bool
	agent   *Agent
}

// NewBootstrap validates the root request without opening its transcript.
func NewBootstrap(registry *Registry, request CreateRequest) (*Bootstrap, error) {
	if registry == nil || request.ParentID != "" || request.Depth != 0 {
		return nil, ErrInvalidConfig
	}
	request.Mode = "continuable"
	return &Bootstrap{registry: registry, request: request}, nil
}

// ID returns the stable root-agent plugin identity.
func (*Bootstrap) ID() string { return "root-agent" }

// Start creates the root and publishes it until scope cleanup. The registry
// owns the root's lifetime and closes it together with every other agent,
// so the root never stops while children can still open turns.
func (bootstrap *Bootstrap) Start(ctx context.Context, scope *plugin.Scope) error {
	bootstrap.mu.Lock()
	if bootstrap.started {
		bootstrap.mu.Unlock()
		return ErrInvalidConfig
	}
	bootstrap.started = true
	bootstrap.mu.Unlock()
	root, err := bootstrap.registry.Create(ctx, bootstrap.request)
	if err != nil {
		return err
	}
	if err := scope.Defer(func(context.Context) error {
		bootstrap.mu.Lock()
		bootstrap.active = false
		bootstrap.agent = nil
		bootstrap.mu.Unlock()
		return nil
	}); err != nil {
		_ = bootstrap.registry.Close(context.WithoutCancel(ctx), root.Status().SessionID)
		return err
	}
	bootstrap.mu.Lock()
	bootstrap.agent, bootstrap.active = root, true
	bootstrap.mu.Unlock()
	return nil
}

// Agent returns the active root handle for later-starting plugins.
func (bootstrap *Bootstrap) Agent() (Controller, error) {
	bootstrap.mu.RLock()
	defer bootstrap.mu.RUnlock()
	if !bootstrap.active || bootstrap.agent == nil {
		return nil, ErrNotRunning
	}
	return bootstrap.agent, nil
}
