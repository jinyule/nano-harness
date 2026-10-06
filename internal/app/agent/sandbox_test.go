package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestSandboxContext_RegistrationProjectionAndCompaction(t *testing.T) {
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
	messages, err := provider.StepContext(t.Context(), ContextRequest{})
	if err != nil || len(messages) != 1 || messages[0].Source.Plugin != "sandbox:policy" || !strings.Contains(session.Text(messages[0]), "workspace-write") {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	events := []session.Event{{Sequence: 1, Record: session.Record{Type: session.RecordUserMessage, Message: &messages[0]}}}
	if next, err := provider.StepContext(t.Context(), ContextRequest{Events: events}); err != nil || len(next) != 0 {
		t.Fatalf("unchanged=%v %v", next, err)
	}
	events = append(events, session.Event{Sequence: 2, Record: session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: session.SandboxReadOnly}}})
	changed, err := provider.StepContext(t.Context(), ContextRequest{Events: events})
	if err != nil || len(changed) != 1 || !strings.Contains(session.Text(changed[0]), "read-only") {
		t.Fatalf("changed=%v %v", changed, err)
	}
	events = append(events, session.Event{Sequence: 3, Record: session.Record{Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "c", ShadowedSeqs: []uint64{1}, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "summary"}}}}})
	if rebuilt, err := provider.StepContext(t.Context(), ContextRequest{Events: events}); err != nil || len(rebuilt) != 1 {
		t.Fatalf("compaction=%v %v", rebuilt, err)
	}
	events[len(events)-1].Record.Compaction.ShadowedSeqs = []uint64{999}
	if _, err := provider.StepContext(t.Context(), ContextRequest{Events: events}); !errors.Is(err, session.ErrInvalidRecord) {
		t.Fatalf("corrupt projection: %v", err)
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
