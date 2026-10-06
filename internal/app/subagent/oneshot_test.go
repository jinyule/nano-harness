package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestService_RunSpawnsOneShotChildAndReleasesIt(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "SPAWN_TASK", first: reply{text: "child answer"}},
	)
	call, results := h.submit("ROOT_HOLD")
	report, err := h.service.Run(context.Background(), start(call, "  scan files ", "SPAWN_TASK", false))
	if err != nil || report.Outcome != session.OutcomeCompleted || report.Text != "child answer" {
		t.Fatalf("Run = %#v, %v", report, err)
	}
	children := session.Children(h.events("root"))
	if len(children) != 1 || children[0].Mode != session.SubagentOneShot || children[0].Label != "  scan files " {
		t.Fatalf("catalog = %#v", children)
	}
	id := children[0].SessionID
	if _, err := h.registry.Find(id); !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("collected child is still live: %v", err)
	}
	header, events, err := h.manager.Inspect(context.Background(), id)
	if err != nil || header.ParentSessionID != "root" || header.DelegationDepth != 1 {
		t.Fatalf("child header = %#v, %v", header, err)
	}
	descriptor := events[0].Record.Subagent
	if descriptor == nil || descriptor.Provider != session.SubagentSpawn || descriptor.Mode != session.SubagentOneShot || descriptor.Inherited != 0 || events[1].Record.Approval.Policy != session.ApprovalNever {
		t.Fatalf("child creation records = %#v", events[:2])
	}
	if tasks := messages(events, SourceDelegation); len(tasks) != 1 || tasks[0] != "SPAWN_TASK" {
		t.Fatalf("delegated task = %q", tasks)
	}
	call.release <- "released"
	if result := receive(t, results); result.Outcome != session.OutcomeCompleted {
		t.Fatalf("root turn = %#v", result)
	}
	if infos, err := h.service.List(""); err != nil || len(infos) != 0 {
		t.Fatalf("live children = %#v, %v", infos, err)
	}
}

func TestService_ForkInheritsOnlyCompletedTurns(t *testing.T) {
	rules := []rule{
		{match: "FIRST", first: reply{text: "first answer"}},
		{match: "SECOND", first: reply{hold: true}, then: reply{text: "second done"}},
		{match: "FORK_TASK", first: reply{text: "fork answer"}},
	}
	h := startHarness(t, rules...)
	first, err := h.root.Submit(context.Background(), userText("FIRST"))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, first)
	prefix := h.events("root")
	call, results := h.submit("SECOND")
	report, err := h.service.Run(context.Background(), start(call, "review", "FORK_TASK", true))
	if err != nil || report.Text != "fork answer" {
		t.Fatalf("fork Run = %#v, %v", report, err)
	}
	requests := h.model.requests("FORK_TASK")
	if len(requests) != 1 {
		t.Fatalf("fork requests = %d", len(requests))
	}
	var seen []string
	for _, node := range requests[0].Surface {
		if node.Message != nil {
			seen = append(seen, session.Text(*node.Message))
		}
	}
	// The child's own runtime context follows its task, after the inherited prefix.
	if strings.Join(seen, "|") != "FIRST|first answer|FORK_TASK|"+delegationContext {
		t.Fatalf("fork surface = %q", seen)
	}
	id := session.Children(h.events("root"))[0].SessionID
	events := h.events(id)
	for index, event := range prefix {
		if events[index].Sequence != event.Sequence || events[index].Record.Type != event.Record.Type {
			t.Fatalf("seed event %d = %#v, want %#v", index, events[index], event)
		}
	}
	descriptor := events[len(prefix)].Record.Subagent
	if descriptor == nil || descriptor.Provider != session.SubagentFork || descriptor.Inherited != uint64(len(prefix)) {
		t.Fatalf("fork descriptor = %#v", events[len(prefix)])
	}
	call.release <- "released"
	receive(t, results)

	// Before any completed turn a fork starts fresh.
	fresh := startHarness(t, rules...)
	call, results = fresh.submit("SECOND")
	if _, err := fresh.service.Run(context.Background(), start(call, "review", "FORK_TASK", true)); err != nil {
		t.Fatal(err)
	}
	events = fresh.events(session.Children(fresh.events("root"))[0].SessionID)
	if descriptor := events[0].Record.Subagent; descriptor == nil || descriptor.Provider != session.SubagentFork || descriptor.Inherited != 0 {
		t.Fatalf("fresh fork descriptor = %#v", events[0])
	}
	call.release <- "released"
	receive(t, results)
}

func TestService_RunCancellationReleasesChild(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "BLOCK_TASK", first: reply{block: true}},
	)
	call, results := h.submit("ROOT_HOLD")
	ctx, cancel := context.WithCancel(context.Background())
	failure := make(chan error, 1)
	go func() {
		_, err := h.service.Run(ctx, start(call, "slow", "BLOCK_TASK", false))
		failure <- err
	}()
	receive(t, h.model.blocked)
	cancel()
	if err := receive(t, failure); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Run = %v", err)
	}
	id := session.Children(h.events("root"))[0].SessionID
	if outcome, ok := session.LastOutcome(h.events(id)); !ok || outcome != session.OutcomeCanceled {
		t.Fatalf("child outcome = %q %t", outcome, ok)
	}
	if infos, _ := h.service.List(""); len(infos) != 0 {
		t.Fatalf("live children = %#v", infos)
	}
	call.release <- "released"
	receive(t, results)
}

func TestService_CreationFailuresReleaseTheChild(t *testing.T) {
	previous := beforePublish
	t.Cleanup(func() { beforePublish = previous })
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
	)
	call, results := h.submit("ROOT_HOLD")
	live := func() int {
		statuses, _ := h.registry.Statuses()
		return len(statuses)
	}

	journal := &memoryJournal{err: errors.New("journal")}
	request := start(call, "scan", "TASK", false)
	request.Journal = journal
	if _, err := h.service.Run(context.Background(), request); !errors.Is(err, journal.err) {
		t.Fatalf("catalog failure = %v", err)
	}

	deep, err := h.registry.Create(context.Background(), agent.CreateRequest{ParentID: "root", Mode: session.SubagentOneShot, Provider: session.SubagentSpawn, Route: testRoute, Label: "deep", Depth: maxDelegationDepth, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	tooDeep := start(call, "scan", "TASK", false)
	tooDeep.ParentID = deep.Status().SessionID
	if _, err := h.service.Run(context.Background(), tooDeep); code(err) != CodeDepthLimit || err.Error() != "subagent depth 5 exceeds maxDepth 4" {
		t.Fatalf("depth limit = %v", err)
	}
	if err := h.registry.Close(context.Background(), deep.Status().SessionID); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.service.Run(cancelled, start(call, "scan", "TASK", true)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fork = %v", err)
	}
	if _, err := h.service.Run(cancelled, start(call, "scan", "TASK", false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled spawn = %v", err)
	}
	// Cancellation after the parent's route was read stops the child's creation.
	if _, err := h.service.Run(&lateCancel{Context: context.Background(), after: 1}, start(call, "scan", "TASK", false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("spawn cancelled during creation = %v", err)
	}

	// The child stops before its task is submitted.
	beforePublish = func(created *agent.Agent) { _ = h.registry.Close(context.Background(), created.Status().SessionID) }
	if _, err := h.service.Run(context.Background(), start(call, "scan", "TASK", false)); !errors.Is(err, agent.ErrNotRunning) {
		t.Fatalf("stopped one-shot = %v", err)
	}
	if _, err := h.service.StartContinuable(context.Background(), start(call, "scan", "TASK", false)); !errors.Is(err, agent.ErrNotRunning) {
		t.Fatalf("stopped continuable = %v", err)
	}
	beforePublish = previous
	if live() != 1 {
		t.Fatalf("failed creations left %d live agents", live())
	}
	call.release <- "released"
	receive(t, results)

	// The service stops between creation and publication.
	call, results = h.submit("ROOT_HOLD")
	beforePublish = func(*agent.Agent) { _ = h.serviceScope.Close(context.Background()) }
	if _, err := h.service.Run(context.Background(), start(call, "scan", "TASK", false)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("publication race = %v", err)
	}
	beforePublish = previous
	if live() != 1 {
		t.Fatalf("publication race left %d live agents", live())
	}
	call.release <- "released"
	receive(t, results)
}

func TestService_StartBackgroundRunsOneShotAsParentJob(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "BG_TASK", first: reply{text: "bg answer"}},
		rule{match: "BG_BLOCK", first: reply{block: true}},
	)
	call, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartBackground(context.Background(), start(call, "background scan", "BG_TASK", true))
	if err != nil || id != "subagent-1" {
		t.Fatalf("StartBackground = %q, %v", id, err)
	}
	if view, err := h.jobs.Wait(context.Background(), "root", id, waitLimit); err != nil || view.Status != job.StatusCompleted || view.Label != "background scan" {
		t.Fatalf("job = %#v, %v", view, err)
	}
	if read, err := h.jobs.Read("root", id); err != nil || read.Result != "bg answer" {
		t.Fatalf("job read = %#v, %v", read, err)
	}

	killed, err := h.service.StartBackground(context.Background(), start(call, "slow scan", "BG_BLOCK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.model.blocked)
	if _, requested, err := h.jobs.Kill("root", killed, new("superseded")); err != nil || !requested {
		t.Fatalf("Kill = %t, %v", requested, err)
	}
	if view, err := h.jobs.Wait(context.Background(), "root", killed, waitLimit); err != nil || view.Status != job.StatusKilled {
		t.Fatalf("killed job = %#v, %v", view, err)
	}
	if infos, _ := h.service.List(""); len(infos) != 0 {
		t.Fatalf("killed job left children %#v", infos)
	}

	if err := h.jobScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.StartBackground(context.Background(), start(call, "late", "BG_TASK", false)); !errors.Is(err, job.ErrNotRunning) {
		t.Fatalf("Launch failure = %v", err)
	}
	if statuses, _ := h.registry.Statuses(); len(statuses) != 1 {
		t.Fatalf("Launch failure left %#v", statuses)
	}
	call.release <- "released"
	receive(t, results)
}
