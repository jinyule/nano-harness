// Package tool owns tool discovery, approval, scheduling, and execution.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidTool identifies an invalid tool definition, registration, or runtime request.
	ErrInvalidTool = errors.New("invalid tool")
	// ErrNotRunning indicates the tool runtime has not started or has stopped.
	ErrNotRunning = errors.New("tool runtime is not running")
	// ErrUnknownTool identifies a requested tool name that is not registered.
	ErrUnknownTool = errors.New("unknown tool")
)

// Concurrency declares whether a tool may overlap adjacent calls.
type Concurrency string

const (
	// ConcurrencyParallel permits overlap with adjacent parallel tool calls.
	ConcurrencyParallel Concurrency = "parallel"
	// ConcurrencyExclusive forms a scheduling barrier around the tool call.
	ConcurrencyExclusive Concurrency = "exclusive"
)

// Execution is the bounded context supplied to a tool implementation.
type Execution struct {
	SessionID string
	Cwd       string
	Arguments json.RawMessage
	Delegated bool
	Elevated  bool
}

// Tool is one registered, stateless tool behavior.
type Tool interface {
	Definition() session.ToolDefinition
	Concurrency() Concurrency
	ApprovalReason(json.RawMessage) string
	Execute(context.Context, Execution) (string, error)
}

// ApprovalRequest is the app-level approval envelope.
type ApprovalRequest struct {
	SessionID string
	Turn      uint64
	Step      uint64
	Call      session.ToolCall
	Reason    string
	Delegated bool
	Journal   Journal
}

// Approver is implemented by the approval service.
type Approver interface {
	Decide(context.Context, ApprovalRequest) (session.ApprovalOutcome, error)
}

// Journal is the durable fact sink required by the approval pipeline.
type Journal interface {
	Append(context.Context, session.Record) (session.Event, error)
}

// BatchRequest executes committed calls without writing their results.
type BatchRequest struct {
	SessionID string
	Cwd       string
	Turn      uint64
	Step      uint64
	Calls     []session.ToolCall
	Delegated bool
	Journal   Journal
}

// Runtime publishes tools and enforces deterministic scheduling barriers.
type Runtime struct {
	approver Approver

	mu      sync.RWMutex
	started bool
	active  bool
	tools   map[string]Tool
}

// New constructs a tool runtime over a fail-closed approver.
func New(approver Approver) (*Runtime, error) {
	if approver == nil {
		return nil, ErrInvalidTool
	}
	return &Runtime{approver: approver, tools: map[string]Tool{}}, nil
}

// ID returns the stable plugin identity.
func (*Runtime) ID() string { return "tools" }

// Start activates the registry until scope cleanup.
func (runtime *Runtime) Start(_ context.Context, scope *plugin.Scope) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.started {
		return ErrInvalidTool
	}
	if err := scope.Defer(func(context.Context) error {
		runtime.mu.Lock()
		runtime.active = false
		runtime.tools = map[string]Tool{}
		runtime.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	runtime.started, runtime.active = true, true
	return nil
}

// Register publishes one tool for exactly the caller's scope lifetime.
func (runtime *Runtime) Register(candidate Tool, scope *plugin.Scope) error {
	if candidate == nil || scope == nil {
		return ErrInvalidTool
	}
	definition := candidate.Definition()
	if err := validateDefinition(definition); err != nil || candidate.Concurrency() != ConcurrencyParallel && candidate.Concurrency() != ConcurrencyExclusive {
		return ErrInvalidTool
	}
	runtime.mu.Lock()
	if !runtime.active {
		runtime.mu.Unlock()
		return ErrNotRunning
	}
	if _, exists := runtime.tools[definition.Name]; exists {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: duplicate %q", ErrInvalidTool, definition.Name)
	}
	runtime.tools[definition.Name] = candidate
	runtime.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		runtime.mu.Lock()
		if runtime.tools[definition.Name] == candidate {
			delete(runtime.tools, definition.Name)
		}
		runtime.mu.Unlock()
		return nil
	}); err != nil {
		runtime.mu.Lock()
		delete(runtime.tools, definition.Name)
		runtime.mu.Unlock()
		return err
	}
	return nil
}

func validateDefinition(definition session.ToolDefinition) error {
	header := session.Record{Type: session.RecordRequestHeader, Turn: 1, Step: 1, Header: &session.RequestHeader{Provider: "test", Model: "test", Tools: []session.ToolDefinition{definition}}}
	return header.Validate()
}

// Definitions freezes the currently registered schemas in lexical order.
func (runtime *Runtime) Definitions(allow []string) ([]session.ToolDefinition, error) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if !runtime.active {
		return nil, ErrNotRunning
	}
	allowed := map[string]struct{}{}
	for _, name := range allow {
		allowed[name] = struct{}{}
	}
	definitions := make([]session.ToolDefinition, 0, len(runtime.tools))
	for name, candidate := range runtime.tools {
		if len(allow) == 0 {
			definitions = append(definitions, candidate.Definition())
		} else if _, ok := allowed[name]; ok {
			definitions = append(definitions, candidate.Definition())
		}
	}
	slices.SortFunc(definitions, func(left, right session.ToolDefinition) int {
		return strings.Compare(left.Name, right.Name)
	})
	return definitions, nil
}

// ExecuteBatch runs adjacent parallel tools concurrently and exclusive tools as barriers.
func (runtime *Runtime) ExecuteBatch(ctx context.Context, request BatchRequest) []session.ToolResult {
	results := make([]session.ToolResult, len(request.Calls))
	for index := 0; index < len(request.Calls); {
		candidate := runtime.lookup(request.Calls[index].Name)
		if candidate == nil || candidate.Concurrency() == ConcurrencyExclusive {
			results[index] = runtime.execute(ctx, request, request.Calls[index], candidate)
			index++
			continue
		}
		end := index
		for end < len(request.Calls) {
			next := runtime.lookup(request.Calls[end].Name)
			if next == nil || next.Concurrency() != ConcurrencyParallel {
				break
			}
			end++
		}
		var group sync.WaitGroup
		for current := index; current < end; current++ {
			group.Add(1)
			go func(position int) {
				defer group.Done()
				results[position] = runtime.execute(ctx, request, request.Calls[position], runtime.lookup(request.Calls[position].Name))
			}(current)
		}
		group.Wait()
		index = end
	}
	return results
}

func (runtime *Runtime) lookup(name string) Tool {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if !runtime.active {
		return nil
	}
	return runtime.tools[name]
}

func (runtime *Runtime) execute(ctx context.Context, request BatchRequest, call session.ToolCall, candidate Tool) (result session.ToolResult) {
	result.CallID = call.ID
	elevated := false
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Output, result.IsError = "tool error: implementation panicked", true
		}
	}()
	if candidate == nil {
		result.Output, result.IsError = "tool error: unknown tool "+call.Name, true
		return result
	}
	if reason := candidate.ApprovalReason(call.Arguments); reason != "" {
		outcome, err := runtime.approver.Decide(ctx, ApprovalRequest{
			SessionID: request.SessionID, Turn: request.Turn, Step: request.Step,
			Call: call, Reason: reason, Delegated: request.Delegated, Journal: request.Journal,
		})
		if err != nil {
			result.Output, result.IsError = "tool error: approval could not be recorded", true
			return result
		}
		if outcome != session.ApprovalAllowedOnce {
			result.Output, result.IsError = "tool error: approval "+string(outcome), true
			return result
		}
		elevated = true
	}
	output, err := candidate.Execute(ctx, Execution{SessionID: request.SessionID, Cwd: request.Cwd, Arguments: call.Arguments, Delegated: request.Delegated, Elevated: elevated})
	if err != nil {
		result.Output, result.IsError = "tool error: "+err.Error(), true
		return result
	}
	if len(output) > session.MaxTextBytes {
		output = output[:session.MaxTextBytes-len("\n[output truncated]")] + "\n[output truncated]"
	}
	result.Output = output
	return result
}
