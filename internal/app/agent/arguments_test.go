package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestEngine_OversizedArgumentsProduceRecoverableResults(t *testing.T) {
	arguments := json.RawMessage(`{"value":"` + strings.Repeat("x", session.MaxArgumentsBytes) + `"}`)
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("tool", session.ToolCall{ID: "large", Name: "inspect", Arguments: arguments})},
		modelAction{completion: assistantCompletion("recovered")},
	)
	candidate := &engineTool{name: "inspect", output: "must not execute"}
	if err := harness.tools.Register(candidate.define(), harness.toolScope); err != nil {
		t.Fatal(err)
	}
	journal, log := turnJournal()
	log.appendHook = func(record session.Record) error { return record.Validate() }
	result := harness.engine.runTurn(t.Context(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: noMessages, notices: noMessages})
	if result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "recovered" {
		t.Fatalf("oversized call ended the turn: %+v", result)
	}
	if len(candidate.seen) != 0 {
		t.Fatal("oversized arguments reached execution")
	}
	var found bool
	for _, event := range log.events {
		if event.Record.Result != nil {
			found = event.Record.Result.IsError && strings.Contains(event.Record.Result.Output, "tool arguments exceed")
		}
	}
	if !found || len(harness.model.seen) != 2 {
		t.Fatal("model did not receive the recoverable error result")
	}
}
