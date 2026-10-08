package job

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowAll struct{}

func (allowAll) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

type dropNotices struct{}

func (dropNotices) QueueNotice(context.Context, string, session.Message) error { return nil }

type harness struct {
	runtime *appTool.Runtime
	jobs    *appJob.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	runtime, _ := appTool.New(allowAll{})
	jobs, _ := appJob.New(dropNotices{})
	runtimeScope, jobScope, providerScope := &plugin.Scope{}, &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Start(context.Background(), jobScope); err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, jobs)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = providerScope.Close(context.Background())
		_ = jobScope.Close(context.Background())
		_ = runtimeScope.Close(context.Background())
	})
	return &harness{runtime: runtime, jobs: jobs}
}

func (h *harness) call(ctx context.Context, t *testing.T, owner, name string, arguments map[string]any) session.ToolResult {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{
		SessionID: owner, Turn: 1, Step: 1,
		Calls: []session.ToolCall{{ID: "call", Name: name, Arguments: encoded}},
	})[0]
}

// stream launches a job that writes output, then holds until released or
// killed.
func (h *harness) stream(t *testing.T, owner string, outcome appJob.Outcome) (string, chan struct{}) {
	t.Helper()
	release, wrote := make(chan struct{}), make(chan struct{})
	id, err := h.jobs.Launch(appJob.Spec{Kind: "bash", Label: "make test", Owner: owner, Run: func(ctx context.Context, output *appJob.Output) appJob.Outcome {
		_, _ = output.Writer(appJob.Stdout).Write([]byte("building"))
		_, _ = output.Writer(appJob.Stderr).Write([]byte("warning\n"))
		close(wrote)
		select {
		case <-release:
			return outcome
		case <-ctx.Done():
			return appJob.Outcome{Status: appJob.StatusKilled, Detail: "signal: SIGKILL"}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-wrote
	return id, release
}

func TestProvider_RegistersUpstreamDefinitionsForItsScope(t *testing.T) {
	runtime, _ := appTool.New(allowAll{})
	jobs, _ := appJob.New(dropNotices{})
	for _, test := range []struct {
		runtime *appTool.Runtime
		jobs    *appJob.Service
	}{{jobs: jobs}, {runtime: runtime}} {
		if _, err := New(test.runtime, test.jobs); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", test, err)
		}
	}
	provider, _ := New(runtime, jobs)
	if provider.ID() != "job-tools" {
		t.Fatalf("ID = %q", provider.ID())
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("inactive runtime = %v", err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	scope := &plugin.Scope{}
	if err := provider.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	catalog, _ := runtime.Catalog(nil)
	names := make([]string, len(catalog.Definitions))
	for index, definition := range catalog.Definitions {
		names[index] = definition.Name
	}
	if strings.Join(names, ",") != "job_kill,job_list,job_output" || len(catalog.Guidance) != 1 || catalog.Guidance[0] != guidance {
		t.Fatalf("catalog = %#v", catalog)
	}
	if string(catalog.Definitions[1].Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("job_list parameters = %s", catalog.Definitions[1].Parameters)
	}
	if catalog, _ := runtime.Catalog([]string{"job_list"}); len(catalog.Guidance) != 0 {
		t.Fatalf("guidance without job_output = %q", catalog.Guidance)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("job tools survived cleanup")
	}
}

func TestJobOutput_ReadsWaitsAndFencesOwners(t *testing.T) {
	h := newHarness(t)
	id, release := h.stream(t, "root", appJob.Outcome{Status: appJob.StatusCompleted, Detail: "exit code: 0"})
	for _, test := range []struct {
		owner     string
		arguments map[string]any
		want      string
		isError   bool
	}{
		{"root", map[string]any{"job_id": ""}, `Error: invalid job_id: expected a non-empty string, got ""`, true},
		{"root", map[string]any{"job_id": "bash-9"}, "Error: unknown job bash-9", true},
		{"child", map[string]any{"job_id": id}, "Error: job bash-1 belongs to another session", true},
		{"child", map[string]any{"job_id": id, "wait": true}, "Error: job bash-1 belongs to another session", true},
		{"root", map[string]any{"job_id": id, "wait": true, "timeout_ms": -5}, "Error: invalid wait timeout: expected a positive number of milliseconds, got -5", true},
		{"root", map[string]any{"job_id": id}, "building\n[stderr]\nwarning\n[status: running]", false},
		{"root", map[string]any{"job_id": id, "wait": true, "timeout_ms": 1}, "(no new output)\n[status: running]", false},
	} {
		result := h.call(context.Background(), t, test.owner, "job_output", test.arguments)
		if result.Output != test.want || result.IsError != test.isError {
			t.Errorf("job_output(%s, %v) = %#v, want %q", test.owner, test.arguments, result, test.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := h.call(ctx, t, "root", "job_output", map[string]any{"job_id": id, "wait": true}); result.Output != "Error: tool call aborted before dispatch" {
		t.Fatalf("aborted wait = %#v", result)
	}
	waited := make(chan session.ToolResult)
	go func() {
		waited <- h.call(context.Background(), t, "root", "job_output", map[string]any{"job_id": id, "wait": true, "timeout_ms": 9e9})
	}()
	close(release)
	if result := <-waited; result.IsError || result.Output != "(no new output)\n[status: completed, exit code: 0]" {
		t.Fatalf("waited = %#v", result)
	}

	// A value result is appended once after the stream.
	valueID, err := h.jobs.Launch(appJob.Spec{Kind: "subagent", Label: "review", Owner: "root", Run: func(_ context.Context, output *appJob.Output) appJob.Outcome {
		_, _ = output.Writer(appJob.Stdout).Write([]byte("progress"))
		return appJob.Outcome{Status: appJob.StatusCompleted, Result: "final report"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result := h.call(context.Background(), t, "root", "job_output", map[string]any{"job_id": valueID, "wait": true}); result.Output != "progress\nfinal report\n[status: completed]" {
		t.Fatalf("value result = %#v", result)
	}
	if result := h.call(context.Background(), t, "root", "job_output", map[string]any{"job_id": valueID}); result.Output != "(no new output)\n[status: completed]" {
		t.Fatalf("second value read = %#v", result)
	}
}

type waitBarrierContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (ctx *waitBarrierContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.entered) })
	return ctx.Context.Done()
}

func TestJobOutput_CancellationDuringWaitPreservesJobAndUnreadOutput(t *testing.T) {
	h := newHarness(t)
	id, _ := h.stream(t, "root", appJob.Outcome{Status: appJob.StatusCompleted})
	base, cancel := context.WithCancel(context.Background())
	ctx := &waitBarrierContext{Context: base, entered: make(chan struct{})}
	finished := make(chan session.ToolResult, 1)
	var group sync.WaitGroup
	t.Cleanup(func() { cancel(); group.Wait() })
	group.Go(func() {
		finished <- h.call(ctx, t, "root", "job_output", map[string]any{"job_id": id, "wait": true})
	})
	select {
	case <-ctx.entered:
	case <-t.Context().Done():
		t.Fatal("test cancelled before job wait")
	}
	cancel()
	select {
	case result := <-finished:
		if !result.IsError || result.Output != "Error: tool call aborted" {
			t.Fatalf("cancelled active wait = %#v", result)
		}
	case <-t.Context().Done():
		t.Fatal("cancelled wait did not return")
	}
	if result := h.call(context.Background(), t, "root", "job_output", map[string]any{"job_id": id}); result.IsError || result.Output != "building\n[stderr]\nwarning\n[status: running]" {
		t.Fatalf("cancelled wait consumed output or stopped job: %#v", result)
	}
}

func TestJobList_ShowsOnlyCallerJobs(t *testing.T) {
	h := newHarness(t)
	if result := h.call(context.Background(), t, "root", "job_list", map[string]any{}); result.Output != "(no background jobs)" {
		t.Fatalf("empty list = %#v", result)
	}
	h.stream(t, "root", appJob.Outcome{Status: appJob.StatusCompleted})
	h.stream(t, "child", appJob.Outcome{Status: appJob.StatusCompleted})
	h.stream(t, "root", appJob.Outcome{Status: appJob.StatusCompleted})
	if result := h.call(context.Background(), t, "root", "job_list", map[string]any{}); result.Output != "bash-1 [bash] running — make test\nbash-3 [bash] running — make test" {
		t.Fatalf("list = %#v", result)
	}
}

func TestJobKill_RequestsCancellationOnce(t *testing.T) {
	h := newHarness(t)
	id, _ := h.stream(t, "root", appJob.Outcome{Status: appJob.StatusCompleted})
	for _, test := range []struct {
		owner     string
		arguments map[string]any
		want      string
	}{
		{"root", map[string]any{"job_id": ""}, `Error: invalid job_id: expected a non-empty string, got ""`},
		{"child", map[string]any{"job_id": id}, "Error: job bash-1 belongs to another session"},
		{"root", map[string]any{"job_id": id, "reason": "superseded"}, "requested cancellation of job bash-1"},
	} {
		if result := h.call(context.Background(), t, test.owner, "job_kill", test.arguments); result.Output != test.want {
			t.Errorf("job_kill(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	if _, err := h.jobs.Wait(context.Background(), "root", id, 1<<62); err != nil {
		t.Fatal(err)
	}
	if result := h.call(context.Background(), t, "root", "job_kill", map[string]any{"job_id": id}); result.Output != "job bash-1 had already finished [status: killed, signal: SIGKILL; superseded]" {
		t.Fatalf("second kill = %#v", result)
	}
}
