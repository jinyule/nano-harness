package compaction

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// longEvents returns count user messages of about tokens estimated tokens each.
func longEvents(count, tokens int) []session.Event {
	events := visibleEvents(count)
	for index := range events {
		events[index].Record.Message.Content[0].Text = strings.Repeat("abcd", tokens)
	}
	return events
}

// memorySettings is a settings backend that keeps the document in memory.
type memorySettings struct{}

func (memorySettings) Load(context.Context) (settings.Document, error) {
	return settings.Defaults(), nil
}
func (memorySettings) Persist(context.Context, settings.Document) error { return nil }
func (memorySettings) Watch(ctx context.Context, _ func(settings.Document, error)) error {
	<-ctx.Done()
	return nil
}

func TestMaybe_UsesTheInheritedRouteForThresholdAndSummary(t *testing.T) {
	harness := newCompactionHarness(t)
	// About 3000 tokens: far below the hot route's 1,050,000-token window,
	// far above the threshold of a 1024-token window.
	journal := &compactionJournal{events: longEvents(3, 1000), failures: map[int]error{}}
	inherited := session.SubagentRoute{Provider: "openai", Model: "small", Effort: session.EffortLow}
	if compacted, err := harness.service.Maybe(context.Background(), Request{Journal: journal, Turn: 1}); err != nil || compacted {
		t.Fatalf("hot route compacted=%t err=%v", compacted, err)
	}
	// An inherited model the catalog no longer lists has no known window, so
	// pressure never triggers on it.
	if compacted, err := harness.service.Maybe(context.Background(), Request{Journal: journal, Turn: 1, Route: inherited}); err != nil || compacted {
		t.Fatalf("uncatalogued inherited route compacted=%t err=%v", compacted, err)
	}
	scope := &plugin.Scope{}
	if err := harness.settings.Mount(context.Background(), memorySettings{}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	_, revision, _ := harness.settings.Snapshot()
	if err := harness.settings.Update(context.Background(), revision, func(document *settings.Document) error {
		provider := document.Providers["openai"]
		provider.Models = append(provider.Models, settings.Model{ID: "small", Name: "Small", Effort: session.EffortMax, ContextWindow: 1024, Tools: true})
		document.Providers["openai"] = provider
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	compacted, err := harness.service.Maybe(context.Background(), Request{Journal: journal, Turn: 1, Route: inherited})
	if err != nil || !compacted {
		t.Fatalf("inherited route compacted=%t err=%v", compacted, err)
	}
	// The summary uses the inherited model and effort, not the catalog's max.
	if summary := journal.records[1].Compaction; summary.Effort != session.EffortLow {
		t.Fatalf("summary effort=%q", summary.Effort)
	}
	if request := harness.prepared.seen[len(harness.prepared.seen)-1]; request.Effort == nil || *request.Effort != session.EffortLow {
		t.Fatalf("summary request effort=%v", request.Effort)
	}
	if model := harness.provider.models[len(harness.provider.models)-1]; model != "small" {
		t.Fatalf("summary prepared model=%q", model)
	}
}

func TestMaybe_SecondCompactionRecordsSortedShadowedSequences(t *testing.T) {
	harness := newCompactionHarness(t)
	// A first compaction left its summary (sequence 9) in front of the
	// retained older messages 3 to 8.
	events := append(visibleEvents(8), session.Event{Sequence: 9, Record: session.Record{Type: session.RecordCompactionSummary, Turn: 1, Compaction: &session.CompactionData{
		ID: "earlier", ShadowedSeqs: []uint64{1, 2}, ShadowedTokenCount: 2, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "earlier"}}, Provider: "openai", Model: "gpt-5.6-luna",
	}}})
	journal := &compactionJournal{events: events, failures: map[int]error{}}
	if compacted, err := harness.service.Maybe(context.Background(), Request{Journal: journal, Turn: 1, Force: true}); err != nil || !compacted {
		t.Fatalf("second compaction=%t err=%v", compacted, err)
	}
	summary := journal.records[1]
	if err := summary.Validate(); err != nil || !slices.IsSorted(summary.Compaction.ShadowedSeqs) || !slices.Equal(summary.Compaction.ShadowedSeqs, []uint64{3, 4, 9}) {
		t.Fatalf("second summary=%#v err=%v", summary.Compaction, err)
	}
	if _, err := session.Surface(append(events, session.Event{Sequence: 10, Record: summary})); err != nil {
		t.Fatalf("second summary does not fold: %v", err)
	}
}
