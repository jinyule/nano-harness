package compaction

import (
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// TestMaybe_ReportsCommittedChangesWhenSummaryFails proves the boolean result
// means "the surface changed" on every failure path: prunes committed before
// a failed summary, and a summary committed before its closing record failed,
// both change the surface the log folds to.
func TestMaybe_ReportsCommittedChangesWhenSummaryFails(t *testing.T) {
	failure := errors.New("injected")
	for name, test := range map[string]struct {
		manual  bool
		prepare func(*compactionHarness, *compactionJournal)
		want    bool
	}{
		"start append": {prepare: func(_ *compactionHarness, journal *compactionJournal) { journal.failures[2] = failure }, want: true},
		"prepare call": {prepare: func(harness *compactionHarness, _ *compactionJournal) { harness.provider.err = failure }, want: true},
		"stream": {prepare: func(harness *compactionHarness, _ *compactionJournal) {
			harness.prepared.errors = []error{failure, failure}
		}, want: true},
		"empty summary": {prepare: func(harness *compactionHarness, _ *compactionJournal) {
			harness.prepared.results = []llm.Completion{{Message: assistantSummary("")}}
		}, want: true},
		"summary append": {prepare: func(_ *compactionHarness, journal *compactionJournal) { journal.failures[3] = failure }, want: true},
		"end append":     {prepare: func(_ *compactionHarness, journal *compactionJournal) { journal.failures[4] = failure }, want: true},
		// Without prunes a summary that failed to commit changed nothing, but
		// one that committed before its closing record failed did.
		"manual summary append": {manual: true, prepare: func(_ *compactionHarness, journal *compactionJournal) { journal.failures[2] = failure }, want: false},
		"manual end append":     {manual: true, prepare: func(_ *compactionHarness, journal *compactionJournal) { journal.failures[3] = failure }, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newCompactionHarness(t)
			journal := &compactionJournal{events: toolEvents(session.ToolResult{Output: long("x")}, session.ToolResult{Output: "small"}), failures: map[int]error{}}
			test.prepare(harness, journal)
			compacted, err := harness.service.Maybe(t.Context(), Request{Journal: journal, Turn: 1, Force: true, Manual: test.manual})
			if err == nil || compacted != test.want {
				t.Fatalf("Maybe = %t, %v; want %t with an error", compacted, err, test.want)
			}
		})
	}
}
