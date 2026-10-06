package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestService_SandboxCapturedAtDelegationAndRestored(t *testing.T) {
	for _, fork := range []bool{false, true} {
		name := "spawn"
		if fork {
			name = "fork"
		}
		t.Run(name, func(t *testing.T) {
			h := startHarness(t,
				rule{match: "FIRST", first: reply{text: "first"}},
				rule{match: "HOLD", first: reply{hold: true}, then: reply{text: "done"}},
				rule{match: "TASK", first: reply{text: "child"}},
			)
			ctx := t.Context()
			if err := h.registry.SetSandboxMode(ctx, "root", session.SandboxDangerFullAccess); err != nil {
				t.Fatal(err)
			}
			turn, err := h.root.Submit(ctx, userText("FIRST"))
			if err != nil {
				t.Fatal(err)
			}
			receive(t, turn)
			call, results := h.submit("HOLD")
			if err := h.registry.SetSandboxMode(ctx, "root", session.SandboxReadOnly); err != nil {
				t.Fatal(err)
			}
			if report, err := h.service.Run(ctx, start(call, "child", "TASK", fork)); err != nil || report.Text != "child" {
				t.Fatalf("delegation: %+v, %v", report, err)
			}
			id := session.Children(h.events("root"))[0].SessionID
			events := h.events(id)
			own := session.OwnEvents(events)
			if len(own) < 3 || own[1].Record.Sandbox == nil || own[1].Record.Sandbox.Mode != session.SandboxReadOnly || own[1].Record.Sandbox.Source != "delegation" || own[2].Record.Approval.Policy != session.ApprovalNever {
				t.Fatalf("child policy records: %+v", own)
			}
			if fork && session.EffectiveSandbox(events[:own[0].Sequence-1]) != session.SandboxDangerFullAccess {
				t.Fatal("fork seed lost older parent policy")
			}
			if err := h.registry.SetSandboxMode(ctx, "root", session.SandboxDangerFullAccess); err != nil {
				t.Fatal(err)
			}
			call.release <- "release"
			receive(t, results)
			resumed, err := h.registry.Create(ctx, agent.CreateRequest{SessionID: id})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := resumed.Events(ctx)
			if err != nil || session.EffectiveSandbox(restored) != session.SandboxReadOnly {
				t.Fatalf("cold policy: %+v, %v", restored, err)
			}
			if err := h.registry.SetSandboxMode(ctx, id, session.SandboxWorkspaceWrite); !errors.Is(err, agent.ErrInvalidConfig) {
				t.Fatalf("delegated switch: %v", err)
			}
			if err := h.registry.SetPolicy(ctx, id, session.ApprovalAsk); !errors.Is(err, agent.ErrInvalidConfig) {
				t.Fatalf("delegated approval: %v", err)
			}
		})
	}
}

func TestService_SandboxChildCreationFailureReleasesReservation(t *testing.T) {
	h := startHarness(t, rule{match: "HOLD", first: reply{hold: true}, then: reply{text: "done"}})
	call, results := h.submit("HOLD")
	dir := filepath.Dir(call.invocation.Journal.(interface{ Path() string }).Path())
	parked := dir + ".parked"
	if err := os.Rename(dir, parked); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir); _ = os.Rename(parked, dir) })
	if err := os.WriteFile(dir, []byte("block new journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.StartContinuable(t.Context(), start(call, "child", "TASK", false)); err == nil {
		t.Fatal("child created without a writable session directory")
	}
	h.service.mu.Lock()
	reserved, children := h.service.reserved["root"], len(h.service.children)
	h.service.mu.Unlock()
	if reserved != 0 || children != 0 {
		t.Fatalf("failed creation retained %d reservations and %d children", reserved, children)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, dir); err != nil {
		t.Fatal(err)
	}
	call.release <- "release"
	receive(t, results)
}

func TestService_SandboxCaptureFailureDoesNotCreateChild(t *testing.T) {
	h := startHarness(t, rule{match: "HOLD", first: reply{hold: true}, then: reply{text: "done"}})
	call, results := h.submit("HOLD")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.service.Run(ctx, start(call, "child", "TASK", false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("capture error: %v", err)
	}
	if children := session.Children(h.events("root")); len(children) != 0 {
		t.Fatalf("failed capture created child: %+v", children)
	}
	call.release <- "release"
	receive(t, results)
}
