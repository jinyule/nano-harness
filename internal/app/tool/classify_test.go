package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// classifiedError is a domain failure as producers declare it.
type classifiedError struct {
	message        string
	classification session.ToolError
}

func (err *classifiedError) Error() string                { return err.message }
func (err *classifiedError) ToolError() session.ToolError { return err.classification }

var notFound = session.ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}

func runOne(ctx context.Context, t *testing.T, runtime *Runtime, name, arguments string) session.ToolResult {
	t.Helper()
	return runtime.ExecuteBatch(ctx, BatchRequest{SessionID: "s", Turn: 1, Step: 1, Journal: fakeJournal{}, Calls: []session.ToolCall{{ID: name, Name: name, Arguments: json.RawMessage(arguments)}}})[0]
}

func classification(result session.ToolResult) string {
	if result.Error == nil {
		return ""
	}
	return result.Error.Name + "/" + result.Error.Code
}

func TestRuntime_ClassifiesItsOwnFailuresOnly(t *testing.T) {
	approver := &fakeApprover{outcome: session.ApprovalRejected}
	runtime, scope := startRuntime(t, approver)
	for _, candidate := range []*Tool{
		Define(Spec[valueArguments]{Name: "typed", Description: "test", Parameters: Parameters{Required("value", String("value"))}, Execute: func(context.Context, Invocation, valueArguments) (Result, error) { return Text("ok"), nil }}),
		simpleTool("approved", false, "needs approval", func(context.Context, Invocation) (Result, error) { return Text("ok"), nil }),
		simpleTool("plain", false, "", func(context.Context, Invocation) (Result, error) { return Result{}, errors.New("plain failure") }),
		simpleTool("panics", false, "", func(context.Context, Invocation) (Result, error) { panic("boom") }),
		Define(Spec[noArguments]{Name: "checked", Description: "test", Check: func(Invocation, noArguments) error { return errors.New("semantic") }, Execute: never2}),
	} {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	oversized := `{"value":"` + strings.Repeat("x", session.MaxArgumentsBytes) + `"}`
	for name, test := range map[string]struct {
		ctx                context.Context
		tool, arguments    string
		output, classified string
	}{
		"unknown tool":     {context.Background(), "missing", `{}`, `Error: unknown tool "missing"`, "ToolNotFoundError/UNKNOWN_TOOL"},
		"schema":           {context.Background(), "typed", `{}`, `Error: invalid arguments: missing required property "value"`, "ToolArgsError/INVALID_ARGS"},
		"before dispatch":  {cancelled, "plain", `{}`, "Error: tool call aborted before dispatch", "AbortError/ABORTED_BEFORE_DISPATCH"},
		"oversized":        {context.Background(), "typed", oversized, fmt.Sprintf("Error: tool arguments exceed %d bytes; submit a smaller call", session.MaxArgumentsBytes), ""},
		"approval":         {context.Background(), "approved", `{}`, "Error: approval rejected", ""},
		"plain failure":    {context.Background(), "plain", `{}`, "Error: plain failure", ""},
		"panic":            {context.Background(), "panics", `{}`, "Error: implementation panicked", ""},
		"semantic check":   {context.Background(), "checked", `{}`, "Error: semantic", ""},
		"cancelled schema": {cancelled, "typed", `{}`, `Error: invalid arguments: missing required property "value"`, "ToolArgsError/INVALID_ARGS"},
	} {
		result := runOne(test.ctx, t, runtime, test.tool, test.arguments)
		if !result.IsError || result.Output != test.output || classification(result) != test.classified || result.Meta != nil {
			t.Errorf("%s: %q %s, want %q %s", name, result.Output, classification(result), test.output, test.classified)
		}
	}
}

func TestRuntime_RecordsDeclaredFailureClassifications(t *testing.T) {
	runtime, scope := startRuntime(t, &fakeApprover{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, candidate := range []*Tool{
		Define(Spec[noArguments]{Name: "check", Description: "test", Check: func(Invocation, noArguments) error {
			return &classifiedError{"cannot read \"a\": not found", notFound}
		}, Execute: never2}),
		simpleTool("wrapped", false, "", func(context.Context, Invocation) (Result, error) {
			return Result{}, fmt.Errorf("context: %w", &classifiedError{"not found", notFound})
		}),
		simpleTool("cancelled", false, "", func(ctx context.Context, _ Invocation) (Result, error) {
			cancel()
			return Result{}, &classifiedError{"read aborted: " + ctx.Err().Error(), session.ToolError{Name: "FsError", Code: "FS_ABORTED"}}
		}),
		simpleTool("unnamed", false, "", func(context.Context, Invocation) (Result, error) {
			return Result{}, &classifiedError{"failed", session.ToolError{Code: "FS_NOT_FOUND"}}
		}),
	} {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ tool, output, classified string }{
		{"check", `Error: cannot read "a": not found`, "FsError/FS_NOT_FOUND"},
		{"wrapped", "Error: context: not found", "FsError/FS_NOT_FOUND"},
		{"cancelled", "Error: read aborted: context canceled", "FsError/FS_ABORTED"},
		{"unnamed", `Error: tool "unnamed" returned invalid output: tool error classification is invalid`, "ToolOutputError/INVALID_TOOL_OUTPUT"},
	} {
		callContext := context.Background()
		if test.tool == "cancelled" {
			callContext = ctx
		}
		result := runOne(callContext, t, runtime, test.tool, `{}`)
		if !result.IsError || result.Output != test.output || classification(result) != test.classified {
			t.Errorf("%s: %q %s, want %q %s", test.tool, result.Output, classification(result), test.output, test.classified)
		}
	}
}

func TestRuntime_FitsMetadataAndRejectsAnotherToolsMember(t *testing.T) {
	runtime, scope := startRuntime(t, &fakeApprover{})
	paths := make([]string, 1000)
	for index := range paths {
		paths[index] = strings.Repeat("p", 100)
	}
	produced := &session.ToolMeta{Glob: &session.GlobMeta{Paths: paths, Total: 1000}}
	image := &session.Image{ID: "sha256:" + strings.Repeat("a", 64), Name: "red.png", MediaType: "image/png", Bytes: 1, Width: 1, Height: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, candidate := range []*Tool{
		simpleTool("glob", true, "", func(context.Context, Invocation) (Result, error) { return Result{Text: "found", Meta: produced}, nil }),
		simpleTool("read", true, "", func(context.Context, Invocation) (Result, error) {
			return Result{Text: "read", Meta: &session.ToolMeta{Glob: &session.GlobMeta{Paths: []string{}}}}, nil
		}),
		simpleTool("read_image", true, "", func(context.Context, Invocation) (Result, error) {
			return Result{Text: "image", Image: image, Meta: &session.ToolMeta{ReadImage: &session.ReadImageMeta{}}}, nil
		}),
		simpleTool("web_fetch", false, "", func(context.Context, Invocation) (Result, error) {
			cancel()
			return Result{Text: "fetched", Meta: &session.ToolMeta{WebFetch: &session.WebFetchMeta{URL: "https://example.com", StatusCode: 200}}}, nil
		}),
	} {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	result := runOne(context.Background(), t, runtime, "glob", `{}`)
	produced.Glob.Paths[0] = "changed"
	if result.IsError || result.Output != "found" || result.Meta == nil || result.Meta.Validate() != nil {
		t.Fatalf("glob result = %q %+v", result.Output, result.Meta)
	}
	if kept := result.Meta.Glob; !kept.Truncated || kept.Total != 1000 || len(kept.Paths) == 0 || len(kept.Paths) == 1000 || kept.Paths[0] == "changed" {
		t.Fatalf("glob metadata was not fitted and detached: %d paths, truncated=%v", len(kept.Paths), kept.Truncated)
	}
	for name, want := range map[string]string{
		"read":       `Error: tool "read" returned invalid output: metadata belongs to tool "glob"`,
		"read_image": `Error: tool "read_image" returned invalid output: read_image metadata path is empty`,
	} {
		result := runOne(context.Background(), t, runtime, name, `{}`)
		if !result.IsError || result.Output != want || classification(result) != "ToolOutputError/INVALID_TOOL_OUTPUT" || result.Meta != nil || result.Image != nil {
			t.Errorf("%s: %+v", name, result)
		}
	}
	result = runOne(ctx, t, runtime, "web_fetch", `{}`)
	if !result.IsError || result.Output != "Error: tool call aborted" || classification(result) != "AbortError/ABORTED" || result.Meta != nil {
		t.Fatalf("cancelled success kept its metadata: %+v", result)
	}
}

func TestRuntime_SpillKeepsMetadataAndPanicsDropIt(t *testing.T) {
	meta := func() *session.ToolMeta {
		return &session.ToolMeta{WebFetch: &session.WebFetchMeta{URL: "https://example.com", StatusCode: 200, Truncated: true}}
	}
	for _, store := range []SpillStore{&memorySpill{}, panicSpill{}} {
		runtime, _ := startRuntime(t, &fakeApprover{})
		scope := &plugin.Scope{}
		t.Cleanup(func() { _ = scope.Close(context.Background()) })
		if err := runtime.UseSpill(store, scope); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Register(simpleTool("web_fetch", false, "", func(context.Context, Invocation) (Result, error) {
			return Result{Text: strings.Repeat("x", 60000), Meta: meta()}, nil
		}), scope); err != nil {
			t.Fatal(err)
		}
		result := runOne(context.Background(), t, runtime, "web_fetch", `{}`)
		if _, panics := store.(panicSpill); panics {
			if !result.IsError || result.Output != "Error: implementation panicked" || result.Meta != nil || result.Error != nil {
				t.Errorf("spill panic kept success data: %+v", result)
			}
			continue
		}
		if result.IsError || !strings.Contains(result.Output, "Full formatted result stored at:") || result.Meta == nil || *result.Meta.WebFetch != *meta().WebFetch {
			t.Errorf("spilled result lost its metadata: %+v", result.Meta)
		}
	}
}
