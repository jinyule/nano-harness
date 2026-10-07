package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	jsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/app/approval"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// barrierLog wraps a real JSONL log: fail may refuse a record before it
// reaches the log, after runs once a record committed, and events may
// refuse a read.
type barrierLog struct {
	*jsonl.Log
	fail   func(session.Record) error
	after  func(session.Record)
	events func() error
}

func (log barrierLog) Append(ctx context.Context, record session.Record) (session.Event, error) {
	if log.fail != nil {
		if err := log.fail(record); err != nil {
			return session.Event{}, err
		}
	}
	event, err := log.Log.Append(ctx, record)
	if err == nil && log.after != nil {
		log.after(record)
	}
	return event, err
}

func (log barrierLog) Events(ctx context.Context) ([]session.Event, error) {
	if log.events != nil {
		if err := log.events(); err != nil {
			return nil, err
		}
	}
	return log.Log.Events(ctx)
}

// batchHarness runs a turn whose model answers with two inspect calls,
// then a turn that answers with text, against a real JSONL session.
func batchHarness(t *testing.T) (*engineHarness, *engineTool, *jsonl.Manager, *jsonl.Log) {
	t.Helper()
	calls := []session.ToolCall{
		{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)},
		{ID: "call-2", Name: "inspect", Arguments: json.RawMessage(`{}`)},
	}
	harness := startEngineHarness(t, 3, modelAction{completion: assistantCompletion("two calls", calls...)}, modelAction{completion: assistantCompletion("next answer")})
	tool := &engineTool{name: "inspect", output: "ok"}
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	if err := harness.tools.Register(tool.define(), scope); err != nil {
		t.Fatal(err)
	}
	manager, err := jsonl.New(jsonl.Config{Root: filepath.Join(t.TempDir(), "sessions"), CompositionID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	log, err := manager.Open(context.Background(), jsonl.OpenOptions{SessionID: "batch", Cwd: t.TempDir(), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	return harness, tool, manager, log
}

func toolResults(events []session.Event) map[string]session.ToolResult {
	results := map[string]session.ToolResult{}
	for _, event := range events {
		if event.Record.Type == session.RecordToolResult {
			results[event.Record.Result.CallID] = *event.Record.Result
		}
	}
	return results
}

// TestEngine_CancelWhileRecordingToolCallsClosesTheStep cancels the turn
// after the first of two tool calls committed. Every recorded call is
// answered as aborted before dispatch, the step and turn close canceled,
// and the next turn opens on the same live log without repair.
func TestEngine_CancelWhileRecordingToolCallsClosesTheStep(t *testing.T) {
	harness, tool, manager, log := batchHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	journal := newJournal(barrierLog{Log: log, after: func(record session.Record) {
		if record.Type == session.RecordToolCall && record.Call.ID == "call-1" {
			cancel()
		}
	}})
	input := runInput{journal: journal, message: agentMessage(session.RoleUser, "run"), notices: noMessages, drain: noMessages}
	first := harness.engine.runTurn(ctx, input)
	if first.Outcome != session.OutcomeCanceled || !errors.Is(first.Err, context.Canceled) || len(tool.seen) != 0 {
		t.Fatalf("first = %+v, tool ran %d times", first, len(tool.seen))
	}
	next := harness.engine.runTurn(context.Background(), input)
	if next.Err != nil || next.Text != "next answer" || next.Turn != 2 {
		t.Fatalf("next = %+v", next)
	}
	events, err := journal.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, callID := range []string{"call-1", "call-2"} {
		result := toolResults(events)[callID]
		if result.Output != "Error: tool call aborted before dispatch" || !result.IsError || result.Error == nil || result.Error.Name != "AbortError" || result.Error.Code != "ABORTED_BEFORE_DISPATCH" {
			t.Fatalf("%s result = %+v", callID, result)
		}
	}
	if outcomes := turnOutcomes(events); !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeCanceled, session.OutcomeCompleted}) {
		t.Fatalf("turn outcomes = %q", outcomes)
	}

	// The committed tail is the reviewed golden record of a cancelled batch.
	transcript, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "cancelled-batch-tail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(transcript, []byte(`{"seq":6,`))
	end := bytes.Index(transcript, []byte(`{"seq":12,`))
	if start < 0 || end < start || !bytes.Equal(transcript[start:end], golden) {
		t.Fatalf("cancelled batch tail differs from golden:\n%s", transcript)
	}

	// Reopening validates the whole log and adds no repair records.
	if err := log.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := manager.Open(context.Background(), jsonl.OpenOptions{SessionID: "batch", Cwd: log.Header().Cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	after, err := reopened.Events(context.Background())
	if err != nil || len(after) != len(events) {
		t.Fatalf("reopened log has %d events, want %d: %v", len(after), len(events), err)
	}
}

// TestEngine_FailedToolRecordingResolvesCommittedCalls fails one append
// inside a batch, while recording calls and while recording results. Each
// call committed without a result gets the interrupted result before the
// step closes, so the turn closes and the next turn opens.
func TestEngine_FailedToolRecordingResolvesCommittedCalls(t *testing.T) {
	const interrupted = "Error: interrupted before a result was committed"
	failure := errors.New("disk full")
	for _, test := range []struct {
		name   string
		failOn func(session.Record) bool
		want   map[string]string
		ran    int
	}{
		{name: "second call", failOn: func(record session.Record) bool {
			return record.Type == session.RecordToolCall && record.Call.ID == "call-2"
		}, want: map[string]string{"call-1": interrupted}},
		{name: "second result", failOn: func(record session.Record) bool {
			return record.Type == session.RecordToolResult && record.Result.CallID == "call-2" && !record.Result.IsError
		}, want: map[string]string{"call-1": "ok", "call-2": interrupted}, ran: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness, tool, _, log := batchHarness(t)
			failed := false
			journal := newJournal(barrierLog{Log: log, fail: func(record session.Record) error {
				if !failed && test.failOn(record) {
					failed = true
					return failure
				}
				return nil
			}})
			input := runInput{journal: journal, message: agentMessage(session.RoleUser, "run"), notices: noMessages, drain: noMessages}
			first := harness.engine.runTurn(context.Background(), input)
			// Recovery itself commits cleanly: the injected failure is the
			// only error the turn reports.
			if !errors.Is(first.Err, failure) || first.Err.Error() != failure.Error() || first.Outcome != session.OutcomeError || len(tool.seen) != test.ran {
				t.Fatalf("first = %+v, tool ran %d times", first, len(tool.seen))
			}
			events, err := journal.Events(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			results := toolResults(events)
			for callID, output := range test.want {
				result := results[callID]
				if result.Output != output {
					t.Fatalf("%s result = %+v", callID, result)
				}
				if output == interrupted && (!result.IsError || result.Error == nil || *result.Error != session.ToolOutcomeUnknown) {
					t.Fatalf("%s result = %+v", callID, result)
				}
			}
			if len(results) != len(test.want) {
				t.Fatalf("results = %+v", results)
			}
			next := harness.engine.runTurn(context.Background(), input)
			if next.Err != nil || next.Turn != 2 {
				t.Fatalf("next = %+v", next)
			}
		})
	}
}

// allowBroker approves every question.
type allowBroker struct{}

func (allowBroker) Ask(context.Context, approval.Question) session.ApprovalOutcome {
	return session.ApprovalAllowedOnce
}

// TestEngine_FailedDecisionRecordingCancelsTheQuestion fails the first
// approval/decided append on a real approval service and JSONL log. That
// call's result cannot commit while its question is open, so the step ends
// abnormally; the closeout cancels the open question before writing the
// interrupted results, and the next turn opens on the same live log.
func TestEngine_FailedDecisionRecordingCancelsTheQuestion(t *testing.T) {
	harness, _, manager, log := batchHarness(t)
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	approver := approval.New()
	if err := approver.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := approver.RegisterBroker(allowBroker{}, scope); err != nil {
		t.Fatal(err)
	}
	tools, err := appTool.New(approver)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	ran := 0
	if err := tools.Register(appTool.Define(appTool.Spec[engineArguments]{
		Name: "inspect", Description: "test tool", Parameters: appTool.Parameters{appTool.Optional("value", appTool.Number(""))},
		Approval: func(engineArguments) string { return "inspect" },
		Execute: func(context.Context, appTool.Invocation, engineArguments) (appTool.Result, error) {
			ran++
			return appTool.Text("ok"), nil
		},
	}), scope); err != nil {
		t.Fatal(err)
	}
	harness.engine.tools = tools
	failed := false
	journal := newJournal(barrierLog{Log: log, fail: func(record session.Record) error {
		if !failed && record.Type == session.RecordApprovalDecided {
			failed = true
			return errors.New("no space left on device")
		}
		return nil
	}})
	input := runInput{journal: journal, message: agentMessage(session.RoleUser, "run"), notices: noMessages, drain: noMessages}
	first := harness.engine.runTurn(context.Background(), input)
	// The rejected result is the only error: the closeout commits cleanly.
	if first.Outcome != session.OutcomeError || first.Err == nil || first.Err.Error() != "corrupt session: tool/result precedes approval decision" || ran != 1 {
		t.Fatalf("first = %+v, tool ran %d times", first, ran)
	}
	next := harness.engine.runTurn(context.Background(), input)
	if next.Err != nil || next.Text != "next answer" || next.Turn != 2 {
		t.Fatalf("next = %+v", next)
	}
	events, err := journal.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	decided := map[string][]session.ApprovalData{}
	for _, event := range events {
		if record := event.Record; record.Type == session.RecordApprovalAsked {
			asked = append(asked, record.Approval.ID)
		} else if record.Type == session.RecordApprovalDecided {
			decided[record.Approval.ID] = append(decided[record.Approval.ID], *record.Approval)
		}
	}
	// Each question has exactly one decision: the one whose commit failed
	// is cancelled as resume repair would, the other keeps the operator's.
	if len(asked) != 2 || !slices.Equal(decided[asked[0]], []session.ApprovalData{{ID: asked[0], Outcome: session.ApprovalCancelled}}) ||
		!slices.Equal(decided[asked[1]], []session.ApprovalData{{ID: asked[1], Outcome: session.ApprovalAllowedOnce, Source: "operator"}}) {
		t.Fatalf("asked %q, decided %+v", asked, decided)
	}
	for _, callID := range []string{"call-1", "call-2"} {
		if result := toolResults(events)[callID]; result.Output != "Error: interrupted before a result was committed" || result.Error == nil || *result.Error != session.ToolOutcomeUnknown {
			t.Fatalf("%s result = %+v", callID, result)
		}
	}
	if outcomes := turnOutcomes(events); !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeError, session.OutcomeCompleted}) {
		t.Fatalf("turn outcomes = %q", outcomes)
	}

	// Reopening validates the whole log and adds no repair records.
	if err := log.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := manager.Open(context.Background(), jsonl.OpenOptions{SessionID: "batch", Cwd: log.Header().Cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	after, err := reopened.Events(context.Background())
	if err != nil || len(after) != len(events) {
		t.Fatalf("reopened log has %d events, want %d: %v", len(after), len(events), err)
	}
}

// TestEngine_CloseoutReportsAnUnreadableLog fails a call append and then
// every log read: the closeout cannot find what the step left open, and the
// turn reports that failure alongside the original one.
func TestEngine_CloseoutReportsAnUnreadableLog(t *testing.T) {
	harness, _, _, log := batchHarness(t)
	failure, unreadable := errors.New("disk full"), errors.New("log unreadable")
	broken := false
	journal := newJournal(barrierLog{Log: log, fail: func(record session.Record) error {
		if record.Type == session.RecordToolCall && record.Call.ID == "call-2" {
			broken = true
			return failure
		}
		return nil
	}, events: func() error {
		if broken {
			return unreadable
		}
		return nil
	}})
	input := runInput{journal: journal, message: agentMessage(session.RoleUser, "run"), notices: noMessages, drain: noMessages}
	first := harness.engine.runTurn(context.Background(), input)
	if !errors.Is(first.Err, failure) || !errors.Is(first.Err, unreadable) || first.Outcome != session.OutcomeError {
		t.Fatalf("first = %+v", first)
	}
}
