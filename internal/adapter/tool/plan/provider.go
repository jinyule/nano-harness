// Package plan exposes the upstream exit_plan_mode tool. The tool stays in
// every request's catalog so entering or leaving plan mode changes only the
// plan policy section; outside plan mode a call fails.
package plan

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	appQuestion "github.com/jinyule/nano-harness/internal/app/question"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/text"
)

const (
	reviewID      = "plan-review"
	approveLabel  = "Approve"
	keepLabel     = "Keep planning"
	approvedText  = "Plan approved — plan mode exited; carry out the plan starting with your next step."
	dismissedText = "The user dismissed the plan review to speak instead; stay in plan mode, stop here, and wait for their message."
	keepText      = "The user chose to keep planning; revise the plan and present it again."
	feedbackText  = "The user chose to keep planning; their feedback: "
)

var (
	errInactive = errors.New("exit_plan_mode is only available in plan mode")
	errHeading  = errors.New("exit_plan_mode requires a non-empty markdown plan starting with a # heading")
)

// reviewError carries an upstream model-visible sentence verbatim; Go
// error-string style does not apply to tool result text.
type reviewError string

func (err reviewError) Error() string { return string(err) }

// Mode is the plan-mode state consumed by the tool.
type Mode interface {
	Active(sessionID string) bool
	Exit(context.Context, string) error
}

// Asker presents the plan review.
type Asker interface {
	Ask(context.Context, appQuestion.Request) ([]appQuestion.Answer, error)
}

// Provider owns the exit_plan_mode registration.
type Provider struct {
	runtime *appTool.Runtime
	mode    Mode
	asker   Asker
}

// New constructs the tool provider.
func New(runtime *appTool.Runtime, mode Mode, asker Asker) (*Provider, error) {
	if runtime == nil || mode == nil || asker == nil {
		return nil, errors.New("invalid plan tool configuration")
	}
	return &Provider{runtime: runtime, mode: mode, asker: asker}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "plan-tools" }

// Start publishes exit_plan_mode for the caller's scope.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	return provider.runtime.Register(provider.exitTool(), scope)
}

type exitArgs struct {
	Plan string `json:"plan"`
}

func (provider *Provider) exitTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[exitArgs]{
		Name:        "exit_plan_mode",
		Description: "Use only in plan mode. Present your plan for the user's review and, on approval, leave plan mode. The user may approve (carry out the plan from your next step) or keep planning — their feedback comes back in the tool result; revise and present again.",
		Parameters: appTool.Parameters{
			appTool.Required("plan", appTool.String("The complete plan, as markdown, starting with a # heading that names it.")),
		},
		Execute: provider.exit,
	})
}

func (provider *Provider) exit(ctx context.Context, invocation appTool.Invocation, arguments exitArgs) (appTool.Result, error) {
	if !provider.mode.Active(invocation.SessionID) {
		return appTool.Result{}, errInactive
	}
	if !startsWithTitle(arguments.Plan) {
		return appTool.Result{}, errHeading
	}
	answers, err := provider.asker.Ask(ctx, appQuestion.Request{
		SessionID: invocation.SessionID, CallID: invocation.CallID, Delegated: invocation.Delegated,
		Questions: []appQuestion.Question{{
			ID: reviewID, Header: "Plan review", Text: "Approve this plan and leave plan mode?", Detail: arguments.Plan,
			Options: []appQuestion.Option{
				{Label: approveLabel, Description: "Leave plan mode; the plan is carried out from the next step."},
				{Label: keepLabel, Description: "Stay in plan mode; feedback goes back to the model."},
			},
			Intent: &appQuestion.Intent{Kind: appQuestion.IntentPlanReview, Approve: approveLabel},
		}},
	})
	if errors.Is(err, appQuestion.ErrCancelled) {
		// A dismissed review is the user taking the turn back, not a
		// rejection; the generic message would name a tool the model never called.
		return appTool.Result{}, reviewError(dismissedText)
	}
	if err != nil {
		return appTool.Result{}, err
	}
	review := answers[0]
	if len(review.Selected) != 1 || review.Selected[0] != approveLabel || review.Custom != "" {
		if review.Custom == "" {
			return appTool.Result{}, reviewError(keepText)
		}
		return appTool.Result{}, reviewError(feedbackText + review.Custom)
	}
	if err := provider.mode.Exit(ctx, invocation.SessionID); err != nil {
		return appTool.Result{}, err
	}
	return appTool.Text(approvedText), nil
}

// startsWithTitle mirrors the upstream /^#\s+\S/ check on the trimmed plan:
// a single # followed by ECMAScript whitespace and then non-whitespace text.
func startsWithTitle(plan string) bool {
	rest, ok := strings.CutPrefix(text.TrimSpace(plan), "#")
	if !ok {
		return false
	}
	first, _ := utf8.DecodeRuneInString(rest)
	return text.IsSpace(first) && !appTool.IsBlank(rest)
}
