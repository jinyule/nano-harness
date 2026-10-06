package compaction

import (
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestMaybe_TruncatedSummaryFailsCompaction(t *testing.T) {
	for _, test := range []struct {
		name   string
		manual bool
		force  bool
		text   string
		prunes int
	}{
		{name: "manual partial text", manual: true, force: true, text: "Do not modify the"},
		{name: "manual empty text", manual: true, force: true},
		{name: "pressure after pruning", text: "Do not modify the", prunes: 3},
		{name: "overflow after pruning", force: true, text: "Do not modify the", prunes: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newCompactionHarness(t)
			harness.useContextWindow(t, 4000)
			harness.prepared.results = []llm.Completion{{Message: assistantSummary(test.text), Stop: llm.StopMaxTokens}}
			events := visibleEvents(4)
			if test.prunes > 0 {
				events = toolEvents(session.ToolResult{Output: long("a")}, session.ToolResult{Output: long("b")}, session.ToolResult{Output: long("c")})
			}
			journal := &compactionJournal{events: events, failures: map[int]error{}}
			changed, err := harness.service.Maybe(t.Context(), Request{Journal: journal, Turn: 1, Force: test.force, Manual: test.manual})
			if !errors.Is(err, errSummaryTruncated) || changed != (test.prunes > 0) {
				t.Fatalf("truncated summary = %t, %v", changed, err)
			}
			if len(prunes(journal.records)) != test.prunes || len(journal.records) != test.prunes+2 || journal.records[test.prunes].Type != session.RecordCompactionStart || journal.records[test.prunes+1].Type != session.RecordCompactionEnd || journal.records[test.prunes+1].Compaction.Error != "max_tokens" {
				t.Fatalf("records = %+v", journal.records)
			}
			if harness.prepared.calls != 1 {
				t.Fatalf("summary requests = %d, want 1 without retry", harness.prepared.calls)
			}
		})
	}
}

func TestMaybe_TruncatedSummaryPreservesFinishFailure(t *testing.T) {
	harness := newCompactionHarness(t)
	harness.prepared.results = []llm.Completion{{Message: assistantSummary("Do not modify the"), Stop: llm.StopMaxTokens}}
	failure := errors.New("disk full")
	journal := &compactionJournal{events: visibleEvents(4), failures: map[int]error{2: failure}}
	changed, err := harness.service.Maybe(t.Context(), Request{Journal: journal, Force: true, Manual: true})
	if changed || !errors.Is(err, failure) || !errors.Is(err, errSummaryTruncated) || len(journal.records) != 2 || journal.records[1].Type != session.RecordCompactionEnd || journal.records[1].Compaction.Error != "max_tokens" {
		t.Fatalf("truncated summary = %t, %v; records = %+v", changed, err, journal.records)
	}
}
