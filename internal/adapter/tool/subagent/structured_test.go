package subagent

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// TestTools_PersistSubagentErrorClassification proves every delegation and
// control tool records the service's SubagentError code in tool/result
// without changing the model-visible text, and that other failures stay
// unclassified.
func TestTools_PersistSubagentErrorClassification(t *testing.T) {
	codes := []appSubagent.Code{
		appSubagent.CodeInvalidRequest, appSubagent.CodeDepthLimit, appSubagent.CodeLimitReached,
		appSubagent.CodeUnauthorized, appSubagent.CodeNotResumable, appSubagent.CodeParentUnavailable,
		appSubagent.CodeAborted, appSubagent.CodeAbortedBeforeDispatch, appSubagent.CodeTeardownFailed,
	}
	calls := map[string]map[string]any{
		"subagent":        {"description": "d", "prompt": "p"},
		"subagent_fork":   {"description": "d", "prompt": "p"},
		"send_message":    {"agent_id": "child", "message": "m"},
		"interrupt_agent": {"agent_id": "child"},
		"list_agents":     {"scope": "descendants"},
	}
	for _, code := range codes {
		service := &fakeService{}
		runtime := startProvider(t, service)
		// The classified error may arrive wrapped or joined with teardown
		// failures, as the service returns it.
		service.err = errors.Join(fmt.Errorf("delegation: %w", &appSubagent.Error{Code: code, Message: "failure " + string(code)}), errors.New("cleanup"))
		for name, arguments := range calls {
			result := call(t, runtime, name, arguments)
			want := &session.ToolError{Name: "SubagentError", Code: string(code)}
			if !result.IsError || result.Output != "Error: delegation: failure "+string(code)+"\ncleanup" || !reflect.DeepEqual(result.Error, want) || result.Meta != nil {
				t.Errorf("%s with %s = %#v", name, code, result)
			}
		}
	}
	service := &fakeService{err: errors.New("plain failure")}
	runtime := startProvider(t, service)
	for name, arguments := range calls {
		if result := call(t, runtime, name, arguments); !result.IsError || result.Error != nil || result.Output != "Error: plain failure" {
			t.Errorf("%s unclassified failure = %#v", name, result)
		}
	}
	// A run that ended without completing is a tool failure the service did
	// not classify, so it carries no error classification.
	service.err, service.report = nil, appSubagent.Report{Outcome: session.OutcomeError}
	if result := call(t, runtime, "subagent_fork", calls["subagent_fork"]); !result.IsError || result.Error != nil || result.Output != "Error: subagent run failed" {
		t.Fatalf("unfinished run = %#v", result)
	}
}
