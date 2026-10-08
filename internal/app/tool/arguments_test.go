package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestRuntime_OversizedArgumentsNeverReachToolCallbacks(t *testing.T) {
	approver := &fakeApprover{outcome: session.ApprovalAllowedOnce}
	runtime, scope := startRuntime(t, approver)
	callback := func() { t.Error("omitted call reached a tool callback") }
	candidate := Define(Spec[noArguments]{
		Name: "tool", Description: "reject oversized calls",
		Concurrent: func(noArguments) bool { callback(); return true },
		Check:      func(context.Context, Invocation, noArguments) error { callback(); return nil },
		Approval:   func(noArguments) string { callback(); return "approval" },
		Execute:    func(context.Context, Invocation, noArguments) (Result, error) { callback(); return Text("wrong"), nil },
	})
	if err := runtime.Register(candidate, scope); err != nil {
		t.Fatal(err)
	}
	results := runtime.ExecuteBatch(t.Context(), BatchRequest{Calls: []session.ToolCall{
		{ID: "raw", Name: "tool", Arguments: json.RawMessage(`{"v":"` + strings.Repeat("x", session.MaxArgumentsBytes) + `"}`)},
		{ID: "omitted", Name: "tool", Arguments: json.RawMessage(`{}`), ArgumentsOmitted: true},
	}})
	for _, result := range results {
		if !result.IsError || result.Output != "Error: tool arguments exceed 786432 bytes; submit a smaller call" {
			t.Fatalf("result=%+v", result)
		}
	}
	if len(approver.seen) != 0 {
		t.Fatal("oversized call requested approval")
	}
}
