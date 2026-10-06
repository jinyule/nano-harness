package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

type panicSpill struct{}

func (panicSpill) Create(context.Context, string, string) (SpillFile, error) {
	panic("spill implementation failure")
}

func TestRuntime_ContainsSpillImplementationPanics(t *testing.T) {
	runtime, scope := startRuntime(t, &fakeApprover{})
	if err := runtime.UseSpill(panicSpill{}, scope); err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		name := "large-success"
		if failed {
			name = "large-error"
		}
		candidate := simpleTool(name, false, "", func(context.Context, Invocation) (Result, error) {
			text := strings.Repeat("x", 60000)
			if failed {
				return Result{}, errors.New(text)
			}
			return Text(text), nil
		})
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
		result := runtime.ExecuteBatch(t.Context(), BatchRequest{SessionID: "s", Calls: []session.ToolCall{{ID: name, Name: name, Arguments: json.RawMessage(`{}`)}}})[0]
		if !result.IsError || result.Output != "Error: implementation panicked" || result.CallID != name {
			t.Fatalf("uncontained spill failure: %+v", result)
		}
	}
}

func TestRuntime_KeepInlineErrorsStayInline(t *testing.T) {
	runtime, scope := startRuntime(t, &fakeApprover{})
	store := &memorySpill{}
	if err := runtime.UseSpill(store, scope); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", 60000)
	for _, phase := range []string{"schema", "check", "execute"} {
		candidate := Define(Spec[noArguments]{Name: phase, Description: phase, KeepInline: true,
			Check: func(Invocation, noArguments) error {
				if phase == "check" {
					return errors.New(large)
				}
				return nil
			}, Execute: func(context.Context, Invocation, noArguments) (Result, error) { return Result{}, errors.New(large) },
		})
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
		arguments := json.RawMessage(`{}`)
		want := "Error: " + large
		if phase == "schema" {
			raw, err := json.Marshal(map[string]bool{large: true})
			if err != nil {
				t.Fatal(err)
			}
			arguments = raw
			want = "Error: invalid arguments: \"" + large + "\" is not a declared property"
		}
		result := runtime.ExecuteBatch(t.Context(), BatchRequest{SessionID: "s", Calls: []session.ToolCall{{ID: phase, Name: phase, Arguments: arguments}}})[0]
		if !result.IsError || result.CallID != phase || result.Output != want || len(store.saved) != 0 {
			t.Fatalf("%s error bypassed KeepInline: bytes=%d artifacts=%d", phase, len(result.Output), len(store.saved))
		}
	}
}

func TestRuntime_SpillsErrorsBeforeDurableTruncation(t *testing.T) {
	runtime, scope := startRuntime(t, &fakeApprover{outcome: session.ApprovalAllowedOnce})
	store := &memorySpill{}
	if err := runtime.UseSpill(store, scope); err != nil {
		t.Fatal(err)
	}
	large := "SEARCH_FAILED: " + strings.Repeat("x", session.MaxTextBytes+100)
	for _, phase := range []string{"check", "execute"} {
		candidate := Define(Spec[noArguments]{Name: phase, Description: phase,
			Check: func(Invocation, noArguments) error {
				if phase == "check" {
					return errors.New(large)
				}
				return nil
			}, Execute: func(context.Context, Invocation, noArguments) (Result, error) { return Result{}, errors.New(large) },
		})
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
		result := runtime.ExecuteBatch(t.Context(), BatchRequest{SessionID: "s", Calls: []session.ToolCall{{ID: phase, Name: phase, Arguments: json.RawMessage(`{}`)}}})[0]
		if !result.IsError || result.CallID != phase || !strings.HasPrefix(result.Output, "Error: SEARCH_FAILED:") || !strings.Contains(result.Output, "Full formatted result stored at:") || estimateTokens(result.Output) > spillInlineTokens {
			t.Fatalf("%s error was not retained as a bounded error preview: bytes=%d error=%v", phase, len(result.Output), result.IsError)
		}
		if len(store.saved) == 0 || store.saved[len(store.saved)-1].content != "Error: "+large {
			t.Fatal("spill lost the complete error envelope")
		}
	}
	key := strings.Repeat("k", 60000)
	raw, err := json.Marshal(map[string]bool{key: true})
	if err != nil {
		t.Fatal(err)
	}
	result := runtime.ExecuteBatch(t.Context(), BatchRequest{SessionID: "s", Calls: []session.ToolCall{{ID: "schema-error", Name: "execute", Arguments: raw}}})[0]
	complete := "Error: invalid arguments: \"" + key + "\" is not a declared property"
	if !result.IsError || !strings.HasPrefix(result.Output, "Error: invalid arguments:") || estimateTokens(result.Output) > spillInlineTokens || store.saved[len(store.saved)-1].content != complete {
		t.Fatal("schema failure bypassed spill or lost full error")
	}
	store.commitErr = errors.New("disk full")
	result = runtime.ExecuteBatch(t.Context(), BatchRequest{SessionID: "s", Calls: []session.ToolCall{{ID: "failed-save", Name: "execute", Arguments: json.RawMessage(`{}`)}}})[0]
	if !result.IsError || len(result.Output) != session.MaxTextBytes || !strings.HasSuffix(result.Output, "\n[output truncated]") {
		t.Fatal("failed save did not retain the bounded error")
	}
}
