package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/llm"
	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func modelFixture(t *testing.T) (*appFixture, model) {
	t.Helper()
	fixture, config := newAppFixture()
	app, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	app.agent = fixture.controller
	return fixture, newModel(context.Background(), app, nil)
}

func update(t *testing.T, current model, message tea.Msg) (model, tea.Cmd) {
	t.Helper()
	next, command := current.Update(message)
	updated, ok := next.(model)
	if !ok {
		t.Fatalf("model type = %T", next)
	}
	return updated, command
}

func TestNewModelInitAndWaitUI(t *testing.T) {
	fixture, config := newAppFixture()
	app, _ := New(config)
	app.agent = fixture.controller
	initial := []session.Event{
		{Sequence: 1, Record: session.Record{Type: session.RecordUserMessage, Turn: 1, Message: messagePointer(session.RoleUser, "hello")}},
		{Sequence: 2, Record: session.Record{Type: session.RecordAssistantMessage, Turn: 1, Step: 1, Message: messagePointer(session.RoleAssistant, "answer")}},
	}
	current := newModel(context.Background(), app, initial)
	if len(current.lines) != 2 || !strings.Contains(current.lines[0], "hello") || !strings.Contains(current.lines[1], "answer") {
		t.Fatalf("initial lines = %#v", current.lines)
	}
	app.events <- operationMessage{text: "event"}
	if message := current.Init()(); message.(operationMessage).text != "event" {
		t.Fatalf("Init message = %#v", message)
	}
	events, stop := make(chan any), make(chan struct{})
	close(stop)
	if message := waitUI(events, stop)(); message == nil {
		t.Fatal("stop did not return tea.Quit message")
	}
}

func TestModelUpdate_HandlesEveryEnvelopeAndTerminalInput(t *testing.T) {
	_, current := modelFixture(t)
	current, command := update(t, current, tea.WindowSizeMsg{Width: 100, Height: 40})
	if command != nil || current.width != 100 || current.viewport.Width != 96 || current.viewport.Height != 33 || current.input.Width != 92 {
		t.Fatalf("window model = %+v", current)
	}
	event := session.Event{Record: session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted}}
	current, command = update(t, current, transcriptMessage{event: event})
	if command == nil || !strings.Contains(current.lines[len(current.lines)-1], "completed") {
		t.Fatalf("transcript update = %#v cmd=%v", current.lines, command)
	}

	approvalResult := make(chan session.ApprovalOutcome, 1)
	current, command = update(t, current, approvalEnvelope{question: approval.Question{Reason: "write"}, result: approvalResult})
	if command == nil || current.mode != modeApproval || current.approval == nil || current.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("approval mode = %+v", current)
	}
	authResultChannel := make(chan authResult, 1)
	current, command = update(t, current, authEnvelope{prompt: llm.AuthPrompt{Type: llm.AuthPromptSecret, Placeholder: "secret"}, result: authResultChannel})
	if command == nil || current.mode != modeAuth || current.input.EchoMode != textinput.EchoPassword {
		t.Fatalf("secret auth mode = %+v", current)
	}
	current, _ = update(t, current, authEnvelope{prompt: llm.AuthPrompt{Type: llm.AuthPromptManual}, result: authResultChannel})
	if current.input.EchoMode != textinput.EchoNormal {
		t.Fatal("manual auth retained password echo")
	}
	current, command = update(t, current, authNoticeMessage{notice: llm.AuthNotice{Message: "open", URL: "https://example.test", UserCode: "CODE"}})
	if command == nil || !strings.Contains(current.lines[len(current.lines)-1], "CODE") || !strings.Contains(current.lines[len(current.lines)-1], "https://") {
		t.Fatalf("auth notice lines = %#v", current.lines)
	}
	current, _ = update(t, current, operationMessage{err: errors.New("operation")})
	current, _ = update(t, current, operationMessage{text: "worked"})
	if !strings.Contains(strings.Join(current.lines, "\n"), "error> operation") || !strings.Contains(strings.Join(current.lines, "\n"), "system> worked") {
		t.Fatalf("operation lines = %#v", current.lines)
	}
	current, _ = update(t, current, attachmentMessage{err: errors.New("image")})
	image := session.Image{Name: "image.png", Width: 10, Height: 20}
	current, _ = update(t, current, attachmentMessage{image: image})
	if len(current.images) != 1 || !strings.Contains(current.lines[len(current.lines)-1], "10x20") {
		t.Fatalf("attachment state = %+v", current)
	}
	current, _ = update(t, current, turnMessage{result: agent.TurnResult{Outcome: session.OutcomeError, Err: errors.New("turn")}})
	current, _ = update(t, current, turnMessage{result: agent.TurnResult{Outcome: session.OutcomeCompleted}})
	if !strings.Contains(strings.Join(current.lines, "\n"), "turn> error") {
		t.Fatalf("turn lines = %#v", current.lines)
	}
	current, command = update(t, current, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !strings.Contains(current.input.Value(), "x") {
		t.Fatalf("key update input=%q cmd=%v", current.input.Value(), command)
	}
	current.restoreInput()
	current, command = update(t, current, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !current.quitting || command == nil {
		t.Fatal("ctrl+c update did not quit")
	}
	current.quitting = false
	current.input.SetValue("/help")
	current, command = update(t, current, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || !strings.Contains(current.lines[len(current.lines)-1], "commands>") {
		t.Fatal("enter update did not submit")
	}
}

func TestModelCancelOrQuitAndSubmitInteractionModes(t *testing.T) {
	_, current := modelFixture(t)
	approvalResult := make(chan session.ApprovalOutcome, 2)
	current.mode, current.approval = modeApproval, &approvalEnvelope{result: approvalResult}
	next, command := current.cancelOrQuit()
	current = next.(model)
	if command != nil || <-approvalResult != session.ApprovalCancelled || current.mode != modeNormal {
		t.Fatalf("approval cancellation = %+v", current)
	}
	authResults := make(chan authResult, 2)
	current.mode, current.auth = modeAuth, &authEnvelope{result: authResults}
	next, command = current.cancelOrQuit()
	current = next.(model)
	if command != nil || !errors.Is((<-authResults).err, context.Canceled) || current.mode != modeNormal {
		t.Fatalf("auth cancellation = %+v", current)
	}
	next, command = current.cancelOrQuit()
	if !next.(model).quitting || command == nil {
		t.Fatal("normal ctrl+c did not quit")
	}

	current.quitting = false
	current.mode, current.approval = modeApproval, &approvalEnvelope{result: approvalResult}
	current.input.SetValue("yes")
	next, _ = current.submit()
	current = next.(model)
	if <-approvalResult != session.ApprovalAllowedOnce {
		t.Fatal("yes approval was not allowed")
	}
	current.mode, current.approval = modeApproval, &approvalEnvelope{result: approvalResult}
	current.input.SetValue("no")
	next, _ = current.submit()
	current = next.(model)
	if <-approvalResult != session.ApprovalRejected {
		t.Fatal("no approval was not rejected")
	}
	current.mode, current.auth = modeAuth, &authEnvelope{result: authResults}
	current.input.SetValue(" value ")
	next, _ = current.submit()
	current = next.(model)
	if result := <-authResults; result.value != "value" || result.err != nil {
		t.Fatalf("auth submit = %+v", result)
	}
}

func TestModelSubmit_NormalMessageImagesErrorsAndCancellation(t *testing.T) {
	fixture, current := modelFixture(t)
	current.input.SetValue("   ")
	next, command := current.submit()
	current = next.(model)
	if command != nil {
		t.Fatal("blank submission produced command")
	}
	current.input.SetValue("/help")
	next, command = current.submit()
	current = next.(model)
	if command != nil || !strings.Contains(current.lines[len(current.lines)-1], "commands>") {
		t.Fatalf("help submission lines = %#v", current.lines)
	}
	current.images = []session.Image{{Name: "one"}, {Name: "two"}}
	current.input.SetValue("inspect")
	next, command = current.submit()
	current = next.(model)
	if command == nil || len(current.images) != 0 {
		t.Fatalf("normal submission = %+v cmd=%v", current, command)
	}
	message := command().(turnMessage)
	if message.result.Outcome != session.OutcomeCompleted || len(fixture.controller.submitted) != 1 || len(fixture.controller.submitted[0].Content) != 3 {
		t.Fatalf("turn message = %+v submitted=%#v", message, fixture.controller.submitted)
	}
	fixture.controller.submitErr = errors.New("submit")
	current.input.SetValue("fail")
	next, command = current.submit()
	current = next.(model)
	if result := command().(turnMessage).result; !errors.Is(result.Err, fixture.controller.submitErr) || result.Outcome != session.OutcomeError {
		t.Fatalf("submit failure = %+v", result)
	}
	fixture.controller.submitErr = nil
	fixture.controller.submitChannel = make(chan agent.TurnResult)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	current.ctx = ctx
	current.input.SetValue("cancel")
	_, command = current.submit()
	if result := command().(turnMessage).result; !errors.Is(result.Err, context.Canceled) || result.Outcome != session.OutcomeCanceled {
		t.Fatalf("canceled submit = %+v", result)
	}
}

func TestModelCommand_LocalImmediateAndUsageBranches(t *testing.T) {
	fixture, current := modelFixture(t)
	for _, commandText := range []string{
		"/models", "/models one two", "/login", "/login one", "/logout", "/logout one two",
		"/model", "/model one", "/permission", "/permission always", "/unknown",
	} {
		next, command := current.command(commandText)
		current = next.(model)
		if command != nil || !strings.HasPrefix(current.lines[len(current.lines)-1], "error>") {
			t.Fatalf("usage command %q = %#v cmd=%v", commandText, current.lines, command)
		}
	}
	next, command := current.command("/help")
	current = next.(model)
	if command != nil || !strings.Contains(current.lines[len(current.lines)-1], "/attach") {
		t.Fatal("help command failed")
	}
	next, command = current.command("/interrupt")
	current = next.(model)
	if command != nil || fixture.controller.interrupts != 1 {
		t.Fatal("interrupt command failed")
	}
	next, command = current.command("/quit")
	if !next.(model).quitting || command == nil {
		t.Fatal("quit command failed")
	}
}

func TestModelCommand_ExecutesAllAsyncUseCases(t *testing.T) {
	fixture, current := modelFixture(t)
	fixture.images.image = session.Image{Name: "attached.png"}
	fixture.models.accounts = []llm.AccountInfo{{Provider: "openai", Kind: llm.CredentialOAuth, Source: "stored"}}
	fixture.models.models = []llm.ModelInfo{{Provider: "openai", ID: "model", ContextWindow: 1000, Vision: true, Tools: true}}
	fixture.subagents.infos = []appSubagent.Info{{SessionID: "child", Label: "worker", Mode: "one-shot", Busy: true, Last: agent.TurnResult{Outcome: session.OutcomeCompleted}}}
	fixture.controller.compact = true

	tests := []struct {
		value string
		want  string
	}{
		{value: "/attach /tmp/image.png"},
		{value: "/accounts", want: "openai kind=oauth source=stored"},
		{value: "/models openai", want: "openai/model context=1000 vision=true tools=true"},
		{value: "/login openai oauth", want: "login stored for openai"},
		{value: "/logout openai", want: "logged out openai"},
		{value: "/model openai gpt-5.6-luna", want: "route=openai/gpt-5.6-luna"},
		{value: "/compact", want: "compaction completed=true"},
		{value: "/permission never", want: "permission policy=never"},
		{value: "/agents", want: "child worker mode=one-shot busy=true outcome=completed"},
		{value: "/steer new direction", want: "steer queued"},
	}
	for _, test := range tests {
		next, command := current.command(test.value)
		current = next.(model)
		if command == nil {
			t.Fatalf("command %q returned nil", test.value)
		}
		message := command()
		if test.value == "/attach /tmp/image.png" {
			attachment := message.(attachmentMessage)
			if attachment.image.Name != "attached.png" || fixture.images.path != "/tmp/image.png" {
				t.Fatalf("attachment = %+v path=%q", attachment, fixture.images.path)
			}
			continue
		}
		operation := message.(operationMessage)
		if operation.err != nil || operation.text != test.want {
			t.Fatalf("command %q = %+v", test.value, operation)
		}
	}
	if fixture.models.login != [2]string{"openai", "oauth"} || fixture.models.logout != "openai" || !fixture.settings.updated || fixture.registry.policy != session.ApprovalNever || len(fixture.controller.steered) != 1 {
		t.Fatalf("use case state: models=%+v settings=%+v registry=%+v steers=%#v", fixture.models, fixture.settings, fixture.registry, fixture.controller.steered)
	}
}

func TestCommandHelpers_EmptyListsAndEveryFailure(t *testing.T) {
	fixture, current := modelFixture(t)
	if message := current.accountsCommand()().(operationMessage); message.text != "no stored accounts" || message.err != nil {
		t.Fatalf("empty accounts = %+v", message)
	}
	if message := current.modelsCommand("openai")().(operationMessage); message.text != "" || message.err != nil {
		t.Fatalf("empty models = %+v", message)
	}
	if message := current.commandMessageForAgents(); message.text != "no subagents" || message.err != nil {
		t.Fatalf("empty agents = %+v", message)
	}

	failure := errors.New("failure")
	fixture.models.accountsErr = failure
	if message := current.accountsCommand()().(operationMessage); !errors.Is(message.err, failure) {
		t.Fatalf("accounts error = %+v", message)
	}
	fixture.models.modelsErr = failure
	if message := current.modelsCommand("openai")().(operationMessage); !errors.Is(message.err, failure) {
		t.Fatalf("models error = %+v", message)
	}
	fixture.settings.snapErr = failure
	if message := current.routeCommand("openai", "model")().(operationMessage); !errors.Is(message.err, failure) {
		t.Fatalf("snapshot error = %+v", message)
	}
	fixture.settings.snapErr = nil
	fixture.settings.updateErr = failure
	if message := current.routeCommand("openai", "model")().(operationMessage); !errors.Is(message.err, failure) {
		t.Fatalf("update error = %+v", message)
	}
	fixture.models.loginErr, fixture.models.logoutErr = failure, failure
	fixture.controller.compactErr, fixture.registry.err, fixture.subagents.err, fixture.controller.steerErr = failure, failure, failure, failure
	for _, value := range []string{"/login openai oauth", "/logout openai", "/compact", "/permission ask", "/agents", "/steer text"} {
		next, command := current.command(value)
		current = next.(model)
		if message := command().(operationMessage); !errors.Is(message.err, failure) {
			t.Fatalf("command %q error = %+v", value, message)
		}
	}
}

func (current model) commandMessageForAgents() operationMessage {
	_, command := current.command("/agents")
	return command().(operationMessage)
}

func TestApplyEvent_ProjectsAllDurablePresentationFacts(t *testing.T) {
	_, current := modelFixture(t)
	image := &session.Image{Name: "image"}
	events := []session.Event{
		{Record: session.Record{Type: session.RecordUserMessage, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "hello"}, {Type: session.ContentImage, Image: image}}}}},
		{Record: session.Record{Type: session.RecordRequestHeader, Header: &session.RequestHeader{Provider: "openai", Model: "model"}}},
		{Record: session.Record{Type: session.RecordAssistantChunk, Chunk: &session.AssistantChunk{Kind: session.ChunkText, Text: "one"}}},
		{Record: session.Record{Type: session.RecordAssistantChunk, Chunk: &session.AssistantChunk{Kind: session.ChunkText, Text: " two"}}},
		{Record: session.Record{Type: session.RecordAssistantChunk, Chunk: &session.AssistantChunk{Kind: session.ChunkReasoning, Text: "think"}}},
		{Record: session.Record{Type: session.RecordAssistantMessage, Message: messagePointer(session.RoleAssistant, "final")}},
		{Record: session.Record{Type: session.RecordToolCall, Call: &session.ToolCall{Name: "read", Arguments: []byte(`{}`)}}},
		{Record: session.Record{Type: session.RecordApprovalAsked, Approval: &session.ApprovalData{Reason: "write"}}},
		{Record: session.Record{Type: session.RecordToolResult, Result: &session.ToolResult{Output: "ok"}}},
		{Record: session.Record{Type: session.RecordToolResult, Result: &session.ToolResult{Output: "bad", IsError: true}}},
		{Record: session.Record{Type: session.RecordRetry, Retry: &session.RetryData{Attempt: 2, DelayMS: 10, Failure: "server"}}},
		{Record: session.Record{Type: session.RecordCompactionStart, Compaction: &session.CompactionData{ID: "one"}}},
		{Record: session.Record{Type: session.RecordCompactionEnd, Compaction: &session.CompactionData{ID: "one"}}},
		{Record: session.Record{Type: session.RecordCompactionEnd, Compaction: &session.CompactionData{ID: "two", Error: "failed"}}},
		{Record: session.Record{Type: session.RecordTurnEnd, Outcome: session.OutcomeCompleted}},
	}
	for _, event := range events {
		current.applyEvent(event, true)
	}
	joined := strings.Join(current.lines, "\n")
	for _, expected := range []string{"you> hello [images=1]", "route> openai/model", "assistant> one two", "reasoning> think", "tool> read", "approval> write", "result> ok", "tool-error> bad", "retry> attempt=2", "compact> started", "compact> completed", "compact> failed", "turn> completed"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("projection missing %q in %s", expected, joined)
		}
	}
	before := len(current.lines)
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordAssistantChunk, Chunk: &session.AssistantChunk{Kind: session.ChunkText, Text: "ignored"}}}, false)
	current.stream = ""
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordAssistantMessage, Message: messagePointer(session.RoleAssistant, "snapshot")}}, false)
	if len(current.lines) != before+1 || !strings.Contains(current.lines[len(current.lines)-1], "snapshot") {
		t.Fatalf("snapshot projection = %#v", current.lines)
	}
}

func TestLineBufferViewAndInputRestoration(t *testing.T) {
	fixture, current := modelFixture(t)
	current.appendStream("assistant", "a")
	current.appendStream("assistant", "b")
	current.appendStream("reasoning", "c")
	if current.lines[len(current.lines)-2] != "assistant> ab" || current.lines[len(current.lines)-1] != "reasoning> c" {
		t.Fatalf("stream lines = %#v", current.lines)
	}
	for index := range 4001 {
		current.addLine(fmt.Sprintf("line-%d", index))
	}
	if len(current.lines) != 4000 || current.lines[0] != "line-1" {
		t.Fatalf("trimmed lines first=%q len=%d", current.lines[0], len(current.lines))
	}
	current.mode = modeAuth
	current.input.SetValue("secret")
	current.input.Placeholder = "changed"
	current.input.EchoMode = textinput.EchoPassword
	current.restoreInput()
	if current.mode != modeNormal || current.input.Value() != "" || current.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("restored input = %+v", current.input)
	}
	if current.View() == "" {
		t.Fatal("normal view is empty")
	}
	current.images = []session.Image{{Name: "ready"}}
	if !strings.Contains(current.View(), "image(s) ready") {
		t.Fatal("image prompt missing")
	}
	current.images = nil
	approvalResult := make(chan session.ApprovalOutcome, 1)
	current.mode, current.approval = modeApproval, &approvalEnvelope{question: approval.Question{Reason: "write", ToolName: "shell"}, result: approvalResult}
	if !strings.Contains(current.View(), "Approval required") {
		t.Fatal("approval prompt missing")
	}
	current.mode, current.auth = modeAuth, &authEnvelope{prompt: llm.AuthPrompt{Message: "sign in"}}
	if !strings.Contains(current.View(), "Authentication") {
		t.Fatal("auth prompt missing")
	}
	fixture.settings.snapErr = errors.New("ignored")
	_ = current.View()
	current.quitting = true
	if current.View() != "" {
		t.Fatal("quitting view is not empty")
	}
}

func messagePointer(role session.MessageRole, text string) *session.Message {
	return &session.Message{Role: role, Source: session.MessageSource{Kind: "test"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}
