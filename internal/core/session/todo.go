package session

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// TodoStatus is the lifecycle state of one todo item.
type TodoStatus string

const (
	// TodoPending identifies a task that has not started.
	TodoPending TodoStatus = "pending"
	// TodoInProgress identifies a task being worked now; parallel work may mark several.
	TodoInProgress TodoStatus = "in_progress"
	// TodoCompleted identifies a finished task.
	TodoCompleted TodoStatus = "completed"
)

const (
	// MaxTodoItems bounds one whole-list todo snapshot.
	MaxTodoItems = 256
	// MaxTodoContentBytes bounds the description of one todo item.
	MaxTodoContentBytes = 2048
)

// TodoItem is one entry of a whole-list todo snapshot. Content is a trimmed, non-empty
// description that is unique within its list; entries carry no identity because every write
// replaces the whole list.
type TodoItem struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
}

// TodoWrite is the complete replacement list committed by one todo_write call. CallID names
// the pending tool call that produced it. Items is never nil; an empty list clears the plan.
type TodoWrite struct {
	CallID string     `json:"call_id"`
	Items  []TodoItem `json:"items"`
}

// ValidateTodoItems checks the whole-list invariants shared by the model-facing tool and the
// durable decoder: a bounded count and, per item, trimmed non-empty bounded content, a known
// status, and content unique within the list. The number of in-progress items is deliberately
// unconstrained so parallel work can mark several tasks. Messages are model-visible.
func ValidateTodoItems(items []TodoItem) error {
	if len(items) > MaxTodoItems {
		return fmt.Errorf("invalid todos: at most %d todos are allowed (got %d)", MaxTodoItems, len(items))
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		switch {
		case item.Content == "":
			return errors.New("invalid todo: `content` must be a non-empty string")
		case item.Content != strings.TrimSpace(item.Content):
			return errors.New("invalid todo: `content` must be trimmed")
		case len(item.Content) > MaxTodoContentBytes:
			return fmt.Errorf("invalid todo: `content` exceeds %d bytes", MaxTodoContentBytes)
		case item.Status != TodoPending && item.Status != TodoInProgress && item.Status != TodoCompleted:
			return fmt.Errorf("invalid todo: unknown status %q", item.Status)
		}
		if _, duplicate := seen[item.Content]; duplicate {
			return fmt.Errorf("invalid todos: duplicate content %q", item.Content)
		}
		seen[item.Content] = struct{}{}
	}
	return nil
}

// StandingTodos folds one committed record into the standing todo plan: the latest todo/write
// list that no later turn/start has superseded. turn/end keeps a finished list visible, and every
// other record returns plan unchanged. A nil result means no plan; the result never aliases record.
func StandingTodos(plan []TodoItem, record Record) []TodoItem {
	if record.Type == RecordTurnStart {
		return nil
	}
	if record.Type == RecordTodoWrite {
		return slices.Clone(record.Todo.Items)
	}
	return plan
}

func (record Record) requireTodo() error {
	if record.Step == 0 || record.Todo == nil || record.Todo.Items == nil || record.hasExtras("todo") {
		return invalid("todo/write shape is invalid")
	}
	if err := validateIdentifier("call ID", record.Todo.CallID, 128); err != nil {
		return err
	}
	if err := ValidateTodoItems(record.Todo.Items); err != nil {
		return invalid("todo/write: %v", err)
	}
	return nil
}
