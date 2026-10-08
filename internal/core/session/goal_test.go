package session

import (
	"errors"
	"strings"
	"testing"
)

func TestGoalChange_ECMAScriptDurableTextAndIDs(t *testing.T) {
	for _, value := range []string{"\u0085", "\u0085x\u0085", "\ufeff", "\ufeffx", "x\ufeff"} {
		for _, field := range []string{"objective", "reason", "id", "cleared id", "source id"} {
			t.Run(field+value, func(t *testing.T) {
				record := goalChange(GoalOpCreate, &GoalSnapshot{ID: "g", Revision: 1, Objective: "ship", Phase: GoalActive, MaxRounds: 3}, 0, 1, 1)
				switch field {
				case "objective":
					record.Goal.Snapshot.Objective = value
				case "reason":
					record.Goal.Operation = GoalOpBlock
					record.Goal.Snapshot.Phase = GoalBlocked
					record.Goal.Snapshot.BlockedReason = &GoalBlockReason{Code: "model-reported", Message: value}
				case "id":
					record.Goal.Snapshot.ID = value
				case "cleared id":
					record = Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, Cleared: &GoalRef{ID: value, Revision: 2}, ClearedAtUnixMS: 1}}
				case "source id":
					record = goalRound(value, 1, 1)
				}
				err := record.Validate()
				valid := strings.Contains(value, "\u0085")
				if (err == nil) != valid {
					t.Errorf("value=%q error=%v want valid=%t", value, err, valid)
				}
			})
		}
	}
}

func goalSnapshot(revision uint64, phase GoalPhase) *GoalSnapshot {
	snapshot := &GoalSnapshot{ID: "goal-1", Revision: revision, Objective: "ship it", Phase: phase, MaxRounds: 3}
	if phase == GoalBlocked {
		snapshot.BlockedReason = &GoalBlockReason{Code: "model-reported", Message: "needs a key"}
	}
	return snapshot
}

func goalChange(operation GoalOperation, snapshot *GoalSnapshot, rounds uint64, created, updated int64) Record {
	return Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: operation, Snapshot: snapshot, RoundsStarted: rounds, CreatedAtUnixMS: created, UpdatedAtUnixMS: updated}}
}

func goalClear(id string, revision uint64, at int64) Record {
	return Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, Cleared: &GoalRef{ID: id, Revision: revision}, ClearedAtUnixMS: at}}
}

func goalRound(id string, revision, round uint64) Record {
	return Record{Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Source: MessageSource{Kind: GoalSource, GoalID: id, GoalRevision: revision, GoalRound: round}, Content: []ContentBlock{{Type: ContentText, Text: "<goal_round>"}}}}
}

func applyAll(t *testing.T, records ...Record) (GoalState, error) {
	t.Helper()
	events := make([]Event, len(records))
	for index, record := range records {
		if err := record.Validate(); err != nil {
			t.Fatalf("record %d is not shape-valid: %v", index, err)
		}
		events[index] = Event{Sequence: uint64(index + 1), Record: record}
	}
	return ProjectGoal(events)
}

func TestGoalChange_ValidatesShapes(t *testing.T) {
	blocked := goalSnapshot(2, GoalBlocked)
	blocked.BlockedReason = &GoalBlockReason{Code: "round-limit2", Message: strings.Repeat("x", MaxGoalTextBytes)}
	maximal := goalSnapshot(1, GoalActive)
	maximal.MaxRounds, maximal.Objective, maximal.ID = MaxGoalRounds, strings.Repeat("o", MaxGoalTextBytes), strings.Repeat("i", 128)
	for name, record := range map[string]Record{
		"create":   goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 10, 10),
		"maximal":  goalChange(GoalOpEdit, maximal, 7, 10, 20),
		"blocked":  goalChange(GoalOpBlock, blocked, 1, 10, 11),
		"paused":   goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 0, 10, 10),
		"complete": goalChange(GoalOpComplete, goalSnapshot(2, GoalComplete), 0, 10, 10),
		"resume":   goalChange(GoalOpResume, goalSnapshot(2, GoalActive), 0, 10, 10),
		"clear":    goalClear("goal-1", 2, 1),
		"round":    goalRound("goal-1", 1, MaxGoalRounds),
	} {
		if err := record.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	snapshot := func(mutate func(*GoalSnapshot)) *GoalSnapshot {
		value := goalSnapshot(1, GoalActive)
		mutate(value)
		return value
	}
	for name, test := range map[string]struct {
		record  Record
		message string
	}{
		"turn":                {Record{Type: RecordGoalChange, Turn: 1, Goal: goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 1, 1).Goal}, "shape"},
		"step":                {Record{Type: RecordGoalChange, Step: 1, Goal: goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 1, 1).Goal}, "shape"},
		"missing payload":     {Record{Type: RecordGoalChange}, "shape"},
		"extra field":         {Record{Type: RecordGoalChange, Goal: goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 1, 1).Goal, Plan: &PlanMode{}}, "shape"},
		"unknown operation":   {goalChange("restart", goalSnapshot(1, GoalActive), 0, 1, 1), `unknown goal operation "restart"`},
		"missing snapshot":    {goalChange(GoalOpCreate, nil, 0, 1, 1), "snapshot change fields"},
		"snapshot cleared":    {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpEdit, Snapshot: goalSnapshot(1, GoalActive), CreatedAtUnixMS: 1, UpdatedAtUnixMS: 1, Cleared: &GoalRef{ID: "goal-1", Revision: 1}}}, "snapshot change fields"},
		"snapshot cleared at": {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpEdit, Snapshot: goalSnapshot(1, GoalActive), CreatedAtUnixMS: 1, UpdatedAtUnixMS: 1, ClearedAtUnixMS: 1}}, "snapshot change fields"},
		"zero created":        {goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 0, 0), "snapshot change fields"},
		"update precedes":     {goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 5, 4), "snapshot change fields"},
		"bad id":              {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.ID = " goal" }), 0, 1, 1), "snapshot fields"},
		"zero revision":       {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.Revision = 0 }), 0, 1, 1), "snapshot fields"},
		"empty objective":     {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.Objective = "" }), 0, 1, 1), "snapshot fields"},
		"untrimmed":           {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.Objective = "x " }), 0, 1, 1), "snapshot fields"},
		"long objective":      {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.Objective = strings.Repeat("o", MaxGoalTextBytes+1) }), 0, 1, 1), "snapshot fields"},
		"zero cap":            {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.MaxRounds = 0 }), 0, 1, 1), "snapshot fields"},
		"unsafe cap":          {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.MaxRounds = MaxGoalRounds + 1 }), 0, 1, 1), "snapshot fields"},
		"unknown phase":       {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) { value.Phase = "done" }), 0, 1, 1), `unknown goal phase "done"`},
		"reason not blocked": {goalChange(GoalOpCreate, snapshot(func(value *GoalSnapshot) {
			value.BlockedReason = &GoalBlockReason{Code: "x", Message: "y"}
		}), 0, 1, 1), "only valid while blocked"},
		"blocked no reason": {goalChange(GoalOpBlock, snapshot(func(value *GoalSnapshot) { value.Phase = GoalBlocked }), 0, 1, 1), "blocked reason is invalid"},
		"blocked bad code": {goalChange(GoalOpBlock, snapshot(func(value *GoalSnapshot) {
			value.Phase, value.BlockedReason = GoalBlocked, &GoalBlockReason{Code: "Model", Message: "y"}
		}), 0, 1, 1), "blocked reason is invalid"},
		"blocked bad message": {goalChange(GoalOpBlock, snapshot(func(value *GoalSnapshot) {
			value.Phase, value.BlockedReason = GoalBlocked, &GoalBlockReason{Code: "model", Message: " y"}
		}), 0, 1, 1), "blocked reason is invalid"},
		"clear snapshot":              {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, Snapshot: goalSnapshot(1, GoalActive), Cleared: &GoalRef{ID: "goal-1", Revision: 2}, ClearedAtUnixMS: 1}}, "clear fields"},
		"clear rounds":                {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, RoundsStarted: 1, Cleared: &GoalRef{ID: "goal-1", Revision: 2}, ClearedAtUnixMS: 1}}, "clear fields"},
		"clear created":               {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, CreatedAtUnixMS: 1, Cleared: &GoalRef{ID: "goal-1", Revision: 2}, ClearedAtUnixMS: 1}}, "clear fields"},
		"clear updated":               {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, UpdatedAtUnixMS: 1, Cleared: &GoalRef{ID: "goal-1", Revision: 2}, ClearedAtUnixMS: 1}}, "clear fields"},
		"clear no tombstone":          {Record{Type: RecordGoalChange, Goal: &GoalChange{Operation: GoalOpClear, ClearedAtUnixMS: 1}}, "clear fields"},
		"clear zero time":             {goalClear("goal-1", 2, 0), "clear fields"},
		"clear bad id":                {goalClear("", 2, 1), "tombstone"},
		"clear zero revision":         {goalClear("goal-1", 0, 1), "tombstone"},
		"round without id":            {goalRound("", 1, 1), "goal round source"},
		"round zero revision":         {goalRound("goal-1", 0, 1), "goal round source"},
		"round zero round":            {goalRound("goal-1", 1, 0), "goal round source"},
		"round unsafe round":          {goalRound("goal-1", 1, MaxGoalRounds+1), "goal round source"},
		"round attribution elsewhere": {Record{Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Source: MessageSource{Kind: "user", GoalRound: 1}, Content: []ContentBlock{{Type: ContentText, Text: "x"}}}}, "requires the goal source"},
		"assistant goal source":       {Record{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: &Message{Role: RoleAssistant, Source: MessageSource{Kind: GoalSource, GoalID: "goal-1", GoalRevision: 1, GoalRound: 1}}}, "goal round source"},
	} {
		err := test.record.Validate()
		if !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), test.message) {
			t.Errorf("%s: got %v, want %q", name, err, test.message)
		}
	}
	if err := (Record{Type: RecordTurnStart, Turn: 1, Goal: goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 1, 1).Goal}).Validate(); err == nil {
		t.Fatal("turn/start accepted an unrelated goal payload")
	}
	if err := (Record{Type: RecordToolCall, Turn: 1, Step: 1, Call: &ToolCall{ID: "c", Name: "t", Arguments: []byte(`{}`)}, Goal: &GoalChange{}}).Validate(); err == nil {
		t.Fatal("tool/call accepted an unrelated goal payload")
	}
}

func TestValidGoalCode_AcceptsOnlyLowerKebabCase(t *testing.T) {
	for code, want := range map[string]bool{
		"model-reported": true, "round-limit": true, "a": true, "a1-2b": true, strings.Repeat("a", 64): true,
		"": false, "1a": false, "-a": false, "a-": false, "a--b": false, "A": false, "a_b": false, "a b": false, strings.Repeat("a", 65): false,
	} {
		if ValidGoalCode(code) != want {
			t.Errorf("ValidGoalCode(%q) = %t", code, !want)
		}
	}
}

func TestProjectGoal_FoldsTheLifecycle(t *testing.T) {
	state, err := applyAll(t,
		goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 10, 10),
		goalRound("goal-1", 1, 1),
		goalChange(GoalOpEdit, &GoalSnapshot{ID: "goal-1", Revision: 2, Objective: "ship more", Phase: GoalActive, MaxRounds: 5}, 1, 10, 12),
		goalRound("goal-1", 2, 2),
		goalChange(GoalOpPause, &GoalSnapshot{ID: "goal-1", Revision: 3, Objective: "ship more", Phase: GoalPaused, MaxRounds: 5}, 2, 10, 12),
		goalChange(GoalOpResume, &GoalSnapshot{ID: "goal-1", Revision: 4, Objective: "ship more", Phase: GoalActive, MaxRounds: 5}, 2, 10, 13),
		goalChange(GoalOpBlock, &GoalSnapshot{ID: "goal-1", Revision: 5, Objective: "ship more", Phase: GoalBlocked, BlockedReason: &GoalBlockReason{Code: "round-limit", Message: "stop"}, MaxRounds: 5}, 2, 10, 13),
		goalChange(GoalOpEdit, &GoalSnapshot{ID: "goal-1", Revision: 6, Objective: "ship most", Phase: GoalBlocked, BlockedReason: &GoalBlockReason{Code: "round-limit", Message: "stop"}, MaxRounds: 5}, 2, 10, 14),
		goalChange(GoalOpResume, &GoalSnapshot{ID: "goal-1", Revision: 7, Objective: "ship most", Phase: GoalActive, MaxRounds: 5}, 2, 10, 15),
		goalChange(GoalOpComplete, &GoalSnapshot{ID: "goal-1", Revision: 8, Objective: "ship most", Phase: GoalComplete, MaxRounds: 5}, 2, 10, 15),
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.Goal.Revision != 8 || state.Goal.Phase != GoalComplete || state.RoundsStarted != 2 || state.CreatedAtUnixMS != 10 || state.UpdatedAtUnixMS != 15 || state.Goal.Ref() != (GoalRef{ID: "goal-1", Revision: 8}) {
		t.Fatalf("state = %+v %+v", state, state.Goal)
	}
	replaced, err := applyAll(t,
		goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 10, 10),
		goalChange(GoalOpComplete, goalSnapshot(2, GoalComplete), 0, 10, 11),
		goalChange(GoalOpCreate, &GoalSnapshot{ID: "goal-2", Revision: 1, Objective: "next", Phase: GoalActive, MaxRounds: 1}, 0, 12, 12),
		goalRound("goal-2", 1, 1),
		goalChange(GoalOpPause, &GoalSnapshot{ID: "goal-2", Revision: 2, Objective: "next", Phase: GoalPaused, MaxRounds: 1}, 1, 12, 12),
		goalClear("goal-2", 3, 12),
	)
	if err != nil || replaced.Goal != nil || replaced.RoundsStarted != 0 || replaced.CreatedAtUnixMS != 0 {
		t.Fatalf("cleared state = %+v, %v", replaced, err)
	}
	empty, err := applyAll(t, Record{Type: RecordUserMessage, Turn: 1, Message: &Message{Role: RoleUser, Source: MessageSource{Kind: "user"}, Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}})
	if err != nil || empty.Goal != nil {
		t.Fatalf("ordinary input changed goal state: %+v, %v", empty, err)
	}
}

func TestProjectGoal_DetachesTheSnapshot(t *testing.T) {
	record := goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 10, 10)
	state, err := applyAll(t, record)
	if err != nil {
		t.Fatal(err)
	}
	record.Goal.Snapshot.Objective = "mutated"
	if state.Goal.Objective != "ship it" {
		t.Fatal("folded goal aliases the record")
	}
	blocked := goalChange(GoalOpBlock, goalSnapshot(2, GoalBlocked), 0, 10, 10)
	cloned := CloneEvent(Event{Sequence: 1, Record: blocked})
	cloned.Record.Goal.Snapshot.BlockedReason.Message = "mutated"
	cleared := CloneEvent(Event{Sequence: 2, Record: goalClear("goal-1", 2, 1)})
	cleared.Record.Goal.Cleared.ID = "mutated"
	original := goalClear("goal-1", 2, 1)
	if blocked.Goal.Snapshot.BlockedReason.Message != "needs a key" || original.Goal.Cleared.ID != "goal-1" {
		t.Fatal("CloneEvent aliases goal payloads")
	}
}

func TestProjectGoal_RejectsIllegalHistories(t *testing.T) {
	create := goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 10, 10)
	at := func(revision uint64, phase GoalPhase, mutate func(*GoalSnapshot)) *GoalSnapshot {
		snapshot := goalSnapshot(revision, phase)
		if mutate != nil {
			mutate(snapshot)
		}
		return snapshot
	}
	for name, test := range map[string]struct {
		records []Record
		message string
	}{
		"edit without goal":      {[]Record{goalChange(GoalOpEdit, goalSnapshot(2, GoalActive), 0, 10, 10)}, "requires a current goal"},
		"clear without goal":     {[]Record{goalClear("goal-1", 2, 10)}, "tombstone the next revision"},
		"clear wrong revision":   {[]Record{create, goalClear("goal-1", 3, 10)}, "tombstone the next revision"},
		"clear wrong id":         {[]Record{create, goalClear("goal-2", 2, 10)}, "tombstone the next revision"},
		"clear before update":    {[]Record{create, goalClear("goal-1", 2, 9)}, "tombstone the next revision"},
		"create revision two":    {[]Record{goalChange(GoalOpCreate, goalSnapshot(2, GoalActive), 0, 10, 10)}, "fresh active revision-one"},
		"create paused":          {[]Record{goalChange(GoalOpCreate, goalSnapshot(1, GoalPaused), 0, 10, 10)}, "fresh active revision-one"},
		"create with rounds":     {[]Record{goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 1, 10, 10)}, "fresh active revision-one"},
		"create over unfinished": {[]Record{create, goalChange(GoalOpCreate, at(1, GoalActive, func(value *GoalSnapshot) { value.ID = "goal-2" }), 0, 10, 10)}, "fresh active revision-one"},
		"create reused id":       {[]Record{create, goalChange(GoalOpComplete, goalSnapshot(2, GoalComplete), 0, 10, 10), create}, "fresh active revision-one"},
		"reused after clear":     {[]Record{create, goalClear("goal-1", 2, 10), create}, "fresh active revision-one"},
		"skipped revision":       {[]Record{create, goalChange(GoalOpPause, goalSnapshot(3, GoalPaused), 0, 10, 10)}, "advance the current goal by one revision"},
		"other goal":             {[]Record{create, goalChange(GoalOpPause, at(2, GoalPaused, func(value *GoalSnapshot) { value.ID = "goal-2" }), 0, 10, 10)}, "advance the current goal by one revision"},
		"changed creation":       {[]Record{create, goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 0, 11, 11)}, "preserve the current counters"},
		"time moved back":        {[]Record{goalChange(GoalOpCreate, goalSnapshot(1, GoalActive), 0, 10, 12), goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 0, 10, 11)}, "preserve the current counters"},
		"changed rounds":         {[]Record{create, goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 1, 10, 10)}, "preserve the current counters"},
		"edit changes phase":     {[]Record{create, goalChange(GoalOpEdit, goalSnapshot(2, GoalPaused), 0, 10, 10)}, "edit cannot change phase"},
		"edit changes reason": {[]Record{create, goalChange(GoalOpBlock, goalSnapshot(2, GoalBlocked), 0, 10, 10), goalChange(GoalOpEdit, at(3, GoalBlocked, func(value *GoalSnapshot) {
			value.BlockedReason = &GoalBlockReason{Code: "other", Message: "x"}
		}), 0, 10, 10)}, "edit cannot change phase"},
		"pause changes objective": {[]Record{create, goalChange(GoalOpPause, at(2, GoalPaused, func(value *GoalSnapshot) { value.Objective = "new" }), 0, 10, 10)}, "cannot change objective"},
		"pause changes cap":       {[]Record{create, goalChange(GoalOpPause, at(2, GoalPaused, func(value *GoalSnapshot) { value.MaxRounds = 9 }), 0, 10, 10)}, "cannot change objective"},
		"pause wrong phase":       {[]Record{create, goalChange(GoalOpPause, goalSnapshot(2, GoalActive), 0, 10, 10)}, "pause has an invalid phase transition"},
		"pause paused":            {[]Record{create, goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 0, 10, 10), goalChange(GoalOpPause, goalSnapshot(3, GoalPaused), 0, 10, 10)}, "pause has an invalid phase transition"},
		"resume complete":         {[]Record{create, goalChange(GoalOpComplete, goalSnapshot(2, GoalComplete), 0, 10, 10), goalChange(GoalOpResume, goalSnapshot(3, GoalActive), 0, 10, 10)}, "resume has an invalid phase transition"},
		"resume to paused":        {[]Record{create, goalChange(GoalOpResume, goalSnapshot(2, GoalPaused), 0, 10, 10)}, "resume has an invalid phase transition"},
		"resume exhausted": {[]Record{goalChange(GoalOpCreate, at(1, GoalActive, func(value *GoalSnapshot) { value.MaxRounds = 1 }), 0, 10, 10), goalRound("goal-1", 1, 1),
			goalChange(GoalOpPause, at(2, GoalPaused, func(value *GoalSnapshot) { value.MaxRounds = 1 }), 1, 10, 10),
			goalChange(GoalOpResume, at(3, GoalActive, func(value *GoalSnapshot) { value.MaxRounds = 1 }), 1, 10, 10)}, "resume has an invalid phase transition"},
		"complete twice":          {[]Record{create, goalChange(GoalOpComplete, goalSnapshot(2, GoalComplete), 0, 10, 10), goalChange(GoalOpComplete, goalSnapshot(3, GoalComplete), 0, 10, 10)}, "complete has an invalid phase transition"},
		"complete to active":      {[]Record{create, goalChange(GoalOpComplete, goalSnapshot(2, GoalActive), 0, 10, 10)}, "complete has an invalid phase transition"},
		"block paused":            {[]Record{create, goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 0, 10, 10), goalChange(GoalOpBlock, goalSnapshot(3, GoalBlocked), 0, 10, 10)}, "block has an invalid phase transition"},
		"block to paused":         {[]Record{create, goalChange(GoalOpBlock, goalSnapshot(2, GoalPaused), 0, 10, 10)}, "block has an invalid phase transition"},
		"round without goal":      {[]Record{goalRound("goal-1", 1, 1)}, "next admitted round"},
		"round of paused goal":    {[]Record{create, goalChange(GoalOpPause, goalSnapshot(2, GoalPaused), 0, 10, 10), goalRound("goal-1", 2, 1)}, "next admitted round"},
		"round of other goal":     {[]Record{create, goalRound("goal-2", 1, 1)}, "next admitted round"},
		"round of stale revision": {[]Record{create, goalChange(GoalOpEdit, at(2, GoalActive, func(value *GoalSnapshot) { value.Objective = "new" }), 0, 10, 10), goalRound("goal-1", 1, 1)}, "next admitted round"},
		"skipped round":           {[]Record{create, goalRound("goal-1", 1, 2)}, "next admitted round"},
		"repeated round":          {[]Record{create, goalRound("goal-1", 1, 1), goalRound("goal-1", 1, 1)}, "next admitted round"},
		"round over cap":          {[]Record{goalChange(GoalOpCreate, at(1, GoalActive, func(value *GoalSnapshot) { value.MaxRounds = 1 }), 0, 10, 10), goalRound("goal-1", 1, 1), goalRound("goal-1", 1, 2)}, "next admitted round"},
	} {
		_, err := applyAll(t, test.records...)
		if !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), test.message) {
			t.Errorf("%s: got %v, want %q", name, err, test.message)
		}
	}
	var state GoalState
	if err := state.Apply(create); err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(goalChange(GoalOpPause, goalSnapshot(3, GoalPaused), 0, 10, 10)); err == nil || state.Goal.Revision != 1 {
		t.Fatalf("rejected change mutated the state: %+v, %v", state.Goal, err)
	}
}
