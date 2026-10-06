package agent

import (
	"context"
	"slices"

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

// StepContext emits a snapshot after user input and before step/start only
// when the last retained snapshot differs. Compaction can remove that snapshot,
// in which case the next step reconstructs it from authoritative mode records.
func (provider *SandboxContext) StepContext(_ context.Context, request ContextRequest) ([]session.Message, error) {
	text := "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n" + session.SandboxPolicyText(session.EffectiveSandbox(request.Events), provider.workspace)
	surface, err := session.Surface(request.Events)
	if err != nil {
		return nil, err
	}
	for _, node := range slices.Backward(surface) {
		if node.Message != nil && node.Message.Source.Kind == "runtime-context" && node.Message.Source.Plugin == "sandbox:policy" {
			if session.Text(*node.Message) == text {
				return nil, nil
			}
			break
		}
	}
	return []session.Message{{Role: session.RoleUser, Source: session.MessageSource{Kind: "runtime-context", Plugin: "sandbox:policy"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}}, nil
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
