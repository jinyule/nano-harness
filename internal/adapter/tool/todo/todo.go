// Package todo exposes the calling session's whole-list task plan as the todo_write tool.
package todo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// ErrInvalidConfig identifies a todo tool provider without its tool runtime.
var ErrInvalidConfig = errors.New("invalid todo tool configuration")

// The model-visible definition matches the upstream Base composition, which enables parallel
// in-progress todos; the single-active variant has no composition here.
const (
	toolName    = "todo_write"
	description = "Record and update a task list to plan multi-step work and show progress; skip it for trivial single-step tasks. Add one todo per concrete step before you start. While work remains, keep the todos being worked on `in_progress`, several only when work runs in parallel. Mark each todo `completed` as soon as it is done."
)

// Provider owns the todo_write registration for its scope lifetime.
type Provider struct {
	runtime *appTool.Runtime
}

// New constructs an inert todo tool provider.
func New(runtime *appTool.Runtime) (*Provider, error) {
	if runtime == nil {
		return nil, ErrInvalidConfig
	}
	return &Provider{runtime: runtime}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "todo-tools" }

// Start publishes todo_write until scope cleanup.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	return provider.runtime.Register(writeTool(), scope)
}

type writeArguments struct {
	Todos []todoArgument `json:"todos"`
}

type todoArgument struct {
	Content string             `json:"content"`
	Status  session.TodoStatus `json:"status"`
}

// writeTool replaces the calling session's list with one durable todo/write snapshot. The tool
// declares no Concurrent classifier, so every call is exclusive and the log order of snapshots
// equals the model's call order within one step.
func writeTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[writeArguments]{
		Name:        toolName,
		Description: description,
		Parameters: appTool.Parameters{
			appTool.Required("todos", appTool.Array("The COMPLETE task list, replacing any previous list.", appTool.Object("", false,
				appTool.Required("content", appTool.String("What the task is — a short imperative line.")),
				appTool.Required("status", appTool.String("pending (not started) | in_progress (now) | completed (done).",
					string(session.TodoPending), string(session.TodoInProgress), string(session.TodoCompleted))),
			))),
		},
		Execute: write,
	})
}

// write validates the trimmed list before checking for an owning session, commits the snapshot
// under the caller's context, and reports per-status counts. A canceled or failed append leaves
// the previous list standing and returns an error result.
func write(ctx context.Context, invocation appTool.Invocation, arguments writeArguments) (appTool.Result, error) {
	items := make([]session.TodoItem, len(arguments.Todos))
	counts := map[session.TodoStatus]int{}
	for index, todo := range arguments.Todos {
		items[index] = session.TodoItem{Content: strings.TrimSpace(todo.Content), Status: todo.Status}
		counts[todo.Status]++
	}
	if err := session.ValidateTodoItems(items); err != nil {
		return appTool.Result{}, err
	}
	if invocation.Journal == nil {
		return appTool.Result{}, errors.New("todo_write requires an owning agent session")
	}
	record := session.Record{Type: session.RecordTodoWrite, Turn: invocation.Turn, Step: invocation.Step, Todo: &session.TodoWrite{CallID: invocation.CallID, Items: items}}
	if _, err := invocation.Journal.Append(ctx, record); err != nil {
		return appTool.Result{}, fmt.Errorf("record todo list: %w", err)
	}
	return appTool.Text(fmt.Sprintf("Updated todo list: %d pending, %d in progress, %d completed.", counts[session.TodoPending], counts[session.TodoInProgress], counts[session.TodoCompleted])), nil
}
