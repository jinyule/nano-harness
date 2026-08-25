package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

type memoryLog struct {
	mu         sync.Mutex
	header     session.Header
	path       string
	events     []session.Event
	appendErr  error
	eventsErr  error
	appendHook func(session.Record) error
	eventsHook func(int) error
	eventCalls int
	flushErr   error
	closeErr   error
	closed     bool
}

func (log *memoryLog) Header() session.Header { return log.header }
func (log *memoryLog) Path() string           { return log.path }
func (log *memoryLog) Append(_ context.Context, record session.Record) (session.Event, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.appendErr != nil {
		return session.Event{}, log.appendErr
	}
	if log.appendHook != nil {
		if err := log.appendHook(record); err != nil {
			return session.Event{}, err
		}
	}
	event := session.CloneEvent(session.Event{Sequence: uint64(len(log.events) + 1), Record: record})
	log.events = append(log.events, event)
	return session.CloneEvent(event), nil
}
func (log *memoryLog) Events(context.Context) ([]session.Event, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.eventCalls++
	if log.eventsErr != nil {
		return nil, log.eventsErr
	}
	if log.eventsHook != nil {
		if err := log.eventsHook(log.eventCalls); err != nil {
			return nil, err
		}
	}
	events := make([]session.Event, len(log.events))
	for index := range log.events {
		events[index] = session.CloneEvent(log.events[index])
	}
	return events, nil
}
func (log *memoryLog) Flush(context.Context) error { return log.flushErr }
func (log *memoryLog) Close(context.Context) error {
	log.closed = true
	return log.closeErr
}

func TestJournal_DelegatesAndPublishesDetachedEvents(t *testing.T) {
	log := &memoryLog{header: session.Header{SessionID: "session"}, path: "/log", flushErr: errors.New("flush"), closeErr: errors.New("close")}
	journal := newJournal(log)
	if journal.Header().SessionID != "session" || journal.Path() != "/log" {
		t.Fatalf("journal identity = %#v %q", journal.Header(), journal.Path())
	}
	if _, _, err := journal.subscribe(0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero buffer error = %v", err)
	}
	if _, _, err := journal.subscribe(4097); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("large buffer error = %v", err)
	}
	updates, dispose, err := journal.subscribe(1)
	if err != nil {
		t.Fatal(err)
	}
	message := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "original"}}}
	event, err := journal.Append(context.Background(), session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message})
	if err != nil || event.Sequence != 1 {
		t.Fatalf("Append() = %#v, %v", event, err)
	}
	message.Content[0].Text = "mutated"
	published := <-updates
	if session.Text(*published.Record.Message) != "original" {
		t.Fatalf("published event aliased input: %#v", published)
	}
	// A full subscriber is lossy by design and cannot block durable appends.
	if _, err := journal.Append(context.Background(), session.Record{Type: session.RecordTurnStart, Turn: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), session.Record{Type: session.RecordTurnEnd, Turn: 2, Outcome: session.OutcomeCompleted}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Events(context.Background())
	if err != nil || len(events) != 3 {
		t.Fatalf("Events() = %#v, %v", events, err)
	}
	if !errors.Is(journal.Flush(context.Background()), log.flushErr) || !errors.Is(journal.Close(context.Background()), log.closeErr) || !log.closed {
		t.Fatal("flush/close were not delegated")
	}
	dispose()
	dispose()
	for range updates {
		continue
	}
}

func TestJournal_ContainsLogFailureAndClosesAllSubscribers(t *testing.T) {
	appendFailure := errors.New("append")
	log := &memoryLog{appendErr: appendFailure, eventsErr: errors.New("events")}
	journal := newJournal(log)
	first, _, _ := journal.subscribe(1)
	second, _, _ := journal.subscribe(1)
	if _, err := journal.Append(context.Background(), session.Record{}); !errors.Is(err, appendFailure) {
		t.Fatalf("append error = %v", err)
	}
	if _, err := journal.Events(context.Background()); !errors.Is(err, log.eventsErr) {
		t.Fatalf("events error = %v", err)
	}
	journal.closeSubscribers()
	journal.closeSubscribers()
	if _, open := <-first; open {
		t.Fatal("first subscriber remained open")
	}
	if _, open := <-second; open {
		t.Fatal("second subscriber remained open")
	}
}
