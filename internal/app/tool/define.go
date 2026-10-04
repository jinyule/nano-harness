package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// Guidance orders, mirroring the upstream system-prompt section table so
// tool paragraphs keep the same relative order as the reference harness.
const (
	// OrderBash positions shell guidance.
	OrderBash = 1000
	// OrderRead positions file-read guidance.
	OrderRead = 1100
	// OrderGlob positions path-discovery guidance.
	OrderGlob = 1400
	// OrderGrep positions content-search guidance.
	OrderGrep = 1500
)

// Invocation is the runtime context of one validated call.
type Invocation struct {
	SessionID string
	Cwd       string
	Delegated bool
	// Approved reports that the call's approval reason received a one-shot
	// grant. Tools that require approval must still check it at their
	// execution point.
	Approved bool
}

// Result is the model-visible content of one successful execution. Text is
// the only content kind today; richer content extends this type without
// changing tools that only return text.
type Result struct {
	Text string
}

// Text returns a text-only result.
func Text(text string) Result { return Result{Text: text} }

// Guidance is an optional system-prompt paragraph contributed while a tool
// is visible in a request. The zero value contributes nothing.
type Guidance struct {
	// Order positions the paragraph among visible tools; ties sort by name.
	Order int
	// Text renders the paragraph. visible reports whether another tool is in
	// the same request; an empty result omits the paragraph.
	Text func(visible func(name string) bool) string
}

// StaticGuidance contributes the same paragraph whenever the tool is visible.
func StaticGuidance(order int, text string) Guidance {
	return Guidance{Order: order, Text: func(func(string) bool) string { return text }}
}

// Spec declares one tool. Arguments are validated against Parameters before
// they are decoded into A, whose exported fields must name exactly the
// declared members with compatible Go kinds; optional members use pointers
// when absence must be distinguished from a zero value.
type Spec[A any] struct {
	Name        string
	Description string
	Parameters  Parameters
	Guidance    Guidance
	// Check rejects schema-valid arguments with semantic errors. It runs when
	// the call's turn comes, after earlier calls in the batch finished and
	// before any approval is requested, so it may inspect the filesystem.
	// Nil accepts every schema-valid value.
	Check func(A) error
	// Concurrent opts a call into overlap with adjacent concurrent calls. It
	// classifies every schema-valid call before the batch runs, so it must be
	// pure and total. Nil, invalid arguments, and unknown tools are exclusive.
	Concurrent func(A) bool
	// Approval returns the one-shot approval reason for a call that passed
	// Check, or an empty string when the call needs no approval.
	Approval func(A) string
	// Execute runs one validated call and must observe ctx.
	Execute func(context.Context, Invocation, A) (Result, error)
}

// Tool is one compiled, immutable tool. Define never fails; an invalid spec
// yields a Tool that Runtime.Register rejects with the compilation error.
type Tool struct {
	definition session.ToolDefinition
	guidance   Guidance
	prepare    func(json.RawMessage) (*call, error)
	err        error
}

// call is one schema-valid invocation bound to its typed arguments.
type call struct {
	concurrent bool
	check      func() error
	reason     func() string
	execute    func(context.Context, Invocation) (Result, error)
}

// Define compiles a spec into a registrable tool.
func Define[A any](spec Spec[A]) *Tool {
	definition := session.ToolDefinition{Name: spec.Name, Description: spec.Description}
	if !validName(spec.Name) {
		return &Tool{definition: definition, err: errors.New("tool name must be 1-64 ASCII letters, digits, '_' or '-'")}
	}
	if err := spec.Parameters.check(); err != nil {
		return &Tool{definition: definition, err: err}
	}
	definition.Parameters = spec.Parameters.marshal()
	if err := validateDefinition(definition); err != nil {
		return &Tool{definition: definition, err: err}
	}
	if err := checkGoType(reflect.TypeFor[A](), spec.Parameters, spec.Name); err != nil {
		return &Tool{definition: definition, err: err}
	}
	if spec.Execute == nil {
		return &Tool{definition: definition, err: errors.New("execute is required")}
	}
	prepare := func(raw json.RawMessage) (*call, error) {
		if err := spec.Parameters.validate(raw); err != nil {
			return nil, err
		}
		var arguments A
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		prepared := &call{
			check: func() error {
				if spec.Check == nil {
					return nil
				}
				return spec.Check(arguments)
			},
			reason: func() string {
				if spec.Approval == nil {
					return ""
				}
				return spec.Approval(arguments)
			},
			execute: func(ctx context.Context, invocation Invocation) (Result, error) {
				return spec.Execute(ctx, invocation, arguments)
			},
		}
		if spec.Concurrent != nil {
			prepared.concurrent = spec.Concurrent(arguments)
		}
		return prepared, nil
	}
	return &Tool{definition: definition, guidance: spec.Guidance, prepare: prepare}
}

// Definition returns the frozen model-visible schema.
func (tool *Tool) Definition() session.ToolDefinition {
	definition := tool.definition
	definition.Parameters = append(json.RawMessage(nil), tool.definition.Parameters...)
	return definition
}

// validName enforces the function-name alphabet shared by every provider wire format.
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, char := range name {
		if char != '_' && char != '-' && (char < '0' || char > '9') && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
			return false
		}
	}
	return true
}

func validateDefinition(definition session.ToolDefinition) error {
	header := session.Record{Type: session.RecordRequestHeader, Turn: 1, Step: 1, Header: &session.RequestHeader{Provider: "test", Model: "test", Tools: []session.ToolDefinition{definition}}}
	return header.Validate()
}
