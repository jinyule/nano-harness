package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// The shared interior line forces a shortest-path search after equal edges
// are removed. Its small JSON representation cannot exhaust the byte budget.
func expensiveDiffFixture() (string, string) {
	before := strings.Repeat("a\n", 1024) + "keep\n" + strings.Repeat("a\n", 1024)
	return before, strings.ReplaceAll(before, "a", "b")
}

func TestEditResult_DiffWorkBudgetOnlyTruncatesMetadata(t *testing.T) {
	h := newHarness(t)
	before, after := expensiveDiffFixture()
	writeFixture(t, h.path("f"), before)
	h.read(t, "f")
	result := persistedFileResult(t, "edit", h.call(t, "edit", map[string]any{
		"file_path": "f", "old_string": "a", "new_string": "b", "replace_all": true,
	}))
	want := &session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{}, Truncated: true}}
	if result.IsError || result.Error != nil || !reflect.DeepEqual(result.Meta, want) || result.Output != fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", h.path("f")) {
		t.Fatalf("budget result = %+v, want successful publication with empty truncated metadata", result)
	}
	if readFixture(t, h.path("f")) != after {
		t.Fatal("diff budget changed the published file")
	}
	// The observation must follow the committed content even when meta is lost.
	if next := h.call(t, "edit", map[string]any{"file_path": "f", "old_string": "keep", "new_string": "kept"}); next.IsError {
		t.Fatalf("observation after bounded diff = %+v", next)
	}
}

type countedDiffContext struct {
	context.Context
	active   bool
	checks   int
	cancelAt int
	cancel   context.CancelFunc
}

func (ctx *countedDiffContext) Err() error {
	if ctx.active {
		ctx.checks++
		if ctx.checks == ctx.cancelAt {
			ctx.cancel()
		}
	}
	return ctx.Context.Err()
}

func TestEditResult_DiffCancellationKeepsPublishedFile(t *testing.T) {
	h := newHarness(t)
	before, after := expensiveDiffFixture()
	writeFixture(t, h.path("f"), before)
	h.read(t, "f")
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &countedDiffContext{Context: base, cancel: cancel, cancelAt: 2}
	restoreHooks(t)
	renameFile = func(from, to string) error {
		err := os.Rename(from, to)
		ctx.active = err == nil
		return err
	}
	args, _ := json.Marshal(map[string]any{"file_path": "f", "old_string": "a", "new_string": "b", "replace_all": true})
	result := h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{SessionID: "session", Journal: nopJournal{}, Calls: []session.ToolCall{{ID: "call", Name: "edit", Arguments: args}}})[0]
	result = persistedFileResult(t, "edit", result)
	if !result.IsError || result.Error == nil || *result.Error != (session.ToolError{Name: "AbortError", Code: "ABORTED"}) || result.Meta != nil || result.Output != "Error: tool call aborted" || ctx.checks != 3 {
		t.Fatalf("diff cancellation = %+v, checks=%d, want ABORTED after two diff checkpoints", result, ctx.checks)
	}
	if readFixture(t, h.path("f")) != after {
		t.Fatal("diff cancellation rolled back the published file")
	}
	if next := h.call(t, "edit", map[string]any{"file_path": "f", "old_string": "keep", "new_string": "kept"}); next.IsError {
		t.Fatalf("observation after diff cancellation = %+v", next)
	}
}

func TestDiffWork_BoundsSearchAndChecksCancellation(t *testing.T) {
	before, after := expensiveDiffFixture()
	a, b := diffLines(before), diffLines(after)
	work := diffWork{}
	if _, complete := work.changedLines(t.Context(), a, b, 0, 0); complete || work.used > 1_048_576 || work.used < 1_048_576-4 {
		t.Fatalf("expensive path: complete=%v, work=%d, want exhaustion within 1,048,576 units", complete, work.used)
	}
	// Cancel from a counted checkpoint while the bidirectional search is active.
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &countedDiffContext{Context: base, active: true, cancel: cancel, cancelAt: 10_000}
	work = diffWork{}
	if _, complete := work.changedLines(ctx, a, b, 0, 0); complete || ctx.checks != 10_000 || work.used >= 1_048_576 {
		t.Fatalf("cancelled search: complete=%v, checkpoints=%d, work=%d", complete, ctx.checks, work.used)
	}
	// The same cap bounds short-line input near the maximum loaded file size;
	// it must stop during indexing, before allocating enormous Myers frontiers.
	before = strings.Repeat("a\n", (10<<20)/2-10)
	after = strings.ReplaceAll(before, "a", "b")
	work = diffWork{}
	if _, complete := work.changedLines(t.Context(), diffLines(before), diffLines(after), 0, 0); complete || work.used > 1_048_576 {
		t.Fatalf("near-limit path: complete=%v, work=%d", complete, work.used)
	}
	meta := editMeta(t.Context(), "f", []byte(before), []byte(after))
	if !meta.Truncated || meta.Diffs == nil || len(meta.Diffs) != 0 {
		t.Fatalf("near-limit metadata = %+v, want empty truncated diff", meta)
	}
}

func TestDiffWork_BudgetBoundaryAndCancelledSubranges(t *testing.T) {
	work := diffWork{}
	if !work.take(t.Context(), 1_048_576) || work.used != 1_048_576 || work.take(t.Context(), 1) || work.used != 1_048_576 {
		t.Fatalf("inclusive work boundary = %d", work.used)
	}
	// Sweep each checkpoint on small paths, so cancellation covers every phase
	// and recursive subrange. Each interrupted computation stops at that check.
	for _, pair := range [][2]string{
		{"same\na\nx\na\nend", "same\nb\nx\nb\nend"},
		{"a\nx\nb\ny\nc", "b\nx\na\ny\nd"},
		{"a\nx\ny", "b\nx\nc\ny"},
		{"a", "b\nx\ny"},
	} {
		a, b := diffLines(pair[0]), diffLines(pair[1])
		for checkpoint := 1; ; checkpoint++ {
			base, cancel := context.WithCancel(t.Context())
			ctx := &countedDiffContext{Context: base, active: true, cancel: cancel, cancelAt: checkpoint}
			work = diffWork{}
			_, complete := work.changedLines(ctx, a, b, 0, 0)
			cancel()
			if complete {
				break
			}
			if ctx.checks != checkpoint {
				t.Fatalf("cancelled checkpoint %d continued to %d", checkpoint, ctx.checks)
			}
		}
	}
	work = diffWork{used: 1_048_576}
	if _, _, complete := work.bisectLines(t.Context(), []string{"a"}, []string{"b"}); complete {
		t.Fatal("allocated frontiers after the work budget was exhausted")
	}
}
