// Package subagent exposes in-process delegation as the upstream Base
// subagent, subagent_fork, send_message, interrupt_agent, and list_agents
// tools. Definitions match the reference composition byte for byte:
// subagent delegates to a fresh continuable child that runs in the
// background by default, and subagent_fork to a one-shot child seeded with
// the conversation that waits by default.
package subagent

import (
	"context"
	"errors"
	"strconv"
	"strings"

	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	descriptionText = "A short (3-5 word) description of the delegated task, for display."
	spawnText       = "Delegate a self-contained task to a subagent (a separate agent that works in its own context) " +
		"to offload focused, independent work — research, a scoped implementation, an analysis — so it does not " +
		"consume this conversation's context. The subagent returns its result, not its intermediate steps. " +
		"It runs in the background by default and returns a subagent id you can continue with `send_message`; " +
		"you are notified when the run settles."
	spawnPromptText = "The complete, self-contained task for the subagent. It does not share this conversation's " +
		"context, so include everything it needs."
	spawnBackgroundText = "Defaults to true. Set false only when your next action depends on the result."
	spawnGuidance       = "Start independent subagent delegations together in one assistant message and continue useful work while they run."
	forkText            = "Delegate a task to a subagent that inherits this conversation: a child agent seeded with all " +
		"completed turns so far (it does not see the current in-flight turn). Use this when the subtask builds on " +
		"this conversation's context — a follow-up analysis, a review, a continuation — without consuming this " +
		"conversation's context for the work itself. You receive its result, not its intermediate steps. " +
		"This call waits for the result by default."
	forkPromptText = "The task for the subagent. It already sees this conversation's completed turns, so build on " +
		"them freely and state only what is new."
	forkBackgroundText = "Run as a background job and return its id (collect with job_output, stop with job_kill). Defaults to false."
	sendText           = "Send a message to an agent. A working agent receives it at its next step; an idle agent " +
		"starts a new turn with it. Returns delivery confirmation, not the agent's answer."
	interruptText = "Ask a subagent to stop its current work. This call returns without waiting for it to stop. " +
		"You can continue a direct child's conversation later with send_message. Subagents it started will keep running."
	listText = "List subagents you started, with their ids, labels, and status. running means it is working; " +
		"inactive means it is not currently working. You will be notified when a subagent finishes; there is no " +
		"need to keep checking its status. Use send_message to continue the conversation."
	scopeText = "children (default) lists direct children, which accept send_message in any status. descendants " +
		"lists the whole tree below you with each entry's parent session id and depth; entries deeper than 1 " +
		"accept only interrupt_agent."
)

// ErrInvalidConfig identifies subagent-tool configuration that cannot be honored.
var ErrInvalidConfig = errors.New("invalid subagent tool configuration")

// Service is the delegation use-case boundary these tools consume.
type Service interface {
	StartContinuable(context.Context, appSubagent.StartRequest) (string, error)
	StartBackground(context.Context, appSubagent.StartRequest) (string, error)
	Run(context.Context, appSubagent.StartRequest) (appSubagent.Report, error)
	SendMessage(ctx context.Context, senderID, targetID, text string) error
	Interrupt(callerID, targetID string) error
	ListChildren(ctx context.Context, parentID string) ([]appSubagent.Entry, error)
	ListDescendants(ctx context.Context, rootID string) ([]appSubagent.Entry, error)
}

// Provider owns the delegation tool registrations.
type Provider struct {
	runtime *appTool.Runtime
	service Service
}

// New constructs an inert delegation tool provider.
func New(runtime *appTool.Runtime, service Service) (*Provider, error) {
	if runtime == nil || service == nil {
		return nil, ErrInvalidConfig
	}
	return &Provider{runtime: runtime, service: service}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "subagent-tools" }

// Start publishes the delegation tools for the caller's scope.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	for _, candidate := range []*appTool.Tool{
		provider.spawnTool(), provider.forkTool(), provider.sendTool(), provider.interruptTool(), provider.listTool(),
	} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

type delegateArgs struct {
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	RunInBackground *bool  `json:"run_in_background"`
}

// request binds a delegation to the calling session's journal and step.
func request(invocation appTool.Invocation, arguments delegateArgs, fork bool) appSubagent.StartRequest {
	return appSubagent.StartRequest{
		ParentID: invocation.SessionID, Journal: invocation.Journal, Turn: invocation.Turn, Step: invocation.Step,
		Description: arguments.Description, Prompt: arguments.Prompt, Fork: fork,
	}
}

// concurrent marks delegations overlap-safe: each creates its own child and
// writes only one catalog record to the caller.
func concurrent(delegateArgs) bool { return true }

func (provider *Provider) spawnTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[delegateArgs]{
		Name:        "subagent",
		Description: spawnText,
		Parameters: appTool.Parameters{
			appTool.Required("description", appTool.String(descriptionText)),
			appTool.Required("prompt", appTool.String(spawnPromptText)),
			appTool.Optional("run_in_background", appTool.Boolean(spawnBackgroundText)),
		},
		Guidance:   appTool.StaticGuidance(appTool.OrderSubagent, spawnGuidance),
		Concurrent: concurrent,
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments delegateArgs) (appTool.Result, error) {
			if arguments.RunInBackground == nil || *arguments.RunInBackground {
				id, err := provider.service.StartContinuable(ctx, request(invocation, arguments, false))
				if err != nil {
					return appTool.Result{}, err
				}
				return appTool.Text("started subagent " + id), nil
			}
			return provider.foreground(ctx, request(invocation, arguments, false))
		},
	})
}

func (provider *Provider) forkTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[delegateArgs]{
		Name:        "subagent_fork",
		Description: forkText,
		Parameters: appTool.Parameters{
			appTool.Required("description", appTool.String(descriptionText)),
			appTool.Required("prompt", appTool.String(forkPromptText)),
			appTool.Optional("run_in_background", appTool.Boolean(forkBackgroundText)),
		},
		Concurrent: concurrent,
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments delegateArgs) (appTool.Result, error) {
			if arguments.RunInBackground != nil && *arguments.RunInBackground {
				id, err := provider.service.StartBackground(ctx, request(invocation, arguments, true))
				if err != nil {
					return appTool.Result{}, err
				}
				return appTool.Text("started background subagent job " + id), nil
			}
			return provider.foreground(ctx, request(invocation, arguments, true))
		},
	})
}

// foreground runs a one-shot child and returns its closing answer. A run
// that did not complete is an error that keeps the child's partial answer.
func (provider *Provider) foreground(ctx context.Context, request appSubagent.StartRequest) (appTool.Result, error) {
	report, err := provider.service.Run(ctx, request)
	if err != nil {
		return appTool.Result{}, err
	}
	headline := ""
	switch report.Outcome {
	case session.OutcomeCompleted:
		return appTool.Text(report.Text), nil
	case session.OutcomeCanceled, session.OutcomeInterrupted:
		headline = "subagent run was cancelled"
	case session.OutcomeError:
		headline = "subagent run failed"
	case session.OutcomeStepLimit:
		headline = "subagent run ended abnormally (" + string(report.Outcome) + ")"
	}
	if report.Text != "" {
		headline += "\nPartial output before the run ended:\n" + report.Text
	}
	return appTool.Result{}, errors.New(headline)
}

type sendArgs struct {
	AgentID string `json:"agent_id"`
	Message string `json:"message"`
}

func (provider *Provider) sendTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[sendArgs]{
		Name:        "send_message",
		Description: sendText,
		Parameters: appTool.Parameters{
			appTool.Required("agent_id", appTool.String("The agent id of your direct continuable child, or your direct parent when you are a resident continuable child.")),
			appTool.Required("message", appTool.String("The message to deliver to the agent.")),
		},
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments sendArgs) (appTool.Result, error) {
			if err := provider.service.SendMessage(ctx, invocation.SessionID, arguments.AgentID, arguments.Message); err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text("message delivered to agent " + arguments.AgentID), nil
		},
	})
}

type interruptArgs struct {
	AgentID string `json:"agent_id"`
}

func (provider *Provider) interruptTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[interruptArgs]{
		Name:        "interrupt_agent",
		Description: interruptText,
		Parameters: appTool.Parameters{
			appTool.Required("agent_id", appTool.String("The id of an agent created under you: your direct child or a deeper descendant.")),
		},
		Execute: func(_ context.Context, invocation appTool.Invocation, arguments interruptArgs) (appTool.Result, error) {
			if err := provider.service.Interrupt(invocation.SessionID, arguments.AgentID); err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text("interrupt requested for agent " + arguments.AgentID), nil
		},
	})
}

type listArgs struct {
	Scope *string `json:"scope"`
}

func (provider *Provider) listTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[listArgs]{
		Name:        "list_agents",
		Description: listText,
		Parameters: appTool.Parameters{
			appTool.Optional("scope", appTool.String(scopeText, "children", "descendants")),
		},
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments listArgs) (appTool.Result, error) {
			descendants := arguments.Scope != nil && *arguments.Scope == "descendants"
			list := provider.service.ListChildren
			if descendants {
				list = provider.service.ListDescendants
			}
			entries, err := list(ctx, invocation.SessionID)
			if err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text(renderEntries(entries, descendants)), nil
		},
	})
}

// renderEntries lists continuable children and unreadable entries. One-shot
// children cannot be continued, so they are omitted even though the walk
// traversed them.
func renderEntries(entries []appSubagent.Entry, descendants bool) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		position := ""
		if descendants {
			position = " parent=" + entry.Parent + " depth=" + strconv.Itoa(entry.Depth)
		}
		switch {
		case entry.Unavailable:
			lines = append(lines, entry.ID+" [diagnostic: unavailable]"+position)
		case entry.Mode == session.SubagentContinuable:
			status := "inactive"
			if entry.Running {
				status = "running"
			}
			lines = append(lines, entry.ID+" ["+status+"]"+position+" — "+entry.Label)
		}
	}
	if len(lines) == 0 {
		return "(no subagents)"
	}
	return strings.Join(lines, "\n")
}
