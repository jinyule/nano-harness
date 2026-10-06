package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type catalogBarrier struct {
	Journal
	entered chan string
	release <-chan struct{}
}

func (barrier catalogBarrier) Append(ctx context.Context, record session.Record) (session.Event, error) {
	barrier.entered <- record.Catalog.SessionID
	select {
	case <-barrier.release:
		return barrier.Journal.Append(ctx, record)
	case <-ctx.Done():
		return session.Event{}, ctx.Err()
	}
}

func TestService_ConcurrentCreationsHoldOneSlotEach(t *testing.T) {
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}}, rule{match: "CHILD", first: reply{block: true}})
	call, _ := h.submit("ROOT")
	gate := make(chan struct{})
	entered, finished := make(chan string, maxActiveChildren), make(chan error, maxActiveChildren)
	var group sync.WaitGroup
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(gate) }); group.Wait() })
	request := start(call, "worker", "CHILD", false)
	request.Journal = catalogBarrier{Journal: request.Journal, entered: entered, release: gate}
	for index := range maxActiveChildren {
		group.Go(func() { _, err := h.service.StartContinuable(context.Background(), request); finished <- err })
		select {
		case <-entered:
		case err := <-finished:
			t.Fatalf("creation %d rejected with only %d published children: %v", index+1, index, err)
		case <-t.Context().Done():
			t.Fatal("test cancelled")
		}
	}
	if _, err := h.service.StartContinuable(context.Background(), request); code(err) != CodeLimitReached {
		t.Fatalf("ninth creation = %v", err)
	}
	unblock.Do(func() { close(gate) })
	for range maxActiveChildren {
		if err := receive(t, finished); err != nil {
			t.Fatalf("admitted creation failed: %v", err)
		}
	}
	group.Wait()
	if len(h.service.reserved) != 0 {
		t.Fatalf("published children retained reservations: %v", h.service.reserved)
	}
}

func TestService_CancelledMessagesNeverReachRecipient(t *testing.T) {
	for _, direction := range []string{"down", "up"} {
		for _, cutoff := range []string{"dispatch", "acceptance"} {
			t.Run(direction+"/"+cutoff, func(t *testing.T) {
				h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}}, rule{match: "CHILD", first: reply{hold: true}})
				call, _ := h.submit("ROOT")
				id, err := h.service.StartContinuable(context.Background(), start(call, "worker", "CHILD", false))
				if err != nil {
					t.Fatal(err)
				}
				receive(t, h.held)
				senderID, targetID := "root", id
				if direction == "up" {
					senderID, targetID = id, "root"
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				want := "tool call aborted before dispatch"
				if cutoff == "dispatch" {
					err = h.service.SendMessage(ctx, senderID, targetID, "MUST_NOT_ARRIVE")
				} else {
					want = "tool call aborted"
					sender, _ := h.registry.Find(senderID)
					_, err = h.service.deliver(ctx, sender, senderID, targetID, "MUST_NOT_ARRIVE")
				}
				if !errors.Is(err, context.Canceled) || err.Error() != want {
					t.Errorf("cancelled delivery = %v, want %q", err, want)
				}
				h.service.mu.Lock()
				pending := h.service.children[id].delivered
				h.service.mu.Unlock()
				if pending != 0 {
					t.Errorf("cancelled message accepted: delivered=%d", pending)
				}
				if err := h.serviceScope.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				h.root.Interrupt()
				h.idle(h.root)
				for _, target := range []string{"root", id} {
					if got := messages(h.events(target), SourceAgentMessage); len(got) != 0 {
						t.Errorf("recipient %s logged %q", target, got)
					}
				}
			})
		}
	}
}

func TestService_TeardownFailureOverridesSuccessfulSettlement(t *testing.T) {
	previous := closeAgent
	t.Cleanup(func() { closeAgent = previous })
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}}, rule{match: "CHILD", first: reply{hold: true}, then: reply{text: "SUCCESS_OUTPUT"}})
	call, results := h.submit("ROOT")
	id, err := h.service.StartContinuable(context.Background(), start(call, "worker", "CHILD", false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	closeAgent = func(registry *agent.Registry, ctx context.Context, target string) error {
		return errors.Join(previous(registry, ctx, target), errors.New("release failed"))
	}
	done := h.done(id)
	childCall.release <- "done"
	receive(t, done)
	call.release <- "done"
	receive(t, results)
	h.idle(h.root)
	notices := messages(h.events("root"), SourceSettled)
	want := "Background subagent " + id + " failed before it finished.It left no closing message."
	if len(notices) != 1 || notices[0] != want {
		t.Fatalf("settlement = %q, want %q", notices, want)
	}
}

func TestService_CancelledJobWithTeardownFailureIsFailed(t *testing.T) {
	previous := closeAgent
	t.Cleanup(func() { closeAgent = previous })
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}}, rule{match: "CHILD", first: reply{block: true}})
	call, _ := h.submit("ROOT")
	id, err := h.service.StartBackground(context.Background(), start(call, "worker", "CHILD", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.model.blocked)
	closeAgent = func(registry *agent.Registry, ctx context.Context, target string) error {
		return errors.Join(previous(registry, ctx, target), errors.New("release failed"))
	}
	if _, _, err := h.jobs.Kill("root", id, "stop"); err != nil {
		t.Fatal(err)
	}
	view, err := h.jobs.Wait(context.Background(), "root", id, waitLimit)
	if err != nil || view.Status != job.StatusFailed || !strings.Contains(view.Detail, "release failed") {
		t.Fatalf("cancelled job settlement = %#v, %v", view, err)
	}
	if read, err := h.jobs.Read("root", id); err != nil || read.Result != "" {
		t.Fatalf("failed job output = %#v, %v", read, err)
	}
}

func TestService_BackgroundAdmissionPrecedesForkSideEffects(t *testing.T) {
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	for range 10 {
		_, err := h.jobs.Launch(job.Spec{Kind: "probe", Label: "probe", Owner: "root", Run: func(ctx context.Context, _ *job.Output) job.Outcome {
			<-ctx.Done()
			return job.Outcome{Status: job.StatusKilled}
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := h.manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.service.StartBackground(context.Background(), start(call, "fork", "TASK", true))
	if !errors.Is(err, job.ErrLimit) {
		t.Fatalf("full jobs = %v", err)
	}
	after, err := h.manager.List(context.Background())
	if err != nil || len(after) != len(before) {
		t.Errorf("rejected fork left transcript: %d -> %d, %v", len(before), len(after), err)
	}
	if children := session.Children(h.events("root")); len(children) != 0 {
		t.Errorf("rejected fork left catalog: %#v", children)
	}
}

func TestService_BackgroundStartupFailureBelongsToJob(t *testing.T) {
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	request := start(call, "fork", "TASK", true)
	request.Journal = &memoryJournal{err: errors.New("catalog failed")}
	id, err := h.service.StartBackground(context.Background(), request)
	if err != nil || id == "" {
		t.Fatalf("startup returned tool error instead of job id: %q, %v", id, err)
	}
	view, err := h.jobs.Wait(context.Background(), "root", id, waitLimit)
	if err != nil || view.Status != job.StatusFailed || !strings.Contains(view.Detail, "catalog failed") {
		t.Fatalf("startup job = %#v, %v", view, err)
	}
	if read, err := h.jobs.Read("root", id); err != nil || read.Result != "" {
		t.Fatalf("startup output = %#v, %v", read, err)
	}
}

func TestService_DelegationPreservesDescriptionAndBlankPrompt(t *testing.T) {
	for _, label := range []struct{ name, text string }{{"long", "  " + strings.Repeat("中文", 100) + " \n"}, {"empty", ""}, {"whitespace", " \n"}} {
		for _, task := range []struct{ name, text string }{{"text", "TASK"}, {"empty", ""}, {"whitespace", " \n"}} {
			t.Run(label.name+"/"+task.name, func(t *testing.T) {
				description, prompt := label.text, task.text
				h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
				call, _ := h.submit("ROOT")
				report, err := h.service.Run(context.Background(), start(call, description, prompt, false))
				if err != nil || report.Outcome != session.OutcomeCompleted {
					t.Fatalf("delegation = %#v, %v", report, err)
				}
				children := session.Children(h.events("root"))
				if len(children) != 1 || children[0].Label != description {
					t.Fatalf("catalog label = %#v", children)
				}
				events := h.events(children[0].SessionID)
				if events[0].Record.Subagent.Label != description {
					t.Fatalf("descriptor lost label: %q", events[0].Record.Subagent.Label)
				}
				if tasks := messages(events, SourceDelegation); len(tasks) != 1 || tasks[0] != prompt {
					t.Fatalf("task = %q", tasks)
				}
			})
		}
	}
}

func TestService_PostInterruptMessageWakesAndReleasesResident(t *testing.T) {
	previous := parked
	t.Cleanup(func() { parked = previous })
	aborted, release := make(chan struct{}), make(chan struct{})
	parkedChild := make(chan string, 4)
	h := startHarness(t,
		rule{match: "ROOT", first: reply{hold: true}},
		rule{match: "CHILD", first: reply{block: true, afterAbort: func() { close(aborted); <-release }}},
		rule{match: "sent a message: AFTER", first: reply{hold: true}, then: reply{text: "handled"}},
	)
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	call, _ := h.submit("ROOT")
	id, err := h.service.StartContinuable(context.Background(), start(call, "worker", "CHILD", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.model.blocked)
	done := h.done(id)
	parked = func(target string) { parkedChild <- target }
	if err := h.service.SendMessage(context.Background(), "root", id, "BEFORE"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Interrupt("root", id); err != nil {
		t.Fatal(err)
	}
	receive(t, aborted)
	if err := h.service.SendMessage(context.Background(), "root", id, "AFTER"); err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(release) })
	select {
	case resumed := <-h.held:
		if resumed.invocation.SessionID != id || resumed.invocation.Turn != 2 {
			t.Fatalf("next turn = %#v", resumed.invocation)
		}
		resumed.release <- "done"
	case target := <-parkedChild:
		t.Fatalf("child %s parked after accepting a new wake", target)
	case <-done:
		t.Fatal("child released without handling its post-interrupt message")
	case <-t.Context().Done():
		t.Fatal("test cancelled")
	}
	receive(t, done)
	if got := messages(h.events(id), SourceAgentMessage); len(got) != 2 || got[0] != "Agent root sent a message: BEFORE" || got[1] != "Agent root sent a message: AFTER" {
		t.Fatalf("committed messages = %q", got)
	}
	if infos, _ := h.service.List("root"); len(infos) != 0 {
		t.Fatalf("finished child still occupies pool: %#v", infos)
	}
}

// acceptanceCancel cancels after the dispatch check returned successfully,
// so the recipient's inbox check decides whether the message is accepted.
type acceptanceCancel struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (ctx *acceptanceCancel) Err() error {
	ctx.checks++
	if ctx.checks == 2 {
		ctx.cancel()
		return nil
	}
	return ctx.Context.Err()
}

func TestService_InboxCancellationKeepsRecipientResident(t *testing.T) {
	for _, direction := range []string{"down", "up"} {
		t.Run(direction, func(t *testing.T) {
			h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}}, rule{match: "CHILD", first: reply{hold: true}})
			call, _ := h.submit("ROOT")
			id, err := h.service.StartContinuable(context.Background(), start(call, "worker", "CHILD", false))
			if err != nil {
				t.Fatal(err)
			}
			childCall := receive(t, h.held)
			senderID, targetID := "root", id
			if direction == "up" {
				senderID, targetID = id, "root"
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &acceptanceCancel{Context: base, cancel: cancel}
			if err := h.service.SendMessage(ctx, senderID, targetID, "REJECTED"); code(err) != CodeAborted || !errors.Is(err, context.Canceled) {
				t.Fatalf("inbox cancellation = %v", err)
			}
			if _, err := h.registry.Find(id); err != nil {
				t.Fatalf("cancelled message released recipient: %v", err)
			}
			done := h.done(id)
			childCall.release <- "done"
			receive(t, done)
			call.release <- "done"
			h.idle(h.root)
			if got := messages(h.events(targetID), SourceAgentMessage); len(got) != 0 {
				t.Fatalf("cancelled message logged: %q", got)
			}
		})
	}
}

func TestService_ConcurrentReleaseSharesTeardownFailure(t *testing.T) {
	previous := closeAgent
	t.Cleanup(func() { closeAgent = previous })
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	current, err := h.service.create(context.Background(), start(call, "worker", "TASK", false), session.SubagentOneShot)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	failure := errors.New("release failed")
	closeAgent = func(registry *agent.Registry, ctx context.Context, target string) error {
		close(reached)
		<-release
		return errors.Join(previous(registry, ctx, target), failure)
	}
	var unblock sync.Once
	first := make(chan error, 1)
	t.Cleanup(func() {
		unblock.Do(func() { close(release) })
		if err := receive(t, first); code(err) != CodeTeardownFailed || !errors.Is(err, failure) {
			t.Errorf("first release lost failure: %v", err)
		}
	})
	go func() { first <- h.service.close(context.Background(), current, nil) }()
	receive(t, reached)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.service.close(cancelled, current, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled release waiter = %v", err)
	}
	unblock.Do(func() { close(release) })
	if err := h.service.close(context.Background(), current, nil); code(err) != CodeTeardownFailed || !errors.Is(err, failure) {
		t.Fatalf("concurrent release lost failure: %v", err)
	}
}

func TestService_BackgroundKillCancelsStartup(t *testing.T) {
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	entered := make(chan string, 1)
	request := start(call, "fork", "TASK", true)
	request.Journal = catalogBarrier{Journal: request.Journal, entered: entered, release: make(chan struct{})}
	returned := make(chan string, 1)
	var group sync.WaitGroup
	t.Cleanup(func() {
		for _, view := range h.jobs.List("root") {
			_, _, _ = h.jobs.Kill("root", view.ID, "cleanup")
		}
		group.Wait()
	})
	group.Go(func() {
		id, err := h.service.StartBackground(context.Background(), request)
		if err != nil {
			returned <- err.Error()
			return
		}
		returned <- id
	})
	childID := receive(t, entered)
	views := h.jobs.List("root")
	if len(views) != 1 {
		t.Fatalf("startup was not job-owned: %#v", views)
	}
	if _, _, err := h.jobs.Kill("root", views[0].ID, "stop startup"); err != nil {
		t.Fatal(err)
	}
	if id := receive(t, returned); id != views[0].ID {
		t.Fatalf("startup result = %q", id)
	}
	view, err := h.jobs.Wait(context.Background(), "root", views[0].ID, waitLimit)
	if err != nil || view.Status != job.StatusKilled {
		t.Fatalf("killed startup = %#v, %v", view, err)
	}
	if children := session.Children(h.events("root")); len(children) != 0 {
		t.Fatalf("cancelled startup wrote catalog: %#v", children)
	}
	if _, err := h.registry.Find(childID); !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("cancelled startup left live child: %v", err)
	}
}

func TestService_ContinuableAllowsEmptyDescriptionAndPrompt(t *testing.T) {
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}}, rule{match: "Your parent agent id", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	id, err := h.service.StartContinuable(context.Background(), start(call, "", "", false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	if tasks := messages(h.events(id), SourceDelegation); len(tasks) != 1 || !strings.HasPrefix(tasks[0], "Your parent agent id") {
		t.Fatalf("empty continuable task = %q", tasks)
	}
	done := h.done(id)
	childCall.release <- "done"
	receive(t, done)
}

func TestService_FailedCreationsReleaseReservations(t *testing.T) {
	previous := beforePublish
	t.Cleanup(func() { beforePublish = previous })
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.service.StartContinuable(ctx, start(call, "worker", "TASK", false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled creation = %v", err)
	}
	if len(h.service.reserved) != 0 {
		t.Fatalf("cancelled creation kept reservation: %v", h.service.reserved)
	}
	beforePublish = func(*agent.Agent) { _ = h.serviceScope.Close(context.Background()) }
	if _, err := h.service.StartContinuable(context.Background(), start(call, "worker", "TASK", false)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("failed publication = %v", err)
	}
	if len(h.service.reserved) != 0 {
		t.Fatalf("failed publication kept reservation: %v", h.service.reserved)
	}
	if _, err := h.service.Run(context.Background(), start(call, "worker", "TASK", false)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped creation = %v", err)
	}
	if statuses, _ := h.registry.Statuses(); len(statuses) != 1 {
		t.Fatalf("failed publication leaked agent: %#v", statuses)
	}
}

func TestService_BackgroundRejectsInvalidAndCancelledCallsBeforeAdmission(t *testing.T) {
	h := startHarness(t)
	request := StartRequest{ParentID: "root", Description: "worker"}
	if _, err := h.service.StartBackground(context.Background(), request); code(err) != CodeInvalidRequest {
		t.Fatalf("missing journal = %v", err)
	}
	request.Journal = &memoryJournal{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.service.StartBackground(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled background call = %v", err)
	}
	if jobs := h.jobs.List("root"); len(jobs) != 0 {
		t.Fatalf("rejected calls admitted jobs: %#v", jobs)
	}
	request.Description = ""
	if _, err := h.service.StartBackground(context.Background(), request); !errors.Is(err, job.ErrInvalidConfig) {
		t.Fatalf("empty job label = %v", err)
	}
	if events, _ := h.manager.List(context.Background()); len(events) != 1 {
		t.Fatalf("rejected job created transcript: %#v", events)
	}
}

func TestService_ColdDeliveryCancellationUsesAbortWording(t *testing.T) {
	h := startHarness(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &acceptanceCancel{Context: base, cancel: cancel}
	if err := h.service.SendMessage(ctx, "root", "missing", "hi"); code(err) != CodeAborted || !errors.Is(err, context.Canceled) || err.Error() != "tool call aborted" {
		t.Fatalf("cold delivery cancellation = %v", err)
	}
}

func TestService_ChildCannotWakeFinishedOneShotParent(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT", first: reply{hold: true}},
		rule{match: "PARENT", first: reply{hold: true}},
		rule{match: "CHILD", first: reply{hold: true}},
	)
	rootCall, _ := h.submit("ROOT")
	parent, err := h.service.create(context.Background(), start(rootCall, "parent", "PARENT", false), session.SubagentOneShot)
	if err != nil {
		t.Fatal(err)
	}
	result, err := parent.agent.Submit(context.Background(), taskMessage("PARENT", "root", false))
	if err != nil {
		t.Fatal(err)
	}
	parentCall := receive(t, h.held)
	id, err := h.service.StartContinuable(context.Background(), start(parentCall, "worker", "CHILD", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.held)
	parentCall.release <- "done"
	receive(t, result)
	if err := h.service.SendMessage(context.Background(), id, parent.id, "late"); code(err) != CodeParentUnavailable || !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("late parent delivery = %v", err)
	}
}

func TestService_StartupCancellationCannotMaskTeardownFailure(t *testing.T) {
	previous := closeAgent
	t.Cleanup(func() { closeAgent = previous })
	h := startHarness(t, rule{match: "ROOT", first: reply{hold: true}})
	call, _ := h.submit("ROOT")
	releaseFailure := errors.New("release failed")
	closeAgent = func(registry *agent.Registry, ctx context.Context, target string) error {
		return errors.Join(previous(registry, ctx, target), releaseFailure)
	}
	request := start(call, "fork", "TASK", true)
	request.Journal = &memoryJournal{err: &Error{Code: CodeAborted, Message: "tool call aborted", Err: context.Canceled}}
	id, err := h.service.StartBackground(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	view, err := h.jobs.Wait(context.Background(), "root", id, waitLimit)
	if err != nil || view.Status != job.StatusFailed || !strings.Contains(view.Detail, "release failed") {
		t.Fatalf("startup cancellation masked cleanup failure: %#v, %v", view, err)
	}
}
