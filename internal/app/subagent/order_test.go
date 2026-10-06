package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// The snapshot follows upstream's runtime-context orders (sandbox policy
// 110, delegation scope 120), not plugin registration order: here the
// sandbox context registers after the delegation service.
func TestService_SandboxPolicyPrecedesDelegationRegardlessOfRegistration(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{text: "child done"}},
	)
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	if err := agent.NewSandboxContext(h.engine, "/workspace").Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	contexts := messages(h.events(id), runtimeContextSource)
	if len(contexts) != 1 {
		t.Fatalf("child runtime contexts = %d", len(contexts))
	}
	policy := strings.Index(contexts[0], session.SandboxPolicyText(session.SandboxWorkspaceWrite, "/workspace"))
	delegation := strings.Index(contexts[0], delegationContext)
	if policy < 0 || delegation < 0 || policy > delegation {
		t.Fatalf("snapshot order: sandbox at %d, delegation at %d\n%s", policy, delegation, contexts[0])
	}
	rootCall.release <- "released"
	receive(t, results)
}
