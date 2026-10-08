package goal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// RootSource publishes the root agent the driver continues.
type RootSource interface {
	Agent() (agent.Controller, error)
}

// Goals is the goal state the driver reads and settles.
type Goals interface {
	Get(context.Context, string) (*View, error)
	Settle(context.Context, string, uint64) (uint64, error)
	Block(context.Context, string, session.GoalRef, session.GoalBlockReason, Actor) (*View, error)
	Disarm(string)
	DisarmRevision(string, session.GoalRef)
	Watch(string, func(Change), *plugin.Scope) error
}

// Driver continues the root agent's active, armed goal through sequential
// goal rounds. At whole-agent idle it settles the turns that ended since it
// last looked, then queues one round prompt through Followup when the goal
// is still active, armed, and under its round cap; the round's admission
// re-checks the goal when the turn opens. Rounds run under whatever plan
// mode and approval policy is in force, like any other turn.
type Driver struct {
	goals Goals
	root  RootSource
}

// NewDriver constructs an inactive driver.
func NewDriver(goals Goals, root RootSource) (*Driver, error) {
	if goals == nil || root == nil {
		return nil, errors.New("invalid goal driver configuration")
	}
	return &Driver{goals: goals, root: root}, nil
}

// ID returns the stable plugin identity.
func (*Driver) ID() string { return "goal-driver" }

// Start takes over the root session with continuation disarmed, so neither
// a restored nor an earlier active goal continues until a person or the
// model resumes it, and runs the driver until scope cleanup.
func (driver *Driver) Start(ctx context.Context, scope *plugin.Scope) error {
	controller, err := driver.root.Agent()
	if err != nil {
		return err
	}
	sessionID := controller.Status().SessionID
	events, err := controller.Events(ctx)
	if err != nil {
		return err
	}
	var seen uint64
	if len(events) > 0 {
		seen = events[len(events)-1].Sequence
	}
	driver.goals.Disarm(sessionID)
	runContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	if err := scope.Defer(func(closeContext context.Context) error {
		// Disarm first so a queued round is refused when its turn opens,
		// then stop the loop, which interrupts an in-flight round. Start
		// closes done itself when it fails before the loop runs.
		driver.goals.Disarm(sessionID)
		cancel()
		select {
		case <-done:
			return nil
		case <-closeContext.Done():
			return fmt.Errorf("stop goal driver: %w", closeContext.Err())
		}
	}); err != nil {
		cancel()
		return err
	}
	wake := make(chan struct{}, 1)
	if err := driver.goals.Watch(sessionID, func(change Change) {
		// A person's pause stops goal work at once: the running turn,
		// whatever it is, is interrupted. A model pause finishes its turn.
		if change.Operation == session.GoalOpPause && change.Actor == ActorHost {
			controller.Interrupt()
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}, scope); err != nil {
		close(done)
		return err
	}
	go driver.run(runContext, controller, sessionID, seen, wake, done)
	return nil
}

func (driver *Driver) run(ctx context.Context, controller agent.Controller, sessionID string, seen uint64, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	// refused is the round whose admission refused it; a refusal that
	// settling the turns since does not explain is persistent.
	var refused *session.MessageSource
	for {
		if controller.WhenIdle(ctx) != nil {
			return
		}
		if settled, err := driver.goals.Settle(ctx, sessionID, seen); err == nil {
			seen = settled
		}
		if view, err := driver.goals.Get(ctx, sessionID); err == nil && view != nil && view.Goal.Phase == session.GoalActive && view.Armed {
			next := RoundMessage(*view).Source
			if refused != nil && *refused == next {
				refused = nil
				reason := session.GoalBlockReason{Code: "prompt-rejected", Message: "Goal round was rejected before entering its step."}
				_, _ = driver.goals.Block(ctx, sessionID, view.Goal.Ref(), reason, ActorDriver)
				continue
			}
			var stop bool
			if refused, stop = driver.round(ctx, controller, sessionID, *view); stop {
				return
			}
			continue
		}
		refused = nil
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

// round blocks an exhausted goal or runs one round to its end. It returns
// the round's source when admission refused it, and whether the driver
// must stop.
func (driver *Driver) round(ctx context.Context, controller agent.Controller, sessionID string, view View) (*session.MessageSource, bool) {
	ref := view.Goal.Ref()
	if view.RoundsStarted >= view.Goal.MaxRounds {
		reason := session.GoalBlockReason{Code: "round-limit", Message: fmt.Sprintf("Goal reached its configured limit of %d rounds.", view.Goal.MaxRounds)}
		_, _ = driver.goals.Block(ctx, sessionID, ref, reason, ActorDriver)
		return nil, false
	}
	message := RoundMessage(view)
	results, err := controller.Followup(ctx, message)
	if err != nil {
		if ctx.Err() != nil {
			return nil, true
		}
		reason := session.GoalBlockReason{Code: "queue-failed", Message: fmt.Sprintf("Could not queue goal round %d: %v", message.Source.GoalRound, err)}
		_, _ = driver.goals.Block(ctx, sessionID, ref, reason, ActorDriver)
		return nil, false
	}
	var result agent.TurnResult
	select {
	case result = <-results:
	case <-ctx.Done():
		controller.Interrupt()
		<-results
		return nil, true
	}
	if errors.Is(result.Err, agent.ErrNotAdmitted) {
		return &message.Source, false
	}
	if result.Err != nil {
		// A failed append can leave no turn/end for Settle to observe.
		driver.goals.DisarmRevision(sessionID, ref)
	}
	return nil, false
}
