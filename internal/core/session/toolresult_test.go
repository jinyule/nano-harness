package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validMetas() map[string]ToolMeta {
	old := "a"
	return map[string]ToolMeta{
		"read":       {Read: &ReadMeta{Path: "a.go", Offset: 2, Lines: []ReadLine{{Number: 2, Text: "b"}, {Number: 3, Text: "c"}}, TotalLines: 3}},
		"read_image": {ReadImage: &ReadImageMeta{Path: "red.png"}},
		"write":      {Write: &WriteMeta{Operation: WriteUpdate, Diffs: []FileDiff{{Path: "a.go", OldText: &old, NewText: "b"}}}},
		"edit":       {Edit: &EditMeta{Diffs: []FileDiff{{Path: "a.go", NewText: "inserted"}}}},
		"glob":       {Glob: &GlobMeta{Paths: []string{"a.go"}, Total: 2, Truncated: true}},
		"grep":       {Grep: &GrepMeta{Files: []GrepFile{{Path: "a.go", Matches: []GrepMatch{{LineNumber: 1, Line: "x"}}}}, Total: 1}},
		"web_search": {WebSearch: &WebSearchMeta{Sources: []WebSource{{URL: "https://example.com", Title: "t"}}, Answer: "a"}},
		"web_fetch":  {WebFetch: &WebFetchMeta{URL: "https://example.com", StatusCode: 404}},
	}
}

func TestToolMeta_ToolNamesTheOnlyMember(t *testing.T) {
	for name, meta := range validMetas() {
		if got := meta.Tool(); got != name {
			t.Errorf("%s meta names %q", name, got)
		}
	}
	both := validMetas()["read"]
	both.Glob = validMetas()["glob"].Glob
	if (ToolMeta{}).Tool() != "" || both.Tool() != "" {
		t.Fatal("zero or two members named a tool")
	}
}

func TestToolMeta_ValidateAcceptsEachTool(t *testing.T) {
	metas := validMetas()
	metas["empty read"] = ToolMeta{Read: &ReadMeta{Path: "empty", Offset: 1, Lines: []ReadLine{}}}
	metas["create"] = ToolMeta{Write: &WriteMeta{Operation: WriteCreate, Diffs: []FileDiff{}}}
	metas["complete glob"] = ToolMeta{Glob: &GlobMeta{Paths: []string{}}}
	for name, meta := range metas {
		if err := meta.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestToolMeta_ValidateRejectsInconsistentFields(t *testing.T) {
	read := func(change func(*ReadMeta)) ToolMeta {
		meta := validMetas()["read"]
		change(meta.Read)
		return meta
	}
	glob := func(change func(*GlobMeta)) ToolMeta {
		meta := validMetas()["glob"]
		change(meta.Glob)
		return meta
	}
	grep := func(change func(*GrepMeta)) ToolMeta {
		meta := validMetas()["grep"]
		change(meta.Grep)
		return meta
	}
	tooLarge := strings.Repeat("x", MaxToolMetaBytes)
	for name, meta := range map[string]ToolMeta{
		"no member":              {},
		"read path":              read(func(data *ReadMeta) { data.Path = "" }),
		"read offset":            read(func(data *ReadMeta) { data.Offset = 0 }),
		"read unsafe offset":     read(func(data *ReadMeta) { data.Offset = maxMetaInteger + 1 }),
		"read negative total":    read(func(data *ReadMeta) { data.TotalLines = -1 }),
		"read unsafe total":      read(func(data *ReadMeta) { data.TotalLines = maxMetaInteger + 1 }),
		"read null lines":        read(func(data *ReadMeta) { data.Lines = nil }),
		"read line order":        read(func(data *ReadMeta) { data.Lines[1].Number = 5 }),
		"read line past total":   read(func(data *ReadMeta) { data.TotalLines = 2 }),
		"read_image path":        {ReadImage: &ReadImageMeta{}},
		"write operation":        {Write: &WriteMeta{Operation: "replace", Diffs: []FileDiff{}}},
		"create with diffs":      {Write: &WriteMeta{Operation: WriteCreate, Diffs: []FileDiff{{Path: "a", NewText: "b"}}}},
		"write null diffs":       {Write: &WriteMeta{Operation: WriteUpdate}},
		"edit diff path":         {Edit: &EditMeta{Diffs: []FileDiff{{NewText: "b"}}}},
		"glob null paths":        glob(func(data *GlobMeta) { data.Paths = nil }),
		"glob empty path":        glob(func(data *GlobMeta) { data.Paths = []string{""} }),
		"glob total below kept":  glob(func(data *GlobMeta) { data.Total = 0 }),
		"glob silent partial":    glob(func(data *GlobMeta) { data.Truncated = false }),
		"glob unsafe total":      glob(func(data *GlobMeta) { data.Total = maxMetaInteger + 1 }),
		"grep null files":        grep(func(data *GrepMeta) { data.Files = nil }),
		"grep file path":         grep(func(data *GrepMeta) { data.Files[0].Path = "" }),
		"grep empty group":       grep(func(data *GrepMeta) { data.Files[0].Matches = nil }),
		"grep line number":       grep(func(data *GrepMeta) { data.Files[0].Matches[0].LineNumber = 0 }),
		"grep unsafe line":       grep(func(data *GrepMeta) { data.Files[0].Matches[0].LineNumber = maxMetaInteger + 1 }),
		"grep silent partial":    grep(func(data *GrepMeta) { data.Total = 2 }),
		"web_search null":        {WebSearch: &WebSearchMeta{}},
		"web_search url":         {WebSearch: &WebSearchMeta{Sources: []WebSource{{Title: "t"}}}},
		"web_fetch url":          {WebFetch: &WebFetchMeta{StatusCode: 200}},
		"web_fetch low status":   {WebFetch: &WebFetchMeta{URL: "u", StatusCode: 99}},
		"web_fetch high status":  {WebFetch: &WebFetchMeta{URL: "u", StatusCode: 600}},
		"invalid UTF-8":          {ReadImage: &ReadImageMeta{Path: "\xff"}},
		"over the general limit": {ReadImage: &ReadImageMeta{Path: tooLarge}},
		"over the search limit":  {Glob: &GlobMeta{Paths: []string{tooLarge[:MaxSearchMetaBytes]}, Total: 1}},
	} {
		if err := meta.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

// globAtSize is a single-path glob meta whose encoding is exactly size bytes.
func globAtSize(t *testing.T, size int) ToolMeta {
	t.Helper()
	meta := ToolMeta{Glob: &GlobMeta{Paths: []string{""}, Total: 1}}
	meta.Glob.Paths[0] = strings.Repeat("p", size-meta.size())
	if meta.size() != size {
		t.Fatalf("size=%d, want %d", meta.size(), size)
	}
	return meta
}

func TestToolMeta_FitKeepsTheEncodingWithinTheBudget(t *testing.T) {
	exact := globAtSize(t, MaxSearchMetaBytes)
	if fitted := exact.Fit(); fitted.Glob.Truncated || len(fitted.Glob.Paths) != 1 || fitted.Validate() != nil {
		t.Fatalf("meta at the limit was cut: %+v", fitted.Glob.Truncated)
	}
	over := globAtSize(t, MaxSearchMetaBytes+1)
	if fitted := over.Fit(); !fitted.Glob.Truncated || len(fitted.Glob.Paths) != 0 || fitted.Glob.Paths == nil || fitted.Glob.Total != 1 || fitted.Validate() != nil {
		t.Fatalf("meta one byte over the limit = %+v", fitted.Glob)
	}

	line := strings.Repeat("l", 1000)
	lines := make([]ReadLine, 300)
	for index := range lines {
		lines[index] = ReadLine{Number: int64(index + 1), Text: line}
	}
	paths := make([]string, 1000)
	for index := range paths {
		paths[index] = strings.Repeat("g", 100)
	}
	diffs := make([]FileDiff, 300)
	for index := range diffs {
		diffs[index] = FileDiff{Path: "a", NewText: line}
	}
	matches := make([]GrepMatch, 100)
	for index := range matches {
		matches[index] = GrepMatch{LineNumber: int64(index + 1), Line: line}
	}
	sources := make([]WebSource, 300)
	for index := range sources {
		sources[index] = WebSource{URL: "https://example.com", Snippet: line}
	}
	for name, test := range map[string]struct {
		meta ToolMeta
		kept func(ToolMeta) int
		// grow re-adds the first dropped item to prove the prefix is maximal.
		grow func(ToolMeta, ToolMeta) ToolMeta
	}{
		"read": {
			meta: ToolMeta{Read: &ReadMeta{Path: "a", Offset: 1, Lines: lines, TotalLines: 300}},
			kept: func(meta ToolMeta) int { return len(meta.Read.Lines) },
			grow: func(fitted, original ToolMeta) ToolMeta {
				fitted.Read.Lines = original.Read.Lines[:len(fitted.Read.Lines)+1]
				return fitted
			},
		},
		"glob": {
			meta: ToolMeta{Glob: &GlobMeta{Paths: paths, Total: 1000}},
			kept: func(meta ToolMeta) int { return len(meta.Glob.Paths) },
			grow: func(fitted, original ToolMeta) ToolMeta {
				fitted.Glob.Paths = original.Glob.Paths[:len(fitted.Glob.Paths)+1]
				return fitted
			},
		},
		"write": {
			meta: ToolMeta{Write: &WriteMeta{Operation: WriteUpdate, Diffs: diffs}},
			kept: func(meta ToolMeta) int { return len(meta.Write.Diffs) },
			grow: func(fitted, original ToolMeta) ToolMeta {
				fitted.Write.Diffs = original.Write.Diffs[:len(fitted.Write.Diffs)+1]
				return fitted
			},
		},
		"edit": {
			meta: ToolMeta{Edit: &EditMeta{Diffs: diffs}},
			kept: func(meta ToolMeta) int { return len(meta.Edit.Diffs) },
			grow: func(fitted, original ToolMeta) ToolMeta {
				fitted.Edit.Diffs = original.Edit.Diffs[:len(fitted.Edit.Diffs)+1]
				return fitted
			},
		},
		"web_search": {
			meta: ToolMeta{WebSearch: &WebSearchMeta{Sources: sources, Answer: "answer"}},
			kept: func(meta ToolMeta) int { return len(meta.WebSearch.Sources) },
			grow: func(fitted, original ToolMeta) ToolMeta {
				fitted.WebSearch.Sources = original.WebSearch.Sources[:len(fitted.WebSearch.Sources)+1]
				return fitted
			},
		},
		"grep": {
			meta: ToolMeta{Grep: &GrepMeta{Files: []GrepFile{{Path: "a", Matches: matches[:1]}, {Path: "b", Matches: matches}}, Total: 101}},
			kept: func(meta ToolMeta) int {
				count := 0
				for _, file := range meta.Grep.Files {
					count += len(file.Matches)
				}
				return count
			},
			grow: func(fitted, original ToolMeta) ToolMeta {
				count := 0
				for _, file := range fitted.Grep.Files {
					count += len(file.Matches)
				}
				fitted.Grep.Files = grepPrefix(original.Grep.Files, count+1)
				return fitted
			},
		},
	} {
		fitted := test.meta.Fit()
		kept := test.kept(fitted)
		if err := fitted.Validate(); err != nil || kept == 0 || kept == test.kept(test.meta) {
			t.Errorf("%s: kept %d of %d, err=%v", name, kept, test.kept(test.meta), err)
			continue
		}
		if grown := test.grow(fitted, test.meta); grown.size() <= grown.limit() {
			t.Errorf("%s: kept prefix %d is not maximal", name, kept)
		}
	}
}

func TestToolMeta_FitDropsAnswerAndEmptyGroups(t *testing.T) {
	huge := strings.Repeat("a", MaxToolMetaBytes)
	search := ToolMeta{WebSearch: &WebSearchMeta{Sources: []WebSource{{URL: "https://a"}, {URL: "https://b"}}, Answer: huge}}
	fitted := search.Fit()
	if data := fitted.WebSearch; data.Answer != "" || len(data.Sources) != 2 || !data.Truncated || fitted.Validate() != nil {
		t.Fatalf("oversized answer = %+v", data.Sources)
	}
	line := strings.Repeat("m", MaxSearchMetaBytes)
	grep := ToolMeta{Grep: &GrepMeta{Files: []GrepFile{{Path: "a", Matches: []GrepMatch{{LineNumber: 1, Line: "x"}}}, {Path: "b", Matches: []GrepMatch{{LineNumber: 1, Line: line}}}}, Total: 2}}
	fitted = grep.Fit()
	if data := fitted.Grep; len(data.Files) != 1 || data.Files[0].Path != "a" || data.Total != 2 || !data.Truncated || fitted.Validate() != nil {
		t.Fatalf("emptied group kept: %+v", data)
	}
	image := ToolMeta{ReadImage: &ReadImageMeta{Path: huge}}
	if fitted := image.Fit(); fitted.ReadImage.Path != huge || fitted.Validate() == nil {
		t.Fatal("an irreducible field was changed or accepted over the limit")
	}
}

func TestToolMeta_FitDetachesAndRepairsUTF8(t *testing.T) {
	meta := ToolMeta{Read: &ReadMeta{Path: "a\xff", Offset: 1, Lines: []ReadLine{{Number: 1, Text: "b"}}, TotalLines: 1}}
	fitted := meta.Fit()
	meta.Read.Lines[0].Text = "changed"
	if fitted.Read.Path != "a�" || fitted.Read.Lines[0].Text != "b" || fitted.Validate() != nil {
		t.Fatalf("fitted = %+v", fitted.Read)
	}
	if missing := (ToolMeta{Glob: &GlobMeta{}}).Fit(); missing.Glob.Paths != nil || missing.Validate() == nil {
		t.Fatal("fitting turned a missing list into an accepted empty one")
	}
	for _, original := range validMetas() {
		if fitted := original.Fit(); fitted.size() != original.size() {
			t.Errorf("%s changed within the budget", original.Tool())
		}
	}
}

func TestToolError_ValidateRequiresIdentifiers(t *testing.T) {
	for _, valid := range []ToolError{ToolOutcomeUnknown, {Name: "FsError", Code: "FS_NOT_FOUND"}, {Name: "a", Code: strings.Repeat("A", 64)}} {
		if err := valid.Validate(); err != nil {
			t.Errorf("%+v: %v", valid, err)
		}
	}
	for _, part := range []string{"", strings.Repeat("A", 65), "1CODE", "_CODE", "BAD-CODE", "CODÉ", "CODE "} {
		for _, failure := range []ToolError{{Name: part, Code: "CODE"}, {Name: "Name", Code: part}} {
			if err := failure.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("%+v accepted", failure)
			}
		}
	}
}

func TestRecord_ResultErrorAndMetaFollowTheOutcome(t *testing.T) {
	result := func(change func(*ToolResult)) Record {
		data := &ToolResult{CallID: "call", Output: "ok"}
		change(data)
		return Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: data}
	}
	image := Image{ID: "sha256:" + strings.Repeat("a", 64), Name: "red.png", MediaType: "image/png", Bytes: 1, Width: 1, Height: 1}
	accepted := []Record{
		result(func(data *ToolResult) {
			data.IsError, data.Error = true, &ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}
		}),
		result(func(data *ToolResult) { data.IsError = true }),
		result(func(data *ToolResult) {
			meta := validMetas()["read_image"]
			data.Image, data.Meta = &image, &meta
		}),
	}
	for index, record := range accepted {
		if err := record.Validate(); err != nil {
			t.Errorf("accepted %d: %v", index, err)
		}
	}
	readMeta := validMetas()["read"]
	invalidMeta := ToolMeta{}
	for name, record := range map[string]Record{
		"classified success":  result(func(data *ToolResult) { data.Error = &ToolError{Name: "FsError", Code: "FS_NOT_FOUND"} }),
		"invalid code":        result(func(data *ToolResult) { data.IsError, data.Error = true, &ToolError{Name: "FsError"} }),
		"error with metadata": result(func(data *ToolResult) { data.IsError, data.Meta = true, &readMeta }),
		"invalid metadata":    result(func(data *ToolResult) { data.Meta = &invalidMeta }),
	} {
		if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func TestCloneEvent_DetachesErrorAndMeta(t *testing.T) {
	for name, meta := range validMetas() {
		failure := ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}
		event := Event{Sequence: 1, Record: Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call", Error: &failure, Meta: &meta}}}
		want, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		cloned := CloneEvent(event)
		failure.Code = "CHANGED"
		mutateMeta(&meta)
		got, err := json.Marshal(cloned)
		if err != nil || string(got) != string(want) {
			t.Errorf("%s clone follows the original:\n%s\n%s", name, got, want)
		}
	}
}

// mutateMeta writes through every slice and pointer the metadata holds.
func mutateMeta(meta *ToolMeta) {
	switch {
	case meta.Read != nil:
		meta.Read.Lines[0].Text = "changed"
	case meta.ReadImage != nil:
		meta.ReadImage.Path = "changed"
	case meta.Write != nil:
		*meta.Write.Diffs[0].OldText = "changed"
	case meta.Edit != nil:
		meta.Edit.Diffs[0].NewText = "changed"
	case meta.Glob != nil:
		meta.Glob.Paths[0] = "changed"
	case meta.Grep != nil:
		meta.Grep.Files[0].Matches[0].Line = "changed"
	case meta.WebSearch != nil:
		meta.WebSearch.Sources[0].URL = "changed"
	default:
		meta.WebFetch.URL = "changed"
	}
}
