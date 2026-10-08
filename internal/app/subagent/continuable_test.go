package subagent

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	jsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestService_ContinuableRunsInBackgroundAndNotifiesParent(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{text: "child report"}},
		rule{match: "Background subagent", first: reply{text: "noted"}},
	)
	call, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(call, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	if _, err := h.registry.Find(id); !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("settled child still live: %v", err)
	}
	// The busy parent receives the notice at its next step boundary.
	call.release <- "released"
	if result := receive(t, results); result.Outcome != session.OutcomeCompleted || result.Text != "noted" {
		t.Fatalf("root turn = %#v", result)
	}
	rootEvents := h.events("root")
	want := "Background subagent " + id + " finished and will do no further work unless you send it more.Its closing message:child report"
	if notices := messages(rootEvents, SourceSettled); len(notices) != 1 || notices[0] != want {
		t.Fatalf("settlement notices = %q", notices)
	}
	if children := session.Children(rootEvents); len(children) != 1 || children[0].Mode != session.SubagentContinuable {
		t.Fatalf("catalog = %#v", children)
	}
	childEvents := h.events(id)
	if descriptor := childEvents[0].Record.Subagent; descriptor.Mode != session.SubagentContinuable || descriptor.Provider != session.SubagentSpawn {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	if tasks := messages(childEvents, SourceDelegation); len(tasks) != 1 || !strings.Contains(tasks[0], `Your parent agent id is "root".`) {
		t.Fatalf("task = %q", tasks)
	}
}

func TestService_SendMessageRoundTripAndColdResume(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{hold: true}, then: reply{text: "child finished"}},
		rule{match: "sent a message: PING", first: reply{text: "pong"}},
		rule{match: "sent a message: UP", first: reply{text: "got up"}},
		rule{match: "sent a message: AGAIN", first: reply{text: "again answer"}},
		rule{match: "Background subagent", first: reply{text: "noted"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	if childCall.invocation.SessionID != id {
		t.Fatalf("held session = %q", childCall.invocation.SessionID)
	}
	if err := h.service.SendMessage(context.Background(), "root", id, "PING"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SendMessage(context.Background(), id, "root", "UP"); err != nil {
		t.Fatal(err)
	}
	childCall.release <- "child held"
	receive(t, h.done(id))
	rootCall.release <- "released"
	receive(t, results)
	h.idle(h.root)
	rootEvents := h.events("root")
	if relayed := messages(rootEvents, SourceAgentMessage); len(relayed) != 1 || relayed[0] != "Agent "+id+" sent a message: UP" {
		t.Fatalf("root relayed = %q", relayed)
	}
	if notices := messages(rootEvents, SourceSettled); len(notices) != 1 || !strings.HasSuffix(notices[0], "Its closing message:pong") {
		t.Fatalf("root notices = %q", notices)
	}
	if relayed := messages(h.events(id), SourceAgentMessage); len(relayed) != 1 || relayed[0] != "Agent root sent a message: PING" {
		t.Fatalf("child relayed = %q", relayed)
	}

	// A settled child is cold-resumed from its transcript for the next message.
	if err := h.service.SendMessage(context.Background(), "root", id, "AGAIN"); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	h.idle(h.root)
	notices := messages(h.events("root"), SourceSettled)
	if len(notices) != 2 || !strings.HasSuffix(notices[1], "Its closing message:again answer") {
		t.Fatalf("resumed notices = %q", notices)
	}
	childEvents := h.events(id)
	if relayed := messages(childEvents, SourceAgentMessage); len(relayed) != 2 || relayed[1] != "Agent root sent a message: AGAIN" {
		t.Fatalf("resumed child relayed = %q", relayed)
	}
	if last, _ := session.LastOutcome(childEvents); last != session.OutcomeCompleted {
		t.Fatalf("resumed child outcome = %q", last)
	}
}

func TestService_SettlementNoticePrecedesReleaseWaiters(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{hold: true}, then: reply{text: "child report"}},
		rule{match: "Background subagent", first: reply{hold: true}, then: reply{text: "noted"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	childCall := receive(t, h.held)
	rootCall.release <- "released"
	receive(t, results)
	previous := released
	t.Cleanup(func() { released = previous })
	// The probe runs the moment release waiters can observe the child gone.
	// An idle parent that does not yet hold the notice reports idle; the
	// notice's wake keeps it busy, and its woken turn holds so the probe
	// cannot see that turn finish.
	probed := make(chan error, 1)
	released = func(childID string) {
		if childID != id {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		probed <- h.root.WhenIdle(ctx)
	}
	childCall.release <- "child held"
	if err := receive(t, probed); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent idle when release waiters woke: %v", err)
	}
	receive(t, h.done(id))
	noticeCall := receive(t, h.held)
	if noticeCall.invocation.SessionID != "root" {
		t.Fatalf("notice turn session = %q", noticeCall.invocation.SessionID)
	}
	noticeCall.release <- "noted"
	h.idle(h.root)
	if notices := messages(h.events("root"), SourceSettled); len(notices) != 1 || !strings.HasSuffix(notices[0], "Its closing message:child report") {
		t.Fatalf("root notices = %q", notices)
	}
}

func TestService_RejectsMessagesOutsideDirectLineage(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "HOLD_TASK", first: reply{hold: true}, then: reply{text: "done"}},
		rule{match: "QUICK_TASK", first: reply{text: "quick"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	first, err := h.service.StartContinuable(context.Background(), start(rootCall, "first", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	firstCall := receive(t, h.held)
	second, err := h.service.StartContinuable(context.Background(), start(rootCall, "second", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	secondCall := receive(t, h.held)
	if _, err := h.service.Run(context.Background(), start(rootCall, "quick", "QUICK_TASK", false)); err != nil {
		t.Fatal(err)
	}
	children := session.Children(h.events("root"))
	oneShot := children[2].SessionID
	for name, test := range map[string]struct {
		sender, target, text string
		code                 Code
		message              string
	}{
		"too long":      {"root", first, strings.Repeat("x", session.MaxTextBytes+1), CodeInvalidRequest, "invalid message: at most 262144 bytes"},
		"ghost sender":  {"ghost", first, "hi", CodeUnauthorized, "message delivery requires the exact live sender agent"},
		"unknown":       {"root", "missing", "hi", CodeNotResumable, `subagent "missing" is unavailable`},
		"one-shot":      {"root", oneShot, "hi", CodeNotResumable, `subagent "` + oneShot + `" has no supported continuation state and cannot be resumed; choose a different target`},
		"sibling":       {first, second, "hi", CodeUnauthorized, `subagent "` + second + `" belongs to another parent session`},
		"child to self": {first, first, "hi", CodeUnauthorized, `subagent "` + first + `" belongs to another parent session`},
		"root to stray": {first, "root", "hi", "", ""},
	} {
		err := h.service.SendMessage(context.Background(), test.sender, test.target, test.text)
		if test.code == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if code(err) != test.code || err.Error() != test.message {
			t.Errorf("%s: SendMessage = %v", name, err)
		}
	}

	// A one-shot child is not resident, so it cannot write to its parent.
	failure := make(chan error, 1)
	go func() {
		_, err := h.service.Run(context.Background(), start(rootCall, "held one-shot", "HOLD_TASK", false))
		failure <- err
	}()
	heldOneShot := receive(t, h.held)
	err = h.service.SendMessage(context.Background(), heldOneShot.invocation.SessionID, "root", "hi")
	if code(err) != CodeUnauthorized || !strings.Contains(err.Error(), "is not a resident continuable child and cannot send to parent \"root\"") {
		t.Fatalf("one-shot to parent = %v", err)
	}
	heldOneShot.release <- "done"
	if err := receive(t, failure); err != nil {
		t.Fatal(err)
	}

	// The second child settles; it is no longer a live sender.
	secondCall.release <- "done"
	receive(t, h.done(second))
	if err := h.service.SendMessage(context.Background(), second, "root", "hi"); code(err) != CodeUnauthorized {
		t.Fatalf("settled sender = %v", err)
	}

	// A resident child whose parent is gone cannot deliver to it.
	rootCall.release <- "released"
	receive(t, results)
	if err := h.registry.Close(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SendMessage(context.Background(), first, "root", "hi"); code(err) != CodeParentUnavailable || err.Error() != "direct parent is not live; the message was not delivered" {
		t.Fatalf("missing parent = %v", err)
	}
	firstCall.release <- "done"
}

func TestService_InterruptReachesDescendantsAndParentWaitsForChildren(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "PARENT_TASK", first: reply{hold: true}, then: reply{text: "parent done"}},
		rule{match: "GRAND_TASK", first: reply{hold: true}, then: reply{text: "grand done"}},
		rule{match: "SIBLING_TASK", first: reply{hold: true}, then: reply{text: "sibling done"}},
		rule{match: "Background subagent", first: reply{text: "noted"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	parent, err := h.service.StartContinuable(context.Background(), start(rootCall, "parent", "PARENT_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	parentCall := receive(t, h.held)
	grand, err := h.service.StartContinuable(context.Background(), start(parentCall, "grand", "GRAND_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.held)
	sibling, err := h.service.StartContinuable(context.Background(), start(rootCall, "sibling", "SIBLING_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	siblingCall := receive(t, h.held)
	for name, test := range map[string]struct {
		caller, target, message string
	}{
		"ghost caller": {"ghost", grand, `interrupting "` + grand + `" requires the exact live ancestor agent`},
		"self":         {parent, parent, `agent "` + parent + `" cannot interrupt itself`},
		"sibling":      {sibling, grand, `subagent "` + grand + `" is not a live descendant of agent "` + sibling + `"`},
		"child parent": {grand, parent, `subagent "` + parent + `" is not a live descendant of agent "` + grand + `"`},
	} {
		if err := h.service.Interrupt(test.caller, test.target); code(err) != CodeUnauthorized || err.Error() != test.message {
			t.Errorf("%s: Interrupt = %v", name, err)
		}
	}
	if err := h.service.Interrupt("root", "missing"); err != nil {
		t.Fatalf("absent target = %v", err)
	}

	// The parent finishes its turn but stays resident while its child works.
	parentCall.release <- "parent held"
	parentAgent, _ := h.registry.Find(parent)
	h.idle(parentAgent)
	if _, err := h.registry.Find(parent); err != nil {
		t.Fatalf("parent settled with a working child: %v", err)
	}
	// The grandparent interrupts the grandchild without waiting.
	if err := h.service.Interrupt("root", grand); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(grand))
	receive(t, h.done(parent))
	if notices := messages(h.events(parent), SourceSettled); len(notices) != 1 || !strings.HasPrefix(notices[0], "Background subagent "+grand+" was stopped before it finished.") {
		t.Fatalf("parent notices = %q", notices)
	}
	siblingCall.release <- "done"
	receive(t, h.done(sibling))
	rootCall.release <- "released"
	receive(t, results)
	h.idle(h.root)
	if notices := messages(h.events("root"), SourceSettled); len(notices) != 2 {
		t.Fatalf("root notices = %q", notices)
	}
}

func TestService_DeliveryWaitsForReleaseThenResumes(t *testing.T) {
	previous := closeAgent
	t.Cleanup(func() { closeAgent = previous })
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{text: "first answer"}},
		rule{match: "sent a message: RESUMED", first: reply{text: "resumed answer"}},
	)
	entered, gate := make(chan struct{}), make(chan struct{})
	closeAgent = func(registry *agent.Registry, ctx context.Context, id string) error {
		if id != "root" {
			select {
			case entered <- struct{}{}:
				<-gate
			default:
			}
		}
		return previous(registry, ctx, id)
	}
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, entered)
	// The child is being released: interrupting it is a no-op and a
	// delivery waits for the release, then cold-resumes it.
	if err := h.service.Interrupt("root", id); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	abandoned := newWaitSignal(cancelled)
	failed := make(chan error, 1)
	go func() { failed <- h.service.SendMessage(abandoned, "root", id, "RESUMED") }()
	receive(t, abandoned.waiting)
	cancel()
	if err := receive(t, failed); code(err) != CodeAborted || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	waiting := newWaitSignal(context.Background())
	delivered := make(chan error, 1)
	go func() { delivered <- h.service.SendMessage(waiting, "root", id, "RESUMED") }()
	receive(t, waiting.waiting)
	close(gate)
	if err := receive(t, delivered); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	if relayed := messages(h.events(id), SourceAgentMessage); len(relayed) != 1 || relayed[0] != "Agent root sent a message: RESUMED" {
		t.Fatalf("resumed child relayed = %q", relayed)
	}
	rootCall.release <- "released"
	receive(t, results)
}

// dropTurns refuses every turn its kind would open.
type dropTurns struct{}

func (dropTurns) Admit(context.Context, agent.Journal, session.Message, func(context.Context) error) error {
	return agent.ErrNotAdmitted
}

func TestService_ResidencyWithoutTurnSettlesAsFinished(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{text: "first answer"}},
		rule{match: "Background subagent", first: reply{text: "noted"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	rootCall.release <- "released"
	receive(t, results)
	h.idle(h.root)
	// The resumed child accepts the message, but the turn it would open is
	// dropped, so this residency commits no turn/end to take an outcome from.
	admissions := &plugin.Scope{}
	t.Cleanup(func() { _ = admissions.Close(context.Background()) })
	if err := h.engine.RegisterAdmission(SourceAgentMessage, dropTurns{}, admissions); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SendMessage(context.Background(), "root", id, "DROPPED"); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	h.idle(h.root)
	notices := messages(h.events("root"), SourceSettled)
	if want := settlementSummary(id, session.OutcomeCompleted) + "It left no closing message."; len(notices) != 2 || notices[1] != want {
		t.Fatalf("root notices = %q", notices)
	}
	if relayed := messages(h.events(id), SourceAgentMessage); len(relayed) != 0 {
		t.Fatalf("dropped message committed: %q", relayed)
	}
}

func TestService_ColdResumeFailures(t *testing.T) {
	previous := beforePublish
	t.Cleanup(func() { beforePublish = previous })
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{text: "answer"}},
		rule{match: "PARENT_TASK", first: reply{hold: true}, then: reply{text: "parent done"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))

	// Another writer holds the transcript.
	locked, err := h.manager.Open(context.Background(), jsonlOpen(id))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.SendMessage(context.Background(), "root", id, "hi"); code(err) != CodeNotResumable || err.Error() != `subagent "`+id+`" is unavailable` || !errors.Is(err, jsonl.ErrSessionInUse) {
		t.Fatalf("locked transcript = %v", err)
	}
	if err := locked.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The catalog and the reopened transcript disagree.
	for name, test := range map[string]struct {
		parent, mode string
		code         Code
	}{
		"other parent": {"other", session.SubagentContinuable, CodeUnauthorized},
		"one-shot":     {"root", session.SubagentOneShot, CodeNotResumable},
	} {
		stray, err := h.registry.Create(context.Background(), agent.CreateRequest{ParentID: test.parent, Mode: test.mode, Provider: session.SubagentSpawn, Route: testRoute, Label: "stray", Depth: 1, Create: true})
		if err != nil {
			t.Fatal(err)
		}
		strayID := stray.Status().SessionID
		if err := h.registry.Close(context.Background(), strayID); err != nil {
			t.Fatal(err)
		}
		catalog := session.Record{Type: session.RecordSubagentCatalog, Turn: rootCall.invocation.Turn, Step: rootCall.invocation.Step, Catalog: &session.SubagentCatalog{SessionID: strayID, Mode: session.SubagentContinuable, Label: "stray"}}
		if _, err := rootCall.invocation.Journal.Append(context.Background(), catalog); err != nil {
			t.Fatal(err)
		}
		if err := h.service.SendMessage(context.Background(), "root", strayID, "hi"); code(err) != test.code {
			t.Errorf("%s: SendMessage = %v", name, err)
		}
		if _, err := h.registry.Find(strayID); !errors.Is(err, agent.ErrAgentNotFound) {
			t.Errorf("%s: rejected resume left the agent live", name)
		}
	}

	// The reopened agent stops before its events are read.
	beforePublish = func(resumed *agent.Agent) { _ = h.registry.Close(context.Background(), resumed.Status().SessionID) }
	if err := h.service.SendMessage(context.Background(), "root", id, "hi"); err == nil || !errors.Is(err, agent.ErrAgentNotFound) {
		t.Fatalf("stopped resume = %v", err)
	}

	// The parent begins closing while its child reopens.
	beforePublish = previous
	parent, err := h.service.StartContinuable(context.Background(), start(rootCall, "parent", "PARENT_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	parentCall := receive(t, h.held)
	grand, err := h.service.StartContinuable(context.Background(), start(parentCall, "grand", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(grand))
	setClosing := func(value bool) {
		h.service.mu.Lock()
		h.service.children[parent].closing = value
		h.service.mu.Unlock()
	}
	beforePublish = func(*agent.Agent) { setClosing(true) }
	if err := h.service.SendMessage(context.Background(), parent, grand, "hi"); code(err) != CodeUnauthorized || !strings.Contains(err.Error(), "is being released") {
		t.Fatalf("closing parent resume = %v", err)
	}
	beforePublish = func(*agent.Agent) { setClosing(true) }
	if _, err := h.service.Run(context.Background(), start(parentCall, "late", "CHILD_TASK", false)); code(err) != CodeUnauthorized {
		t.Fatalf("closing parent create = %v", err)
	}
	setClosing(false)
	beforePublish = previous
	parentCall.release <- "done"
	receive(t, h.done(parent))

	// The service stops while the child reopens.
	beforePublish = func(*agent.Agent) {
		go func() { _ = h.serviceScope.Close(context.Background()) }()
		for h.service.running() {
			runtime.Gosched()
		}
	}
	if err := h.service.SendMessage(context.Background(), "root", id, "hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopping resume = %v", err)
	}
	beforePublish = previous
	if statuses, _ := h.registry.Statuses(); len(statuses) != 1 {
		t.Fatalf("live agents = %#v", statuses)
	}
	rootCall.release <- "released"
	receive(t, results)
}

func TestService_LimitsContinuableChildrenPerPool(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "HOLD_TASK", first: reply{hold: true}, then: reply{text: "done"}},
		rule{match: "QUICK_TASK", first: reply{text: "quick"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	quick, err := h.service.StartContinuable(context.Background(), start(rootCall, "quick", "QUICK_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(quick))
	calls := make([]held, 0, maxActiveChildren)
	ids := make([]string, 0, maxActiveChildren)
	for index := range maxActiveChildren {
		id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker "+strconv.Itoa(index), "HOLD_TASK", false))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		calls = append(calls, receive(t, h.held))
	}
	const limit = "subagent limit reached (active child limit: 8); wait for an existing child to finish or complete this work with the current agents"
	if _, err := h.service.StartContinuable(context.Background(), start(rootCall, "extra", "HOLD_TASK", false)); code(err) != CodeLimitReached || err.Error() != limit {
		t.Fatalf("ninth child = %v", err)
	}
	if err := h.service.SendMessage(context.Background(), "root", quick, "hi"); code(err) != CodeLimitReached {
		t.Fatalf("resume at capacity = %v", err)
	}
	// One-shot children are outside the pool; a continuable child's
	// continuable children share it.
	if _, err := h.service.StartBackground(context.Background(), start(rootCall, "one-shot", "QUICK_TASK", false)); err != nil {
		t.Fatalf("one-shot at capacity = %v", err)
	}
	if _, err := h.service.StartContinuable(context.Background(), start(calls[0], "nested", "HOLD_TASK", false)); code(err) != CodeLimitReached {
		t.Fatalf("nested child at capacity = %v", err)
	}
	for index, call := range calls {
		call.release <- "done"
		receive(t, h.done(ids[index]))
	}
	rootCall.release <- "released"
	receive(t, results)
}

func TestService_ListsChildrenAndDescendants(t *testing.T) {
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "HOLD_TASK", first: reply{hold: true}, then: reply{text: "done"}},
		rule{match: "QUICK_TASK", first: reply{text: "quick"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	if _, err := h.service.Run(context.Background(), start(rootCall, "once", "QUICK_TASK", false)); err != nil {
		t.Fatal(err)
	}
	parent, err := h.service.StartContinuable(context.Background(), start(rootCall, "parent", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	parentCall := receive(t, h.held)
	grand, err := h.service.StartContinuable(context.Background(), start(parentCall, "grand", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	grandCall := receive(t, h.held)
	once := session.Children(h.events("root"))[0].SessionID
	// A catalog entry for a missing transcript, and one shared with another
	// parent, exercise the unreadable and already-visited branches.
	for _, entry := range []struct {
		call held
		id   string
	}{{rootCall, "ghost"}, {parentCall, once}} {
		record := session.Record{Type: session.RecordSubagentCatalog, Turn: entry.call.invocation.Turn, Step: entry.call.invocation.Step, Catalog: &session.SubagentCatalog{SessionID: entry.id, Mode: session.SubagentContinuable, Label: "extra"}}
		if _, err := entry.call.invocation.Journal.Append(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	children, err := h.service.ListChildren(context.Background(), "root")
	if err != nil || len(children) != 3 {
		t.Fatalf("children = %#v, %v", children, err)
	}
	if children[0] != (Entry{ID: once, Parent: "root", Label: "once", Mode: session.SubagentOneShot, Depth: 1}) ||
		children[1] != (Entry{ID: parent, Parent: "root", Label: "parent", Mode: session.SubagentContinuable, Depth: 1, Running: true}) {
		t.Fatalf("children = %#v", children)
	}
	descendants, err := h.service.ListDescendants(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, entry := range descendants {
		rows = append(rows, entry.ID+"@"+entry.Parent+"/"+strconv.Itoa(entry.Depth)+"/"+strconv.FormatBool(entry.Unavailable))
	}
	if strings.Join(rows, " ") != once+"@root/1/false "+parent+"@root/1/false "+grand+"@"+parent+"/2/false ghost@root/1/true" {
		t.Fatalf("descendants = %q", rows)
	}
	infos, err := h.service.List(parent)
	if err != nil || len(infos) != 1 || infos[0].SessionID != grand || infos[0].Label != "grand" || !infos[0].Busy || infos[0].Depth != 2 {
		t.Fatalf("List(parent) = %#v, %v", infos, err)
	}
	if all, _ := h.service.List(""); len(all) != 2 {
		t.Fatalf("List() = %#v", all)
	}

	if _, err := h.service.ListChildren(context.Background(), "ghost"); err == nil {
		t.Fatal("unreadable parent listed")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.service.ListChildren(cancelled, "root"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ListChildren = %v", err)
	}
	if _, err := h.service.ListDescendants(cancelled, "root"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ListDescendants = %v", err)
	}
	if _, err := h.service.ListDescendants(&lateCancel{Context: context.Background(), after: 1}, "root"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListDescendants cancelled mid-walk = %v", err)
	}
	grandCall.release <- "done"
	receive(t, h.done(grand))
	parentCall.release <- "done"
	receive(t, h.done(parent))
	rootCall.release <- "released"
	receive(t, results)
}

// lateCancel reports cancellation after its first after checks.
type lateCancel struct {
	context.Context
	after int
	calls int
}

func (ctx *lateCancel) Err() error {
	ctx.calls++
	if ctx.calls > ctx.after {
		return context.Canceled
	}
	return nil
}

func TestService_ReleaseEndsChildJobsAndStopClosesDeepestFirst(t *testing.T) {
	previous, previousParked := closeAgent, parked
	t.Cleanup(func() { closeAgent, parked = previous, previousParked })
	parkedChild := observeParked()
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "HOLD_TASK", first: reply{hold: true}, then: reply{text: "done"}},
		rule{match: "BG_BLOCK", first: reply{block: true}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	parent, err := h.service.StartContinuable(context.Background(), start(rootCall, "parent", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	parentCall := receive(t, h.held)
	jobID, err := h.service.StartBackground(context.Background(), start(parentCall, "background", "BG_BLOCK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.model.blocked)
	// The parent settles with its background job still running; the release
	// ends the job and its one-shot child.
	parentCall.release <- "done"
	receive(t, h.done(parent))
	if views := h.jobs.List(parent); len(views) != 0 {
		t.Fatalf("released owner kept jobs %#v (%s)", views, jobID)
	}
	if statuses, _ := h.registry.Statuses(); len(statuses) != 1 {
		t.Fatalf("live agents after release = %#v", statuses)
	}

	// Stop closes a parked parent and its working child, deepest first, and
	// joins close failures.
	parent, err = h.service.StartContinuable(context.Background(), start(rootCall, "parent", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	parentCall = receive(t, h.held)
	grand, err := h.service.StartContinuable(context.Background(), start(parentCall, "grand", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.held)
	parentCall.release <- "done"
	// Stop finds the parent's watcher parked, so only its cancellation ends it.
	awaitParked(t, parkedChild, parent)
	var order []string
	failure := errors.New("close failed")
	closeAgent = func(registry *agent.Registry, ctx context.Context, id string) error {
		order = append(order, id)
		if err := previous(registry, ctx, id); err != nil {
			return err
		}
		if id == grand {
			return failure
		}
		return nil
	}
	if err := h.serviceScope.Close(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("stop = %v", err)
	}
	if strings.Join(order, ",") != grand+","+parent {
		t.Fatalf("close order = %q", order)
	}
	if statuses, _ := h.registry.Statuses(); len(statuses) != 1 {
		t.Fatalf("live agents after stop = %#v", statuses)
	}
	if views := h.jobs.List(parent); len(views) != 0 {
		t.Fatalf("stopped owner kept jobs %#v", views)
	}
	if _, err := h.jobs.Launch(job.Spec{Kind: "probe", Label: "probe", Owner: "root", Run: func(context.Context, *job.Output) job.Outcome { return job.Outcome{Status: job.StatusCompleted} }}); err != nil {
		t.Fatalf("job service stopped with subagents: %v", err)
	}
	rootCall.release <- "released"
	receive(t, results)
}

func TestService_OneShotParentReleasesItsContinuableTree(t *testing.T) {
	previousParked := parked
	t.Cleanup(func() { parked = previousParked })
	parkedChild := observeParked()
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "ONCE_TASK", first: reply{hold: true}, then: reply{text: "once done"}},
		rule{match: "MIDDLE_TASK", first: reply{hold: true}, then: reply{text: "middle idle"}},
		rule{match: "LEAF_TASK", first: reply{hold: true}, then: reply{text: "leaf done"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	report := make(chan Report, 1)
	go func() {
		result, _ := h.service.Run(context.Background(), start(rootCall, "once", "ONCE_TASK", false))
		report <- result
	}()
	onceCall := receive(t, h.held)
	middle, err := h.service.StartContinuable(context.Background(), start(onceCall, "middle", "MIDDLE_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	middleCall := receive(t, h.held)
	leaf, err := h.service.StartContinuable(context.Background(), start(middleCall, "leaf", "LEAF_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	leafCall := receive(t, h.held)
	// A resident continuable child may write to its one-shot parent.
	if err := h.service.SendMessage(context.Background(), middle, onceCall.invocation.SessionID, "UP"); err != nil {
		t.Fatal(err)
	}
	// The middle child goes idle and parks on its working child; the release
	// below wakes its watcher, which then finds it closing.
	middleCall.release <- "done"
	awaitParked(t, parkedChild, middle)
	// Collecting the one-shot run releases the parked child and its child.
	onceCall.release <- "done"
	if result := receive(t, report); result.Outcome != session.OutcomeCompleted {
		t.Fatalf("one-shot report = %#v", result)
	}
	receive(t, h.done(middle))
	receive(t, h.done(leaf))
	if statuses, _ := h.registry.Statuses(); len(statuses) != 1 {
		t.Fatalf("live agents = %#v", statuses)
	}
	// Teardown releases are not settlements: no notices reach the parents.
	if notices := messages(h.events(middle), SourceSettled); len(notices) != 0 {
		t.Fatalf("middle notices = %q", notices)
	}
	select {
	case leafCall.release <- "late":
	default:
	}
	rootCall.release <- "released"
	receive(t, results)
}

func TestService_DeliveryEdgeCases(t *testing.T) {
	previous := beforeResume
	t.Cleanup(func() { beforeResume = previous })
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "HOLD_TASK", first: reply{hold: true}, then: reply{text: "done"}},
		rule{match: "QUICK_TASK", first: reply{text: "quick"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	// A parked resident child whose agent was stopped outside the service.
	stray, err := h.service.StartContinuable(context.Background(), start(rootCall, "stray", "HOLD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	strayCall := receive(t, h.held)
	if _, err := h.service.StartContinuable(context.Background(), start(strayCall, "leaf", "HOLD_TASK", false)); err != nil {
		t.Fatal(err)
	}
	receive(t, h.held)
	strayCall.release <- "done"
	strayAgent, _ := h.registry.Find(stray)
	h.idle(strayAgent)
	if err := h.registry.Close(context.Background(), stray); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SendMessage(context.Background(), "root", stray, "hi"); !errors.Is(err, agent.ErrNotRunning) {
		t.Fatalf("stopped resident = %v", err)
	}
	if infos, _ := h.service.List(""); len(infos) != 0 {
		t.Fatalf("stopped resident handle kept: %#v", infos)
	}

	settled, err := h.service.StartContinuable(context.Background(), start(rootCall, "settled", "QUICK_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(settled))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.service.SendMessage(cancelled, "root", settled, "hi"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled catalog read = %v", err)
	}
	// The service stops after the catalog read.
	beforeResume = func() {
		go func() { _ = h.serviceScope.Close(context.Background()) }()
		for h.service.running() {
			runtime.Gosched()
		}
	}
	if err := h.service.SendMessage(context.Background(), "root", settled, "hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stop during resume = %v", err)
	}
	rootCall.release <- "released"
	receive(t, results)
}

func TestService_InterruptedChildKeepsDeliveredMessages(t *testing.T) {
	previousParked, previousClose := parked, closeAgent
	t.Cleanup(func() { parked, closeAgent = previousParked, previousClose })
	h := startHarness(t,
		rule{match: "ROOT_HOLD", first: reply{hold: true}, then: reply{text: "root done"}},
		rule{match: "CHILD_TASK", first: reply{block: true}},
		rule{match: "sent a message: AGAIN", first: reply{text: "handled both"}},
	)
	rootCall, results := h.submit("ROOT_HOLD")
	id, err := h.service.StartContinuable(context.Background(), start(rootCall, "worker", "CHILD_TASK", false))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, h.model.blocked)
	parkedChild, closed := make(chan string, 4), make(chan string, 4)
	parked = func(id string) { parkedChild <- id }
	closeAgent = func(registry *agent.Registry, ctx context.Context, target string) error {
		closed <- target
		return previousClose(registry, ctx, target)
	}
	// The message is accepted while the child's model request runs; the
	// interrupt cancels the turn before any boundary commits the message.
	if err := h.service.SendMessage(context.Background(), "root", id, "QUEUED"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Interrupt("root", id); err != nil {
		t.Fatal(err)
	}
	select {
	case parkedID := <-parkedChild:
		if parkedID != id {
			t.Fatalf("parked %q", parkedID)
		}
	case closedID := <-closed:
		t.Fatalf("child %q settled with an accepted message still queued", closedID)
	case <-time.After(waitLimit):
		t.Fatal("watcher neither parked nor settled")
	}
	if outcome, _ := session.LastOutcome(h.events(id)); outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted turn outcome = %q", outcome)
	}
	// The next delivery wakes the resident child, which handles both messages.
	parked = previousParked
	if err := h.service.SendMessage(context.Background(), "root", id, "AGAIN"); err != nil {
		t.Fatal(err)
	}
	receive(t, h.done(id))
	relayed := messages(h.events(id), SourceAgentMessage)
	if len(relayed) != 2 || relayed[0] != "Agent root sent a message: QUEUED" || relayed[1] != "Agent root sent a message: AGAIN" {
		t.Fatalf("child messages = %q", relayed)
	}
	rootCall.release <- "released"
	receive(t, results)
	h.idle(h.root)
	if notices := messages(h.events("root"), SourceSettled); len(notices) != 1 || !strings.HasSuffix(notices[0], "Its closing message:handled both") {
		t.Fatalf("root notices = %q", notices)
	}
}
