package session

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// MaxToolMetaBytes bounds the JSON encoding of one result's metadata.
	MaxToolMetaBytes = 256 << 10
	// MaxSearchMetaBytes bounds glob and grep metadata, upstream's default
	// search presentation budget.
	MaxSearchMetaBytes = 65536
	// maxToolErrorPart bounds a classification name or code.
	maxToolErrorPart = 64
	// maxMetaInteger is the largest count or line number that survives a
	// JSON round trip through an IEEE 754 double.
	maxMetaInteger = 1<<53 - 1
)

// Write operations recorded in WriteMeta.
const (
	WriteCreate = "create"
	WriteUpdate = "update"
)

// ToolError is the persisted classification of a failed tool call: the
// upstream HarnessError name and its stable machine code. Only failures
// with such a classification carry one; the model never sees it.
type ToolError struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// ToolOutcomeUnknown classifies the result that resume writes for a call
// committed without a result: the call may have run.
var ToolOutcomeUnknown = ToolError{Name: "ToolOutcomeUnknownError", Code: "TOOL_OUTCOME_UNKNOWN"}

// ToolMeta is the presentation data of one successful result, recorded for
// replay and never sent to the model. Exactly one member is set; its JSON
// key is the name of the tool that produced it.
type ToolMeta struct {
	Read      *ReadMeta      `json:"read,omitempty"`
	ReadImage *ReadImageMeta `json:"read_image,omitempty"`
	Write     *WriteMeta     `json:"write,omitempty"`
	Edit      *EditMeta      `json:"edit,omitempty"`
	Glob      *GlobMeta      `json:"glob,omitempty"`
	Grep      *GrepMeta      `json:"grep,omitempty"`
	WebSearch *WebSearchMeta `json:"web_search,omitempty"`
	WebFetch  *WebFetchMeta  `json:"web_fetch,omitempty"`
}

// ReadMeta is the returned window of a read. Lines continue from Offset;
// Truncated reports that the metadata budget dropped trailing lines.
type ReadMeta struct {
	Path       string     `json:"path"`
	Offset     int64      `json:"offset"`
	Lines      []ReadLine `json:"lines"`
	TotalLines int64      `json:"total_lines"`
	Truncated  bool       `json:"truncated"`
}

// ReadLine is one returned line with its 1-based file line number.
type ReadLine struct {
	Number int64  `json:"number"`
	Text   string `json:"text"`
}

// ReadImageMeta is the display path of an image whose bytes the result's
// image reference owns.
type ReadImageMeta struct {
	Path string `json:"path"`
}

// WriteMeta is the applied change of a write. A create and an unchanged
// overwrite have no diffs; Truncated reports diffs missing for lack of a
// diff basis or metadata budget.
type WriteMeta struct {
	Operation string     `json:"operation"`
	Diffs     []FileDiff `json:"diffs"`
	Truncated bool       `json:"truncated"`
}

// EditMeta is the applied change of an edit.
type EditMeta struct {
	Diffs     []FileDiff `json:"diffs"`
	Truncated bool       `json:"truncated"`
}

// FileDiff is one applied hunk with its context lines. OldText is nil for
// a pure insertion.
type FileDiff struct {
	Path    string  `json:"path"`
	OldText *string `json:"old_text"`
	NewText string  `json:"new_text"`
}

// GlobMeta is the kept prefix of the matched paths. Total counts every
// match; Truncated reports that paths holds fewer.
type GlobMeta struct {
	Paths     []string `json:"paths"`
	Total     int64    `json:"total"`
	Truncated bool     `json:"truncated"`
}

// GrepMeta is the kept prefix of the matches grouped by file in first-seen
// order. Total counts every match; Truncated reports that files hold fewer.
type GrepMeta struct {
	Files     []GrepFile `json:"files"`
	Total     int64      `json:"total"`
	Truncated bool       `json:"truncated"`
}

// GrepFile is the matches kept for one file.
type GrepFile struct {
	Path    string      `json:"path"`
	Matches []GrepMatch `json:"matches"`
}

// GrepMatch is one matching line preview.
type GrepMatch struct {
	LineNumber int64  `json:"line_number"`
	Line       string `json:"line"`
}

// WebSearchMeta is the merged answer and deduplicated sources of a search.
// Truncated reports a truncated result or sources dropped by the budget.
type WebSearchMeta struct {
	Sources   []WebSource `json:"sources"`
	Answer    string      `json:"answer,omitempty"`
	Truncated bool        `json:"truncated"`
}

// WebSource is one search source.
type WebSource struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
}

// WebFetchMeta is the final URL and status of a fetch. Truncated reports
// that the returned content was cut.
type WebFetchMeta struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Truncated  bool   `json:"truncated"`
}

// Tool returns the name of the tool whose member is set, or "" unless
// exactly one member is set.
func (meta ToolMeta) Tool() string {
	name, count := "", 0
	for _, member := range []struct {
		name string
		set  bool
	}{
		{"read", meta.Read != nil}, {"read_image", meta.ReadImage != nil},
		{"write", meta.Write != nil}, {"edit", meta.Edit != nil},
		{"glob", meta.Glob != nil}, {"grep", meta.Grep != nil},
		{"web_search", meta.WebSearch != nil}, {"web_fetch", meta.WebFetch != nil},
	} {
		if member.set {
			name, count = member.name, count+1
		}
	}
	if count != 1 {
		return ""
	}
	return name
}

// Validate checks that exactly one member is set, its fields are
// consistent, every string is valid UTF-8, and the encoding fits its budget.
func (meta ToolMeta) Validate() error {
	if meta.Tool() == "" {
		return invalid("tool result metadata must set exactly one tool")
	}
	if err := meta.validateMember(); err != nil {
		return err
	}
	valid := true
	meta.mapStrings(func(text string) string {
		valid = valid && utf8.ValidString(text)
		return text
	})
	if !valid {
		return invalid("tool result metadata contains invalid UTF-8")
	}
	if limit := meta.limit(); meta.size() > limit {
		return invalid("tool result metadata exceeds %d bytes", limit)
	}
	return nil
}

func (meta ToolMeta) validateMember() error {
	switch {
	case meta.Read != nil:
		data := meta.Read
		if data.Path == "" || data.Offset < 1 || data.Offset > maxMetaInteger || data.TotalLines < 0 || data.TotalLines > maxMetaInteger || data.Lines == nil {
			return invalid("read metadata fields are invalid")
		}
		for index, line := range data.Lines {
			if line.Number != data.Offset+int64(index) || line.Number > data.TotalLines {
				return invalid("read metadata line %d is out of order or range", index)
			}
		}
	case meta.ReadImage != nil:
		if meta.ReadImage.Path == "" {
			return invalid("read_image metadata path is empty")
		}
	case meta.Write != nil:
		data := meta.Write
		if data.Operation != WriteCreate && data.Operation != WriteUpdate || data.Operation == WriteCreate && len(data.Diffs) != 0 {
			return invalid("write metadata operation is invalid")
		}
		return validateDiffs("write", data.Diffs)
	case meta.Edit != nil:
		return validateDiffs("edit", meta.Edit.Diffs)
	case meta.Glob != nil:
		data := meta.Glob
		if data.Paths == nil || slices.Contains(data.Paths, "") || !validTotal(data.Total, len(data.Paths), data.Truncated) {
			return invalid("glob metadata fields are invalid")
		}
	case meta.Grep != nil:
		data := meta.Grep
		kept := 0
		for _, file := range data.Files {
			if file.Path == "" || len(file.Matches) == 0 {
				return invalid("grep metadata file group is invalid")
			}
			for _, match := range file.Matches {
				if match.LineNumber < 1 || match.LineNumber > maxMetaInteger {
					return invalid("grep metadata line number is invalid")
				}
			}
			kept += len(file.Matches)
		}
		if data.Files == nil || !validTotal(data.Total, kept, data.Truncated) {
			return invalid("grep metadata fields are invalid")
		}
	case meta.WebSearch != nil:
		data := meta.WebSearch
		if data.Sources == nil || slices.ContainsFunc(data.Sources, func(source WebSource) bool { return source.URL == "" }) {
			return invalid("web_search metadata sources are invalid")
		}
	default:
		data := meta.WebFetch
		if data.URL == "" || data.StatusCode < 100 || data.StatusCode > 599 {
			return invalid("web_fetch metadata fields are invalid")
		}
	}
	return nil
}

func validateDiffs(tool string, diffs []FileDiff) error {
	if diffs == nil || slices.ContainsFunc(diffs, func(diff FileDiff) bool { return diff.Path == "" }) {
		return invalid("%s metadata diffs are invalid", tool)
	}
	return nil
}

// validTotal requires total to cover the kept items, and a partial list to
// say so.
func validTotal(total int64, kept int, truncated bool) bool {
	return total >= int64(kept) && total <= maxMetaInteger && (total == int64(kept) || truncated)
}

// Fit returns a detached copy whose strings are valid UTF-8 and whose
// encoding fits the budget. Trailing read lines, paths, grep matches,
// hunks, and search sources are dropped in that order of position, then a
// search answer; the kept items are a prefix and Truncated is set. Fields
// that cannot shrink are left for Validate to judge.
func (meta ToolMeta) Fit() ToolMeta {
	fitted := meta.clone(func(text string) string { return strings.ToValidUTF8(text, "�") })
	limit := fitted.limit()
	fits := func() bool { return fitted.size() <= limit }
	if fits() {
		return fitted
	}
	switch {
	case fitted.Read != nil:
		data := fitted.Read
		data.Truncated = true
		lines := data.Lines
		data.Lines = lines[:keepPrefix(len(lines), func(count int) bool { data.Lines = lines[:count]; return fits() })]
	case fitted.Write != nil:
		data := fitted.Write
		data.Truncated = true
		diffs := data.Diffs
		data.Diffs = diffs[:keepPrefix(len(diffs), func(count int) bool { data.Diffs = diffs[:count]; return fits() })]
	case fitted.Edit != nil:
		data := fitted.Edit
		data.Truncated = true
		diffs := data.Diffs
		data.Diffs = diffs[:keepPrefix(len(diffs), func(count int) bool { data.Diffs = diffs[:count]; return fits() })]
	case fitted.Glob != nil:
		data := fitted.Glob
		data.Truncated = true
		paths := data.Paths
		data.Paths = paths[:keepPrefix(len(paths), func(count int) bool { data.Paths = paths[:count]; return fits() })]
	case fitted.Grep != nil:
		data := fitted.Grep
		data.Truncated = true
		files := data.Files
		total := 0
		for _, file := range files {
			total += len(file.Matches)
		}
		data.Files = grepPrefix(files, keepPrefix(total, func(count int) bool { data.Files = grepPrefix(files, count); return fits() }))
	case fitted.WebSearch != nil:
		data := fitted.WebSearch
		data.Truncated = true
		sources := data.Sources
		keep := func(count int) bool { data.Sources = sources[:count]; return fits() }
		bound := len(sources)
		if bound == 0 || !keep(0) {
			// Without the answer every source may fit again.
			data.Answer = ""
			bound++
		}
		data.Sources = sources[:keepPrefix(bound, keep)]
	}
	return fitted
}

// keepPrefix returns the largest count below n for which fits holds, given
// that fits holds for every count up to some bound and none above it; 0
// when no count fits. The bound excludes n because the complete list did
// not fit, even if setting Truncated alone saved enough bytes.
func keepPrefix(n int, fits func(count int) bool) int {
	return max(sort.Search(n, func(count int) bool { return !fits(count) })-1, 0)
}

// grepPrefix keeps the first count matches in file order and drops groups
// left empty.
func grepPrefix(files []GrepFile, count int) []GrepFile {
	kept := make([]GrepFile, 0, len(files))
	for _, file := range files {
		if count == 0 {
			break
		}
		take := min(count, len(file.Matches))
		kept = append(kept, GrepFile{Path: file.Path, Matches: file.Matches[:take]})
		count -= take
	}
	return kept
}

func (meta ToolMeta) limit() int {
	if meta.Glob != nil || meta.Grep != nil {
		return MaxSearchMetaBytes
	}
	return MaxToolMetaBytes
}

// size is the length of the persisted encoding; DTOs of strings, integers,
// and booleans always encode.
func (meta ToolMeta) size() int {
	encoded, _ := json.Marshal(meta)
	return len(encoded)
}

// clone returns a deep copy with every string passed through text.
func (meta ToolMeta) clone(text func(string) string) ToolMeta {
	var cloned ToolMeta
	if data := meta.Read; data != nil {
		cloned.Read = &ReadMeta{Path: text(data.Path), Offset: data.Offset, Lines: mapSlice(data.Lines, func(line ReadLine) ReadLine {
			return ReadLine{Number: line.Number, Text: text(line.Text)}
		}), TotalLines: data.TotalLines, Truncated: data.Truncated}
	}
	if data := meta.ReadImage; data != nil {
		cloned.ReadImage = &ReadImageMeta{Path: text(data.Path)}
	}
	if data := meta.Write; data != nil {
		cloned.Write = &WriteMeta{Operation: text(data.Operation), Diffs: cloneDiffs(data.Diffs, text), Truncated: data.Truncated}
	}
	if data := meta.Edit; data != nil {
		cloned.Edit = &EditMeta{Diffs: cloneDiffs(data.Diffs, text), Truncated: data.Truncated}
	}
	if data := meta.Glob; data != nil {
		cloned.Glob = &GlobMeta{Paths: mapSlice(data.Paths, text), Total: data.Total, Truncated: data.Truncated}
	}
	if data := meta.Grep; data != nil {
		cloned.Grep = &GrepMeta{Files: mapSlice(data.Files, func(file GrepFile) GrepFile {
			return GrepFile{Path: text(file.Path), Matches: mapSlice(file.Matches, func(match GrepMatch) GrepMatch {
				return GrepMatch{LineNumber: match.LineNumber, Line: text(match.Line)}
			})}
		}), Total: data.Total, Truncated: data.Truncated}
	}
	if data := meta.WebSearch; data != nil {
		cloned.WebSearch = &WebSearchMeta{Sources: mapSlice(data.Sources, func(source WebSource) WebSource {
			return WebSource{URL: text(source.URL), Title: text(source.Title), Snippet: text(source.Snippet), PublishedAt: text(source.PublishedAt)}
		}), Answer: text(data.Answer), Truncated: data.Truncated}
	}
	if data := meta.WebFetch; data != nil {
		cloned.WebFetch = &WebFetchMeta{URL: text(data.URL), StatusCode: data.StatusCode, Truncated: data.Truncated}
	}
	return cloned
}

// mapStrings visits every string of the metadata.
func (meta ToolMeta) mapStrings(text func(string) string) { meta.clone(text) }

func cloneDiffs(diffs []FileDiff, text func(string) string) []FileDiff {
	return mapSlice(diffs, func(diff FileDiff) FileDiff {
		cloned := FileDiff{Path: text(diff.Path), NewText: text(diff.NewText)}
		if diff.OldText != nil {
			old := text(*diff.OldText)
			cloned.OldText = &old
		}
		return cloned
	})
}

// mapSlice preserves nil, so a missing required list stays detectable.
func mapSlice[T any](values []T, convert func(T) T) []T {
	if values == nil {
		return nil
	}
	mapped := make([]T, len(values))
	for index, value := range values {
		mapped[index] = convert(value)
	}
	return mapped
}

// Validate requires the name and code to be ASCII identifiers of at most
// 64 bytes that start with a letter.
func (failure ToolError) Validate() error {
	if !validToolErrorPart(failure.Name) || !validToolErrorPart(failure.Code) {
		return invalid("tool error classification is invalid")
	}
	return nil
}

// validToolErrorPart accepts an ASCII identifier that starts with a letter.
func validToolErrorPart(part string) bool {
	if part == "" || len(part) > maxToolErrorPart {
		return false
	}
	for index, char := range part {
		letter := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
		if !letter && (index == 0 || char != '_' && (char < '0' || char > '9')) {
			return false
		}
	}
	return true
}
