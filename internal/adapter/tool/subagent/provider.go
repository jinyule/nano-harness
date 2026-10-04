// Package subagent exposes in-process delegation as model-callable tools.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// Provider owns delegation tool registrations.
type Provider struct {
	runtime *appTool.Runtime
	service Service
}

// Service is the narrow delegation use-case boundary consumed by these tools.
type Service interface {
	Spawn(context.Context, appSubagent.SpawnRequest) (appSubagent.Info, error)
	Wait(context.Context, string) (appSubagent.Info, error)
	Followup(context.Context, string, string, string) (appSubagent.Info, error)
	Interrupt(string, string) error
	Report(string) (appSubagent.Info, error)
	List(string) ([]appSubagent.Info, error)
}

// New constructs a delegation tool provider.
func New(runtime *appTool.Runtime, service Service) (*Provider, error) {
	if runtime == nil || service == nil {
		return nil, errors.New("invalid subagent tool configuration")
	}
	return &Provider{runtime: runtime, service: service}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "subagent-tools" }

// Start publishes all delegation operations for the caller's scope.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	for _, candidate := range []*appTool.Tool{
		provider.spawnTool(), provider.followupTool(), provider.interruptTool(),
		provider.reportTool(), provider.listTool(),
	} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

// concurrent marks delegation calls as overlap-safe: each one owns a
// distinct child or only reads service state.
func concurrent[A any](A) bool { return true }

type spawnArgs struct {
	Label   string   `json:"label"`
	Task    string   `json:"task"`
	Mode    string   `json:"mode"`
	Persona string   `json:"persona"`
	Tools   []string `json:"tools"`
	Fork    bool     `json:"fork"`
}

func (provider *Provider) spawnTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[spawnArgs]{
		Name:        "spawn_subagent",
		Description: "Create a full in-process subagent. one-shot waits for its report; continuable returns a handle after the initial report.",
		Parameters: appTool.Parameters{
			appTool.Required("label", appTool.String("")),
			appTool.Required("task", appTool.String("")),
			appTool.Required("mode", appTool.String("", "one-shot", "continuable")),
			appTool.Optional("persona", appTool.String("")),
			appTool.Optional("tools", appTool.Array("", appTool.String(""))),
			appTool.Optional("fork", appTool.Boolean("")),
		},
		Concurrent: concurrent[spawnArgs],
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments spawnArgs) (appTool.Result, error) {
			info, err := provider.service.Spawn(ctx, appSubagent.SpawnRequest{
				ParentSessionID: invocation.SessionID, Label: arguments.Label, Task: arguments.Task,
				Mode: arguments.Mode, Persona: arguments.Persona, Tools: arguments.Tools, Fork: arguments.Fork,
			})
			if err != nil {
				return appTool.Result{}, err
			}
			info, err = provider.service.Wait(ctx, info.SessionID)
			if err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text(formatInfo(info)), nil
		},
	})
}

type followupArgs struct {
	SessionID string `json:"session_id"`
	Task      string `json:"task"`
}

func (provider *Provider) followupTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[followupArgs]{
		Name:        "subagent_followup",
		Description: "Send a follow-up task to a continuable in-process subagent and wait for its report.",
		Parameters: appTool.Parameters{
			appTool.Required("session_id", appTool.String("")),
			appTool.Required("task", appTool.String("")),
		},
		Concurrent: concurrent[followupArgs],
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments followupArgs) (appTool.Result, error) {
			info, err := provider.service.Followup(ctx, invocation.SessionID, arguments.SessionID, arguments.Task)
			if err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text(formatInfo(info)), nil
		},
	})
}

type sessionArgs struct {
	SessionID string `json:"session_id"`
}

func (provider *Provider) interruptTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[sessionArgs]{
		Name:        "subagent_interrupt",
		Description: "Cancel the active turn of an in-process subagent.",
		Parameters:  appTool.Parameters{appTool.Required("session_id", appTool.String(""))},
		Concurrent:  concurrent[sessionArgs],
		Execute: func(_ context.Context, invocation appTool.Invocation, arguments sessionArgs) (appTool.Result, error) {
			if err := provider.service.Interrupt(invocation.SessionID, arguments.SessionID); err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text("subagent interrupted"), nil
		},
	})
}

func (provider *Provider) reportTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[sessionArgs]{
		Name:        "subagent_report",
		Description: "Return the current status and latest report of one in-process subagent.",
		Parameters:  appTool.Parameters{appTool.Required("session_id", appTool.String(""))},
		Concurrent:  concurrent[sessionArgs],
		Execute: func(_ context.Context, _ appTool.Invocation, arguments sessionArgs) (appTool.Result, error) {
			info, err := provider.service.Report(arguments.SessionID)
			if err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text(formatInfo(info)), nil
		},
	})
}

type noArgs struct{}

func (provider *Provider) listTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[noArgs]{
		Name:        "list_subagents",
		Description: "List in-process subagents created by this parent session.",
		Concurrent:  concurrent[noArgs],
		Execute: func(_ context.Context, invocation appTool.Invocation, _ noArgs) (appTool.Result, error) {
			infos, err := provider.service.List(invocation.SessionID)
			if err != nil {
				return appTool.Result{}, err
			}
			if len(infos) == 0 {
				return appTool.Text("no subagents"), nil
			}
			lines := make([]string, len(infos))
			for index, info := range infos {
				lines[index] = formatInfo(info)
			}
			return appTool.Text(strings.Join(lines, "\n")), nil
		},
	})
}

func formatInfo(info appSubagent.Info) string {
	status := "idle"
	if info.Busy || info.Pending > 0 {
		status = "running"
	}
	return fmt.Sprintf("session=%s label=%s mode=%s depth=%d status=%s outcome=%s report=%s", info.SessionID, info.Label, info.Mode, info.Depth, status, info.Last.Outcome, info.Last.Text)
}
