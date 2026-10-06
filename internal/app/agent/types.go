// Package agent owns live agent handles, turn workers, and the core agent loop.
package agent

import (
	"context"
	"errors"

	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidConfig identifies an invalid agent request or composition.
	ErrInvalidConfig = errors.New("invalid agent configuration")
	// ErrNotRunning indicates the requested agent component has not started or has stopped.
	ErrNotRunning = errors.New("agent is not running")
	// ErrAgentExists indicates a live agent already owns the requested session ID.
	ErrAgentExists = errors.New("agent already exists")
	// ErrAgentNotFound indicates no live agent owns the requested session ID.
	ErrAgentNotFound = errors.New("agent not found")
	// ErrAgentIdle indicates a steer requires an active turn but the agent is idle.
	ErrAgentIdle = errors.New("agent has no active turn")
)

// TurnResult is the terminal result of one submitted message.
type TurnResult struct {
	SessionID string
	Turn      uint64
	Outcome   session.TurnOutcome
	Text      string
	Err       error
	// opened reports that turn/start committed, so the opening message
	// left its queue for good.
	opened bool
}

// Status is a secret-free live-agent snapshot.
type Status struct {
	SessionID string
	ParentID  string
	Label     string
	Mode      string
	Depth     int
	Busy      bool
	Pending   int
	Last      TurnResult
}

// CreateRequest defines a root or delegated agent. A created delegated
// agent records Provider ("spawn" or "fork") and the Route it inherited in
// its descriptor; Seed is the closed prefix of the parent's events a fork
// starts with. Roots take none of them and follow the hot settings route.
type CreateRequest struct {
	SessionID string
	ParentID  string
	Label     string
	Mode      string
	Provider  string
	Route     session.SubagentRoute
	Persona   string
	Tools     []string
	// Sandbox is the parent override captured at delegation, never a one-shot grant.
	Sandbox session.SandboxMode
	Seed    []session.Event
	Depth   int
	Create  bool
}

// Controller is the live-agent boundary consumed by TUI and subagent tools.
type Controller interface {
	Submit(context.Context, session.Message) (<-chan TurnResult, error)
	Followup(context.Context, session.Message) (<-chan TurnResult, error)
	Steer(context.Context, session.Message) error
	Interrupt()
	WhenIdle(context.Context) error
	Subscribe(int) (<-chan session.Event, func(), error)
	Status() Status
	Events(context.Context) ([]session.Event, error)
	Surface(context.Context) ([]session.SurfaceNode, error)
	Compact(context.Context) (bool, error)
}
