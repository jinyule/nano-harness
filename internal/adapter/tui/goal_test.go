package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jinyule/nano-harness/internal/app/agent"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// goalJournal is the root log behind the real goal service in these tests.
type goalJournal struct {
	mu     sync.Mutex
	events []session.Event
}

func (*goalJournal) Header() session.Header { return session.Header{SessionID: "root"} }
func (journal *goalJournal) Events(context.Context) ([]session.Event, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]session.Event(nil), journal.events...), nil
}
func (journal *goalJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	event := session.CloneEvent(session.Event{Sequence: uint64(len(journal.events) + 1), Record: record})
	journal.events = append(journal.events, event)
	return event, nil
}

type goalJournals struct{ journal *goalJournal }

func (journals goalJournals) Journal(string) (agent.Journal, error) { return journals.journal, nil }

type noAdmissions struct{}

func (noAdmissions) RegisterAdmission(string, agent.Admission, *plugin.Scope) error { return nil }

// fakeGoals is the real goal service with injectable failures.
type fakeGoals struct {
	*appGoal.Service
	journal  *goalJournal
	getErr   error
	clearErr error
	actors   []appGoal.Actor
}

func newFakeGoals() *fakeGoals {
	journal := &goalJournal{}
	service, _ := appGoal.New(goalJournals{journal}, noAdmissions{}, appGoal.Config{})
	_ = service.Start(context.Background(), &plugin.Scope{})
	goals := &fakeGoals{Service: service, journal: journal}
	_ = service.Watch("root", func(change appGoal.Change) { goals.actors = append(goals.actors, change.Actor) }, &plugin.Scope{})
	return goals
}

func (goals *fakeGoals) Get(ctx context.Context, sessionID string) (*appGoal.View, error) {
	if goals.getErr != nil {
		return nil, goals.getErr
	}
	return goals.Service.Get(ctx, sessionID)
}

func (goals *fakeGoals) Clear(ctx context.Context, sessionID string, ref session.GoalRef, actor appGoal.Actor) error {
	if goals.clearErr != nil {
		return goals.clearErr
	}
	return goals.Service.Clear(ctx, sessionID, ref, actor)
}

func goalCommandOutput(t *testing.T, current model, input string) (model, string) {
	t.Helper()
	before := len(current.lines)
	current.input.SetValue(input)
	current, command := update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	current = runCommand(t, current, command)
	return current, strings.Join(current.lines[before:], "\n")
}

func TestModelGoalCommand_ECMAScriptEditSeparator(t *testing.T) {
	for _, test := range []struct {
		separator string
		edit      bool
	}{
		{"\u2003", true}, {"\ufeff", true}, {"\u00a0", true}, {"\u0085", false},
	} {
		t.Run(test.separator, func(t *testing.T) {
			fixture, current := modelFixture(t)
			if _, err := fixture.goals.Create(t.Context(), "root", "old", nil, appGoal.ActorHost); err != nil {
				t.Fatal(err)
			}
			_, got := goalCommandOutput(t, current, "/goal edit"+test.separator+"new")
			view, err := fixture.goals.Get(t.Context(), "root")
			want := "old"
			if test.edit {
				want = "new"
			}
			if err != nil || view.Goal.Objective != want || strings.Contains(got, "Goal updated") != test.edit {
				t.Fatalf("output=%s view=%+v error=%v", got, view, err)
			}
		})
	}
}

func TestRunGoalCommand_ECMAScriptObjectiveBoundary(t *testing.T) {
	for _, test := range []struct{ argument, want string }{
		{"\u0085", "\u0085"},
		{"\u0085ship\u0085", "\u0085ship\u0085"},
		{"\ufeffship\ufeff", "ship"},
	} {
		t.Run(test.argument, func(t *testing.T) {
			fixture, _ := modelFixture(t)
			output, failed, err := runGoalCommand(t.Context(), fixture.goals, "root", test.argument)
			if err != nil || failed {
				t.Fatalf("output=%v failed=%t error=%v", output, failed, err)
			}
			view, err := fixture.goals.Get(t.Context(), "root")
			if err != nil || view == nil || view.Goal.Objective != test.want {
				t.Fatalf("argument=%q output=%v view=%+v error=%v want=%q", test.argument, output, view, err, test.want)
			}
		})
	}
}

func TestModelGoalCommand_FollowsTheUpstreamGrammar(t *testing.T) {
	fixture, current := modelFixture(t)
	steps := []struct{ input, want string }{
		{"/goal", "goal> No goal is currently set.\ngoal> " + goalUsage},
		{"/goal pause", "error> No goal is currently set; /goal pause requires one. " + goalUsage},
		{"/goal resume", "error> No goal is currently set; /goal resume requires one. " + goalUsage},
		{"/goal edit fix", "error> No goal is currently set; /goal edit requires one. " + goalUsage},
		{"/goal clear", "goal> No goal to clear."},
		{"/goal Edit", "error> Goal editing requires a replacement objective.\n" + goalUsage},
		{"/goal   keep docs green  ", "goal> Goal created\ngoal> Status: active\ngoal> Objective: keep docs green\ngoal> Rounds: 0/256\ngoal> Activation: armed\ngoal> \ngoal> Commands: /goal edit <objective>, /goal pause, /goal clear"},
		{"/goal pause after verification", "error> A goal is already active. Use /goal edit <objective> to change it or /goal clear before replacing it."},
		{"/goal EDIT\tkeep every gate green", "goal> Goal updated\ngoal> Status: active\ngoal> Objective: keep every gate green\ngoal> Rounds: 0/256\ngoal> Activation: armed\ngoal> \ngoal> Commands: /goal edit <objective>, /goal pause, /goal clear"},
		{"/goal PAUSE", "goal> Goal paused\ngoal> Status: paused\ngoal> Objective: keep every gate green\ngoal> Rounds: 0/256\ngoal> Activation: disarmed\ngoal> \ngoal> Commands: /goal edit <objective>, /goal resume, /goal clear"},
		{"/goal pause", "error> The goal command is not valid for the current state. Run /goal to view available commands."},
		{"/goal resume", "goal> Goal resumed\ngoal> Status: active\ngoal> Objective: keep every gate green\ngoal> Rounds: 0/256\ngoal> Activation: armed\ngoal> \ngoal> Commands: /goal edit <objective>, /goal pause, /goal clear"},
		{"/goal clear", "goal> Goal cleared."},
	}
	for _, step := range steps {
		var got string
		current, got = goalCommandOutput(t, current, step.input)
		if got != step.want {
			t.Fatalf("%s:\n%s\nwant:\n%s", step.input, got, step.want)
		}
	}
	for _, actor := range fixture.goals.actors {
		if actor != appGoal.ActorHost {
			t.Fatalf("command mutated as %s", actor)
		}
	}
}

func TestModelGoalCommand_ShowsEveryPhaseAndReplacesCompletedGoals(t *testing.T) {
	fixture, current := modelFixture(t)
	ctx := context.Background()
	view, _ := fixture.goals.Create(ctx, "root", "ship", nil, appGoal.ActorModel)
	fixture.goals.Disarm("root")
	if _, got := goalCommandOutput(t, current, "/goal"); !strings.Contains(got, "goal> Activation: disarmed") || !strings.HasSuffix(got, "Commands: /goal edit <objective>, /goal resume, /goal clear") {
		t.Fatalf("disarmed = %s", got)
	}
	fixture.goals.Disarm("root")
	resumed, _ := fixture.goals.Resume(ctx, "root", view.Goal.Ref(), appGoal.ActorHost)
	blocked, _ := fixture.goals.Block(ctx, "root", resumed.Goal.Ref(), session.GoalBlockReason{Code: "round-limit", Message: "limit"}, appGoal.ActorDriver)
	if _, got := goalCommandOutput(t, current, "/goal"); !strings.Contains(got, "goal> Status: blocked\ngoal> Blocker: round-limit: limit\n") || !strings.HasSuffix(got, "/goal edit <objective>, /goal resume, /goal clear") {
		t.Fatalf("blocked = %s", got)
	}
	if _, err := fixture.goals.Complete(ctx, "root", blocked.Goal.Ref(), appGoal.ActorHost); err != nil {
		t.Fatal(err)
	}
	if _, got := goalCommandOutput(t, current, "/goal"); !strings.Contains(got, "Status: complete") || !strings.HasSuffix(got, "Commands: /goal <objective>, /goal clear") {
		t.Fatalf("complete = %s", got)
	}
	if _, got := goalCommandOutput(t, current, "/goal edit next thing"); !strings.HasPrefix(got, "goal> Goal created\n") || !strings.Contains(got, "Objective: next thing") {
		t.Fatalf("edit after complete = %s", got)
	}
	latest, _ := fixture.goals.Get(ctx, "root")
	if _, err := fixture.goals.Complete(ctx, "root", latest.Goal.Ref(), appGoal.ActorHost); err != nil {
		t.Fatal(err)
	}
	if _, got := goalCommandOutput(t, current, "/goal fresh"); !strings.HasPrefix(got, "goal> Goal created\n") {
		t.Fatalf("create after complete = %s", got)
	}
}

func TestModelGoalCommand_ContainsFailuresAndAttachments(t *testing.T) {
	fixture, current := modelFixture(t)
	current.images = []pendingImage{{ref: session.Image{Name: "shot.png"}}}
	current.input.SetValue("/goal ship")
	current, command := update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil || !strings.HasSuffix(current.lines[len(current.lines)-1], "error> attachments cannot accompany /goal; send them as a message") || len(current.images) != 1 {
		t.Fatalf("attachments: %v", current.lines)
	}
	current.images = nil
	fixture.goals.getErr = errors.New("log unreadable")
	if _, got := goalCommandOutput(t, current, "/goal"); got != "error> log unreadable" {
		t.Fatalf("get failure = %s", got)
	}
	fixture.goals.getErr = nil
	if _, err := fixture.goals.Create(context.Background(), "root", "ship", nil, appGoal.ActorModel); err != nil {
		t.Fatal(err)
	}
	fixture.goals.clearErr = errors.New("disk full")
	if _, got := goalCommandOutput(t, current, "/goal clear"); got != "error> disk full" {
		t.Fatalf("clear failure = %s", got)
	}
	fixture.goals.journal.mu.Lock()
	fixture.goals.journal.events = nil
	fixture.goals.journal.mu.Unlock()
	fixture.goals.clearErr = nil
	if _, got := goalCommandOutput(t, current, "/goal "+strings.Repeat("x", session.MaxGoalTextBytes+1)); got != "error> The goal command is not valid for the current state. Run /goal to view available commands." {
		t.Fatalf("invalid objective = %s", got)
	}
}

func TestApplyEvent_ShowsGoalStateAndRounds(t *testing.T) {
	_, current := modelFixture(t)
	snapshot := &session.GoalSnapshot{ID: "goal-1", Revision: 1, Objective: "keep\ndocs  green", Phase: session.GoalActive, MaxRounds: 3}
	round := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: session.GoalSource, GoalID: "goal-1", GoalRevision: 1, GoalRound: 1}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "<goal_round>..."}}}
	closing := appGoal.WrapUpMessage("keep docs green", "no host")
	blocked := &session.GoalSnapshot{ID: "goal-1", Revision: 2, Objective: "keep\ndocs  green", Phase: session.GoalBlocked, BlockedReason: &session.GoalBlockReason{Code: "model-reported", Message: "no host"}, MaxRounds: 3}
	for _, record := range []session.Record{
		{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpCreate, Snapshot: snapshot, CreatedAtUnixMS: 1, UpdatedAtUnixMS: 1}},
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordUserMessage, Turn: 1, Message: &round},
		{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpBlock, Snapshot: blocked, RoundsStarted: 1, CreatedAtUnixMS: 1, UpdatedAtUnixMS: 2}},
		{Type: session.RecordUserMessage, Turn: 1, Message: &closing},
	} {
		current.applyEvent(session.Event{Record: record}, true)
	}
	want := []string{"goal> create: keep docs green (active 0/3)", "goal> round 1", "goal> block: keep docs green (blocked 1/3) blocker=model-reported: no host", "goal> <goal_blocked>"}
	if got := current.lines[len(current.lines)-len(want):]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines = %q", got)
	}
	if header := ansi.Strip(current.View().Content); !strings.Contains(header, "goal=blocked 1/3") {
		t.Fatalf("header lacks goal status:\n%s", header)
	}
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpClear, Cleared: &session.GoalRef{ID: "goal-1", Revision: 3}, ClearedAtUnixMS: 3}}}, true)
	if current.lines[len(current.lines)-1] != "goal> cleared" || strings.Contains(ansi.Strip(current.View().Content), "goal=") {
		t.Fatalf("clear = %q", current.lines[len(current.lines)-1])
	}
}
