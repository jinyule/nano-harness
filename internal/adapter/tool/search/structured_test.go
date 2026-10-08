package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

// diskCall executes against a real journal and independently decodes its file.
func diskCall(ctx context.Context, t *testing.T, h *harness, name string, arguments map[string]any) session.ToolResult {
	t.Helper()
	manager, err := sessionjsonl.New(sessionjsonl.Config{Root: filepath.Join(t.TempDir(), "sessions"), CompositionID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := manager.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := manager.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "session", Cwd: h.root, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	call := session.ToolCall{ID: "call", Name: name, Arguments: encoded}
	for _, record := range []session.Record{
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordStepStart, Turn: 1, Step: 1},
		{Type: session.RecordAssistantMessage, Turn: 1, Step: 1, Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "model"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "search"}}}},
		{Type: session.RecordToolCall, Turn: 1, Step: 1, Call: &call},
	} {
		if _, err := log.Append(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	result := h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{SessionID: "session", Turn: 1, Step: 1, Journal: log, Calls: []session.ToolCall{call}})[0]
	if _, err := log.Append(t.Context(), session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &result}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	var event session.Event
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Record.Result != nil {
			return *event.Record.Result
		}
	}
	t.Fatal("no result on disk")
	return session.ToolResult{}
}

type runnerFunc func(context.Context, platformProcess.Request) (platformProcess.Result, error)

func (run runnerFunc) Run(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
	if len(request.Args) == 1 && request.Args[0] == "--version" {
		return supported(), nil
	}
	return run(ctx, request)
}

func TestSearch_PersistsFailureClassifications(t *testing.T) {
	for _, test := range []struct {
		name, tool, code, text string
		result                 platformProcess.Result
		err                    error
		cancel                 bool
	}{
		{name: "regex", tool: "grep", code: "SEARCH_INVALID_PATTERN", text: "grep pattern rejected by ripgrep: regex parse error", result: platformProcess.Result{ExitCode: 2, Stderr: platformProcess.Output{Text: "regex parse error"}}},
		{name: "glob", tool: "glob", code: "SEARCH_INVALID_PATTERN", text: "glob pattern rejected by ripgrep: error parsing glob", result: platformProcess.Result{ExitCode: 2, Stderr: platformProcess.Output{Text: "error parsing glob"}}},
		{name: "signal", tool: "grep", code: "SEARCH_FAILED", text: "grep search command was killed by signal SIGSEGV", result: platformProcess.Result{Signal: "SIGSEGV", ExitCode: -1}},
		{name: "exit", tool: "grep", code: "SEARCH_FAILED", text: "grep search failed (exit 2): denied", result: platformProcess.Result{ExitCode: 2, Stderr: platformProcess.Output{Text: "denied"}}},
		{name: "launch", tool: "grep", code: "SEARCH_FAILED", text: "grep could not start its search command (ripgrep launch failed): launch", err: errors.New("launch")},
		{name: "malformed", tool: "grep", code: "SEARCH_FAILED", text: "grep received malformed ripgrep --json output (a line is not JSON)", result: platformProcess.Result{Stdout: platformProcess.Output{Text: "not JSON"}}},
		{name: "overflow", tool: "glob", code: "SEARCH_RAW_OUTPUT_OVERFLOW", text: "glob produced more raw output than the 20000000-byte cap; narrow pattern, path, or include and retry", result: platformProcess.Result{Stdout: platformProcess.Output{Truncated: true}}},
		{name: "timeout", tool: "grep", code: "SEARCH_ABORTED", text: "grep was aborted before completion (tool timeout or caller cancellation)", result: platformProcess.Result{TimedOut: true}},
		{name: "cancel", tool: "grep", code: "SEARCH_ABORTED", text: "grep was aborted before completion (tool timeout or caller cancellation)", err: context.Canceled, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := newHarness(t, runnerFunc(func(context.Context, platformProcess.Request) (platformProcess.Result, error) {
				if test.cancel {
					cancel()
				}
				return test.result, test.err
			}))
			result := diskCall(ctx, t, h, test.tool, map[string]any{"pattern": "x"})
			want := &session.ToolError{Name: "SearchError", Code: test.code}
			if !result.IsError || result.Output != "Error: "+test.text || !reflect.DeepEqual(result.Error, want) || result.Meta != nil {
				t.Fatalf("disk result = %+v; classification = %+v", result, result.Error)
			}
			if test.err != nil && !test.cancel {
				_, _, err := h.provider.run(t.Context(), test.tool, nil)
				var failure appTool.Failure
				if !errors.Is(err, test.err) || !errors.As(fmt.Errorf("wrapped: %w", err), &failure) || failure.ToolError() != *want {
					t.Fatalf("error chain = %v", err)
				}
			}
		})
	}
}

func TestSearch_PersistsRootFailuresAndLeavesSemanticErrorsUnclassified(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t, platformProcess.New())
	if err := syscall.Mkfifo(filepath.Join(h.root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ tool, path string }{{"glob", "missing"}, {"grep", "missing"}, {"grep", "pipe"}, {"glob", ".."}, {"grep", ".."}} {
		result := diskCall(t.Context(), t, h, test.tool, map[string]any{"pattern": "x", "path": test.path})
		if !reflect.DeepEqual(result.Error, &session.ToolError{Name: "SearchError", Code: "SEARCH_FAILED"}) || result.Meta != nil {
			t.Fatalf("%s/%s = %+v", test.tool, test.path, result)
		}
	}
	cause := errors.New("stat failed")
	lstatPath = func(string) (os.FileInfo, error) { return nil, cause }
	result := diskCall(t.Context(), t, h, "grep", map[string]any{"pattern": "x"})
	if !reflect.DeepEqual(result.Error, &session.ToolError{Name: "SearchError", Code: "SEARCH_FAILED"}) || result.Output != "Error: grep search failed: stat failed" {
		t.Fatalf("stat = %+v", result)
	}
	_, err := h.provider.locate(t.Context(), appTool.Invocation{}, "grep", nil, true)
	if !errors.Is(err, cause) {
		t.Fatalf("stat cause lost: %v", err)
	}
	for _, tool := range []string{"glob", "grep"} {
		result := diskCall(t.Context(), t, h, tool, map[string]any{"pattern": ""})
		if !result.IsError || result.Error != nil || result.Meta != nil {
			t.Fatalf("semantic failure classified: %+v", result)
		}
	}
}

func TestSearch_PersistsRealRipgrepMetadataAtResultCaps(t *testing.T) {
	for _, count := range []int{0, 1, 100, 101} {
		t.Run(fmt.Sprintf("glob/%d", count), func(t *testing.T) {
			h := newHarness(t, platformProcess.New())
			paths := []string{}
			for index := range count {
				name := fmt.Sprintf("f%03d.txt", index)
				h.write(t, name, "needle\n")
				h.touch(t, name, time.Duration(index)*time.Minute)
				if index < 100 {
					paths = append(paths, name)
				}
			}
			result := diskCall(t.Context(), t, h, "glob", map[string]any{"pattern": "*.txt"})
			want := &session.ToolMeta{Glob: &session.GlobMeta{Paths: paths, Total: int64(count), Truncated: count > 100}}
			if result.IsError || !reflect.DeepEqual(result.Meta, want) {
				t.Fatalf("glob meta = %+v; want %+v", result.Meta, want)
			}
		})
	}
	for _, count := range []int{0, 1, 250, 251} {
		t.Run(fmt.Sprintf("grep/%d", count), func(t *testing.T) {
			h := newHarness(t, platformProcess.New())
			h.write(t, "a.txt", strings.Repeat("needle\n", count))
			files := []session.GrepFile{}
			if count > 0 {
				matches := []session.GrepMatch{}
				for index := range min(count, 250) {
					matches = append(matches, session.GrepMatch{LineNumber: int64(index + 1), Line: "needle"})
				}
				files = append(files, session.GrepFile{Path: "a.txt", Matches: matches})
			}
			result := diskCall(t.Context(), t, h, "grep", map[string]any{"pattern": "needle", "path": "a.txt"})
			want := &session.ToolMeta{Grep: &session.GrepMeta{Files: files, Total: int64(count), Truncated: count > 250}}
			if result.IsError || !reflect.DeepEqual(result.Meta, want) {
				t.Fatalf("grep meta = %+v; want %+v", result.Meta, want)
			}
		})
	}
}

func matchJSON(path string, line int, text string) string {
	return fmt.Sprintf(`{"type":"match","data":{"path":{"text":%q},"line_number":%d,"lines":{"text":%q}}}`+"\n", path, line, text)
}

func TestSearch_PersistsMetadataThroughSaveAndSpill(t *testing.T) {
	for _, save := range []string{"saved", "failed", "no store"} {
		t.Run(save, func(t *testing.T) {
			store := &directorySpill{dir: t.TempDir()}
			var spill appTool.SpillStore = store
			if save == "failed" {
				store.createErr = errors.New("disk full")
			}
			if save == "no store" {
				spill = nil
			}
			runner := &scriptedRunner{version: supported()}
			h := newHarnessWith(t, runner, "", spill)
			paths := []string{}
			for index := range 101 {
				paths = append(paths, fmt.Sprintf("f%03d", index))
			}
			runner.result.Stdout.Text = strings.Join(paths, "\n")
			result := diskCall(t.Context(), t, h, "glob", map[string]any{"pattern": "*"})
			if !reflect.DeepEqual(result.Meta, &session.ToolMeta{Glob: &session.GlobMeta{Paths: paths[:100], Total: 101, Truncated: true}}) {
				t.Fatalf("glob lost meta: %+v", result)
			}
			footer := "The complete result could not be saved;"
			if save == "saved" {
				footer = "Full sorted result stored at:"
			}
			if !strings.Contains(result.Output, footer) {
				t.Fatalf("glob footer = %q", result.Output)
			}
			runner.result.Stdout.Text = matchJSON("b", 2, "second\r\n") + matchJSON("a", 1, strings.Repeat("界", 670)+"\n") + matchJSON("b", 4, "last\n") + matchJSON("a", 3, "again\n")
			result = diskCall(t.Context(), t, h, "grep", map[string]any{"pattern": "x"})
			want := &session.ToolMeta{Grep: &session.GrepMeta{Total: 4, Files: []session.GrepFile{
				{Path: "b", Matches: []session.GrepMatch{{LineNumber: 2, Line: "second"}, {LineNumber: 4, Line: "last"}}},
				{Path: "a", Matches: []session.GrepMatch{{LineNumber: 1, Line: strings.Repeat("界", 666) + " (line truncated)"}, {LineNumber: 3, Line: "again"}}},
			}}}
			if !reflect.DeepEqual(result.Meta, want) {
				t.Fatalf("grouped meta = %+v", result.Meta)
			}
			runner.result.Stdout.Text = ""
			for index := range 251 {
				runner.result.Stdout.Text += matchJSON("a", index+1, strings.Repeat("x", 2001)+"\n")
			}
			result = diskCall(t.Context(), t, h, "grep", map[string]any{"pattern": "x"})
			if result.IsError || result.Meta == nil || result.Meta.Grep == nil {
				t.Fatalf("grep lost meta: %+v", result)
			}
			meta := result.Meta.Grep
			if meta.Total != 251 || !meta.Truncated || len(meta.Files) != 1 || len(meta.Files[0].Matches) != 31 || meta.Files[0].Matches[30] != (session.GrepMatch{LineNumber: 31, Line: strings.Repeat("x", 2000) + " (line truncated)"}) {
				t.Fatalf("grep prefix = %+v", meta)
			}
			encoded, err := json.Marshal(result.Meta)
			if err != nil || len(encoded) > 65536 {
				t.Fatalf("meta size = %d, %v", len(encoded), err)
			}
			if save == "saved" && !strings.Contains(result.Output, "Full formatted result stored at:") {
				t.Fatalf("expected runtime spill: %q", result.Output[:min(300, len(result.Output))])
			}
			if save == "saved" {
				entries, err := os.ReadDir(store.dir)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range entries {
					if !strings.HasSuffix(entry.Name(), "-grep-results.txt") {
						continue
					}
					found = true
					data, err := os.ReadFile(filepath.Join(store.dir, entry.Name()))
					var expected strings.Builder
					expected.WriteString("Found 251 matches\n\na\n")
					for line := range 251 {
						if line > 0 {
							expected.WriteByte('\n')
						}
						fmt.Fprintf(&expected, "Line %d: %s (line truncated)", line+1, strings.Repeat("x", 2000))
					}
					if err != nil || string(data) != expected.String() {
						t.Fatalf("complete grep artifact = %d bytes, %v", len(data), err)
					}
				}
				if !found {
					t.Fatal("no complete grep artifact")
				}
			}
		})
	}
}

func TestSearch_DropsASingleOversizedMetadataItem(t *testing.T) {
	runner := &scriptedRunner{version: supported(), result: platformProcess.Result{Stdout: platformProcess.Output{Text: strings.Repeat("x", 65536)}}}
	h := newHarness(t, runner)
	result := diskCall(t.Context(), t, h, "glob", map[string]any{"pattern": "*"})
	want := &session.ToolMeta{Glob: &session.GlobMeta{Paths: []string{}, Total: 1, Truncated: true}}
	if result.IsError || !reflect.DeepEqual(result.Meta, want) {
		t.Fatalf("oversized item: error=%+v, meta=%+v", result.Error, result.Meta)
	}
	runner.result.Stdout.Text = matchJSON(strings.Repeat("x", 65536), 1, "needle\n")
	result = diskCall(t.Context(), t, h, "grep", map[string]any{"pattern": "*"})
	want = &session.ToolMeta{Grep: &session.GrepMeta{Files: []session.GrepFile{}, Total: 1, Truncated: true}}
	if result.IsError || !reflect.DeepEqual(result.Meta, want) {
		t.Fatalf("oversized file group: error=%+v, meta=%+v", result.Error, result.Meta)
	}
}

func TestSearch_PersistsSpilledFailure(t *testing.T) {
	for _, save := range []string{"saved", "failed", "no store"} {
		t.Run(save, func(t *testing.T) {
			store := &directorySpill{dir: t.TempDir()}
			var spill appTool.SpillStore = store
			if save == "failed" {
				store.createErr = errors.New("disk full")
			}
			if save == "no store" {
				spill = nil
			}
			diagnostic := "regex parse error: " + strings.Repeat("x", 60000)
			runner := &scriptedRunner{version: supported(), result: platformProcess.Result{ExitCode: 2, Stderr: platformProcess.Output{Text: diagnostic}}}
			h := newHarnessWith(t, runner, "", spill)
			result := diskCall(t.Context(), t, h, "grep", map[string]any{"pattern": "("})
			if !result.IsError || !reflect.DeepEqual(result.Error, &session.ToolError{Name: "SearchError", Code: "SEARCH_INVALID_PATTERN"}) || result.Meta != nil {
				t.Fatalf("spilled error classification = %+v", result.Error)
			}
			want := "Error: grep pattern rejected by ripgrep: " + diagnostic
			if save != "saved" {
				if result.Output != want {
					t.Fatal("failed error spill changed the diagnostic")
				}
				return
			}
			if !strings.Contains(result.Output, "Full formatted result stored at:") {
				t.Fatal("long error did not spill")
			}
			entries, err := os.ReadDir(store.dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("error spill files = %d, %v", len(entries), err)
			}
			data, err := os.ReadFile(filepath.Join(store.dir, entries[0].Name()))
			if err != nil || string(data) != want {
				t.Fatalf("complete error = %d bytes, %v", len(data), err)
			}
		})
	}
}
