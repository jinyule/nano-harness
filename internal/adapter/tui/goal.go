package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const goalUsage = "Usage: /goal [<objective>|clear|edit <objective>|pause|resume]"

// GoalService exposes the root session's goal use cases to /goal.
type GoalService interface {
	Get(context.Context, string) (*appGoal.View, error)
	Create(context.Context, string, string, *float64, appGoal.Actor) (*appGoal.View, error)
	Edit(context.Context, string, session.GoalRef, *string, *float64, appGoal.Actor) (*appGoal.View, error)
	Pause(context.Context, string, session.GoalRef, appGoal.Actor) (*appGoal.View, error)
	Resume(context.Context, string, session.GoalRef, appGoal.Actor) (*appGoal.View, error)
	Clear(context.Context, string, session.GoalRef, appGoal.Actor) error
}

// goalMessage carries a /goal result; failed reports a direct command error.
type goalMessage struct {
	lines  []string
	failed bool
	err    error
}

// goalCommand runs the upstream /goal grammar: the control words clear,
// pause, resume, and edit count only when they fill the whole argument or,
// for edit, lead it; any other text is a new objective. Output stays in the
// terminal and never reaches the model.
func (model model) goalCommand(argument string) (tea.Model, tea.Cmd) {
	if len(model.images) > 0 {
		return model.withError("attachments cannot accompany /goal; send them as a message")
	}
	goals, sessionID := model.app.config.Goals, model.app.agent.Status().SessionID
	return model, func() tea.Msg {
		lines, failed, err := runGoalCommand(model.ctx, goals, sessionID, argument)
		return goalMessage{lines: lines, failed: failed, err: err}
	}
}

func runGoalCommand(ctx context.Context, goals GoalService, sessionID, argument string) ([]string, bool, error) {
	current, err := goals.Get(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	lines, failed, err := applyGoalCommand(ctx, goals, sessionID, current, argument)
	var rejection *appGoal.Error
	if errors.As(err, &rejection) {
		return []string{"The goal command is not valid for the current state. Run /goal to view available commands."}, true, nil
	}
	return lines, failed, err
}

func applyGoalCommand(ctx context.Context, goals GoalService, sessionID string, current *appGoal.View, argument string) ([]string, bool, error) {
	input := strings.TrimSpace(argument)
	control := strings.ToLower(input)
	missing := func(action string) ([]string, bool, error) {
		return []string{"No goal is currently set; /goal " + action + " requires one. " + goalUsage}, true, nil
	}
	var view *appGoal.View
	var err error
	title := ""
	switch {
	case input == "":
		if current == nil {
			return []string{"No goal is currently set.", goalUsage}, false, nil
		}
		return renderGoal("Goal", *current), false, nil
	case control == "clear":
		if current == nil {
			return []string{"No goal to clear."}, false, nil
		}
		if err := goals.Clear(ctx, sessionID, current.Goal.Ref(), appGoal.ActorHost); err != nil {
			return nil, false, err
		}
		return []string{"Goal cleared."}, false, nil
	case control == "pause" || control == "resume":
		if current == nil {
			return missing(control)
		}
		if control == "pause" {
			title = "Goal paused"
			view, err = goals.Pause(ctx, sessionID, current.Goal.Ref(), appGoal.ActorHost)
		} else {
			title = "Goal resumed"
			view, err = goals.Resume(ctx, sessionID, current.Goal.Ref(), appGoal.ActorHost)
		}
	case control == "edit":
		return []string{"Goal editing requires a replacement objective.", goalUsage}, true, nil
	case len(input) > 4 && strings.EqualFold(input[:4], "edit") && unicode.IsSpace(rune(input[4])):
		objective := strings.TrimSpace(input[4:])
		switch {
		case current == nil:
			return missing("edit")
		case current.Goal.Phase == session.GoalComplete:
			title = "Goal created"
			view, err = goals.Create(ctx, sessionID, objective, nil, appGoal.ActorHost)
		default:
			title = "Goal updated"
			view, err = goals.Edit(ctx, sessionID, current.Goal.Ref(), &objective, nil, appGoal.ActorHost)
		}
	default:
		if current != nil && current.Goal.Phase != session.GoalComplete {
			return []string{fmt.Sprintf("A goal is already %s. Use /goal edit <objective> to change it or /goal clear before replacing it.", current.Goal.Phase)}, true, nil
		}
		title = "Goal created"
		view, err = goals.Create(ctx, sessionID, input, nil, appGoal.ActorHost)
	}
	if err != nil {
		return nil, false, err
	}
	return renderGoal(title, *view), false, nil
}

// renderGoal shows a goal without its compare-and-set identity.
func renderGoal(title string, view appGoal.View) []string {
	lines := []string{title, "Status: " + string(view.Goal.Phase)}
	if reason := view.Goal.BlockedReason; reason != nil {
		lines = append(lines, "Blocker: "+reason.Code+": "+reason.Message)
	}
	activation := "disarmed"
	if view.Armed {
		activation = "armed"
	}
	lines = append(lines, "Objective: "+view.Goal.Objective, fmt.Sprintf("Rounds: %d/%d", view.RoundsStarted, view.Goal.MaxRounds), "Activation: "+activation, "")
	var hint string
	switch view.Goal.Phase {
	case session.GoalActive:
		hint = "/goal edit <objective>, /goal resume, /goal clear"
		if view.Armed {
			hint = "/goal edit <objective>, /goal pause, /goal clear"
		}
	case session.GoalComplete:
		hint = "/goal <objective>, /goal clear"
	case session.GoalPaused, session.GoalBlocked:
		hint = "/goal edit <objective>, /goal resume, /goal clear"
	}
	return append(lines, "Commands: "+hint)
}

// goalStatus summarizes the durable goal for the header, or "" without one.
func goalStatus(state session.GoalState) string {
	if state.Goal == nil {
		return ""
	}
	return fmt.Sprintf("goal=%s %d/%d ", state.Goal.Phase, state.RoundsStarted, state.Goal.MaxRounds)
}

// goalLine presents one goal/change record.
func goalLine(change session.GoalChange) string {
	if change.Operation == session.GoalOpClear {
		return "goal> cleared"
	}
	snapshot := change.Snapshot
	line := fmt.Sprintf("goal> %s: %s (%s %d/%d)", change.Operation, strings.Join(strings.Fields(snapshot.Objective), " "), snapshot.Phase, change.RoundsStarted, snapshot.MaxRounds)
	if reason := snapshot.BlockedReason; reason != nil {
		line += " blocker=" + reason.Code + ": " + reason.Message
	}
	return line
}
