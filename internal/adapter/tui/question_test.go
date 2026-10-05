package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/question"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestQuestionBroker_AllTerminalStates(t *testing.T) {
	_, config := newAppFixture()
	app, _ := New(config)
	broker := questionBroker{app: app}
	request := question.Request{SessionID: "root", Questions: []question.Question{{ID: "q", Text: "?"}}}
	done := make(chan questionResult, 1)
	go func() {
		answers, err := broker.Ask(context.Background(), request)
		done <- questionResult{answers: answers, err: err}
	}()
	envelope := (<-app.events).(questionEnvelope)
	if envelope.request.SessionID != "root" {
		t.Fatalf("envelope = %+v", envelope)
	}
	envelope.result <- questionResult{answers: []question.Answer{{ID: "q", Selected: []string{}}}}
	if result := <-done; result.err != nil || result.answers[0].ID != "q" {
		t.Fatalf("answered = %+v", result)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := questionBroker{app: &App{events: make(chan any), stop: make(chan struct{}), uiGone: make(chan struct{})}}
	if _, err := blocked.Ask(canceled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
	waitContext, waitCancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := broker.Ask(waitContext, request); waiting <- err }()
	<-app.events
	waitCancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}
	for name, closeApp := range map[string]func(*App){
		"stop":    func(app *App) { close(app.stop) },
		"UI gone": func(app *App) { close(app.uiGone) },
	} {
		t.Run(name+" before send", func(t *testing.T) {
			gone := &App{events: make(chan any), stop: make(chan struct{}), uiGone: make(chan struct{})}
			closeApp(gone)
			if _, err := (questionBroker{app: gone}).Ask(context.Background(), request); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("error = %v", err)
			}
		})
		t.Run(name+" while waiting", func(t *testing.T) {
			pending := &App{events: make(chan any, 1), stop: make(chan struct{}), uiGone: make(chan struct{})}
			result := make(chan error, 1)
			go func() { _, err := (questionBroker{app: pending}).Ask(context.Background(), request); result <- err }()
			<-pending.events
			closeApp(pending)
			if err := <-result; !errors.Is(err, ErrNotRunning) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestModelQuestion_WalksTheBatchAndReturnsAnswers(t *testing.T) {
	_, current := modelFixture(t)
	result := make(chan questionResult, 1)
	request := question.Request{Questions: []question.Question{
		{ID: "mode", Header: "Choose Mode", Text: "Which mode?", Options: []question.Option{{Label: "Fast", Description: "Less work"}, {Label: "Thorough (Recommended)"}}},
		{ID: "plan-review", Header: "Plan review", Text: "Approve this plan?", Detail: "# Plan\n- step", MultiSelect: true, Options: []question.Option{{Label: "API"}, {Label: "UI"}}},
		{ID: "why", Text: "Why?"},
	}}
	current, command := update(t, current, questionEnvelope{request: request, result: result})
	transcript := strings.Join(current.lines, "\n")
	if command == nil || current.mode != modeQuestion || current.input.Value() != "2" ||
		!strings.Contains(transcript, "question> [Choose Mode] Which mode? (1/3)") ||
		!strings.Contains(transcript, "  1. Fast — Less work") || !strings.Contains(transcript, "  2. Thorough (Recommended)") {
		t.Fatalf("first question: value=%q lines=%s", current.input.Value(), transcript)
	}
	if current.input.Placeholder != "an option number, or type an answer; empty skips" {
		t.Fatalf("placeholder = %q", current.input.Placeholder)
	}
	if view := current.View(); !strings.Contains(view.Content, "Question 1/3: Which mode?") {
		t.Fatalf("view = %s", view.Content)
	}
	current, _ = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	transcript = strings.Join(current.lines, "\n")
	if !strings.Contains(transcript, "answer> mode: Thorough (Recommended)") || !strings.Contains(transcript, "  │ # Plan\n  │ - step") {
		t.Fatalf("second question lines = %s", transcript)
	}
	if current.input.Value() != "" || current.input.Placeholder != "option numbers separated by commas, or type an answer; empty skips" {
		t.Fatalf("second input = %q / %q", current.input.Value(), current.input.Placeholder)
	}
	current.input.SetValue("3")
	current, _ = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	if current.question.index != 1 || !strings.HasSuffix(current.lines[len(current.lines)-1], "error> option numbers range from 1 to 2") {
		t.Fatalf("invalid choice accepted: %v", current.lines[len(current.lines)-1])
	}
	current.input.SetValue("2, 1")
	current, _ = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	if current.input.Placeholder != "type an answer; empty skips" {
		t.Fatalf("third placeholder = %q", current.input.Placeholder)
	}
	current.input.SetValue("  because  ")
	current, _ = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	if current.mode != modeNormal || current.question != nil {
		t.Fatalf("batch did not finish: %+v", current)
	}
	answers := (<-result).answers
	if len(answers) != 3 || answers[0].Selected[0] != "Thorough (Recommended)" || strings.Join(answers[1].Selected, ",") != "UI,API" || answers[2].Custom != "because" || len(answers[2].Selected) != 0 {
		t.Fatalf("answers = %+v", answers)
	}
	if !strings.Contains(strings.Join(current.lines, "\n"), `answer> why: "because"`) {
		t.Fatalf("lines = %v", current.lines)
	}
}

func TestModelQuestion_CtrlCCancels(t *testing.T) {
	_, current := modelFixture(t)
	result := make(chan questionResult, 1)
	current, _ = update(t, current, questionEnvelope{request: question.Request{Questions: []question.Question{{ID: "q", Text: "?"}}}, result: result})
	current, command := update(t, current, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if command != nil || current.quitting || current.mode != modeNormal || !errors.Is((<-result).err, question.ErrCancelled) {
		t.Fatalf("cancel = %+v", current)
	}
}

func TestParseAnswer_ChoicesTextAndSkips(t *testing.T) {
	single := question.Question{ID: "s", Options: []question.Option{{Label: "A"}, {Label: "B"}}}
	multi := question.Question{ID: "m", MultiSelect: true, Options: single.Options}
	free := question.Question{ID: "f"}
	for _, test := range []struct {
		name     string
		question question.Question
		input    string
		selected string
		custom   string
		err      string
	}{
		{name: "skip", question: single, input: "  "},
		{name: "single", question: single, input: "2", selected: "B"},
		{name: "single text", question: single, input: "neither", custom: "neither"},
		{name: "two singles", question: single, input: "1 2", err: "choose exactly one option number"},
		{name: "zero", question: single, input: "0", err: "option numbers range from 1 to 2"},
		{name: "duplicate", question: multi, input: "1,1", err: "option 1 is chosen twice"},
		{name: "multi", question: multi, input: "1,2", selected: "A,B"},
		{name: "signed is text", question: multi, input: "-1", custom: "-1"},
		{name: "overflow is text", question: multi, input: "99999999999999999999999", custom: "99999999999999999999999"},
		{name: "separators are text", question: multi, input: ",,", custom: ",,"},
		{name: "numbers without options", question: free, input: "42", custom: "42"},
		{name: "oversized", question: free, input: strings.Repeat("x", question.MaxCustomBytes+1), err: "answers are limited to 16384 bytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			answer, err := parseAnswer(test.question, test.input)
			if test.err != "" {
				if err == nil || err.Error() != test.err {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || answer.ID != test.question.ID || strings.Join(answer.Selected, ",") != test.selected || answer.Selected == nil || answer.Custom != test.custom {
				t.Fatalf("answer = %+v, %v", answer, err)
			}
		})
	}
	if describeAnswer(question.Answer{ID: "s", Selected: []string{}}) != "s: (skipped)" {
		t.Fatal("skip description")
	}
}

func runCommand(t *testing.T, current model, command tea.Cmd) model {
	t.Helper()
	for command != nil {
		current, command = update(t, current, command())
	}
	return current
}

func TestModelPlanCommand_SelectsAndDeliversMessages(t *testing.T) {
	fixture, current := modelFixture(t)
	fixture.registry.planChange = plan.Committed
	current.input.SetValue("/plan")
	current, command := update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	current = runCommand(t, current, command)
	if !strings.HasSuffix(current.lines[len(current.lines)-1], "system> Plan mode on. Use /plan off to leave.") || fixture.registry.session != "root" || len(fixture.controller.submitted) != 0 {
		t.Fatalf("bare /plan: lines=%v submitted=%d", current.lines, len(fixture.controller.submitted))
	}

	current.images = []pendingImage{{ref: session.Image{Name: "shot.png"}}}
	current.input.SetValue("/plan off")
	current, command = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil || !strings.HasSuffix(current.lines[len(current.lines)-1], "error> attachments cannot accompany /plan off") || len(current.images) != 1 {
		t.Fatalf("/plan off with attachments: %v", current.lines)
	}

	fixture.controller.steerErr = agent.ErrAgentIdle
	current.input.SetValue("/plan design the cache")
	current, command = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	current = runCommand(t, current, command)
	submitted := fixture.controller.submitted
	if len(current.images) != 0 || len(submitted) != 1 || submitted[0].Content[0].Image.Name != "shot.png" || session.Text(submitted[0]) != "design the cache" {
		t.Fatalf("idle /plan TEXT: images=%d submitted=%+v", len(current.images), submitted)
	}

	fixture.controller.steerErr = nil
	fixture.registry.planChange = plan.Queued
	current.input.SetValue("/plan keep going")
	current, command = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	current = runCommand(t, current, command)
	if len(fixture.controller.steered) != 2 || !strings.HasSuffix(current.lines[len(current.lines)-1], "system> steer queued") {
		t.Fatalf("busy /plan TEXT: steered=%d lines=%v", len(fixture.controller.steered), current.lines)
	}
	fixture.controller.steerErr = errors.New("steer failed")
	current = runCommand(t, current, current.deliverCommand(session.Message{}, nil))
	if !strings.HasSuffix(current.lines[len(current.lines)-1], "error> steer failed") {
		t.Fatalf("steer failure: %v", current.lines)
	}

	fixture.registry.planErr = errors.New("not root")
	current.input.SetValue("/plan off")
	current, command = update(t, current, tea.KeyPressMsg{Code: tea.KeyEnter})
	current = runCommand(t, current, command)
	if !strings.HasSuffix(current.lines[len(current.lines)-1], "error> not root") {
		t.Fatalf("selection failure: %v", current.lines)
	}
	if got := fixture.registry.planActive; len(got) != 4 || !got[0] || got[3] {
		t.Fatalf("selections = %v", got)
	}
}

func TestPlanChangeText_CoversEveryOutcome(t *testing.T) {
	for _, test := range []struct {
		change            plan.Change
		active, wasActive bool
		want              string
	}{
		{plan.Committed, true, false, "Plan mode on. Use /plan off to leave."},
		{plan.Cancelled, true, true, "Plan mode exit cancelled; plan mode stays on."},
		{plan.Queued, true, false, "Entering plan mode (applies from the next step). Use /plan off to leave."},
		{plan.Unchanged, true, false, "Entering plan mode (applies from the next step). Use /plan off to leave."},
		{plan.Unchanged, true, true, "Plan mode is already on."},
		{plan.Committed, false, true, "Plan mode off."},
		{plan.Cancelled, false, false, "Plan mode entry cancelled."},
		{plan.Queued, false, true, "Leaving plan mode (applies from the next step)."},
		{plan.Unchanged, false, true, "Leaving plan mode (applies from the next step)."},
		{plan.Unchanged, false, false, "Plan mode is already off."},
	} {
		if got := planChangeText(test.change, test.active, test.wasActive); got != test.want {
			t.Errorf("planChangeText(%s, %t, %t) = %q", test.change, test.active, test.wasActive, got)
		}
	}
}

func TestApplyEvent_ShowsPlanModeAndNotices(t *testing.T) {
	_, current := modelFixture(t)
	notice := &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: plan.NoticeSource}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "The user switched this session to plan mode."}}}
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordPlanMode, Plan: &session.PlanMode{Active: true}}}, true)
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordUserMessage, Turn: 1, Message: notice}}, true)
	if !current.planActive || !strings.Contains(current.View().Content, "busy=false mode=plan") {
		t.Fatalf("plan state not shown: %s", current.View().Content)
	}
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordPlanMode, Plan: &session.PlanMode{}}}, true)
	want := []string{"mode> plan mode on", "mode> The user switched this session to plan mode.", "mode> plan mode off"}
	if current.planActive || strings.Join(current.lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v", current.lines)
	}
}
