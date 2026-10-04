package tui

import (
	"sync"

	tea "charm.land/bubbletea/v2"
)

// Bubble Tea does not join commands on exit. The gate prevents queued commands
// from starting after Run begins waiting for already executing work.
type commandGroup struct {
	mu      sync.Mutex
	closed  bool
	pending sync.WaitGroup
}

func (group *commandGroup) wrap(command tea.Cmd) tea.Cmd {
	if command == nil {
		return nil
	}
	return func() tea.Msg {
		group.mu.Lock()
		if group.closed {
			group.mu.Unlock()
			return nil
		}
		group.pending.Add(1)
		group.mu.Unlock()
		defer group.pending.Done()
		return command()
	}
}

func (group *commandGroup) close() {
	group.mu.Lock()
	group.closed = true
	group.mu.Unlock()
	group.pending.Wait()
}
