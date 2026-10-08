package goal

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestService_SettleUsesStoppedRoundRef(t *testing.T) {
	for _, outcome := range []session.TurnOutcome{session.OutcomeError, session.OutcomeMaxTokens} {
		for _, replacement := range []bool{false, true} {
			for _, checkpoint := range []bool{false, true} {
				name := string(outcome) + "/" + map[bool]string{false: "pause-resume", true: "clear-create"}[replacement] + "/" + map[bool]string{false: "full-log", true: "after-authorization"}[checkpoint]
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					fixture := startService(t)
					old := fixture.create(t, "old goal", nil)
					message := RoundMessage(*old)
					fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 1}, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message})
					ending := make(chan struct{})
					done := make(chan struct{})
					release := sync.OnceFunc(func() { close(ending) })
					go func() {
						<-ending
						fixture.journal.raw(session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: outcome})
						close(done)
					}()
					t.Cleanup(func() { release(); <-done })
					var authorized *View
					var err error
					if replacement {
						if err := fixture.service.Clear(ctx, "root", old.Goal.Ref(), ActorHost); err != nil {
							t.Fatal(err)
						}
						authorized, err = fixture.service.Create(ctx, "root", "new goal", nil, ActorHost)
					} else {
						paused, pauseErr := fixture.service.Pause(ctx, "root", old.Goal.Ref(), ActorHost)
						if pauseErr != nil {
							t.Fatal(pauseErr)
						}
						authorized, err = fixture.service.Resume(ctx, "root", paused.Goal.Ref(), ActorHost)
					}
					if err != nil {
						t.Fatal(err)
					}
					var after uint64
					if checkpoint {
						events, _ := fixture.journal.Events(ctx)
						after = events[len(events)-1].Sequence
					}
					release()
					select {
					case <-done:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if _, err := fixture.service.Settle(ctx, "root", after); err != nil {
						t.Fatal(err)
					}
					current, err := fixture.service.Get(ctx, "root")
					if err != nil || current == nil || current.Goal.Ref() != authorized.Goal.Ref() || current.Goal.Phase != session.GoalActive || !current.Armed {
						t.Fatalf("old %s revoked new authorization: %+v, %v", outcome, current, err)
					}
					opened := false
					if err := fixture.service.Admit(ctx, fixture.journal, RoundMessage(*current), func(context.Context) error { opened = true; return nil }); err != nil || !opened {
						t.Fatalf("new authorization cannot advance: %v, opened=%t", err, opened)
					}
				})
			}
		}
	}
}

func TestService_SettleNonRoundStopsDisarmCurrentGoal(t *testing.T) {
	for _, outcome := range []session.TurnOutcome{session.OutcomeError, session.OutcomeMaxTokens} {
		t.Run(string(outcome), func(t *testing.T) {
			fixture := startService(t)
			message := agentText("work")
			fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 1}, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message})
			created := fixture.create(t, "ship", nil)
			fixture.journal.raw(session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: outcome})
			if _, err := fixture.service.Settle(t.Context(), "root", 0); err != nil {
				t.Fatal(err)
			}
			current, err := fixture.service.Get(t.Context(), "root")
			if err != nil || current == nil || current.Goal.Ref() != created.Goal.Ref() || current.Armed {
				t.Fatalf("non-round stop did not disarm its current goal: %+v, %v", current, err)
			}
		})
	}
	t.Run("no goal", func(t *testing.T) {
		fixture := startService(t)
		message := agentText("work")
		fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 1}, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}, session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeError})
		if _, err := fixture.service.Settle(t.Context(), "root", 0); err != nil {
			t.Fatal(err)
		}
		if current, err := fixture.service.Get(t.Context(), "root"); err != nil || current != nil {
			t.Fatalf("goal-less stop created a goal: %+v, %v", current, err)
		}
	})
}
