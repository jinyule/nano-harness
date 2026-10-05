package agent

import (
	"context"
	"slices"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// ContextRequest is the committed state a context provider observes before
// one model step opens.
type ContextRequest struct {
	// Tools names every tool visible to the step in lexical order.
	Tools []string
	// Events is the complete committed log, including this turn's input and
	// the contributions of providers that ran earlier for the same step.
	Events []session.Event
}

// ContextProvider contributes durable user-role context before a model
// step. The engine commits returned messages in order as user/message
// records of the active turn before step/start, so replay, compaction, fork,
// and resume observe them like any other input. A provider whose observation
// is incomplete returns no messages; an error ends the turn.
type ContextProvider interface {
	StepContext(context.Context, ContextRequest) ([]session.Message, error)
}

// contextEntry gives each registration a distinct identity, so a cleanup
// removes exactly its own contribution even if a provider registers twice.
type contextEntry struct{ provider ContextProvider }

// RegisterContext publishes a provider for exactly the caller's scope
// lifetime. Providers run in registration order.
func (engine *Engine) RegisterContext(provider ContextProvider, scope *plugin.Scope) error {
	if provider == nil || scope == nil {
		return ErrInvalidConfig
	}
	entry := &contextEntry{provider: provider}
	engine.mu.Lock()
	if !engine.active {
		engine.mu.Unlock()
		return ErrNotRunning
	}
	engine.contexts = append(engine.contexts, entry)
	engine.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		engine.removeContext(entry)
		return nil
	}); err != nil {
		engine.removeContext(entry)
		return err
	}
	return nil
}

func (engine *Engine) removeContext(entry *contextEntry) {
	engine.mu.Lock()
	engine.contexts = slices.DeleteFunc(engine.contexts, func(candidate *contextEntry) bool { return candidate == entry })
	engine.mu.Unlock()
}

// stepContext commits every registered provider's contribution for the next
// step of turn. Each provider rereads the log so it observes earlier
// contributions of the same step.
func (engine *Engine) stepContext(ctx context.Context, input runInput, turn uint64) error {
	engine.mu.RLock()
	entries := slices.Clone(engine.contexts)
	engine.mu.RUnlock()
	if len(entries) == 0 {
		return nil
	}
	catalog, err := engine.tools.Catalog(input.tools)
	if err != nil {
		return err
	}
	names := make([]string, len(catalog.Definitions))
	for index, definition := range catalog.Definitions {
		names[index] = definition.Name
	}
	for _, entry := range entries {
		events, err := input.journal.Events(ctx)
		if err != nil {
			return err
		}
		messages, err := entry.provider.StepContext(ctx, ContextRequest{Tools: slices.Clone(names), Events: events})
		if err != nil {
			return err
		}
		if err := appendUserMessages(ctx, input.journal, turn, messages); err != nil {
			return err
		}
	}
	return nil
}
