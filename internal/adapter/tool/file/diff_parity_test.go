package file

import (
	"fmt"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// TestBisectLines_RefusesFrontiersTheBudgetCannotInitialize proves the
// frontier guard: when the remaining work cannot even initialize both
// frontiers, the search gives up without allocating them.
func TestBisectLines_RefusesFrontiersTheBudgetCannotInitialize(t *testing.T) {
	a, b := make([]string, 100_000), make([]string, 100_000)
	a[0], a[len(a)-1], b[0], b[len(b)-1] = "a", "c", "b", "d"
	frontier := 2*((len(a)+len(b)+1)/2+1) + 1
	for _, used := range []int{maxDiffWork, maxDiffWork - frontier + 1} {
		t.Run(fmt.Sprint(maxDiffWork-used, " units left"), func(t *testing.T) {
			complete := true
			allocations := testing.AllocsPerRun(1, func() {
				work := diffWork{used: used}
				_, _, complete = work.bisectLines(t.Context(), a, b)
			})
			if complete || allocations != 0 {
				t.Fatalf("complete=%v after %.0f allocations, want a refusal without allocating", complete, allocations)
			}
		})
	}
}

// TestEditMeta_MergesHunksAtJsdiffBoundary freezes jsdiff 9's merge rule for
// three-line contexts: changes six equal lines apart share one hunk, seven
// apart split. Expected hunks come from structuredPatch(..., {context: 3}).
func TestEditMeta_MergesHunksAtJsdiffBoundary(t *testing.T) {
	for _, test := range []struct {
		gap  int
		want [][2]string
	}{
		{5, [][2]string{{"old-A\ncommon-0\ncommon-1\ncommon-2\ncommon-3\ncommon-4\nold-B", "new-A\ncommon-0\ncommon-1\ncommon-2\ncommon-3\ncommon-4\nnew-B"}}},
		{6, [][2]string{{"old-A\ncommon-0\ncommon-1\ncommon-2\ncommon-3\ncommon-4\ncommon-5\nold-B", "new-A\ncommon-0\ncommon-1\ncommon-2\ncommon-3\ncommon-4\ncommon-5\nnew-B"}}},
		{7, [][2]string{
			{"old-A\ncommon-0\ncommon-1\ncommon-2", "new-A\ncommon-0\ncommon-1\ncommon-2"},
			{"common-4\ncommon-5\ncommon-6\nold-B", "common-4\ncommon-5\ncommon-6\nnew-B"},
		}},
	} {
		t.Run(fmt.Sprint(test.gap, " lines apart"), func(t *testing.T) {
			before, after := "old-A\n", "new-A\n"
			for index := range test.gap {
				line := fmt.Sprintf("common-%d\n", index)
				before, after = before+line, after+line
			}
			before, after = before+"old-B\n", after+"new-B\n"
			assertHunks(t, editMeta(t.Context(), "f", []byte(before), []byte(after)).Diffs, test.want)
		})
	}
}

// TestEditMeta_RepeatedLinesKeepAShortestPath fixes the current choice among
// equally short paths for inputs with repeated lines and proves it is
// shortest. jsdiff may pick another path of the same length; the first case
// is the reviewed counterexample, where jsdiff's hunk spans all six lines.
func TestEditMeta_RepeatedLinesKeepAShortestPath(t *testing.T) {
	for _, test := range []struct {
		before, after string
		want          [][2]string
	}{
		{"\nb\nb\nb\n\na\n", "\n}\nb\nb\n\na\n", [][2]string{{"\nb\nb\nb\n", "\n}\nb\nb\n"}}},
		{"}\n\n}\nx\n}\n\n}\n", "}\n\n}\n\n}\ny\n}\n", [][2]string{{"}\n\n}\nx\n}\n\n}", "}\n\n}\n\n}\ny\n}"}}},
		{"a\nb\na\nb\na\n", "b\na\nb\na\nb\n", [][2]string{{"a\nb\na\nb\na", "b\na\nb\na\nb"}}},
	} {
		t.Run(fmt.Sprintf("%q", test.before), func(t *testing.T) {
			assertHunks(t, editMeta(t.Context(), "f", []byte(test.before), []byte(test.after)).Diffs, test.want)
			a, b := diffLines(test.before), diffLines(test.after)
			work := diffWork{}
			changes, complete := work.changedLines(t.Context(), a, b, 0, 0)
			cost := 0
			for _, change := range changes {
				cost += change.endA - change.startA + change.endB - change.startB
			}
			if want := lcsDistance(a, b); !complete || cost != want {
				t.Fatalf("path cost %d (complete=%v), want shortest %d: %v", cost, complete, want, changes)
			}
		})
	}
}

func assertHunks(t *testing.T, diffs []session.FileDiff, want [][2]string) {
	t.Helper()
	if len(diffs) != len(want) {
		t.Fatalf("%d hunks, want %d: %+v", len(diffs), len(want), diffs)
	}
	for index, diff := range diffs {
		if diff.OldText == nil || *diff.OldText != want[index][0] || diff.NewText != want[index][1] {
			t.Fatalf("hunk %d = %q -> %q, want %q -> %q", index, deref(diff.OldText), diff.NewText, want[index][0], want[index][1])
		}
	}
}

func deref(text *string) string {
	if text == nil {
		return "<nil>"
	}
	return *text
}

// lcsDistance is the independent shortest line-edit distance.
func lcsDistance(a, b []string) int {
	lcs := make([][]int, len(a)+1)
	for index := range lcs {
		lcs[index] = make([]int, len(b)+1)
	}
	for i := range a {
		for j := range b {
			if a[i] == b[j] {
				lcs[i+1][j+1] = lcs[i][j] + 1
			} else {
				lcs[i+1][j+1] = max(lcs[i][j+1], lcs[i+1][j])
			}
		}
	}
	return len(a) + len(b) - 2*lcs[len(a)][len(b)]
}
