package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func appendFailure(recordType session.RecordType, occurrence int, failure error) func(session.Record) error {
	seen := 0
	return func(record session.Record) error {
		if record.Type == recordType {
			seen++
			if seen == occurrence {
				return failure
			}
		}
		return nil
	}
}

func TestEngine_ContainsInitialCancellationAndComponentFailures(t *testing.T) {
	failure := errors.New("injected")
	t.Run("initial events", func(t *testing.T) {
		harness := startEngineHarness(t, 1)
		journal, log := turnJournal()
		log.eventsErr = failure
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go")})
		if !errors.Is(result.Err, failure) || result.Outcome != session.OutcomeError {
			t.Fatalf("result = %+v", result)
		}
	})
	for _, recordType := range []session.RecordType{session.RecordTurnStart, session.RecordUserMessage} {
		t.Run(string(recordType), func(t *testing.T) {
			harness := startEngineHarness(t, 1)
			journal, log := turnJournal()
			log.appendHook = appendFailure(recordType, 1, failure)
			result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go")})
			if !errors.Is(result.Err, failure) || result.Outcome != session.OutcomeError {
				t.Fatalf("result = %+v", result)
			}
		})
	}
	t.Run("context before step", func(t *testing.T) {
		harness := startEngineHarness(t, 1)
		journal, _ := turnJournal()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := harness.engine.runTurn(ctx, runInput{journal: journal, message: agentMessage(session.RoleUser, "go")})
		if !errors.Is(result.Err, context.Canceled) || result.Outcome != session.OutcomeCanceled {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("compaction inactive", func(t *testing.T) {
		harness := startEngineHarness(t, 1)
		_ = harness.scopes[2].Close(context.Background())
		journal, _ := turnJournal()
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go")})
		if result.Err == nil || !strings.Contains(result.Err.Error(), "proactive compaction") {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestEngine_ContainsStepPreparationFailures(t *testing.T) {
	failure := errors.New("injected")
	for _, test := range []struct {
		name      string
		configure func(*engineHarness, *memoryLog)
		match     string
	}{
		{name: "step append", match: "injected", configure: func(_ *engineHarness, log *memoryLog) {
			log.appendHook = appendFailure(session.RecordStepStart, 1, failure)
		}},
		{name: "settings", match: "settings service", configure: func(harness *engineHarness, log *memoryLog) {
			log.appendHook = func(record session.Record) error {
				if record.Type == session.RecordStepStart {
					_ = harness.settingsScope.Close(context.Background())
				}
				return nil
			}
		}},
		{name: "tools", match: "tool runtime", configure: func(harness *engineHarness, log *memoryLog) {
			log.appendHook = func(record session.Record) error {
				if record.Type == session.RecordStepStart {
					_ = harness.toolScope.Close(context.Background())
				}
				return nil
			}
		}},
		{name: "prompt", match: "prompt assembler", configure: func(harness *engineHarness, log *memoryLog) {
			log.appendHook = func(record session.Record) error {
				if record.Type == session.RecordStepStart {
					_ = harness.promptScope.Close(context.Background())
				}
				return nil
			}
		}},
		{name: "request header append", match: "injected", configure: func(_ *engineHarness, log *memoryLog) {
			log.appendHook = appendFailure(session.RecordRequestHeader, 1, failure)
		}},
		{name: "prepare call", match: "prepare", configure: func(harness *engineHarness, _ *memoryLog) {
			harness.provider.prepareErr = errors.New("prepare")
		}},
		{name: "second events", match: "events", configure: func(_ *engineHarness, log *memoryLog) {
			log.eventsHook = func(call int) error {
				if call == 3 {
					return errors.New("events")
				}
				return nil
			}
		}},
		{name: "surface", match: "shadows no visible", configure: func(_ *engineHarness, log *memoryLog) {
			log.appendHook = func(record session.Record) error {
				if record.Type == session.RecordRequestHeader {
					log.events = append(log.events, session.Event{Sequence: uint64(len(log.events) + 1), Record: session.Record{
						Type:       session.RecordCompactionSummary,
						Compaction: &session.CompactionData{ID: "invalid", ShadowedSeqs: []uint64{999}, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "summary"}}},
					}})
				}
				return nil
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := startEngineHarness(t, 1)
			journal, log := turnJournal()
			test.configure(harness, log)
			result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
			if result.Err == nil || !strings.Contains(result.Err.Error(), test.match) || result.Outcome != session.OutcomeError {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestEngine_ContainsEveryDurableCompletionBoundary(t *testing.T) {
	failure := errors.New("append boundary")
	call := session.ToolCall{ID: "call", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	for _, test := range []struct {
		name       string
		recordType session.RecordType
		actions    []modelAction
		withTool   bool
		steer      bool
	}{
		{name: "chunk", recordType: session.RecordAssistantChunk, actions: []modelAction{{chunks: []session.AssistantChunk{{Kind: session.ChunkText, Text: "chunk"}}, completion: assistantCompletion("done")}}},
		{name: "assistant", recordType: session.RecordAssistantMessage, actions: []modelAction{{completion: assistantCompletion("done")}}},
		{name: "tool call", recordType: session.RecordToolCall, actions: []modelAction{{completion: assistantCompletion("tool", call)}}, withTool: true},
		{name: "terminal step", recordType: session.RecordStepEnd, actions: []modelAction{{completion: assistantCompletion("done")}}},
		{name: "terminal turn", recordType: session.RecordTurnEnd, actions: []modelAction{{completion: assistantCompletion("done")}}},
		{name: "tool result", recordType: session.RecordToolResult, actions: []modelAction{{completion: assistantCompletion("tool", call)}}, withTool: true},
		{name: "tool step", recordType: session.RecordStepEnd, actions: []modelAction{{completion: assistantCompletion("tool", call)}}, withTool: true},
		{name: "steer", recordType: session.RecordUserMessage, actions: []modelAction{{completion: assistantCompletion("tool", call)}}, withTool: true, steer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := startEngineHarness(t, 1, test.actions...)
			if test.withTool {
				scope := &plugin.Scope{}
				if err := harness.tools.Register(&engineTool{name: "inspect", output: "ok"}, scope); err != nil {
					t.Fatal(err)
				}
			}
			journal, log := turnJournal()
			occurrence := 1
			if test.recordType == session.RecordUserMessage {
				occurrence = 2
			}
			log.appendHook = appendFailure(test.recordType, occurrence, failure)
			drain := func() []session.Message { return nil }
			if test.steer {
				drain = func() []session.Message { return []session.Message{agentMessage(session.RoleUser, "steer")} }
			}
			result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), tools: []string{"inspect"}, drain: drain})
			if !errors.Is(result.Err, failure) || result.Outcome != session.OutcomeError {
				t.Fatalf("result = %+v records=%#v", result, recordTypes(log.events))
			}
		})
	}
}

func TestEngine_ContextWindowCompactionFailureAndRecovery(t *testing.T) {
	contextFailure := &llm.Error{Code: llm.ErrorContextWindow, Provider: "openai"}
	t.Run("step close failure", func(t *testing.T) {
		harness := startEngineHarness(t, 2, modelAction{err: contextFailure})
		journal, log := turnJournal()
		log.appendHook = appendFailure(session.RecordStepEnd, 1, errors.New("close"))
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
		if result.Err == nil || !strings.Contains(result.Err.Error(), "close") {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("nothing compactable", func(t *testing.T) {
		harness := startEngineHarness(t, 2, modelAction{err: contextFailure})
		journal, _ := turnJournal()
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
		if !errors.Is(result.Err, contextFailure) || result.Outcome != session.OutcomeError {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("compaction call fails", func(t *testing.T) {
		harness := startEngineHarness(t, 2,
			modelAction{err: contextFailure},
			modelAction{err: &llm.Error{Code: llm.ErrorInvalid, Provider: "openai"}},
			modelAction{err: &llm.Error{Code: llm.ErrorInvalid, Provider: "openai"}},
		)
		journal, log := turnJournal()
		seedSurface(log)
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
		if result.Err == nil || !strings.Contains(result.Err.Error(), "invalid_request") {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("successful recovery", func(t *testing.T) {
		harness := startEngineHarness(t, 2,
			modelAction{err: contextFailure},
			modelAction{completion: assistantCompletion("summary")},
			modelAction{completion: llm.Completion{Message: session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "custom", Plugin: "script"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "recovered"}}}}},
		)
		journal, log := turnJournal()
		seedSurface(log)
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
		if result.Outcome != session.OutcomeCompleted || result.Text != "recovered" || result.Err != nil {
			t.Fatalf("result = %+v", result)
		}
		if countType(recordTypes(log.events), session.RecordCompactionSummary) != 1 || countType(recordTypes(log.events), session.RecordStepStart) != 2 {
			t.Fatalf("records = %#v", recordTypes(log.events))
		}
	})
}

func seedSurface(log *memoryLog) {
	for _, message := range []session.Message{
		agentMessage(session.RoleUser, "old one"),
		{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "old two"}}},
		agentMessage(session.RoleUser, "old three"),
	} {
		log.events = append(log.events, session.Event{Sequence: uint64(len(log.events) + 1), Record: session.Record{Type: map[session.MessageRole]session.RecordType{session.RoleUser: session.RecordUserMessage, session.RoleAssistant: session.RecordAssistantMessage}[message.Role], Turn: 1, Step: 1, Message: &message}})
	}
}
