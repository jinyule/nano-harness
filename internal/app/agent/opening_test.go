package agent

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// unclosedTurns lists turns whose turn/start has no turn/end.
func unclosedTurns(events []session.Event) []uint64 {
	open := map[uint64]bool{}
	var order []uint64
	for _, event := range events {
		record := event.Record
		if record.Type == session.RecordTurnStart {
			open[record.Turn] = true
			order = append(order, record.Turn)
		}
		if record.Type == session.RecordTurnEnd {
			delete(open, record.Turn)
		}
	}
	return slices.DeleteFunc(order, func(turn uint64) bool { return !open[turn] })
}

// TestAgent_InterruptWhileOpeningCommitsTheWholeOpening interrupts a woken
// notice turn right after its turn/start commits. The opening message still
// commits and the turn closes canceled, so the next turn is valid.
func TestAgent_InterruptWhileOpeningCommitsTheWholeOpening(t *testing.T) {
	_, registry, repository := durableHarness(t, 2, modelAction{completion: assistantCompletion("next answer")})
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	var interrupted atomic.Bool
	repository.logs["root"].appendHook = func(record session.Record) error {
		if record.Type == session.RecordTurnStart && interrupted.CompareAndSwap(false, true) {
			root.Interrupt()
		}
		return nil
	}
	if err := registry.QueueNotice(context.Background(), "root", noticeMessage("job finished")); err != nil {
		t.Fatal(err)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := root.Status().Last; last.Outcome != session.OutcomeCanceled || last.Turn != 1 {
		t.Fatalf("woken turn = %+v", last)
	}
	next, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "next"))
	if err != nil {
		t.Fatal(err)
	}
	if result := <-next; result.Err != nil || result.Text != "next answer" || result.Turn != 2 {
		t.Fatalf("next = %+v", result)
	}
	events, _ := root.Events(context.Background())
	if open := unclosedTurns(events); len(open) != 0 {
		t.Fatalf("turns left open: %v", open)
	}
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:job finished", "2:next"}) || !slices.Equal(deliveredNotices(events), []string{"1:notice-1"}) {
		t.Fatalf("user messages = %q, delivered = %q", texts, deliveredNotices(events))
	}
}

// TestAgent_WokenTurnCancelledBeforeOpeningKeepsItsNotice cancels a woken
// turn after it took its opening notice but before turn/start commits.
// Nothing is committed, the notice returns to the head of the queue, and
// the next turn delivers it once, for durable and in-memory notices alike.
func TestAgent_WokenTurnCancelledBeforeOpeningKeepsItsNotice(t *testing.T) {
	for _, durable := range []bool{true, false} {
		t.Run(map[bool]string{true: "durable", false: "in memory"}[durable], func(t *testing.T) {
			harness, registry, repository := durableHarness(t, 2, modelAction{completion: assistantCompletion("next answer")})
			root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
			log := repository.logs["root"]
			var armed atomic.Bool
			// The woken turn's first read of the log is the last moment
			// before it would commit turn/start.
			log.eventsHook = func(int) error {
				if armed.CompareAndSwap(true, false) {
					root.Interrupt()
				}
				return nil
			}
			if durable {
				log.appendHook = func(record session.Record) error {
					if record.Type == session.RecordNoticeQueued {
						armed.Store(true)
					}
					return nil
				}
				if err := root.QueueNotice(context.Background(), noticeMessage("job finished")); err != nil {
					t.Fatal(err)
				}
			} else {
				armed.Store(true)
				if err := root.Notify(noticeMessage("job finished")); err != nil {
					t.Fatal(err)
				}
			}
			if err := root.WhenIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if last := root.Status().Last; last.Outcome != session.OutcomeCanceled {
				t.Fatalf("woken turn = %+v", last)
			}
			events, _ := root.Events(context.Background())
			if turns := countType(recordTypes(events), session.RecordTurnStart); turns != 0 {
				t.Fatalf("a cancelled opening committed %d turns", turns)
			}
			next, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "next"))
			if result := <-next; result.Text != "next answer" {
				t.Fatalf("next = %+v", result)
			}
			events, _ = root.Events(context.Background())
			if texts := userTexts(events); !slices.Equal(texts, []string{"1:next", "1:job finished"}) {
				t.Fatalf("user messages = %q", texts)
			}
			harness.model.mu.Lock()
			calls := len(harness.model.seen)
			harness.model.mu.Unlock()
			if calls != 1 {
				t.Fatalf("model calls = %d", calls)
			}
		})
	}
}

// TestAgent_StaleWakeDoesNotReopenAfterInterrupt reproduces a wake that
// was pending when a submitted turn started instead of the notice turn: the
// worker's select took the turn request while the wake was set. The turn
// takes the notice at its start, so the wake is spent; a notice queued
// before a later interrupt must wait for the next turn.
func TestAgent_StaleWakeDoesNotReopenAfterInterrupt(t *testing.T) {
	// The hook is installed before any worker starts and restored after
	// the harness cleanup has stopped them all.
	parked, release := make(chan *Agent), make(chan struct{})
	var held atomic.Bool
	previous := beforeAgentWait
	t.Cleanup(func() { beforeAgentWait = previous })
	beforeAgentWait = func(agent *Agent) {
		if held.CompareAndSwap(false, true) {
			parked <- agent
			<-release
		}
	}
	started := make(chan struct{}, 1)
	harness, registry, _ := durableHarness(t, 2,
		modelAction{started: started, wait: make(chan struct{})},
		modelAction{completion: assistantCompletion("unexpected turn")},
	)
	created := make(chan *Agent, 1)
	go func() {
		root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
		created <- root
	}()
	worker := <-parked
	root := <-created
	if worker != root {
		t.Fatal("a different worker reached its wait")
	}
	// The worker is between claimWake and select. An idle notice sets the
	// wake; select then takes the turn request, modeled by removing the
	// wake token before the request arrives.
	if err := root.Notify(noticeMessage("idle notice")); err != nil {
		t.Fatal(err)
	}
	<-root.wake
	results, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "block"))
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	<-started
	if err := root.Notify(noticeMessage("before interrupt")); err != nil {
		t.Fatal(err)
	}
	root.Interrupt()
	if result := <-results; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	if texts := userTexts(events); slices.ContainsFunc(texts, func(text string) bool { return strings.HasSuffix(text, ":before interrupt") }) || !slices.Equal(texts, []string{"1:block", "1:idle notice"}) {
		t.Fatalf("user messages = %q", texts)
	}
	harness.model.mu.Lock()
	calls := len(harness.model.seen)
	harness.model.mu.Unlock()
	if calls != 1 || root.Status().Last.Turn != 1 {
		t.Fatalf("model calls = %d, status = %+v", calls, root.Status())
	}
}

// TestAgent_InterruptAtStepContextEndsTheTurnCanceled interrupts a turn
// right after a step context provider's message commits, before the step
// opens. The message stays committed, the turn closes canceled rather than
// failing, and the next turn opens normally.
func TestAgent_InterruptAtStepContextEndsTheTurnCanceled(t *testing.T) {
	harness, registry, repository := durableHarness(t, 2, modelAction{completion: assistantCompletion("next answer")})
	scope := &plugin.Scope{}
	if err := harness.engine.RegisterContext(&contextProbe{kind: "runtime-context", messages: 1}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	var interrupted atomic.Bool
	repository.logs["root"].appendHook = func(record session.Record) error {
		if record.Type == session.RecordUserMessage && record.Message.Source.Kind == "runtime-context" && interrupted.CompareAndSwap(false, true) {
			root.Interrupt()
		}
		return nil
	}
	first, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "work"))
	if result := <-first; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted at step context = %+v", result)
	}
	next, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "next"))
	if result := <-next; result.Err != nil || result.Text != "next answer" {
		t.Fatalf("next = %+v", result)
	}
	events, _ := root.Events(context.Background())
	if open := unclosedTurns(events); len(open) != 0 {
		t.Fatalf("turns left open: %v", open)
	}
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:work", "1:runtime-context", "2:next", "2:runtime-context"}) {
		t.Fatalf("user messages = %q", texts)
	}
	if outcomes := turnOutcomes(events); !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeCanceled, session.OutcomeCompleted}) {
		t.Fatalf("turn outcomes = %q", outcomes)
	}
}
