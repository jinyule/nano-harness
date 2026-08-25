package agent

import (
	"context"
	"sync"

	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type journal struct {
	log transcript.Log

	mu          sync.Mutex
	subscribers map[uint64]chan session.Event
	nextID      uint64
}

func newJournal(log transcript.Log) *journal {
	return &journal{log: log, subscribers: map[uint64]chan session.Event{}}
}

func (journal *journal) Header() session.Header { return journal.log.Header() }
func (journal *journal) Path() string           { return journal.log.Path() }

func (journal *journal) Append(ctx context.Context, record session.Record) (session.Event, error) {
	event, err := journal.log.Append(ctx, record)
	if err != nil {
		return session.Event{}, err
	}
	journal.mu.Lock()
	for _, subscriber := range journal.subscribers {
		select {
		case subscriber <- session.CloneEvent(event):
		default:
		}
	}
	journal.mu.Unlock()
	return event, nil
}

func (journal *journal) Events(ctx context.Context) ([]session.Event, error) {
	return journal.log.Events(ctx)
}

func (journal *journal) Flush(ctx context.Context) error { return journal.log.Flush(ctx) }
func (journal *journal) Close(ctx context.Context) error { return journal.log.Close(ctx) }

func (journal *journal) subscribe(buffer int) (<-chan session.Event, func(), error) {
	if buffer < 1 || buffer > 4096 {
		return nil, nil, ErrInvalidConfig
	}
	channel := make(chan session.Event, buffer)
	journal.mu.Lock()
	journal.nextID++
	id := journal.nextID
	journal.subscribers[id] = channel
	journal.mu.Unlock()
	dispose := func() {
		journal.mu.Lock()
		if current, exists := journal.subscribers[id]; exists {
			delete(journal.subscribers, id)
			close(current)
		}
		journal.mu.Unlock()
	}
	return channel, dispose, nil
}

func (journal *journal) closeSubscribers() {
	journal.mu.Lock()
	for id, subscriber := range journal.subscribers {
		delete(journal.subscribers, id)
		close(subscriber)
	}
	journal.mu.Unlock()
}
