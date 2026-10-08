package agent

import (
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestEngine_OutputLimitStopsBeforeToolsAndNotices(t *testing.T) {
	for _, calls := range [][]session.ToolCall{nil, {{ID: "c", Name: "inspect", Arguments: []byte(`{}`)}}} {
		completion := assistantCompletion("partial", calls...)
		completion.Stop = "max_tokens"
		harness := startEngineHarness(t, 2, modelAction{completion: completion}, modelAction{completion: assistantCompletion("unexpected continuation")})
		tool := &engineTool{name: "inspect"}
		if err := harness.tools.Register(tool.define(), harness.toolScope); err != nil {
			t.Fatal(err)
		}
		journal, log := turnJournal()
		notices := 0
		result := harness.engine.runTurn(t.Context(), runInput{journal: journal, message: agentMessage(session.RoleUser, "work"), drain: noMessages, notices: func() []session.Message {
			notices++
			if notices > 1 {
				return []session.Message{agentMessage(session.RoleUser, "later")}
			}
			return nil
		}})
		if result.Err != nil || result.Outcome != session.TurnOutcome("max_tokens") || result.Text != "partial" {
			t.Fatalf("truncation treated as completion: %+v", result)
		}
		if len(harness.model.seen) != 1 || len(tool.seen) != 0 || notices != 1 {
			t.Fatalf("models=%d tools=%d notice drains=%d", len(harness.model.seen), len(tool.seen), notices)
		}
		events, err := log.Events(t.Context())
		if err != nil || events[len(events)-1].Record.Outcome != result.Outcome || events[len(events)-2].Record.Usage.OutputTokens != 2 {
			t.Fatalf("durable ending = %+v, %v", events, err)
		}
	}
}

func TestEngine_OutputLimitPropagatesStepCommitFailure(t *testing.T) {
	completion := assistantCompletion("partial")
	completion.Stop = "max_tokens"
	harness := startEngineHarness(t, 1, modelAction{completion: completion})
	journal, log := turnJournal()
	failure := errors.New("disk full")
	log.appendHook = appendFailure(session.RecordStepEnd, 1, failure)
	result := harness.engine.runTurn(t.Context(), runInput{journal: journal, message: agentMessage(session.RoleUser, "work"), notices: noMessages})
	if !errors.Is(result.Err, failure) || result.Outcome != session.OutcomeError {
		t.Fatalf("failed truncation commit = %+v", result)
	}
	events, err := log.Events(t.Context())
	if err != nil || events[len(events)-1].Record.Outcome != session.OutcomeError {
		t.Fatalf("durable ending = %+v, %v", events, err)
	}
}
