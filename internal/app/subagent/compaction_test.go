package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// shrinkInheritedModel hot-switches the global route to gpt-5.4, whose
// window keeps root requests far below the compaction threshold, and gives
// the model the child inherited the smallest window and a lower effort.
func shrinkInheritedModel(t *testing.T, h *harness) {
	t.Helper()
	scope := &plugin.Scope{}
	if err := h.settings.Mount(context.Background(), memorySettings{}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	_, revision, err := h.settings.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.settings.Update(context.Background(), revision, func(document *settings.Document) error {
		document.Route = settings.Route{Provider: "openai", Model: "gpt-5.4"}
		provider := document.Providers["openai"]
		provider.Models[0].ContextWindow, provider.Models[0].Effort = 1024, session.EffortLow
		document.Providers["openai"] = provider
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// summaries returns the compaction summaries in events.
func summaries(events []session.Event) []session.CompactionData {
	var found []session.CompactionData
	for _, event := range events {
		if event.Record.Type == session.RecordCompactionSummary {
			found = append(found, *event.Record.Compaction)
		}
	}
	return found
}

func TestService_ChildCompactsOnTheRouteItInherited(t *testing.T) {
	long := strings.Repeat("context ", 600)
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{hold: true}, then: reply{text: "child done"}},
		rule{match: "sent a message: AGAIN", first: reply{text: "again"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK "+long, false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	// After delegation the inherited model's window shrinks and the global
	// route moves to a large-window model: the child's next step must measure
	// pressure against, and summarize with, the route it inherited.
	shrinkInheritedModel(t, h)
	// A long tool result keeps a recent tail worth retaining, so the older
	// task message becomes a summarizable prefix.
	childCall.release <- long
	receive(t, h.done(id))
	// A cold-resumed child compacts on the same inherited route.
	if err := h.service.SendMessage(context.Background(), "root", id, "AGAIN "+long); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	compacted := summaries(h.events(id))
	if len(compacted) != 2 {
		t.Fatalf("child compactions = %d", len(compacted))
	}
	for index, summary := range compacted {
		if summary.Provider != "openai" || summary.Model != "gpt-5.6-luna" || summary.Effort != session.EffortMax {
			t.Errorf("child summary %d route = %s/%s effort %q", index, summary.Provider, summary.Model, summary.Effort)
		}
	}
	h.model.mu.Lock()
	var efforts []string
	for _, request := range h.model.seen {
		if request.Purpose == "compaction" {
			if request.Effort == nil {
				efforts = append(efforts, "<catalog>")
			} else {
				efforts = append(efforts, string(*request.Effort))
			}
		}
	}
	h.model.mu.Unlock()
	if strings.Join(efforts, ",") != "max,max" {
		t.Fatalf("compaction request efforts = %q", efforts)
	}
	// The root follows the hot route and its large window: no compaction.
	rootCall.release <- "released"
	receive(t, results)
	if rootSummaries := summaries(h.events("root")); len(rootSummaries) != 0 {
		t.Fatalf("root compactions = %#v", rootSummaries)
	}
}
