package agent

import (
	"context"
	"errors"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// ErrNotAdmitted reports a queued turn whose opening message an Admission
// rejected as stale. No record is committed for it, so the TurnResult has no
// turn number or outcome.
var ErrNotAdmitted = errors.New("turn input is no longer admissible")

// Journal is the durable log of one live agent, as seen by session-state
// services that commit their own facts, such as goals.
type Journal interface {
	Header() session.Header
	Events(context.Context) ([]session.Event, error)
	Append(context.Context, session.Record) (session.Event, error)
}

// Admission vets queued input whose message source kind it registered for,
// at the moment the input's turn opens. State that decided the message may
// have changed while the turn waited behind other work, so Admit re-checks
// it and calls open, which commits turn/start and the opening user/message,
// while excluding every change that could make the message stale. It
// returns ErrNotAdmitted without calling open to drop the turn. Admit is
// called from the agent worker and must observe ctx.
type Admission interface {
	Admit(ctx context.Context, journal Journal, message session.Message, open func(context.Context) error) error
}

// RegisterAdmission publishes admission for opening messages of one source
// kind for exactly the caller's scope lifetime. A kind has at most one
// admission; without one, turns open unconditionally.
func (engine *Engine) RegisterAdmission(kind string, admission Admission, scope *plugin.Scope) error {
	if kind == "" || admission == nil || scope == nil {
		return ErrInvalidConfig
	}
	entry := &admission
	engine.mu.Lock()
	if !engine.active {
		engine.mu.Unlock()
		return ErrNotRunning
	}
	if engine.admissions[kind] != nil {
		engine.mu.Unlock()
		return ErrInvalidConfig
	}
	if engine.admissions == nil {
		engine.admissions = map[string]*Admission{}
	}
	engine.admissions[kind] = entry
	engine.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		engine.removeAdmission(kind, entry)
		return nil
	}); err != nil {
		engine.removeAdmission(kind, entry)
		return err
	}
	return nil
}

// removeAdmission withdraws exactly one registration, never a later one for the same kind.
func (engine *Engine) removeAdmission(kind string, entry *Admission) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.admissions[kind] == entry {
		delete(engine.admissions, kind)
	}
}

// openTurn commits a turn's opening records through the admission
// registered for the message's source kind, if any.
func (engine *Engine) openTurn(ctx context.Context, input runInput, open func(context.Context) error) error {
	engine.mu.RLock()
	entry := engine.admissions[input.message.Source.Kind]
	engine.mu.RUnlock()
	if entry == nil {
		return open(ctx)
	}
	return (*entry).Admit(ctx, input.journal, cloneMessage(input.message), open)
}

// Journal returns the durable log of one live agent. Appends through it
// reach the agent's subscribers like the agent's own records.
func (registry *Registry) Journal(sessionID string) (Journal, error) {
	current, err := registry.Find(sessionID)
	if err != nil {
		return nil, err
	}
	return current.journal, nil
}
