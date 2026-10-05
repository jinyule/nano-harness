// Package tool owns tool definitions, discovery, approval, scheduling, and execution.
package tool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// maxReasonBytes keeps model-derived approval reasons well inside the
// durable approval record limit.
const maxReasonBytes = 1024

var (
	// ErrInvalidTool identifies an invalid tool definition, registration, or runtime request.
	ErrInvalidTool = errors.New("invalid tool")
	// ErrNotRunning indicates the tool runtime has not started or has stopped.
	ErrNotRunning = errors.New("tool runtime is not running")
)

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
	Route     Route
	Turn      uint64
	Step      uint64
	Calls     []session.ToolCall
	Delegated bool
	Journal   Journal
}

// Catalog is one consistent snapshot of visible tool schemas in lexical
// order and the prompt guidance those tools contribute.
type Catalog struct {
	Definitions []session.ToolDefinition
	Guidance    []string
}

// Runtime publishes tools and enforces deterministic scheduling barriers.
type Runtime struct {
	approver Approver

	mu      sync.RWMutex
	started bool
	active  bool
	tools   map[string]*Tool
	spill   SpillStore
}

// New constructs a tool runtime over a fail-closed approver.
func New(approver Approver) (*Runtime, error) {
	if approver == nil {
		return nil, ErrInvalidTool
	}
	return &Runtime{approver: approver, tools: map[string]*Tool{}}, nil
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
		runtime.tools = map[string]*Tool{}
		runtime.spill = nil
		runtime.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	runtime.started, runtime.active = true, true
	return nil
}

// Register publishes one tool for exactly the caller's scope lifetime.
func (runtime *Runtime) Register(candidate *Tool, scope *plugin.Scope) error {
	if candidate == nil || scope == nil {
		return ErrInvalidTool
	}
	if candidate.err != nil {
		return fmt.Errorf("%w %q: %w", ErrInvalidTool, candidate.definition.Name, candidate.err)
	}
	name := candidate.definition.Name
	runtime.mu.Lock()
	if !runtime.active {
		runtime.mu.Unlock()
		return ErrNotRunning
	}
	if _, exists := runtime.tools[name]; exists {
		runtime.mu.Unlock()
		return fmt.Errorf("%w: duplicate %q", ErrInvalidTool, name)
	}
	runtime.tools[name] = candidate
	runtime.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		runtime.mu.Lock()
		if runtime.tools[name] == candidate {
			delete(runtime.tools, name)
		}
		runtime.mu.Unlock()
		return nil
	}); err != nil {
		runtime.mu.Lock()
		delete(runtime.tools, name)
		runtime.mu.Unlock()
		return err
	}
	return nil
}

// Catalog freezes the visible schemas and guidance. An empty allow list
// selects every registered tool.
func (runtime *Runtime) Catalog(allow []string) (Catalog, error) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	if !runtime.active {
		return Catalog{}, ErrNotRunning
	}
	visible := make([]*Tool, 0, len(runtime.tools))
	for name, candidate := range runtime.tools {
		if len(allow) == 0 || slices.Contains(allow, name) {
			visible = append(visible, candidate)
		}
	}
	slices.SortFunc(visible, func(left, right *Tool) int {
		return strings.Compare(left.definition.Name, right.definition.Name)
	})
	catalog := Catalog{Definitions: make([]session.ToolDefinition, len(visible))}
	names := map[string]struct{}{}
	for index, candidate := range visible {
		catalog.Definitions[index] = candidate.Definition()
		names[candidate.definition.Name] = struct{}{}
	}
	isVisible := func(name string) bool {
		_, ok := names[name]
		return ok
	}
	guided := slices.DeleteFunc(slices.Clone(visible), func(candidate *Tool) bool { return candidate.guidance.Text == nil })
	slices.SortStableFunc(guided, func(left, right *Tool) int { return left.guidance.Order - right.guidance.Order })
	for _, candidate := range guided {
		if text := candidate.guidance.Text(isVisible); text != "" {
			catalog.Guidance = append(catalog.Guidance, text)
		}
	}
	return catalog, nil
}

// prepared is one call after lookup and schema validation. A nil call
// carries a terminal error result and is scheduled as a barrier.
type prepared struct {
	call   *call
	result session.ToolResult
}

// ExecuteBatch validates every call against its schema and classifies it,
// then runs adjacent concurrent calls together and every other call as an
// exclusive barrier. Each call's Check, approval, and execution happen at
// its turn. Results keep the original call order.
func (runtime *Runtime) ExecuteBatch(ctx context.Context, request BatchRequest) []session.ToolResult {
	calls := make([]prepared, len(request.Calls))
	for index, candidate := range request.Calls {
		calls[index] = runtime.prepare(candidate)
	}
	results := make([]session.ToolResult, len(request.Calls))
	for index := 0; index < len(calls); {
		end := index + 1
		if calls[index].call != nil && calls[index].call.concurrent {
			for end < len(calls) && calls[end].call != nil && calls[end].call.concurrent {
				end++
			}
		}
		var group sync.WaitGroup
		for current := index; current < end; current++ {
			group.Go(func() {
				results[current] = runtime.execute(ctx, request, request.Calls[current], calls[current])
			})
		}
		group.Wait()
		index = end
	}
	return results
}

func (runtime *Runtime) prepare(candidate session.ToolCall) (result prepared) {
	result.result.CallID = candidate.ID
	defer func() {
		if recovered := recover(); recovered != nil {
			result.call = nil
			result.result.Output, result.result.IsError = "Error: implementation panicked", true
		}
	}()
	runtime.mu.RLock()
	registered := runtime.tools[candidate.Name]
	if !runtime.active {
		registered = nil
	}
	runtime.mu.RUnlock()
	if registered == nil {
		result.result.Output, result.result.IsError = finishText(fmt.Sprintf("Error: unknown tool %q", candidate.Name)), true
		return result
	}
	validated, err := registered.prepare(candidate.Arguments)
	if err != nil {
		result.result.Output, result.result.IsError = errorText(err), true
		return result
	}
	result.call = validated
	return result
}

func (runtime *Runtime) execute(ctx context.Context, request BatchRequest, candidate session.ToolCall, validated prepared) (result session.ToolResult) {
	if validated.call == nil {
		return validated.result
	}
	result.CallID = candidate.ID
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Output, result.IsError = "Error: implementation panicked", true
		}
	}()
	runtime.mu.RLock()
	store := runtime.spill
	runtime.mu.RUnlock()
	invocation := Invocation{
		SessionID: request.SessionID, Cwd: request.Cwd, Route: request.Route, Turn: request.Turn, Step: request.Step, CallID: candidate.ID,
		Journal: request.Journal, Delegated: request.Delegated, spill: store,
	}
	if err := validated.call.check(invocation); err != nil {
		result.Output, result.IsError = errorText(err), true
		return result
	}
	if reason := validated.call.reason(); reason != "" {
		outcome, err := runtime.approver.Decide(ctx, ApprovalRequest{
			SessionID: request.SessionID, Turn: request.Turn, Step: request.Step,
			Call: candidate, Reason: clamp(reason, maxReasonBytes, "…"), Delegated: request.Delegated, Journal: request.Journal,
		})
		if err != nil {
			result.Output, result.IsError = "Error: approval could not be recorded", true
			return result
		}
		if outcome != session.ApprovalAllowedOnce {
			result.Output, result.IsError = "Error: approval "+string(outcome), true
			return result
		}
		invocation.Approved = true
	}
	output, err := validated.call.execute(ctx, invocation)
	if err != nil {
		result.Output, result.IsError = errorText(err), true
		return result
	}
	text := strings.ToValidUTF8(output.Text, "�")
	// The spill store holds text only; an image result stays inline whole.
	if !validated.call.keepInline && output.Image == nil {
		text = retainInline(ctx, invocation, candidate.Name, text)
	}
	result.Output = finishText(text)
	result.Image = output.Image
	return result
}

// errorText renders a failed call with the upstream "Error: " envelope.
func errorText(err error) string { return finishText("Error: " + err.Error()) }

// finishText makes tool output durable: invalid UTF-8 is replaced and the
// complete text, including the truncation marker, fits one tool result.
func finishText(text string) string {
	return clamp(strings.ToValidUTF8(text, "�"), session.MaxTextBytes, "\n[output truncated]")
}

// clamp cuts text at a rune boundary so text plus suffix fits limit bytes.
func clamp(text string, limit int, suffix string) string {
	if len(text) <= limit {
		return text
	}
	cut := limit - len(suffix)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + suffix
}
