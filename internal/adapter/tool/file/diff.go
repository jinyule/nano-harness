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

func editMeta(path string, before, after []byte, needle, replacement string) session.EditMeta {
	old, next := diffBasis(before), diffBasis(after)
	if old == next {
		return session.EditMeta{Diffs: []session.FileDiff{}}
	}
	needle = strings.ReplaceAll(needle, "\r\n", "\n")
	replacement = strings.ReplaceAll(replacement, "\r\n", "\n")
	var changes []lineChange
	position, lineA, lineB, columnB := 0, 0, 0, 0
	for {
		index := strings.Index(old[position:], needle)
		if index < 0 {
			break
		}
		start := position + index
		gap := old[position:start]
		unchanged := strings.Count(gap, "\n")
		lineA += unchanged
		lineB += unchanged
		columnB = advanceColumn(columnB, gap)
		endA := lineA + strings.Count(needle, "\n")
		endB := lineB + strings.Count(replacement, "\n")
		columnB = advanceColumn(columnB, replacement)
		tailB := 0
		if columnB > 0 {
			tailB = 1
		}
		changes = append(changes, lineChange{lineA, endA + lineTail(needle), lineB, endB + tailB})
		position, lineA, lineB = start+len(needle), endA, endB
	}
	diffs, truncated := contextualDiffs(path, diffLines(old), diffLines(next), changes, false)
	return session.EditMeta{Diffs: diffs, Truncated: truncated}
}

func advanceColumn(column int, text string) int {
	if index := strings.LastIndexByte(text, '\n'); index >= 0 {
		return len(text) - index - 1
	}
	return column + len(text)
}

func lineTail(text string) int {
	if strings.HasSuffix(text, "\n") {
		return 0
	}
	return 1
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
