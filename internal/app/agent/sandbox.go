package agent

import (
	"context"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// SandboxContext contributes Base's sandbox:policy as durable runtime context.
// It owns only its scoped registration; policy remains in each session's log.
type SandboxContext struct {
	engine    *Engine
	workspace string
}

// NewSandboxContext constructs the inert context plugin for this composition.
func NewSandboxContext(engine *Engine, workspace string) *SandboxContext {
	return &SandboxContext{engine: engine, workspace: workspace}
}

// ID returns the stable plugin identity.
func (*SandboxContext) ID() string { return "sandbox-policy" }

// Start registers context until scope cleanup withdraws the contribution.
func (provider *SandboxContext) Start(_ context.Context, scope *plugin.Scope) error {
	return provider.engine.RegisterContext(provider, scope)
}

// StepContext rebuilds the sandbox:policy section from authoritative mode
// records. The engine owns snapshot aggregation, comparison, and publication.
func (provider *SandboxContext) StepContext(_ context.Context, request ContextRequest) (ContextContribution, error) {
	return ContextContribution{Sections: []ContextSection{{Order: OrderSandboxPolicy, Text: session.SandboxPolicyText(session.EffectiveSandbox(request.Events), provider.workspace)}}}, nil
}

// SetSandboxMode is the human-only entry point for a live root. Its event is
// committed immediately, including during a step; subsequent operations see it.
// Existing processes retain their launch profile. Approval policy is independent.
func (registry *Registry) SetSandboxMode(ctx context.Context, sessionID string, mode session.SandboxMode) error {
	current, err := registry.Find(sessionID)
	if err != nil {
		return err
	}
	if current.delegated || !mode.Valid() {
		return ErrInvalidConfig
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if !current.active {
		return ErrNotRunning
	}
	_, err = current.journal.Append(ctx, session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: mode}})
	return err
}
