package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// memoryBackend serves one fixed settings document.
type memoryBackend struct{ document settings.Document }

func (backend *memoryBackend) Load(context.Context) (settings.Document, error) {
	return backend.document, nil
}
func (*memoryBackend) Persist(context.Context, settings.Document) error { return nil }
func (*memoryBackend) Watch(ctx context.Context, _ func(settings.Document, error)) error {
	<-ctx.Done()
	return nil
}

// useContextWindow routes compaction to a model with the given window, so
// pressure thresholds are small enough to cross with a few tool results.
func (harness *compactionHarness) useContextWindow(t *testing.T, window int) {
	t.Helper()
	document := settings.Defaults()
	models := document.Providers[document.Route.Provider].Models
	for index := range models {
		if models[index].ID == document.Route.Model {
			models[index].ContextWindow = window
		}
	}
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	if err := harness.settings.Mount(t.Context(), &memoryBackend{document: document}, scope); err != nil {
		t.Fatal(err)
	}
}

// toolEvents returns a visible user message followed by one tool call and
// result per output, each in its own step of turn 1.
func toolEvents(outputs ...session.ToolResult) []session.Event {
	events := visibleEvents(1)
	for index, result := range outputs {
		id := "call-" + string(rune('a'+index))
		result.CallID = id
		events = append(events,
			session.Event{Record: session.Record{Type: session.RecordAssistantMessage, Turn: 1, Step: uint64(index + 1), Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "calling"}}}}},
			session.Event{Record: session.Record{Type: session.RecordToolCall, Turn: 1, Step: uint64(index + 1), Call: &session.ToolCall{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)}}},
			session.Event{Record: session.Record{Type: session.RecordToolResult, Turn: 1, Step: uint64(index + 1), Result: &result}},
		)
	}
	for index := range events {
		events[index].Sequence = uint64(index + 1)
	}
	return events
}

func long(fill string) string { return strings.Repeat(fill, 40_000) }

func prunes(records []session.Record) []session.ToolResultPrune {
	var found []session.ToolResultPrune
	for _, record := range records {
		if record.Type == session.RecordCompactionPrune {
			found = append(found, *record.Prune)
		}
	}
	return found
}

func TestMaybe_PruningAloneRelievesPressure(t *testing.T) {
	harness := newCompactionHarness(t)
	harness.useContextWindow(t, 4000)
	image := &session.Image{ID: "img", Name: "shot.png", MediaType: "image/png", Bytes: 10, Width: 1, Height: 1}
	events := toolEvents(session.ToolResult{Output: long("x"), IsError: true, Image: image}, session.ToolResult{Output: "small"})
	journal := &compactionJournal{events: events, failures: map[int]error{}}
	compacted, err := harness.service.Maybe(t.Context(), Request{Journal: journal, Turn: 1})
	if err != nil || !compacted {
		t.Fatalf("Maybe = %t, %v", compacted, err)
	}
	want, _ := session.PruneToolOutput(long("x"))
	if found := prunes(journal.records); len(found) != 1 || len(journal.records) != 1 || found[0].Seq != 4 || found[0].Output != want || journal.records[0].Turn != 1 {
		t.Fatalf("records = %+v", journal.records)
	}
	if harness.prepared.calls != 0 {
		t.Fatalf("pruning that relieved pressure still asked for %d summaries", harness.prepared.calls)
	}
	surface, err := session.Surface(append(events, session.Event{Sequence: uint64(len(events) + 1), Record: journal.records[0]}))
	if err != nil || surface[3].Result.Output != want || !surface[3].Result.IsError || surface[3].Result.Image == nil {
		t.Fatalf("replayed surface = %+v, %v", surface, err)
	}
}

func TestMaybe_SummarizesThePrunedSurfaceWhenPressureRemains(t *testing.T) {
	harness := newCompactionHarness(t)
	harness.useContextWindow(t, 4000)
	events := toolEvents(session.ToolResult{Output: long("a")}, session.ToolResult{Output: long("b")}, session.ToolResult{Output: long("c")})
	journal := &compactionJournal{events: events, failures: map[int]error{}}
	compacted, err := harness.service.Maybe(t.Context(), Request{Journal: journal, Turn: 1})
	if err != nil || !compacted {
		t.Fatalf("Maybe = %t, %v", compacted, err)
	}
	kinds := make([]session.RecordType, len(journal.records))
	for index, record := range journal.records {
		kinds[index] = record.Type
	}
	if len(prunes(journal.records)) != 3 || len(kinds) != 6 || kinds[3] != session.RecordCompactionStart || kinds[4] != session.RecordCompactionSummary {
		t.Fatalf("records = %v", kinds)
	}
	summarized := harness.prepared.seen[0].Surface
	for _, node := range summarized {
		if node.Result != nil && len(node.Result.Output) > session.PruneThreshold {
			t.Fatalf("the summary request saw an unpruned result of %d bytes", len(node.Result.Output))
		}
	}
}

func TestMaybe_ForcedAndManualRequests(t *testing.T) {
	harness := newCompactionHarness(t)
	events := toolEvents(session.ToolResult{Output: long("x")}, session.ToolResult{Output: "small"})
	forced := &compactionJournal{events: events, failures: map[int]error{}}
	if compacted, err := harness.service.Maybe(t.Context(), Request{Journal: forced, Turn: 1, Force: true}); err != nil || !compacted {
		t.Fatalf("forced Maybe = %t, %v", compacted, err)
	}
	if len(prunes(forced.records)) != 1 || forced.records[1].Type != session.RecordCompactionStart || harness.prepared.calls != 1 {
		t.Fatalf("context-window recovery did not prune and then summarize: %+v", forced.records)
	}
	manual := &compactionJournal{events: events, failures: map[int]error{}}
	if compacted, err := harness.service.Maybe(t.Context(), Request{Journal: manual, Force: true, Manual: true}); err != nil || !compacted {
		t.Fatalf("manual Maybe = %t, %v", compacted, err)
	}
	if len(prunes(manual.records)) != 0 || manual.records[0].Type != session.RecordCompactionStart {
		t.Fatalf("manual compaction pruned: %+v", manual.records)
	}
	// A forced request that can only prune still reports a changed surface.
	short := &compactionJournal{events: toolEvents(session.ToolResult{Output: long("y")})[2:4], failures: map[int]error{}}
	if compacted, err := harness.service.Maybe(t.Context(), Request{Journal: short, Turn: 1, Force: true}); err != nil || !compacted || len(short.records) != 1 {
		t.Fatalf("prune-only forced Maybe = %t, %v, %+v", compacted, err, short.records)
	}
}

func TestMaybe_PruneFailureKeepsEarlierPrunes(t *testing.T) {
	harness := newCompactionHarness(t)
	harness.useContextWindow(t, 4000)
	failure := errors.New("disk full")
	journal := &compactionJournal{events: toolEvents(session.ToolResult{Output: long("a")}, session.ToolResult{Output: long("b")}), failures: map[int]error{2: failure}}
	compacted, err := harness.service.Maybe(context.Background(), Request{Journal: journal, Turn: 1})
	if !errors.Is(err, failure) || !compacted || !strings.Contains(err.Error(), "prune tool result 7") {
		t.Fatalf("Maybe = %t, %v", compacted, err)
	}
	if len(journal.records) != 2 || harness.prepared.calls != 0 {
		t.Fatalf("records after failure = %d, summaries = %d", len(journal.records), harness.prepared.calls)
	}
}
