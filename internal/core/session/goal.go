package session

import (
	"fmt"
	"strings"

	"github.com/jinyule/nano-harness/internal/core/text"
)

// GoalSource is the message source kind of an admitted automatic goal round.
// Such a user/message also names the goal revision and round it continues.
const GoalSource = "goal"

const (
	// MaxGoalRounds is the largest round cap, the upstream positive safe integer bound.
	MaxGoalRounds = 1<<53 - 1
	// MaxGoalTextBytes bounds a goal objective and a blocker explanation.
	MaxGoalTextBytes = 16 << 10
	// maxGoalCodeBytes bounds a blocker classification code.
	maxGoalCodeBytes = 64
)

// GoalPhase is the durable continuation phase of a goal. Whether this process
// may continue an active goal automatically is separate and never persisted.
type GoalPhase string

const (
	// GoalActive identifies a goal that may receive automatic rounds.
	GoalActive GoalPhase = "active"
	// GoalPaused identifies a goal a person or the driver stopped.
	GoalPaused GoalPhase = "paused"
	// GoalBlocked identifies a goal stopped with a recorded blocker.
	GoalBlocked GoalPhase = "blocked"
	// GoalComplete identifies an achieved goal; a new goal may replace it.
	GoalComplete GoalPhase = "complete"
)

// GoalOperation is the verb one goal/change committed.
type GoalOperation string

const (
	// GoalOpCreate starts a fresh active goal.
	GoalOpCreate GoalOperation = "create"
	// GoalOpEdit replaces the objective and/or round cap without changing the phase.
	GoalOpEdit GoalOperation = "edit"
	// GoalOpPause moves an active goal to paused.
	GoalOpPause GoalOperation = "pause"
	// GoalOpResume moves an active, paused, or blocked goal with remaining rounds to active.
	GoalOpResume GoalOperation = "resume"
	// GoalOpComplete marks an unfinished goal complete.
	GoalOpComplete GoalOperation = "complete"
	// GoalOpBlock moves an active goal to blocked with a reason.
	GoalOpBlock GoalOperation = "block"
	// GoalOpClear removes the current goal, leaving a tombstone.
	GoalOpClear GoalOperation = "clear"
)

// GoalRef is the compare-and-set identity of one goal revision.
type GoalRef struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

// GoalBlockReason explains a blocked goal: a stable lower-kebab-case code
// chosen by the blocking policy and a trimmed human-readable message.
type GoalBlockReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GoalSnapshot is the complete durable state of one goal revision.
// BlockedReason is present exactly while Phase is blocked.
type GoalSnapshot struct {
	ID            string           `json:"id"`
	Revision      uint64           `json:"revision"`
	Objective     string           `json:"objective"`
	Phase         GoalPhase        `json:"phase"`
	BlockedReason *GoalBlockReason `json:"blocked_reason,omitempty"`
	MaxRounds     uint64           `json:"max_goal_rounds"`
}

// Ref returns the snapshot's compare-and-set identity.
func (snapshot GoalSnapshot) Ref() GoalRef {
	return GoalRef{ID: snapshot.ID, Revision: snapshot.Revision}
}

// GoalChange is one goal/change payload: the full post-mutation snapshot
// with its preserved counters and timestamps, or a clear tombstone that
// names the revision one past the cleared snapshot.
type GoalChange struct {
	Operation       GoalOperation `json:"operation"`
	Snapshot        *GoalSnapshot `json:"snapshot,omitempty"`
	RoundsStarted   uint64        `json:"rounds_started,omitempty"`
	CreatedAtUnixMS int64         `json:"created_at_unix_ms,omitempty"`
	UpdatedAtUnixMS int64         `json:"updated_at_unix_ms,omitempty"`
	Cleared         *GoalRef      `json:"cleared,omitempty"`
	ClearedAtUnixMS int64         `json:"cleared_at_unix_ms,omitempty"`
}

// GoalState is the strict replay fold of the goal facts in a log prefix.
// The zero value is the state of a log with no goal facts. Apply mutates the
// state in place, so copies must not be applied independently.
type GoalState struct {
	// Goal is the current goal, nil before the first create and after a clear.
	Goal *GoalSnapshot
	// RoundsStarted is the highest admitted round of the current goal.
	RoundsStarted uint64
	// CreatedAtUnixMS and UpdatedAtUnixMS time the current goal.
	CreatedAtUnixMS int64
	UpdatedAtUnixMS int64
	// seen retains every goal ID created in the session so none is reused.
	seen map[string]struct{}
}

// ProjectGoal folds a log through the strict goal rules.
func ProjectGoal(events []Event) (GoalState, error) {
	var state GoalState
	for _, event := range events {
		if err := state.Apply(event.Record); err != nil {
			return GoalState{}, fmt.Errorf("event %d: %w", event.Sequence, err)
		}
	}
	return state, nil
}

// Apply validates one shape-valid record against the preceding goal state
// and folds it in. Only goal/change and goal-sourced user/message records
// affect the state; a rejected record leaves the state unchanged.
func (state *GoalState) Apply(record Record) error {
	switch {
	case record.Type == RecordGoalChange:
		return state.applyChange(*record.Goal)
	case record.Type == RecordUserMessage && record.Message.Source.Kind == GoalSource:
		source := record.Message.Source
		current := state.Goal
		if current == nil || current.Phase != GoalActive || source.GoalID != current.ID || source.GoalRevision != current.Revision || source.GoalRound != state.RoundsStarted+1 || source.GoalRound > current.MaxRounds {
			return invalid("goal round is not the next admitted round of the active goal")
		}
		state.RoundsStarted = source.GoalRound
	}
	return nil
}

func (state *GoalState) applyChange(change GoalChange) error {
	current := state.Goal
	if change.Operation == GoalOpClear {
		if current == nil || *change.Cleared != (GoalRef{ID: current.ID, Revision: current.Revision + 1}) || change.ClearedAtUnixMS < state.UpdatedAtUnixMS {
			return invalid("goal clear must tombstone the next revision of the current goal")
		}
		seen := state.seen
		*state = GoalState{seen: seen}
		return nil
	}
	next := change.Snapshot
	if change.Operation == GoalOpCreate {
		_, reused := state.seen[next.ID]
		if next.Revision != 1 || next.Phase != GoalActive || change.RoundsStarted != 0 || current != nil && current.Phase != GoalComplete || reused {
			return invalid("goal create requires a fresh active revision-one goal with zero rounds")
		}
		if state.seen == nil {
			state.seen = map[string]struct{}{}
		}
		state.seen[next.ID] = struct{}{}
	} else if err := state.checkTransition(change); err != nil {
		return err
	}
	snapshot := cloneGoalSnapshot(*next)
	state.Goal = &snapshot
	state.RoundsStarted = change.RoundsStarted
	state.CreatedAtUnixMS, state.UpdatedAtUnixMS = change.CreatedAtUnixMS, change.UpdatedAtUnixMS
	return nil
}

// checkTransition validates a non-create snapshot against the current goal.
func (state *GoalState) checkTransition(change GoalChange) error {
	current, next := state.Goal, change.Snapshot
	if current == nil {
		return invalid("goal %s requires a current goal", change.Operation)
	}
	if next.ID != current.ID || next.Revision != current.Revision+1 {
		return invalid("goal %s must advance the current goal by one revision", change.Operation)
	}
	if change.CreatedAtUnixMS != state.CreatedAtUnixMS || change.UpdatedAtUnixMS < state.UpdatedAtUnixMS || change.RoundsStarted != state.RoundsStarted {
		return invalid("goal %s does not preserve the current counters and timestamps", change.Operation)
	}
	if change.Operation == GoalOpEdit {
		if next.Phase != current.Phase || !sameBlockReason(next.BlockedReason, current.BlockedReason) {
			return invalid("goal edit cannot change phase or blocked reason")
		}
		return nil
	}
	if next.Objective != current.Objective || next.MaxRounds != current.MaxRounds {
		return invalid("goal %s cannot change objective or max_goal_rounds", change.Operation)
	}
	var valid bool
	switch change.Operation {
	case GoalOpPause:
		valid = current.Phase == GoalActive && next.Phase == GoalPaused
	case GoalOpResume:
		valid = current.Phase != GoalComplete && next.Phase == GoalActive && state.RoundsStarted < next.MaxRounds
	case GoalOpComplete:
		valid = current.Phase != GoalComplete && next.Phase == GoalComplete
	case GoalOpBlock:
		valid = current.Phase == GoalActive && next.Phase == GoalBlocked
	case GoalOpCreate, GoalOpEdit, GoalOpClear:
		// applyChange handles these operations before checking a transition.
	}
	if !valid {
		return invalid("goal %s has an invalid phase transition", change.Operation)
	}
	return nil
}

func sameBlockReason(left, right *GoalBlockReason) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func cloneGoalSnapshot(snapshot GoalSnapshot) GoalSnapshot {
	if snapshot.BlockedReason != nil {
		reason := *snapshot.BlockedReason
		snapshot.BlockedReason = &reason
	}
	return snapshot
}

func cloneGoalChange(change GoalChange) GoalChange {
	if change.Snapshot != nil {
		snapshot := cloneGoalSnapshot(*change.Snapshot)
		change.Snapshot = &snapshot
	}
	if change.Cleared != nil {
		cleared := *change.Cleared
		change.Cleared = &cleared
	}
	return change
}

// ValidGoalCode reports whether code is a lower-kebab-case blocker code:
// lowercase ASCII words of letters and digits, the first starting with a
// letter, joined by single hyphens.
func ValidGoalCode(code string) bool {
	if code == "" || len(code) > maxGoalCodeBytes || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for word := range strings.SplitSeq(code, "-") {
		if word == "" || strings.ContainsFunc(word, func(char rune) bool { return (char < 'a' || char > 'z') && (char < '0' || char > '9') }) {
			return false
		}
	}
	return true
}

// validGoalText reports whether value is trimmed, non-empty, and within the goal text bound.
func validGoalText(value string) bool {
	return value != "" && value == text.TrimSpace(value) && len(value) <= MaxGoalTextBytes
}

// validGoalID applies the goal tool's ECMAScript trimming rule to durable references.
func validGoalID(value string) bool {
	return value != "" && len(value) <= 128 && value == text.TrimSpace(value) && !strings.ContainsAny(value, "\r\n")
}

func (record Record) requireGoal() error {
	if record.Turn != 0 || record.Step != 0 || record.Goal == nil || record.hasExtras("goal") {
		return invalid("goal/change shape is invalid")
	}
	change := record.Goal
	switch change.Operation {
	case GoalOpClear:
		if change.Snapshot != nil || change.RoundsStarted != 0 || change.CreatedAtUnixMS != 0 || change.UpdatedAtUnixMS != 0 || change.Cleared == nil || change.ClearedAtUnixMS <= 0 {
			return invalid("goal clear fields are invalid")
		}
		if !validGoalID(change.Cleared.ID) || change.Cleared.Revision == 0 {
			return invalid("goal clear tombstone is invalid")
		}
		return nil
	case GoalOpCreate, GoalOpEdit, GoalOpPause, GoalOpResume, GoalOpComplete, GoalOpBlock:
		return requireGoalSnapshot(*change)
	default:
		return invalid("unknown goal operation %q", change.Operation)
	}
}

func requireGoalSnapshot(change GoalChange) error {
	if change.Snapshot == nil || change.Cleared != nil || change.ClearedAtUnixMS != 0 || change.CreatedAtUnixMS <= 0 || change.UpdatedAtUnixMS < change.CreatedAtUnixMS {
		return invalid("goal snapshot change fields are invalid")
	}
	snapshot := change.Snapshot
	if !validGoalID(snapshot.ID) || snapshot.Revision == 0 || !validGoalText(snapshot.Objective) || snapshot.MaxRounds == 0 || snapshot.MaxRounds > MaxGoalRounds {
		return invalid("goal snapshot fields are invalid")
	}
	switch snapshot.Phase {
	case GoalActive, GoalPaused, GoalComplete:
		if snapshot.BlockedReason != nil {
			return invalid("goal blocked reason is only valid while blocked")
		}
	case GoalBlocked:
		if snapshot.BlockedReason == nil || !ValidGoalCode(snapshot.BlockedReason.Code) || !validGoalText(snapshot.BlockedReason.Message) {
			return invalid("goal blocked reason is invalid")
		}
	default:
		return invalid("unknown goal phase %q", snapshot.Phase)
	}
	return nil
}

// validateGoalSource checks the round attribution carried only by goal-sourced user messages.
func validateGoalSource(source MessageSource, user bool) error {
	if source.Kind != GoalSource {
		if source.GoalID != "" || source.GoalRevision != 0 || source.GoalRound != 0 {
			return invalid("goal round attribution requires the goal source")
		}
		return nil
	}
	if !user || !validGoalID(source.GoalID) || source.GoalRevision == 0 || source.GoalRound == 0 || source.GoalRound > MaxGoalRounds {
		return invalid("goal round source is invalid")
	}
	return nil
}
