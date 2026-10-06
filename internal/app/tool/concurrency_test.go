package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestRuntime_LimitsConcurrentCallsThroughCheckApprovalAndExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		approver := &fakeApprover{outcome: session.ApprovalAllowedOnce}
		runtime, scope := startRuntime(t, approver)
		const count = 32
		var mu sync.Mutex
		active, peak := 0, 0
		entered := make(chan struct{}, count)
		// Every execution stays in flight until the caller's virtual deadline.
		// The barrier prevents early completions from hiding an unbounded dispatch.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		read := Define(Spec[noArguments]{
			Name: "read", Description: "blocked concurrent read",
			Concurrent: func(noArguments) bool { return true },
			Check: func(Invocation, noArguments) error {
				mu.Lock()
				active++
				peak = max(peak, active)
				mu.Unlock()
				return nil
			},
			Approval: func(noArguments) string { return "read" },
			Execute: func(ctx context.Context, invocation Invocation, _ noArguments) (Result, error) {
				if !invocation.Approved {
					t.Error("execution preceded approval")
				}
				entered <- struct{}{}
				<-ctx.Done()
				mu.Lock()
				active--
				mu.Unlock()
				return Text(invocation.CallID), nil
			},
		})
		if err := runtime.Register(read, scope); err != nil {
			t.Fatal(err)
		}
		calls := make([]session.ToolCall, count)
		for index := range calls {
			calls[index] = session.ToolCall{ID: fmt.Sprint(index), Name: "read", Arguments: json.RawMessage(`{}`)}
		}
		done := make(chan []session.ToolResult, 1)
		finished := make(chan struct{})
		t.Cleanup(func() { cancel(); <-finished })
		go func() {
			defer close(finished)
			done <- runtime.ExecuteBatch(ctx, BatchRequest{Calls: calls})
		}()
		for range 10 {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("ten calls did not reach the execution barrier")
			}
		}
		results := <-done
		if peak != 10 || active != 0 {
			t.Fatalf("in-flight calls: peak=%d, active after return=%d; want 10, 0", peak, active)
		}
		for index, result := range results {
			want := "Error: tool call aborted before dispatch"
			if index < 10 {
				want = "Error: tool call aborted"
			}
			if !result.IsError || result.CallID != calls[index].ID || result.Output != want {
				t.Fatalf("result %d = %+v, want %q", index, result, want)
			}
		}
		if len(approver.seen) != 10 {
			t.Fatalf("approvals=%d, want 10; cancelled queued calls must not reach approval", len(approver.seen))
		}
	})
}

func TestRuntime_RefillsConcurrentSlotsBeforeTheGroupFinishes(t *testing.T) {
	runtime, scope := startRuntime(t, &fakeApprover{})
	entered := make(chan string, 11)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan []session.ToolResult, 1)
	t.Cleanup(func() { cancel(); <-done })
	read := simpleTool("read", true, "", func(ctx context.Context, invocation Invocation) (Result, error) {
		entered <- invocation.CallID
		if invocation.CallID == "0" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		} else {
			<-ctx.Done()
		}
		return Text(invocation.CallID), nil
	})
	if err := runtime.Register(read, scope); err != nil {
		t.Fatal(err)
	}
	calls := make([]session.ToolCall, 11)
	for index := range calls {
		calls[index] = session.ToolCall{ID: fmt.Sprint(index), Name: "read", Arguments: json.RawMessage(`{}`)}
	}
	go func() { done <- runtime.ExecuteBatch(ctx, BatchRequest{Calls: calls}) }()
	for range 10 {
		<-entered
	}
	close(release)
	if id := <-entered; id != "10" {
		t.Fatalf("refilled call=%s, want 10", id)
	}
}
