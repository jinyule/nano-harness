package file

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func diffBasis(raw []byte) string {
	return strings.ReplaceAll(string(bytes.TrimPrefix(raw, utf8BOM)), "\r\n", "\n")
}

func writeMeta(path string, exists bool, before, after []byte) session.WriteMeta {
	meta := session.WriteMeta{Operation: "create", Diffs: []session.FileDiff{}}
	if !exists {
		return meta
	}
	meta.Operation = "update"
	if before == nil || len(after) >= maxEditBytes || bytes.IndexByte(before, 0) >= 0 || !utf8.Valid(before) || bytes.IndexByte(after, 0) >= 0 || !utf8.Valid(after) {
		meta.Truncated = true
		return meta
	}
	old, next := diffBasis(before), diffBasis(after)
	if old == next {
		return meta
	}
	a, b := diffLines(old), diffLines(next)
	prefix := 0
	for prefix < min(len(a), len(b)) && a[prefix] == b[prefix] {
		prefix++
	}
	endA, endB := len(a), len(b)
	for endA > prefix && endB > prefix && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	meta.Diffs, meta.Truncated = contextualDiffs(path, a, b, []lineChange{{prefix, endA, prefix, endB}}, true)
	return meta
}

// lineChange names half-open ranges of changed lines on the two LF bases.
type lineChange struct{ startA, endA, startB, endB int }

func editMeta(path string, before, after []byte) session.EditMeta {
	a, b := diffLines(diffBasis(before)), diffLines(diffBasis(after))
	diffs, truncated := contextualDiffs(path, a, b, changedLines(a, b, 0, 0), false)
	return session.EditMeta{Diffs: diffs, Truncated: truncated}
}

// changedLines finds actual line changes on the two file bases, independently
// of the edit's matching block. Common edges and disjoint replacements avoid
// diff work; remaining ranges use a shortest edit path with linear memory.
func changedLines(a, b []string, startA, startB int) []lineChange {
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b, startA, startB = a[1:], b[1:], startA+1, startB+1
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	shared := make(map[string]bool, len(a))
	for _, line := range a {
		shared[line] = true
	}
	for _, line := range b {
		if shared[line] {
			x, y := bisectLines(a, b)
			left := changedLines(a[:x], b[:y], startA, startB)
			return append(left, changedLines(a[x:], b[y:], startA+x, startB+y)...)
		}
	}
	return []lineChange{{startA, startA + len(a), startB, startB + len(b)}}
}

// bisectLines meets forward and reverse Myers frontiers at a shortest-path
// split. Callers strip equal edges and require a shared interior line, so the
// split makes progress. Odd path lengths meet after a forward step; even ones
// meet after a reverse step. Only the current two frontiers are retained.
func bisectLines(a, b []string) (int, int) {
	n, m := len(a), len(b)
	offset := (n+m+1)/2 + 1
	forward, reverse := make([]int, 2*offset+1), make([]int, 2*offset+1)
	for i := range forward {
		forward[i], reverse[i] = -1, -1
	}
	forward[offset+1], reverse[offset+1] = 0, 0
	delta := n - m
	for distance := 0; ; distance++ {
		for side, frontier := range [][]int{forward, reverse} {
			for diagonal := -distance; diagonal <= distance; diagonal += 2 {
				index := offset + diagonal
				x := frontier[index-1] + 1
				if diagonal == -distance || diagonal != distance && frontier[index-1] < frontier[index+1] {
					x = frontier[index+1]
				}
				y := x - diagonal
				for x < n && y < m {
					i, j := x, y
					if side == 1 {
						i, j = n-x-1, m-y-1
					}
					if a[i] != b[j] {
						break
					}
					x, y = x+1, y+1
				}
				frontier[index] = x
				other := delta - diagonal
				if side == 0 && delta%2 != 0 && other >= 1-distance && other <= distance-1 && x+reverse[offset+other] >= n {
					return x, y
				}
				if side == 1 && delta%2 == 0 && other >= -distance && other <= distance && forward[offset+other]+x >= n {
					return forward[offset+other], forward[offset+other] - other
				}
			}
		}
	}
}

// diffLines retains terminators for comparison, so a changed final newline
// produces a hunk even though patch-only newline markers are not persisted.
func diffLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// contextualDiffs merges overlapping three-line contexts. It counts escaped
// JSON bytes before joining a hunk, so an oversized fragment is never copied.
func contextualDiffs(path string, a, b []string, changes []lineChange, write bool) ([]session.FileDiff, bool) {
	var hunks []lineChange
	for _, change := range changes {
		change = lineChange{max(0, change.startA-3), min(len(a), change.endA+3), max(0, change.startB-3), min(len(b), change.endB+3)}
		if len(hunks) > 0 && change.startA <= hunks[len(hunks)-1].endA && change.startB <= hunks[len(hunks)-1].endB {
			last := &hunks[len(hunks)-1]
			last.endA, last.endB = max(last.endA, change.endA), max(last.endB, change.endB)
		} else {
			hunks = append(hunks, change)
		}
	}
	diffs, truncated := []session.FileDiff{}, false
	for _, hunk := range hunks {
		old, next := a[hunk.startA:hunk.endA], b[hunk.startB:hunk.endB]
		prototype := session.FileDiff{Path: path}
		if len(old) > 0 {
			prototype.OldText = new("")
		}
		meta := session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{prototype}}}
		if write {
			meta = session.ToolMeta{Write: &session.WriteMeta{Operation: "update", Diffs: []session.FileDiff{prototype}}}
		}
		empty, _ := json.Marshal(meta)
		if len(empty)+fragmentBytes(old)+fragmentBytes(next) > session.MaxToolMetaBytes {
			truncated = true
			continue
		}
		diff := session.FileDiff{Path: path, NewText: fragment(next)}
		if len(old) > 0 {
			diff.OldText = new(fragment(old))
		}
		diffs = append(diffs, diff)
	}
	return diffs, truncated
}

func fragment(lines []string) string {
	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(strings.TrimSuffix(line, "\n"))
	}
	return out.String()
}

func fragmentBytes(lines []string) int {
	size := max(0, len(lines)-1) * 2 // each separating LF encodes as two bytes
	for _, line := range lines {
		for _, char := range strings.TrimSuffix(line, "\n") {
			switch char {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				size += 2
			case '<', '>', '&', '\u2028', '\u2029':
				size += 6
			default:
				if char < 0x20 {
					size += 6
				} else {
					size += utf8.RuneLen(char)
				}
			}
		}
	}
	return size
}
