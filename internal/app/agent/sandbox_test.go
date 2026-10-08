package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestSandboxContext_RegistrationAndAuthoritativeSection(t *testing.T) {
	h := startEngineHarness(t, 1)
	provider := NewSandboxContext(h.engine, "/workspace")
	if provider.ID() != "sandbox-policy" {
		t.Fatal(provider.ID())
	}
	closed := &plugin.Scope{}
	_ = closed.Close(t.Context())
	if err := provider.Start(t.Context(), closed); !errors.Is(err, plugin.ErrScopeClosed) || len(h.engine.contexts) != 0 {
		t.Fatalf("failed registration: %v", err)
	}
	scope := &plugin.Scope{}
	if err := provider.Start(t.Context(), scope); err != nil || len(h.engine.contexts) != 1 {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	for _, mode := range []session.SandboxMode{session.SandboxWorkspaceWrite, session.SandboxReadOnly, session.SandboxDangerFullAccess} {
		events := []session.Event{{Sequence: 1, Record: session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: mode}}}}
		contribution, err := provider.StepContext(t.Context(), ContextRequest{Events: events})
		if err != nil || len(contribution.Sections) != 1 || contribution.Sections[0] != (ContextSection{Order: OrderSandboxPolicy, Text: session.SandboxPolicyText(mode, "/workspace")}) || len(contribution.Messages) != 0 {
			t.Fatalf("section=%+v err=%v", contribution, err)
		}
	}
	if err := scope.Close(t.Context()); err != nil || len(h.engine.contexts) != 0 {
		t.Fatalf("cleanup=%v", err)
	}
}

func TestRegistry_HumanSandboxModeAndDelegationFailures(t *testing.T) {
	h := startEngineHarness(t, 1)
	repository := newMemoryRepository()
	registry, _ := startRegistry(t, h, repository, newMemoryPolicy())
	if err := registry.SetSandboxMode(t.Context(), "missing", session.SandboxReadOnly); !errors.Is(err, ErrAgentNotFound) {
		t.Fatal(err)
	}
	root, err := registry.Create(t.Context(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetSandboxMode(t.Context(), "root", "invalid"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if err := registry.SetSandboxMode(t.Context(), "root", session.SandboxDangerFullAccess); err != nil {
		t.Fatal(err)
	}
	child, err := registry.Create(t.Context(), CreateRequest{SessionID: "child", ParentID: "root", Depth: 1, Provider: session.SubagentSpawn, Label: "child", Create: true, Sandbox: session.SandboxDangerFullAccess})
	if err != nil {
		t.Fatal(err)
	}
	events, _ := child.Events(t.Context())
	if len(events) != 3 || events[1].Record.Sandbox.Source != "delegation" || session.EffectiveSandbox(events) != session.SandboxDangerFullAccess || events[2].Record.Approval.Policy != session.ApprovalNever {
		t.Fatalf("delegation=%+v", events)
	}
	if err := registry.SetSandboxMode(t.Context(), "child", session.SandboxReadOnly); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	failure := errors.New("append failed")
	repository.logs["root"].appendErr = failure
	if err := registry.SetSandboxMode(t.Context(), "root", session.SandboxReadOnly); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	repository.logs["root"].appendErr = nil
	repository.newLog = func(options transcript.OpenOptions) *memoryLog {
		return &memoryLog{header: session.Header{SessionID: options.SessionID}, appendHook: func(record session.Record) error {
			if record.Type == session.RecordSandboxMode {
				return failure
			}
			return nil
		}}
	}
	if _, err := registry.Create(t.Context(), CreateRequest{SessionID: "failed-child", ParentID: "root", Depth: 1, Provider: session.SubagentSpawn, Label: "child", Create: true, Sandbox: session.SandboxReadOnly}); !errors.Is(err, failure) || !repository.logs["failed-child"].closed {
		t.Fatalf("rollback=%v", err)
	}
	root.mu.Lock()
	root.active = false
	root.mu.Unlock()
	if err := registry.SetSandboxMode(t.Context(), "root", session.SandboxReadOnly); !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
}
