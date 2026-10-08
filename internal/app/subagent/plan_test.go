package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// A fork inherits the parent's conversation, not its plan mode: the child
// has no review channel and cannot select a mode, so inherited plan mode
// could never be left, and the seed stops at the last completed turn, before
// an exit approved in the current turn is recorded.
func TestService_ForkChildStartsOutsidePlanMode(t *testing.T) {
	h := startHarness(t,
		rule{match: "PLAN_FIRST", first: reply{text: "planned"}},
		rule{match: "PLAN_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "FORK_TASK", first: reply{text: "implementing"}},
	)
	ctx := context.Background()
	if change, err := h.registry.SetPlanMode(ctx, "root", true); err != nil || change != plan.Committed {
		t.Fatalf("enter plan mode = %s, %v", change, err)
	}
	first, err := h.root.Submit(ctx, userText("PLAN_FIRST"))
	if err != nil {
		t.Fatal(err)
	}
	if result := receive(t, first); result.Outcome != session.OutcomeCompleted {
		t.Fatalf("first root turn = %#v", result)
	}
	call, results := h.submit("PLAN_HOLD")
	report, err := h.service.Run(ctx, start(call, "implement", "FORK_TASK", true))
	if err != nil || report.Text != "implementing" {
		t.Fatalf("Run = %#v, %v", report, err)
	}
	forked := h.model.requests("FORK_TASK")
	if len(forked) != 1 || strings.Contains(forked[0].System, plan.Section) {
		t.Fatalf("forked child ran in plan mode: %d requests", len(forked))
	}
	inherited := false
	for _, node := range forked[0].Surface {
		inherited = inherited || node.Message != nil && session.Text(*node.Message) == "PLAN_FIRST"
	}
	if !inherited {
		t.Fatal("fork lost the parent's conversation")
	}
	child := session.Children(h.events("root"))[0].SessionID
	childEvents := h.events(child)
	if seeded := session.ProjectPlan(childEvents[:session.OwnEvents(childEvents)[0].Sequence-1]); !seeded.Active {
		t.Fatal("the seed no longer carries the parent's plan/mode record")
	}
	if !strings.Contains(h.model.requests("PLAN_HOLD")[0].System, plan.Section) {
		t.Fatal("the parent left plan mode")
	}
	call.release <- "released"
	if result := receive(t, results); result.Outcome != session.OutcomeCompleted {
		t.Fatalf("root turn = %#v", result)
	}
	if !session.ProjectPlan(h.events("root")).Active {
		t.Fatal("the parent's recorded plan mode changed")
	}
}
