package agent

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// calledFrom reports whether the journal method running this hook was
// called directly by a function whose name ends with caller.
func calledFrom(caller string) bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(0, pcs)])
	for frame, more := frames.Next(); more; frame, more = frames.Next() {
		if strings.HasSuffix(frame.Function, ".(*journal).Events") || strings.HasSuffix(frame.Function, ".(*journal).Append") {
			next, _ := frames.Next()
			return strings.HasSuffix(next.Function, caller)
		}
	}
	return false
}

// TestAgent_InterruptedTurnIsCanceledWhereverItFails interrupts the root's
// turn at each point where the failure that follows does not itself carry
// a cancellation the outcome was derived from. A notice queued just before
// the interrupt must wait for the next turn, as it does for any cancelled
// turn: the returned outcome, the durable turn/end, and the absence of a
// wake all agree, and the original failure is kept.
func TestAgent_InterruptedTurnIsCanceledWhereverItFails(t *testing.T) {
	timeout := &llm.Error{Code: llm.ErrorTimeout, Provider: "openai"}
	contextFailure := &llm.Error{Code: llm.ErrorContextWindow, Provider: "openai"}
	for _, test := range []struct {
		name    string
		actions func(gate chan struct{}, started chan struct{}) []modelAction
		// hook interrupts from inside a journal call and fails it like the
		// JSONL log refuses a cancelled context; nil interrupts from the test.
		events func(log *memoryLog) bool
		append func(record session.Record) bool
		seed   bool
		cause  error
	}{
		{
			name:   "proactive compaction",
			events: func(*memoryLog) bool { return calledFrom("compaction.(*Service).Maybe") },
			cause:  context.Canceled,
		},
		{
			name:   "request header",
			append: func(record session.Record) bool { return record.Type == session.RecordRequestHeader },
			cause:  context.Canceled,
		},
		{
			name: "step events",
			events: func(log *memoryLog) bool {
				return calledFrom("agent.(*Engine).runTurn") && log.events[len(log.events)-1].Record.Type == session.RecordRequestHeader
			},
			cause: context.Canceled,
		},
		{
			name: "assistant message",
			actions: func(chan struct{}, chan struct{}) []modelAction {
				return []modelAction{{completion: assistantCompletion("late")}}
			},
			append: func(record session.Record) bool { return record.Type == session.RecordAssistantMessage },
			cause:  context.Canceled,
		},
		{
			name: "forced compaction",
			actions: func(gate, started chan struct{}) []modelAction {
				return []modelAction{{err: contextFailure}, {started: started, wait: gate}}
			},
			seed:  true,
			cause: contextFailure,
		},
		{
			// The provider's own timeout ends the stream first; the user
			// interrupts before the failure is classified.
			name: "provider timeout then interrupt",
			actions: func(gate, started chan struct{}) []modelAction {
				return []modelAction{{started: started, wait: gate, uncancelable: true, chunks: []session.AssistantChunk{{Kind: session.ChunkReasoning, Text: "thinking"}}, err: timeout}}
			},
			cause: timeout,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate, started := make(chan struct{}), make(chan struct{}, 1)
			var actions []modelAction
			if test.actions != nil {
				actions = test.actions(gate, started)
			}
			actions = append(actions, modelAction{completion: assistantCompletion("next")})
			harness := startEngineHarness(t, 2, actions...)
			repository := newMemoryRepository()
			registry, _ := startRegistry(t, harness, repository, newMemoryPolicy())
			root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
			if err != nil {
				t.Fatal(err)
			}
			log := repository.logs["root"]
			interrupt := func() error {
				if err := root.Notify(noticeMessage("pending")); err != nil {
					t.Error(err)
				}
				root.Interrupt()
				return context.Canceled
			}
			armed := true
			log.mu.Lock()
			if test.seed {
				seedSurface(log)
			}
			if test.events != nil {
				log.eventsHook = func(int) error {
					if armed && test.events(log) {
						armed = false
						return interrupt()
					}
					return nil
				}
			}
			if test.append != nil {
				log.appendHook = func(record session.Record) error {
					if armed && test.append(record) {
						armed = false
						return interrupt()
					}
					return nil
				}
			}
			log.mu.Unlock()
			results, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "go"))
			if err != nil {
				t.Fatal(err)
			}
			if test.actions != nil && test.events == nil && test.append == nil {
				<-started
				_ = interrupt()
				close(gate)
			}
			result := <-results
			if result.Outcome != session.OutcomeCanceled || !errors.Is(result.Err, test.cause) {
				t.Fatalf("interrupted turn = %+v", result)
			}
			if err := root.WhenIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if last := root.Status().Last; last.Turn != 1 || last.Outcome != session.OutcomeCanceled {
				t.Fatalf("the interrupt woke a notice turn: %+v", last)
			}
			events, _ := root.Events(context.Background())
			if outcomes := turnOutcomes(events); !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeCanceled}) {
				t.Fatalf("durable outcomes = %v", outcomes)
			}
			// The notice was not lost: the next turn delivers it.
			next, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "again"))
			if result := <-next; result.Outcome != session.OutcomeCompleted || result.Text != "next" {
				t.Fatalf("next turn = %+v", result)
			}
			events, _ = root.Events(context.Background())
			if texts := userTexts(events); !slices.Contains(texts, "2:pending") {
				t.Fatalf("pending notice not delivered by the next turn: %q", texts)
			}
		})
	}
}
