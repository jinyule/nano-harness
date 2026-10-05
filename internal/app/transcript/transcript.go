// Package transcript defines the session repository boundary consumed by use cases.
package transcript

import (
	"context"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// OpenOptions selects create or resume and immutable delegation metadata.
// Seed is a closed event prefix, numbered from 1, that a created session
// starts with, such as a forked child's copy of its parent's completed turns.
type OpenOptions struct {
	SessionID       string
	Create          bool
	Cwd             string
	ParentSessionID string
	DelegationDepth int
	Seed            []session.Event
}

// Log is one exclusively written durable event stream.
type Log interface {
	Header() session.Header
	Path() string
	Append(context.Context, session.Record) (session.Event, error)
	Events(context.Context) ([]session.Event, error)
	Flush(context.Context) error
	Close(context.Context) error
}

// Repository opens and inspects versioned transcripts.
type Repository interface {
	OpenSession(context.Context, OpenOptions) (Log, error)
	Inspect(context.Context, string) (session.Header, []session.Event, error)
	List(context.Context) ([]session.Header, error)
}
