package session

import (
	"errors"
	"testing"
)

func TestPlanMode_ValidateShape(t *testing.T) {
	for _, record := range []Record{
		{Type: RecordPlanMode, Plan: &PlanMode{Active: true}},
		{Type: RecordPlanMode, Turn: 3, Plan: &PlanMode{}},
	} {
		if err := record.Validate(); err != nil {
			t.Errorf("valid %+v: %v", record, err)
		}
	}
	for name, record := range map[string]Record{
		"missing payload": {Type: RecordPlanMode, Turn: 1},
		"inside step":     {Type: RecordPlanMode, Turn: 1, Step: 1, Plan: &PlanMode{Active: true}},
		"extras":          {Type: RecordPlanMode, Plan: &PlanMode{Active: true}, Outcome: OutcomeCompleted},
		"bare with plan":  {Type: RecordTurnStart, Turn: 1, Plan: &PlanMode{Active: true}},
		"message extras":  {Type: RecordUserMessage, Turn: 1, Message: textMessage(RoleUser, "x"), Plan: &PlanMode{}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestProjectPlan_FoldsModeAndLatestHeader(t *testing.T) {
	header := Record{Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m"}}
	mode := func(active bool) Event {
		return Event{Record: Record{Type: RecordPlanMode, Plan: &PlanMode{Active: active}}}
	}
	if view := ProjectPlan(nil); view != (PlanView{}) {
		t.Fatalf("empty view = %+v", view)
	}
	view := ProjectPlan([]Event{mode(true), {Record: header}, mode(false)})
	if view != (PlanView{Active: false, Requested: true, Told: true}) {
		t.Fatalf("view = %+v", view)
	}
	view = ProjectPlan([]Event{{Record: header}, mode(true)})
	if view != (PlanView{Active: true, Requested: true, Told: false}) {
		t.Fatalf("view after later mode = %+v", view)
	}
}

func TestProjectPlan_SkipsRecordsAForkInherited(t *testing.T) {
	inherited := []Event{
		{Sequence: 1, Record: Record{Type: RecordPlanMode, Plan: &PlanMode{Active: true}}},
		{Sequence: 2, Record: Record{Type: RecordRequestHeader, Turn: 1, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m"}}},
	}
	descriptor := Event{Sequence: 3, Record: Record{Type: RecordSubagentDescriptor, Subagent: &SubagentDescriptor{Version: SubagentDescriptorVersion, Route: testRoute, Provider: SubagentFork, Mode: SubagentOneShot, Label: "fork", Inherited: 2}}}
	child := append(append([]Event(nil), inherited...), descriptor)
	if view := ProjectPlan(child); view != (PlanView{}) {
		t.Fatalf("forked child inherited plan mode: %+v", view)
	}
	if view := ProjectPlan(inherited); view != (PlanView{Active: true, Requested: true, Told: true}) {
		t.Fatalf("parent view = %+v", view)
	}
	own := append(append([]Event(nil), child...), Event{Sequence: 4, Record: Record{Type: RecordRequestHeader, Turn: 2, Step: 1, Header: &RequestHeader{Provider: "p", Model: "m"}}})
	if view := ProjectPlan(own); view != (PlanView{Requested: true}) {
		t.Fatalf("child's own header view = %+v", view)
	}
}

func TestPlanMode_SurfaceSkipsAndCloneDetaches(t *testing.T) {
	event := Event{Sequence: 1, Record: Record{Type: RecordPlanMode, Plan: &PlanMode{Active: true}}}
	surface, err := Surface([]Event{event})
	if err != nil || len(surface) != 0 {
		t.Fatalf("surface = %+v, %v", surface, err)
	}
	cloned := CloneEvent(event)
	cloned.Record.Plan.Active = false
	if !event.Record.Plan.Active {
		t.Fatal("CloneEvent aliases the plan payload")
	}
}
