// Package subagent exposes in-process delegation as model-callable tools.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
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
	for _, candidate := range []appTool.Tool{
		spawnTool{service: provider.service}, followupTool{service: provider.service},
		interruptTool{service: provider.service}, reportTool{service: provider.service},
		listTool{service: provider.service},
	} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

func definition(name, description, schema string) session.ToolDefinition {
	return session.ToolDefinition{Name: name, Description: description, Parameters: json.RawMessage(schema)}
}

func decode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("arguments contain a trailing value")
	}
	return nil
}

type spawnTool struct{ service Service }

func (spawnTool) Definition() session.ToolDefinition {
	return definition("spawn_subagent", "Create a full in-process subagent. one-shot waits for its report; continuable returns a handle after the initial report.", `{"type":"object","properties":{"label":{"type":"string"},"task":{"type":"string"},"mode":{"type":"string","enum":["one-shot","continuable"]},"persona":{"type":"string"},"tools":{"type":"array","items":{"type":"string"},"maxItems":32},"fork":{"type":"boolean"}},"required":["label","task","mode"],"additionalProperties":false}`)
}
func (spawnTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (spawnTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool spawnTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		Label   string   `json:"label"`
		Task    string   `json:"task"`
		Mode    string   `json:"mode"`
		Persona string   `json:"persona"`
		Tools   []string `json:"tools"`
		Fork    bool     `json:"fork"`
	}
	if err := decode(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	info, err := tool.service.Spawn(ctx, appSubagent.SpawnRequest{
		ParentSessionID: execution.SessionID, Label: arguments.Label, Task: arguments.Task,
		Mode: arguments.Mode, Persona: arguments.Persona, Tools: arguments.Tools, Fork: arguments.Fork,
	})
	if err != nil {
		return "", err
	}
	info, err = tool.service.Wait(ctx, info.SessionID)
	if err != nil {
		return "", err
	}
	return formatInfo(info), nil
}

type followupTool struct{ service Service }

func (followupTool) Definition() session.ToolDefinition {
	return definition("subagent_followup", "Send a follow-up task to a continuable in-process subagent and wait for its report.", `{"type":"object","properties":{"session_id":{"type":"string"},"task":{"type":"string"}},"required":["session_id","task"],"additionalProperties":false}`)
}
func (followupTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (followupTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool followupTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		SessionID string `json:"session_id"`
		Task      string `json:"task"`
	}
	if err := decode(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	info, err := tool.service.Followup(ctx, execution.SessionID, arguments.SessionID, arguments.Task)
	if err != nil {
		return "", err
	}
	return formatInfo(info), nil
}

type interruptTool struct{ service Service }

func (interruptTool) Definition() session.ToolDefinition {
	return definition("subagent_interrupt", "Cancel the active turn of an in-process subagent.", `{"type":"object","properties":{"session_id":{"type":"string"}},"required":["session_id"],"additionalProperties":false}`)
}
func (interruptTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (interruptTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool interruptTool) Execute(_ context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		SessionID string `json:"session_id"`
	}
	if err := decode(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	return "subagent interrupted", tool.service.Interrupt(execution.SessionID, arguments.SessionID)
}

type listTool struct{ service Service }

type reportTool struct{ service Service }

func (reportTool) Definition() session.ToolDefinition {
	return definition("subagent_report", "Return the current status and latest report of one in-process subagent.", `{"type":"object","properties":{"session_id":{"type":"string"}},"required":["session_id"],"additionalProperties":false}`)
}
func (reportTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (reportTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool reportTool) Execute(_ context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		SessionID string `json:"session_id"`
	}
	if err := decode(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	info, err := tool.service.Report(arguments.SessionID)
	if err != nil {
		return "", err
	}
	return formatInfo(info), nil
}

func (listTool) Definition() session.ToolDefinition {
	return definition("list_subagents", "List in-process subagents created by this parent session.", `{"type":"object","properties":{},"additionalProperties":false}`)
}
func (listTool) Concurrency() appTool.Concurrency      { return appTool.ConcurrencyParallel }
func (listTool) ApprovalReason(json.RawMessage) string { return "" }
func (tool listTool) Execute(_ context.Context, execution appTool.Execution) (string, error) {
	infos, err := tool.service.List(execution.SessionID)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "no subagents", nil
	}
	lines := make([]string, len(infos))
	for index, info := range infos {
		lines[index] = formatInfo(info)
	}
	return strings.Join(lines, "\n"), nil
}

func formatInfo(info appSubagent.Info) string {
	status := "idle"
	if info.Busy || info.Pending > 0 {
		status = "running"
	}
	return fmt.Sprintf("session=%s label=%s mode=%s depth=%d status=%s outcome=%s report=%s", info.SessionID, info.Label, info.Mode, info.Depth, status, info.Last.Outcome, info.Last.Text)
}
