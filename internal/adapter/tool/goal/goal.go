// Package goal exposes the upstream get_goal, create_goal, and update_goal
// tools over the calling session's long-running goal.
//
// Authority is decided at the execution point from the call's own turn:
// create, edit, pause, and resume need a person's message in a root agent's
// turn; complete and blocked also accept the exact current goal round,
// blocked only from the third round on. A successful complete or blocked in
// a goal round queues a closing instruction so the model addresses the user
// once before the turn ends.
package goal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// BlockedAfterRounds is the fewest admitted rounds before a goal round may
// report its goal blocked, the upstream default named in the guidance.
const BlockedAfterRounds = 3

// Model-visible definitions, verbatim from the upstream Base composition.
const (
	createDescription = "Create a persisted goal that keeps this session working across automatic continuation rounds. Use it when the direct human request is a long-running objective, even if the user did not say \"goal\"; not for single-turn work."
	getDescription    = "Read the current session goal, including the id and revision that update_goal requires."
	updateDescription = "Update the current goal."
	actionDescription = "edit, pause, and resume require a direct top-level human request. complete and blocked are also allowed during an automatic continuation of this goal; blocked is rejected before the configured minimum round count."
)

var guidance = "create_goal may infer goal intent from a direct human request in any language. After session resume or fork, an active goal is disarmed: when a human asks to continue or resume in any wording or language, use update_goal action resume to rearm it. Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least " + strconv.Itoa(BlockedAfterRounds) + " consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked."

// ErrInvalidConfig identifies a provider without its dependencies.
var ErrInvalidConfig = errors.New("invalid goal tool configuration")

// toolError is an expected tool-policy rejection carrying upstream text.
type toolError struct {
	code    string
	message string
}

func (err *toolError) Error() string { return err.message }

// Code returns the stable upstream classification.
func (err *toolError) Code() string { return err.code }

func invalidUpdate(message string) error {
	return &toolError{code: "GOAL_TOOL_INVALID_UPDATE", message: message}
}

// Goals is the goal state the tools read and change.
type Goals interface {
	Get(context.Context, string) (*appGoal.View, error)
	Authority(context.Context, string, uint64, bool) (appGoal.Authority, error)
	Create(context.Context, string, string, *float64, appGoal.Actor) (*appGoal.View, error)
	Edit(context.Context, string, session.GoalRef, *string, *float64, appGoal.Actor) (*appGoal.View, error)
	Pause(context.Context, string, session.GoalRef, appGoal.Actor) (*appGoal.View, error)
	Resume(context.Context, string, session.GoalRef, appGoal.Actor) (*appGoal.View, error)
	Complete(context.Context, string, session.GoalRef, appGoal.Actor) (*appGoal.View, error)
	Block(context.Context, string, session.GoalRef, session.GoalBlockReason, appGoal.Actor) (*appGoal.View, error)
}

// Notifier delivers the closing instruction to the calling agent's turn.
type Notifier interface {
	Notify(string, session.Message) error
}

// Provider owns the three goal tool registrations.
type Provider struct {
	runtime  *appTool.Runtime
	goals    Goals
	notifier Notifier
}

// New constructs an inert provider.
func New(runtime *appTool.Runtime, goals Goals, notifier Notifier) (*Provider, error) {
	if runtime == nil || goals == nil || notifier == nil {
		return nil, ErrInvalidConfig
	}
	return &Provider{runtime: runtime, goals: goals, notifier: notifier}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "goal-tools" }

// Start publishes the tools until scope cleanup. None declares concurrency,
// so every call is exclusive and later calls in a batch see earlier revisions.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	for _, candidate := range []*appTool.Tool{provider.createTool(), provider.getTool(), provider.updateTool()} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

type getArguments struct{}

func (provider *Provider) getTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[getArguments]{
		Name:        "get_goal",
		Description: getDescription,
		Parameters:  appTool.Parameters{},
		Execute: func(ctx context.Context, invocation appTool.Invocation, _ getArguments) (appTool.Result, error) {
			if err := requireSession(invocation); err != nil {
				return appTool.Result{}, err
			}
			view, err := provider.goals.Get(ctx, invocation.SessionID)
			if err != nil {
				return appTool.Result{}, err
			}
			return render(view), nil
		},
	})
}

type createArguments struct {
	Objective     string   `json:"objective"`
	MaxGoalRounds *float64 `json:"max_goal_rounds"`
}

func (provider *Provider) createTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[createArguments]{
		Name:        "create_goal",
		Description: createDescription,
		Parameters: appTool.Parameters{
			appTool.Required("objective", appTool.String("The concrete completion objective inferred from the direct human request.")),
			appTool.Optional("max_goal_rounds", appTool.Number("Optional positive safe-integer limit on automatic continuation rounds.")),
		},
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments createArguments) (appTool.Result, error) {
			if err := provider.requireHuman(ctx, invocation); err != nil {
				return appTool.Result{}, err
			}
			view, err := provider.goals.Create(ctx, invocation.SessionID, arguments.Objective, arguments.MaxGoalRounds, appGoal.ActorModel)
			if err != nil {
				return appTool.Result{}, err
			}
			return render(view), nil
		},
	})
}

type updateArguments struct {
	GoalID        string   `json:"goal_id"`
	Revision      float64  `json:"revision"`
	Action        string   `json:"action"`
	Objective     *string  `json:"objective"`
	MaxGoalRounds *float64 `json:"max_goal_rounds"`
	BlockedReason *string  `json:"blocked_reason"`
}

func (provider *Provider) updateTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[updateArguments]{
		Name:        "update_goal",
		Description: updateDescription,
		Parameters: appTool.Parameters{
			appTool.Required("goal_id", appTool.String("Exact id returned by get_goal.")),
			appTool.Required("revision", appTool.Number("Exact positive revision returned by get_goal.")),
			appTool.Required("action", appTool.String(actionDescription, "edit", "pause", "resume", "complete", "blocked")),
			appTool.Optional("objective", appTool.String("Replacement objective; valid only with action edit.")),
			appTool.Optional("max_goal_rounds", appTool.Number("Replacement cap; valid only with action edit.")),
			appTool.Optional("blocked_reason", appTool.String("Required only with action blocked: the concrete condition that persisted across rounds and blocks progress.")),
		},
		Guidance: appTool.StaticGuidance(appTool.OrderGoal, guidance),
		Execute:  provider.update,
	})
}

// update applies one action. Strict-schema fillers — an empty string or a
// zero cap — count as omitted; meaningful values stay limited to their action.
func (provider *Provider) update(ctx context.Context, invocation appTool.Invocation, arguments updateArguments) (appTool.Result, error) {
	if err := requireSession(invocation); err != nil {
		return appTool.Result{}, err
	}
	if arguments.GoalID == "" || arguments.GoalID != strings.TrimSpace(arguments.GoalID) || arguments.Revision < 1 || arguments.Revision > session.MaxGoalRounds || arguments.Revision != math.Trunc(arguments.Revision) {
		return appTool.Result{}, invalidUpdate("goal_id must be non-empty and revision must be a positive safe integer")
	}
	ref := session.GoalRef{ID: arguments.GoalID, Revision: uint64(arguments.Revision)}
	objective, rounds, reason := text(arguments.Objective), roundCap(arguments.MaxGoalRounds), text(arguments.BlockedReason)
	var view *appGoal.View
	var err error
	switch arguments.Action {
	case "edit":
		if err := provider.requireHuman(ctx, invocation); err != nil {
			return appTool.Result{}, err
		}
		if reason != nil {
			return appTool.Result{}, invalidUpdate("blocked_reason is valid only with action blocked")
		}
		view, err = provider.goals.Edit(ctx, invocation.SessionID, ref, objective, rounds, appGoal.ActorModel)
	case "pause", "resume":
		if err := provider.requireHuman(ctx, invocation); err != nil {
			return appTool.Result{}, err
		}
		if objective != nil || rounds != nil || reason != nil {
			return appTool.Result{}, invalidUpdate("objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
		}
		if arguments.Action == "pause" {
			view, err = provider.goals.Pause(ctx, invocation.SessionID, ref, appGoal.ActorModel)
			break
		}
		current, getErr := provider.goals.Get(ctx, invocation.SessionID)
		if getErr != nil {
			return appTool.Result{}, getErr
		}
		if current != nil && current.Goal.Ref() == ref && current.Goal.Phase == session.GoalPaused {
			return appTool.Result{}, &toolError{code: "GOAL_TOOL_RESUME_PAUSED", message: "the model cannot resume a paused goal; the user must resume it"}
		}
		view, err = provider.goals.Resume(ctx, invocation.SessionID, ref, appGoal.ActorModel)
	default:
		return provider.finish(ctx, invocation, arguments.Action, ref, objective, rounds, reason)
	}
	if err != nil {
		return appTool.Result{}, err
	}
	return render(view), nil
}

// finish marks the goal complete or blocked under direct human or exact
// goal-round authority, and closes an autonomous round.
func (provider *Provider) finish(ctx context.Context, invocation appTool.Invocation, action string, ref session.GoalRef, objective *string, rounds *float64, reason *string) (appTool.Result, error) {
	authority, err := provider.goals.Authority(ctx, invocation.SessionID, invocation.Turn, invocation.Delegated)
	if err != nil {
		return appTool.Result{}, err
	}
	if !authority.Human && authority.Round == nil {
		return appTool.Result{}, &toolError{code: "GOAL_TOOL_AUTHORITY_REQUIRED", message: "complete and blocked require a direct human turn or the current goal round"}
	}
	if objective != nil || rounds != nil {
		return appTool.Result{}, invalidUpdate("objective and max_goal_rounds are valid only with action edit")
	}
	autonomous := !authority.Human
	var view *appGoal.View
	if action == "complete" {
		if reason != nil {
			return appTool.Result{}, invalidUpdate("blocked_reason is valid only with action blocked")
		}
		view, err = provider.goals.Complete(ctx, invocation.SessionID, ref, appGoal.ActorModel)
	} else {
		if reason == nil || strings.TrimSpace(*reason) == "" {
			return appTool.Result{}, invalidUpdate("blocked_reason is required with action blocked")
		}
		if autonomous && authority.Round.RoundsStarted < BlockedAfterRounds {
			return appTool.Result{}, &toolError{code: "GOAL_TOOL_BLOCK_THRESHOLD", message: fmt.Sprintf("blocked requires at least %d consecutive goal rounds; current round is %d", BlockedAfterRounds, authority.Round.RoundsStarted)}
		}
		view, err = provider.goals.Block(ctx, invocation.SessionID, ref, session.GoalBlockReason{Code: "model-reported", Message: *reason}, appGoal.ActorModel)
	}
	if err != nil {
		return appTool.Result{}, err
	}
	if autonomous {
		closing := ""
		if reason != nil {
			closing = *reason
		}
		// The change is committed; a closing instruction that cannot be
		// queued only leaves the turn to end without it.
		_ = provider.notifier.Notify(invocation.SessionID, appGoal.WrapUpMessage(view.Goal.Objective, closing))
	}
	return render(view), nil
}

// requireHuman demands a person's message in the calling root agent's turn.
func (provider *Provider) requireHuman(ctx context.Context, invocation appTool.Invocation) error {
	if err := requireSession(invocation); err != nil {
		return err
	}
	authority, err := provider.goals.Authority(ctx, invocation.SessionID, invocation.Turn, invocation.Delegated)
	if err != nil {
		return err
	}
	if !authority.Human {
		return &toolError{code: "GOAL_TOOL_AUTHORITY_REQUIRED", message: "this goal operation requires a direct human turn on a top-level agent"}
	}
	return nil
}

func requireSession(invocation appTool.Invocation) error {
	if invocation.SessionID == "" || invocation.Turn == 0 {
		return &toolError{code: "GOAL_TOOL_AGENT_REQUIRED", message: "goal tools require a calling agent"}
	}
	return nil
}

// text treats an empty string as an omitted strict-schema filler.
func text(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

// roundCap treats zero as an omitted strict-schema filler.
func roundCap(value *float64) *float64 {
	if value == nil || *value == 0 {
		return nil
	}
	return value
}

// render is the canonical compact result shared by all three tools.
func render(view *appGoal.View) appTool.Result {
	if view == nil {
		return appTool.Text(`{"goal":null}`)
	}
	var output strings.Builder
	goal := view.Goal
	fmt.Fprintf(&output, `{"goal":{"id":%s,"revision":%d,"objective":%s,"phase":%s,"roundsStarted":%d,"maxGoalRounds":%d`,
		appGoal.Quote(goal.ID), goal.Revision, appGoal.Quote(goal.Objective), appGoal.Quote(string(goal.Phase)), view.RoundsStarted, goal.MaxRounds)
	if reason := goal.BlockedReason; reason != nil {
		fmt.Fprintf(&output, `,"blockedReason":{"code":%s,"message":%s}`, appGoal.Quote(reason.Code), appGoal.Quote(reason.Message))
	}
	activation := "disarmed"
	if view.Armed {
		activation = "armed"
	}
	fmt.Fprintf(&output, `},"activation":%q}`, activation)
	return appTool.Text(output.String())
}
