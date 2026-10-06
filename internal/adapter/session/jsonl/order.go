package jsonl

import (
	"fmt"
	"reflect"
	"slices"

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
	plan       bool
}

func validateOrder(events []coresession.Event, requireClosed bool) (orderState, error) {
	state := orderState{}
	pendingCalls := map[string]*coresession.ToolCall{}
	approvalCalls := map[string]string{}
	seenApprovals := map[string]struct{}{}
	todoCalls := map[string]struct{}{}
	searchQueries := map[string][]string{}
	children := map[string]struct{}{}
	// notices holds queued notices until a user/message delivers them;
	// noticeIDs remembers every ID so none is queued twice.
	notices := map[string]coresession.Message{}
	noticeIDs := map[string]struct{}{}
	var goals coresession.GoalState
	for _, event := range events {
		record := event.Record
		if err := goals.Apply(record); err != nil {
			return state, orderError("invalid goal history at event %d: %v", event.Sequence, err)
		}
		switch record.Type {
		case coresession.RecordTurnStart:
			if state.turn != 0 || record.Turn != state.lastTurn+1 {
				return state, orderError("invalid turn/start %d", record.Turn)
			}
			state.turn, state.lastTurn, state.lastStep, state.assistant = record.Turn, record.Turn, 0, false
		case coresession.RecordUserMessage:
			if record.Turn != state.turn || record.Step != 0 && record.Step != state.step {
				return state, orderError("user/message outside active turn or step")
			}
			if id := record.Message.Source.NoticeID; id != "" {
				queued, owed := notices[id]
				if !owed {
					return state, orderError("user/message delivers notice %q that is not owed", id)
				}
				if !reflect.DeepEqual(queued, *record.Message) {
					return state, orderError("user/message differs from queued notice %q", id)
				}
				delete(notices, id)
			}
		case coresession.RecordNoticeQueued:
			// A notice is owed from its own commit on, independent of turns.
			id := record.Message.Source.NoticeID
			if _, exists := noticeIDs[id]; exists {
				return state, orderError("duplicate notice/queued %q", id)
			}
			noticeIDs[id] = struct{}{}
			notices[id] = *record.Message
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
			if _, exists := pendingCalls[record.Call.ID]; exists {
				return state, orderError("duplicate pending call %q", record.Call.ID)
			}
			pendingCalls[record.Call.ID] = record.Call
			state.calls = append(state.calls, record.Call.ID)
		case coresession.RecordApprovalAsked:
			call := pendingCalls[record.Approval.CallID]
			if record.Turn != state.turn || record.Step != state.step || call == nil || call.Name != record.Approval.ToolName || call.ArgumentsOmitted {
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
			call := pendingCalls[record.Result.CallID]
			if call == nil {
				return state, orderError("tool/result has no pending call")
			}
			if call.ArgumentsOmitted && !record.Result.IsError {
				return state, orderError("omitted tool arguments require an error result")
			}
			if meta := record.Result.Meta; meta != nil && meta.Tool() != call.Name {
				return state, orderError("tool/result metadata does not belong to %q", call.Name)
			}
			for _, callID := range approvalCalls {
				if callID == record.Result.CallID {
					return state, orderError("tool/result precedes approval decision")
				}
			}
			delete(pendingCalls, record.Result.CallID)
			delete(todoCalls, record.Result.CallID)
			delete(searchQueries, record.Result.CallID)
			state.calls = remove(state.calls, record.Result.CallID)
		case coresession.RecordTodoWrite:
			if record.Turn != state.turn || record.Step != state.step {
				return state, orderError("todo/write outside active step")
			}
			call := pendingCalls[record.Todo.CallID]
			if call == nil || call.Name != "todo_write" || call.ArgumentsOmitted {
				return state, orderError("todo/write does not name a pending todo_write call")
			}
			if _, exists := todoCalls[record.Todo.CallID]; exists {
				return state, orderError("duplicate todo/write for call %q", record.Todo.CallID)
			}
			todoCalls[record.Todo.CallID] = struct{}{}
		case coresession.RecordWebSearchRequest:
			if record.Turn != state.turn || record.Step != state.step || state.step == 0 {
				return state, orderError("web/search-request outside active step")
			}
			data := record.Search
			call := pendingCalls[data.CallID]
			if call == nil || call.Name != "web_search" || call.ArgumentsOmitted {
				return state, orderError("web/search-request does not name a pending web_search call")
			}
			queries := searchQueries[data.CallID]
			if data.Index != len(queries)+1 || slices.Contains(queries, data.Query) {
				return state, orderError("web/search-request query order is invalid")
			}
			searchQueries[data.CallID] = append(queries, data.Query)
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
		case coresession.RecordCompactionPrune:
			// Pruning lands before a summary is chosen: outside any step and
			// compaction transaction, in the active turn or between turns.
			// The surface fold below checks what it replaces.
			if record.Turn != state.turn || state.step != 0 || state.compaction != "" {
				return state, orderError("compaction/prune outside a compaction boundary")
			}
		case coresession.RecordStepEnd:
			if record.Turn != state.turn || record.Step != state.step || len(state.calls) != 0 || len(state.approvals) != 0 || state.compaction != "" {
				return state, orderError("step/end has unfinished work")
			}
			state.step = 0
		case coresession.RecordTurnEnd:
			if record.Turn != state.turn || state.step != 0 || state.compaction != "" {
				return state, orderError("turn/end has unfinished work")
			}
			if record.Outcome == coresession.OutcomeMaxTokens && !state.assistant {
				return state, orderError("max_tokens turn/end has no assistant completion")
			}
			state.turn = 0
		case coresession.RecordPlanMode:
			// A mode change takes effect at a step boundary: between turns
			// (turn 0) or inside the active turn before its next step. A
			// record that repeats the current mode is never written.
			if record.Turn != state.turn || state.step != 0 || record.Plan.Active == state.plan {
				return state, orderError("plan/mode is not a mode change at a step boundary")
			}
			state.plan = record.Plan.Active
		case coresession.RecordSubagentDescriptor:
			// A descriptor is the first record a child writes itself, after
			// the closed prefix it inherited from a forked parent.
			if state.turn != 0 || record.Subagent.Inherited != event.Sequence-1 {
				return state, orderError("subagent/descriptor does not follow its inherited prefix")
			}
		case coresession.RecordSubagentCatalog:
			if record.Turn != state.turn || record.Step != state.step {
				return state, orderError("subagent/catalog outside active step")
			}
			if _, exists := children[record.Catalog.SessionID]; exists {
				return state, orderError("duplicate subagent/catalog for %q", record.Catalog.SessionID)
			}
			children[record.Catalog.SessionID] = struct{}{}
		case coresession.RecordApprovalPolicy, coresession.RecordGoalChange:
			// Durable metadata is independent of the model surface. A goal
			// change may be committed at any point by a person, a tool, or
			// the round driver; the goal fold above orders it causally.
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
