package goal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowApprover struct{}

func (allowApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

// memoryJournal enforces record shapes and the goal fold like the JSONL store.
type memoryJournal struct {
	mu     sync.Mutex
	events []session.Event
}

func (*memoryJournal) Header() session.Header { return session.Header{SessionID: "root"} }
func (journal *memoryJournal) Events(context.Context) ([]session.Event, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]session.Event(nil), journal.events...), nil
}
func (journal *memoryJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if err := record.Validate(); err != nil {
		return session.Event{}, err
	}
	event := session.CloneEvent(session.Event{Sequence: uint64(len(journal.events) + 1), Record: record})
	if _, err := session.ProjectGoal(append(journal.events[:len(journal.events):len(journal.events)], event)); err != nil {
		return session.Event{}, err
	}
	journal.events = append(journal.events, event)
	return event, nil
}

type journals struct{ journal *memoryJournal }

func (journals journals) Journal(sessionID string) (agent.Journal, error) {
	if sessionID != "root" {
		return nil, agent.ErrAgentNotFound
	}
	return journals.journal, nil
}

type admissions struct{}

func (admissions) RegisterAdmission(string, agent.Admission, *plugin.Scope) error { return nil }

type recordingNotifier struct {
	sessions []string
	messages []session.Message
	err      error
}

func (notifier *recordingNotifier) Notify(sessionID string, message session.Message) error {
	notifier.sessions = append(notifier.sessions, sessionID)
	notifier.messages = append(notifier.messages, message)
	return notifier.err
}

// failingGet fails Get while every other operation reaches the service.
type failingGet struct {
	*appGoal.Service
	err error
}

func (goals failingGet) Get(ctx context.Context, sessionID string) (*appGoal.View, error) {
	if goals.err != nil {
		return nil, goals.err
	}
	return goals.Service.Get(ctx, sessionID)
}

type harness struct {
	runtime  *appTool.Runtime
	goals    *appGoal.Service
	journal  *memoryJournal
	notifier *recordingNotifier
	turn     uint64
	getErr   *failingGet
}

func start(t *testing.T) *harness {
	t.Helper()
	current := &harness{journal: &memoryJournal{}, notifier: &recordingNotifier{}}
	runtime, _ := appTool.New(allowApprover{})
	service, err := appGoal.New(journals{current.journal}, admissions{}, appGoal.Config{Random: bytes.NewReader(bytes.Repeat([]byte{1}, 1024))})
	if err != nil {
		t.Fatal(err)
	}
	scopes := []*plugin.Scope{{}, {}, {}}
	t.Cleanup(func() {
		for _, scope := range scopes {
			_ = scope.Close(context.Background())
		}
	})
	if err := runtime.Start(t.Context(), scopes[2]); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(t.Context(), scopes[1]); err != nil {
		t.Fatal(err)
	}
	current.getErr = &failingGet{Service: service}
	provider, err := New(runtime, current.getErr, current.notifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(t.Context(), scopes[0]); err != nil {
		t.Fatal(err)
	}
	current.runtime, current.goals = runtime, service
	return current
}

// open commits a new turn whose opening message has source.
func (current *harness) open(t *testing.T, source session.MessageSource) {
	t.Helper()
	current.turn++
	for _, record := range []session.Record{
		{Type: session.RecordTurnStart, Turn: current.turn},
		{Type: session.RecordUserMessage, Turn: current.turn, Message: &session.Message{Role: session.RoleUser, Source: source, Content: []session.ContentBlock{{Type: session.ContentText, Text: "input"}}}},
	} {
		if _, err := current.journal.Append(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
}

func (current *harness) human(t *testing.T) { current.open(t, session.MessageSource{Kind: "user"}) }

// round admits the next round of the current goal as its own turn.
func (current *harness) round(t *testing.T) {
	t.Helper()
	view, err := current.goals.Get(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	current.open(t, appGoal.RoundMessage(*view).Source)
}

func (current *harness) callAs(t *testing.T, delegated bool, name string, arguments any) session.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(arguments)
	return current.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{
		SessionID: "root", Turn: current.turn, Step: 1, Delegated: delegated,
		Calls: []session.ToolCall{{ID: "call", Name: name, Arguments: raw}},
	})[0]
}

func (current *harness) call(t *testing.T, name string, arguments any) session.ToolResult {
	t.Helper()
	return current.callAs(t, false, name, arguments)
}

func expect(t *testing.T, result session.ToolResult, want string) {
	t.Helper()
	if result.Output != want || result.IsError != strings.HasPrefix(want, "Error: ") {
		t.Fatalf("result = %+v, want %q", result, want)
	}
}

const goalID = "goal-01010101010101010101010101010101"

func update(revision float64, action string, extra map[string]any) map[string]any {
	arguments := map[string]any{"goal_id": goalID, "revision": revision, "action": action}
	maps.Copy(arguments, extra)
	return arguments
}

func TestProvider_PublishesTheUpstreamDefinitions(t *testing.T) {
	current := start(t)
	catalog, err := current.runtime.Catalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"create_goal": {createDescription, `{"type":"object","properties":{"objective":{"type":"string","description":"The concrete completion objective inferred from the direct human request."},"max_goal_rounds":{"type":"number","description":"Optional positive safe-integer limit on automatic continuation rounds."}},"required":["objective"]}`},
		"get_goal":    {getDescription, `{"type":"object","properties":{}}`},
		"update_goal": {updateDescription, `{"type":"object","properties":{"goal_id":{"type":"string","description":"Exact id returned by get_goal."},"revision":{"type":"number","description":"Exact positive revision returned by get_goal."},"action":{"type":"string","description":"edit, pause, and resume require a direct top-level human request. complete and blocked are also allowed during an automatic continuation of this goal; blocked is rejected before the configured minimum round count.","enum":["edit","pause","resume","complete","blocked"]},"objective":{"type":"string","description":"Replacement objective; valid only with action edit."},"max_goal_rounds":{"type":"number","description":"Replacement cap; valid only with action edit."},"blocked_reason":{"type":"string","description":"Required only with action blocked: the concrete condition that persisted across rounds and blocks progress."}},"required":["goal_id","revision","action"]}`},
	}
	if len(catalog.Definitions) != 3 {
		t.Fatalf("definitions = %+v", catalog.Definitions)
	}
	for _, definition := range catalog.Definitions {
		if expected := want[definition.Name]; definition.Description != expected[0] || string(definition.Parameters) != expected[1] {
			t.Errorf("%s = %q %s", definition.Name, definition.Description, definition.Parameters)
		}
	}
	const policy = "create_goal may infer goal intent from a direct human request in any language. After session resume or fork, an active goal is disarmed: when a human asks to continue or resume in any wording or language, use update_goal action resume to rearm it. Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least 3 consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked."
	if len(catalog.Guidance) != 1 || catalog.Guidance[0] != policy {
		t.Fatalf("guidance = %q", catalog.Guidance)
	}
}

func TestProvider_ValidatesConstructionAndRegistration(t *testing.T) {
	current := start(t)
	runtime, _ := appTool.New(allowApprover{})
	for _, test := range []struct {
		runtime  *appTool.Runtime
		goals    Goals
		notifier Notifier
	}{{nil, current.goals, current.notifier}, {runtime, nil, current.notifier}, {runtime, current.goals, nil}} {
		if _, err := New(test.runtime, test.goals, test.notifier); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New = %v", err)
		}
	}
	duplicate, _ := New(current.runtime, current.goals, current.notifier)
	if duplicate.ID() != "goal-tools" {
		t.Fatalf("ID = %q", duplicate.ID())
	}
	scope := &plugin.Scope{}
	if err := duplicate.Start(t.Context(), scope); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	_ = scope.Close(context.Background())
}

func TestTools_CreateReadAndEditUnderHumanAuthority(t *testing.T) {
	current := start(t)
	expect(t, current.call(t, "create_goal", map[string]any{"objective": "ship"}), "Error: goal tools require a calling agent")
	current.open(t, session.MessageSource{Kind: "tool-jobs"})
	expect(t, current.call(t, "get_goal", map[string]any{}), `{"goal":null}`)
	expect(t, current.call(t, "create_goal", map[string]any{"objective": "ship"}), "Error: this goal operation requires a direct human turn on a top-level agent")
	current.human(t)
	expect(t, current.callAs(t, true, "create_goal", map[string]any{"objective": "ship"}), "Error: this goal operation requires a direct human turn on a top-level agent")
	expect(t, current.call(t, "create_goal", map[string]any{"objective": " "}), "Error: goal objective must be a non-empty string")
	expect(t, current.call(t, "create_goal", map[string]any{"objective": "ship", "max_goal_rounds": 0}), "Error: maxGoalRounds must be a positive safe integer")
	expect(t, current.call(t, "create_goal", map[string]any{"objective": " ship \"it\" ", "max_goal_rounds": 5}),
		`{"goal":{"id":"`+goalID+`","revision":1,"objective":"ship \"it\"","phase":"active","roundsStarted":0,"maxGoalRounds":5},"activation":"armed"}`)
	expect(t, current.call(t, "update_goal", update(1, "edit", map[string]any{"objective": "", "max_goal_rounds": 0})), "Error: goal edit requires objective and/or maxGoalRounds")
	expect(t, current.call(t, "update_goal", update(1, "edit", map[string]any{"objective": "x", "blocked_reason": "no"})), "Error: blocked_reason is valid only with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "edit", map[string]any{"max_goal_rounds": 9, "blocked_reason": ""})),
		`{"goal":{"id":"`+goalID+`","revision":2,"objective":"ship \"it\"","phase":"active","roundsStarted":0,"maxGoalRounds":9},"activation":"armed"}`)
	expect(t, current.call(t, "update_goal", update(1, "pause", nil)), `Error: stale goal ref "`+goalID+`" revision 1; current is "`+goalID+`" revision 2`)
	current.open(t, session.MessageSource{Kind: "plan-mode"})
	expect(t, current.call(t, "update_goal", update(2, "edit", map[string]any{"objective": "x"})), "Error: this goal operation requires a direct human turn on a top-level agent")
	expect(t, current.callAs(t, true, "get_goal", map[string]any{}), `{"goal":{"id":"`+goalID+`","revision":2,"objective":"ship \"it\"","phase":"active","roundsStarted":0,"maxGoalRounds":9},"activation":"armed"}`)
}

func TestTools_RejectMalformedRefs(t *testing.T) {
	current := start(t)
	current.human(t)
	for _, arguments := range []map[string]any{
		{"goal_id": "", "revision": 1, "action": "edit", "objective": "x"},
		{"goal_id": " g", "revision": 1, "action": "edit", "objective": "x"},
		{"goal_id": "g", "revision": 0, "action": "edit", "objective": "x"},
		{"goal_id": "g", "revision": 1.5, "action": "edit", "objective": "x"},
		{"goal_id": "g", "revision": float64(session.MaxGoalRounds) + 1, "action": "edit", "objective": "x"},
	} {
		expect(t, current.call(t, "update_goal", arguments), "Error: goal_id must be non-empty and revision must be a positive safe integer")
	}
	current.turn = 0
	expect(t, current.call(t, "update_goal", update(1, "pause", nil)), "Error: goal tools require a calling agent")
	expect(t, current.call(t, "get_goal", map[string]any{}), "Error: goal tools require a calling agent")
}

func TestTools_PauseAndResumeBelongToHumans(t *testing.T) {
	current := start(t)
	current.human(t)
	current.call(t, "create_goal", map[string]any{"objective": "ship"})
	expect(t, current.call(t, "update_goal", update(1, "pause", map[string]any{"objective": "x"})), "Error: objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "resume", map[string]any{"max_goal_rounds": 2})), "Error: objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "resume", map[string]any{"blocked_reason": "x"})), "Error: objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "resume", nil)), `Error: goal "`+goalID+`" is already active and armed`)
	current.goals.Disarm("root")
	expect(t, current.call(t, "update_goal", update(1, "resume", map[string]any{"objective": "", "max_goal_rounds": 0, "blocked_reason": ""})),
		`{"goal":{"id":"`+goalID+`","revision":2,"objective":"ship","phase":"active","roundsStarted":0,"maxGoalRounds":256},"activation":"armed"}`)
	expect(t, current.call(t, "update_goal", update(2, "pause", nil)),
		`{"goal":{"id":"`+goalID+`","revision":3,"objective":"ship","phase":"paused","roundsStarted":0,"maxGoalRounds":256},"activation":"disarmed"}`)
	expect(t, current.call(t, "update_goal", update(3, "resume", nil)), "Error: the model cannot resume a paused goal; the user must resume it")
	current.getErr.err = errors.New("log unreadable")
	expect(t, current.call(t, "update_goal", update(3, "resume", nil)), "Error: log unreadable")
	expect(t, current.call(t, "get_goal", map[string]any{}), "Error: log unreadable")
	current.getErr.err = nil
	current.open(t, session.MessageSource{Kind: "tool-jobs"})
	expect(t, current.call(t, "update_goal", update(3, "pause", nil)), "Error: this goal operation requires a direct human turn on a top-level agent")
	if len(current.notifier.messages) != 0 {
		t.Fatal("human mutations queued a closing instruction")
	}
}

func TestTools_GoalRoundsMayCompleteAndBlockFromTheThreshold(t *testing.T) {
	current := start(t)
	current.human(t)
	current.call(t, "create_goal", map[string]any{"objective": "ship", "max_goal_rounds": 5})
	current.round(t)
	expect(t, current.call(t, "update_goal", update(1, "edit", map[string]any{"objective": "x"})), "Error: this goal operation requires a direct human turn on a top-level agent")
	expect(t, current.call(t, "update_goal", update(1, "complete", map[string]any{"objective": "x"})), "Error: objective and max_goal_rounds are valid only with action edit")
	expect(t, current.call(t, "update_goal", update(1, "complete", map[string]any{"blocked_reason": "x"})), "Error: blocked_reason is valid only with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "blocked", nil)), "Error: blocked_reason is required with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "blocked", map[string]any{"blocked_reason": " "})), "Error: blocked_reason is required with action blocked")
	expect(t, current.call(t, "update_goal", update(1, "blocked", map[string]any{"blocked_reason": "no key"})), "Error: blocked requires at least 3 consecutive goal rounds; current round is 1")
	current.round(t)
	current.round(t)
	expect(t, current.call(t, "update_goal", update(1, "blocked", map[string]any{"blocked_reason": " no key "})),
		`{"goal":{"id":"`+goalID+`","revision":2,"objective":"ship","phase":"blocked","roundsStarted":3,"maxGoalRounds":5,"blockedReason":{"code":"model-reported","message":"no key"}},"activation":"disarmed"}`)
	if len(current.notifier.messages) != 1 || current.notifier.sessions[0] != "root" || session.Text(current.notifier.messages[0]) != session.Text(appGoal.WrapUpMessage("ship", " no key ")) {
		t.Fatalf("closing = %+v", current.notifier.messages)
	}
	// The round of the previous revision no longer carries authority.
	expect(t, current.call(t, "update_goal", update(2, "complete", nil)), "Error: complete and blocked require a direct human turn or the current goal round")

	current.human(t)
	current.goals.Disarm("root")
	expect(t, current.call(t, "update_goal", update(2, "resume", nil)),
		`{"goal":{"id":"`+goalID+`","revision":3,"objective":"ship","phase":"active","roundsStarted":3,"maxGoalRounds":5},"activation":"armed"}`)
	current.round(t)
	current.notifier.err = errors.New("agent stopped")
	expect(t, current.call(t, "update_goal", update(3, "complete", nil)),
		`{"goal":{"id":"`+goalID+`","revision":4,"objective":"ship","phase":"complete","roundsStarted":4,"maxGoalRounds":5},"activation":"disarmed"}`)
	if len(current.notifier.messages) != 2 || session.Text(current.notifier.messages[1]) != session.Text(appGoal.WrapUpMessage("ship", "")) {
		t.Fatalf("closing = %+v", current.notifier.messages)
	}
	expect(t, current.call(t, "update_goal", update(4, "complete", nil)), "Error: complete and blocked require a direct human turn or the current goal round")
	current.human(t)
	expect(t, current.call(t, "update_goal", update(4, "complete", nil)), `Error: cannot complete goal "`+goalID+`" from phase "complete"; expected active or paused or blocked`)
}

func TestTools_HumansMayStopGoalsAtOnce(t *testing.T) {
	current := start(t)
	current.human(t)
	current.call(t, "create_goal", map[string]any{"objective": "ship"})
	current.round(t)
	current.open(t, session.MessageSource{Kind: "user"})
	expect(t, current.callAs(t, true, "update_goal", update(1, "blocked", map[string]any{"blocked_reason": "stop"})), "Error: complete and blocked require a direct human turn or the current goal round")
	expect(t, current.call(t, "update_goal", update(1, "blocked", map[string]any{"blocked_reason": "stop"})),
		`{"goal":{"id":"`+goalID+`","revision":2,"objective":"ship","phase":"blocked","roundsStarted":1,"maxGoalRounds":256,"blockedReason":{"code":"model-reported","message":"stop"}},"activation":"disarmed"}`)
	expect(t, current.call(t, "update_goal", update(2, "complete", nil)),
		`{"goal":{"id":"`+goalID+`","revision":3,"objective":"ship","phase":"complete","roundsStarted":1,"maxGoalRounds":256},"activation":"disarmed"}`)
	if len(current.notifier.messages) != 0 {
		t.Fatal("human stop queued a closing instruction")
	}
	missing := current.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "ghost", Turn: 1, Step: 1, Calls: []session.ToolCall{
		{ID: "a", Name: "get_goal", Arguments: json.RawMessage(`{}`)},
		{ID: "b", Name: "create_goal", Arguments: json.RawMessage(`{"objective":"x"}`)},
		{ID: "c", Name: "update_goal", Arguments: json.RawMessage(`{"goal_id":"g","revision":1,"action":"complete"}`)},
	}})
	for _, result := range missing {
		expect(t, result, `Error: agent "ghost" is not live in this registry`)
	}
}

func TestRender_ToolErrorsKeepTheirCodes(t *testing.T) {
	var failure *toolError
	if !errors.As(invalidUpdate("x"), &failure) || failure.Code() != "GOAL_TOOL_INVALID_UPDATE" {
		t.Fatalf("code = %v", failure)
	}
}
