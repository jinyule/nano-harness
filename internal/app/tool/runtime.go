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

// maxConcurrentCalls bounds in-flight calls in one agent's parallel group.
const maxConcurrentCalls = 10

var (
	// ErrInvalidTool identifies an invalid tool definition, registration, or runtime request.
	ErrInvalidTool = errors.New("invalid tool")
	// ErrNotRunning indicates the tool runtime has not started or has stopped.
	ErrNotRunning = errors.New("tool runtime is not running")
)

// Classifications the runtime owns, named as upstream's ToolRegistry names
// them. Results take copies with new, so none shares these values.
var (
	unknownTool           = session.ToolError{Name: "ToolNotFoundError", Code: "UNKNOWN_TOOL"}
	invalidArguments      = session.ToolError{Name: "ToolArgsError", Code: "INVALID_ARGS"}
	invalidToolOutput     = session.ToolError{Name: "ToolOutputError", Code: "INVALID_TOOL_OUTPUT"}
	abortedBeforeDispatch = session.ToolError{Name: "AbortError", Code: "ABORTED_BEFORE_DISPATCH"}
	aborted               = session.ToolError{Name: "AbortError", Code: "ABORTED"}
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
	call       *call
	keepInline bool
	result     session.ToolResult
}

// ExecuteBatch validates every call against its schema and classifies it,
// then runs at most ten adjacent concurrent calls at once. Every other call
// is an exclusive barrier. Each call's Check, approval, and execution happen
// at its turn. Results keep the original call order.
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
		slots := make(chan struct{}, maxConcurrentCalls)
		for current := index; current < end; current++ {
			slots <- struct{}{}
			group.Go(func() {
				defer func() { <-slots }()
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
	if candidate.LimitArguments().ArgumentsOmitted {
		result.result.Output = fmt.Sprintf("Error: tool arguments exceed %d bytes; submit a smaller call", session.MaxArgumentsBytes)
		result.result.IsError = true
		return result
	}
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
		result.result.Output, result.result.IsError, result.result.Error = fmt.Sprintf("Error: unknown tool %q", candidate.Name), true, new(unknownTool)
		return result
	}
	result.keepInline = registered.keepInline
	validated, err := registered.prepare(candidate.Arguments)
	if err != nil {
		// prepare fails only on schema validation and decoding.
		result.result.Output, result.result.IsError, result.result.Error = errorText(err), true, new(invalidArguments)
		return result
	}
	result.call = validated
	return result
}

func (runtime *Runtime) execute(ctx context.Context, request BatchRequest, candidate session.ToolCall, validated prepared) (result session.ToolResult) {
	runtime.mu.RLock()
	store := runtime.spill
	runtime.mu.RUnlock()
	invocation := Invocation{
		SessionID: request.SessionID, Cwd: request.Cwd, Route: request.Route, Turn: request.Turn, Step: request.Step, CallID: candidate.ID,
		Journal: request.Journal, Delegated: request.Delegated, spill: store,
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Output, result.IsError = "Error: implementation panicked", true
			result.Image, result.Error, result.Meta = nil, nil, nil
		}
		result.Output = finishText(result.Output)
	}()
	// This runs inside the panic boundary above, including store callbacks.
	defer func() {
		text := strings.ToValidUTF8(result.Output, "�")
		// Errors and successful text share the budget; image results stay inline.
		if !validated.keepInline && result.Image == nil {
			text = retainInline(ctx, invocation, candidate.Name, text)
		}
		result.Output = text
	}()
	if validated.call == nil {
		return validated.result
	}
	result.CallID = candidate.ID
	if ctx.Err() != nil {
		result.Output, result.IsError, result.Error = "Error: tool call aborted before dispatch", true, new(abortedBeforeDispatch)
		return result
	}
	if err := validated.call.check(invocation); err != nil {
		result.Output, result.Error = failureText(candidate.Name, err)
		result.IsError = true
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
	if ctx.Err() != nil {
		result.Output, result.IsError, result.Error = "Error: tool call aborted before dispatch", true, new(abortedBeforeDispatch)
		return result
	}
	output, err := validated.call.execute(ctx, invocation)
	// Like upstream, cancellation supersedes only a successful body result;
	// a failure the body returned keeps its own text and classification.
	if err != nil {
		result.Output, result.Error = failureText(candidate.Name, err)
		result.IsError = true
		return result
	}
	if ctx.Err() != nil {
		result.Output, result.IsError, result.Error = "Error: tool call aborted", true, new(aborted)
		return result
	}
	if output.Meta != nil {
		meta := output.Meta.Fit()
		if reason := metaViolation(candidate.Name, meta); reason != "" {
			result.Output, result.IsError, result.Error = invalidOutputText(candidate.Name, reason), true, new(invalidToolOutput)
			return result
		}
		result.Meta = &meta
	}
	result.Output = output.Text
	result.Image = output.Image
	return result
}

// errorText renders a failed call with the upstream "Error: " envelope.
func errorText(err error) string { return "Error: " + err.Error() }

// failureText renders a Check or Execute error and its classification, if
// any. A classification that would not persist is a tool defect.
func failureText(name string, err error) (string, *session.ToolError) {
	var failure Failure
	if !errors.As(err, &failure) {
		return errorText(err), nil
	}
	classification := failure.ToolError()
	if invalid := classification.Validate(); invalid != nil {
		return invalidOutputText(name, violation(invalid)), new(invalidToolOutput)
	}
	return errorText(err), &classification
}

// metaViolation reports why fitted metadata cannot be recorded for the
// named tool, or "" when it can.
func metaViolation(name string, meta session.ToolMeta) string {
	if err := meta.Validate(); err != nil {
		return violation(err)
	}
	if owner := meta.Tool(); owner != name {
		return fmt.Sprintf("metadata belongs to tool %q", owner)
	}
	return ""
}

// invalidOutputText is upstream's ToolOutputError message in the "Error: " envelope.
func invalidOutputText(name, reason string) string {
	return fmt.Sprintf("Error: tool %q returned invalid output: %s", name, reason)
}

// violation strips the record-validation prefix from a metadata or
// classification error.
func violation(err error) string {
	return strings.TrimPrefix(err.Error(), session.ErrInvalidRecord.Error()+": ")
}

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
