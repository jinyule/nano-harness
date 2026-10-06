package agent

import (
	"context"
	"slices"
	"strings"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// ContextRequest is the committed state a context provider observes before
// one model step opens.
type ContextRequest struct {
	// Tools names every tool visible to the step in lexical order.
	Tools []string
	// Events is the complete committed log, including this turn's input.
	// All providers for a step observe the same log before any contribution.
	Events []session.Event
}

// ContextContribution separates sections of the complete runtime snapshot
// from independent user-role inputs, such as skill catalogs and invocations.
type ContextContribution struct {
	// Sections describes current runtime state without a replacement preamble.
	// Providers contribute all their sections even when an older copy is visible.
	Sections []string
	// Messages follows the complete snapshot in registration order.
	Messages []session.Message
}

// ContextProvider contributes context before a model step. The engine gathers
// every contribution before committing one complete runtime snapshot followed
// by independent messages as user/message records before step/start. Replay,
// compaction, fork, and resume observe these like any other input. A provider
// whose observation is incomplete returns no contribution; an error ends the turn.
type ContextProvider interface {
	StepContext(context.Context, ContextRequest) (ContextContribution, error)
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

// stepContext gathers the next step's context from one committed observation,
// then commits its complete snapshot and independent messages before step/start.
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
	events, err := input.journal.Events(ctx)
	if err != nil {
		return err
	}
	var sections []string
	var messages []session.Message
	for _, entry := range entries {
		contribution, err := entry.provider.StepContext(ctx, ContextRequest{Tools: slices.Clone(names), Events: events})
		if err != nil {
			return err
		}
		sections = append(sections, contribution.Sections...)
		messages = append(messages, contribution.Messages...)
	}
	snapshot, err := runtimeSnapshot(events, sections)
	if err != nil {
		return err
	}
	return appendUserMessages(ctx, input.journal, turn, append(snapshot, messages...))
}

// runtimeSnapshot compares the whole current context with the latest retained
// engine snapshot. A hidden snapshot is reconstructed after compaction; a fork
// preserves its prefix and appends a replacement only when its own scope differs.
func runtimeSnapshot(events []session.Event, sections []string) ([]session.Message, error) {
	if len(sections) == 0 {
		return nil, nil
	}
	text := "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n" + strings.Join(sections, "\n\n")
	surface, err := session.Surface(events)
	if err != nil {
		return nil, err
	}
	for _, node := range slices.Backward(surface) {
		if node.Message != nil && node.Message.Source.Kind == "runtime-context" && node.Message.Source.Plugin == "agent-engine" {
			if session.Text(*node.Message) == text {
				return nil, nil
			}
			break
		}
	}
	return []session.Message{{Role: session.RoleUser, Source: session.MessageSource{Kind: "runtime-context", Plugin: "agent-engine"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}}, nil
}
