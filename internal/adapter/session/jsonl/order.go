package jsonl

import (
	"fmt"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

type orderState struct {
	lastTurn   uint64
	turn       uint64
	step       uint64
	lastStep   uint64
	assistant  bool
	calls      []string
	approvals  []string
	compaction string
}

func validateOrder(events []coresession.Event, requireClosed bool) (orderState, error) {
	state := orderState{}
	callNames := map[string]string{}
	approvalCalls := map[string]string{}
	seenApprovals := map[string]struct{}{}
	for _, event := range events {
		record := event.Record
		switch record.Type {
		case coresession.RecordTurnStart:
			if state.turn != 0 || record.Turn != state.lastTurn+1 {
				return state, orderError("invalid turn/start %d", record.Turn)
			}
			state.turn, state.lastTurn, state.lastStep = record.Turn, record.Turn, 0
		case coresession.RecordUserMessage:
			if record.Turn != state.turn || record.Step != 0 && record.Step != state.step {
				return state, orderError("user/message outside active turn or step")
			}
		case coresession.RecordStepStart:
			if record.Turn != state.turn || state.step != 0 || record.Step != state.lastStep+1 {
				return state, orderError("invalid step/start %d", record.Step)
			}
			state.step, state.lastStep, state.assistant = record.Step, record.Step, false
		case coresession.RecordRequestHeader, coresession.RecordAssistantChunk:
			if record.Turn != state.turn || record.Step != state.step {
				return state, orderError("%s outside active step", record.Type)
			}
		case coresession.RecordAssistantMessage:
			if record.Turn != state.turn || record.Step != state.step || state.assistant {
				return state, orderError("assistant/message is not the step completion")
			}
			state.assistant = true
		case coresession.RecordToolCall:
			if record.Turn != state.turn || record.Step != state.step || !state.assistant {
				return state, orderError("tool/call precedes its assistant message")
			}
			if _, exists := callNames[record.Call.ID]; exists {
				return state, orderError("duplicate pending call %q", record.Call.ID)
			}
			callNames[record.Call.ID] = record.Call.Name
			state.calls = append(state.calls, record.Call.ID)
		case coresession.RecordApprovalAsked:
			if record.Turn != state.turn || record.Step != state.step || callNames[record.Approval.CallID] != record.Approval.ToolName {
				return state, orderError("approval/asked does not name a pending call")
			}
			if _, exists := seenApprovals[record.Approval.ID]; exists {
				return state, orderError("duplicate approval ID %q", record.Approval.ID)
			}
			seenApprovals[record.Approval.ID] = struct{}{}
			approvalCalls[record.Approval.ID] = record.Approval.CallID
			state.approvals = append(state.approvals, record.Approval.ID)
		case coresession.RecordApprovalDecided:
			if record.Turn != state.turn || record.Step != state.step {
				return state, orderError("approval/decided outside active step")
			}
			if _, exists := approvalCalls[record.Approval.ID]; !exists {
				return state, orderError("approval/decided has no question")
			}
			delete(approvalCalls, record.Approval.ID)
			state.approvals = remove(state.approvals, record.Approval.ID)
		case coresession.RecordToolResult:
			if record.Turn != state.turn || record.Step != state.step {
				return state, orderError("tool/result outside active step")
			}
			if _, exists := callNames[record.Result.CallID]; !exists {
				return state, orderError("tool/result has no pending call")
			}
			for _, callID := range approvalCalls {
				if callID == record.Result.CallID {
					return state, orderError("tool/result precedes approval decision")
				}
			}
			delete(callNames, record.Result.CallID)
			state.calls = remove(state.calls, record.Result.CallID)
		case coresession.RecordRetry, coresession.RecordRetryStarted:
			if record.Turn != state.turn || record.Step != state.step || state.assistant {
				return state, orderError("%s is not a request recovery fact", record.Type)
			}
		case coresession.RecordCompactionStart:
			if state.compaction != "" || record.Turn != 0 && record.Turn != state.turn {
				return state, orderError("nested or misplaced compaction/start")
			}
			state.compaction = record.Compaction.ID
		case coresession.RecordCompactionSummary:
			if record.Compaction.ID != state.compaction {
				return state, orderError("compaction/summary has no matching start")
			}
		case coresession.RecordCompactionEnd:
			if record.Compaction.ID != state.compaction {
				return state, orderError("compaction/end has no matching start")
			}
			state.compaction = ""
		case coresession.RecordStepEnd:
			if record.Turn != state.turn || record.Step != state.step || len(state.calls) != 0 || len(state.approvals) != 0 || state.compaction != "" {
				return state, orderError("step/end has unfinished work")
			}
			state.step, state.assistant = 0, false
		case coresession.RecordTurnEnd:
			if record.Turn != state.turn || state.step != 0 || state.compaction != "" {
				return state, orderError("turn/end has unfinished work")
			}
			state.turn = 0
		case coresession.RecordApprovalPolicy, coresession.RecordSubagentDescriptor:
			// Durable metadata is independent of the model surface.
		}
	}
	if _, err := coresession.Surface(events); err != nil {
		return state, fmt.Errorf("%w: invalid replay surface: %w", ErrCorruptSession, err)
	}
	if requireClosed && (state.turn != 0 || state.compaction != "") {
		return state, orderError("session has an interrupted tail")
	}
	return state, nil
}

func remove(values []string, target string) []string {
	for index, value := range values {
		if value == target {
			return append(values[:index], values[index+1:]...)
		}
	}
	return values
}

func orderError(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrCorruptSession, fmt.Sprintf(format, values...))
}
