package goal

import (
	"context"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// TestService_SettleWindowKeepsACancelledRoundPause covers several turns
// ending between two driver checkpoints. Following upstream, a later
// cancelled turn keeps the cancelled round's pause; a later failure or
// output limit disarms the revision first, so the round is not paused;
// and a later human authorization keeps its own activation until a stop of
// its own revision.
func TestService_SettleWindowKeepsACancelledRoundPause(t *testing.T) {
	ctx := context.Background()
	turn := func(journal *memoryJournal, number uint64, message session.Message, outcome session.TurnOutcome) {
		journal.raw(session.Record{Type: session.RecordTurnStart, Turn: number}, session.Record{Type: session.RecordUserMessage, Turn: number, Message: &message}, session.Record{Type: session.RecordTurnEnd, Turn: number, Outcome: outcome})
	}
	for _, test := range []struct {
		name  string
		after func(*testing.T, *fixture, *View)
		phase session.GoalPhase
		armed bool
	}{
		{"cancelled then cancelled", func(_ *testing.T, fixture *fixture, _ *View) {
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeCanceled)
		}, session.GoalPaused, false},
		{"cancelled then cancelled twice", func(_ *testing.T, fixture *fixture, _ *View) {
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeCanceled)
			turn(fixture.journal, 3, agentText("again"), session.OutcomeCanceled)
		}, session.GoalPaused, false},
		{"cancelled then completed", func(_ *testing.T, fixture *fixture, _ *View) {
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeCompleted)
		}, session.GoalPaused, false},
		{"cancelled then failed", func(_ *testing.T, fixture *fixture, _ *View) {
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeError)
		}, session.GoalActive, false},
		{"cancelled then truncated", func(_ *testing.T, fixture *fixture, _ *View) {
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeMaxTokens)
		}, session.GoalActive, false},
		{"cancelled then failed then cancelled", func(_ *testing.T, fixture *fixture, _ *View) {
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeError)
			turn(fixture.journal, 3, agentText("again"), session.OutcomeCanceled)
		}, session.GoalActive, false},
		{"cancelled then reauthorized", func(t *testing.T, fixture *fixture, view *View) {
			paused, err := fixture.service.Pause(ctx, "root", view.Goal.Ref(), ActorHost)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.service.Resume(ctx, "root", paused.Goal.Ref(), ActorHost); err != nil {
				t.Fatal(err)
			}
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeCompleted)
		}, session.GoalActive, true},
		{"cancelled then reauthorized then cancelled", func(t *testing.T, fixture *fixture, view *View) {
			paused, err := fixture.service.Pause(ctx, "root", view.Goal.Ref(), ActorHost)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.service.Resume(ctx, "root", paused.Goal.Ref(), ActorHost); err != nil {
				t.Fatal(err)
			}
			turn(fixture.journal, 2, agentText("mine"), session.OutcomeCanceled)
		}, session.GoalActive, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := startService(t)
			view := fixture.create(t, "ship", nil)
			turn(fixture.journal, 1, RoundMessage(*view), session.OutcomeCanceled)
			test.after(t, fixture, view)
			if _, err := fixture.service.Settle(ctx, "root", 0); err != nil {
				t.Fatal(err)
			}
			current, err := fixture.service.Get(ctx, "root")
			if err != nil || current.Goal.Phase != test.phase || current.Armed != test.armed {
				t.Fatalf("goal = %+v, %v; want phase %s armed %t", current, err, test.phase, test.armed)
			}
		})
	}
}
