// Package tui implements the full-screen local agent interface.
package tui

import (
	"context"
	"errors"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/question"
	"github.com/jinyule/nano-harness/internal/app/settings"
	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidConfig identifies an incomplete or invalid TUI composition.
	ErrInvalidConfig = errors.New("invalid TUI configuration")
	// ErrNotRunning indicates the TUI has not started, has already run, or has stopped.
	ErrNotRunning = errors.New("TUI is not running")
	runProgram    = func(ctx context.Context, input io.Reader, output io.Writer, initial tea.Model) error {
		program := tea.NewProgram(initial, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output))
		_, err := program.Run()
		return err
	}
)

// Config supplies the local interaction surface and its use cases.
type Config struct {
	Root      RootSource
	Registry  PolicyRegistry
	LLM       ModelService
	Settings  SettingsService
	Approval  ApprovalRegistry
	Questions QuestionRegistry
	Images    ImageNormalizer
	Subagents SubagentService
}

// RootSource publishes the root controller after composition startup.
type RootSource interface {
	Agent() (agent.Controller, error)
}

// PolicyRegistry changes durable root session policy: the approval policy
// and plan mode.
type PolicyRegistry interface {
	SetPolicy(context.Context, string, session.ApprovalPolicy) error
	SetPlanMode(context.Context, string, bool) (plan.Change, error)
}

// ModelService exposes account and model operations used by commands.
type ModelService interface {
	Login(context.Context, string, string, llm.AuthInteraction) error
	Logout(context.Context, string) error
	Models(string) ([]llm.ModelInfo, error)
	Accounts(context.Context) ([]llm.AccountInfo, error)
}

// SettingsService exposes hot route reads and optimistic updates.
type SettingsService interface {
	Snapshot() (settings.Document, uint64, error)
	Update(context.Context, uint64, func(*settings.Document) error) error
}

// ApprovalRegistry publishes the local approval broker for one scope.
type ApprovalRegistry interface {
	RegisterBroker(approval.Broker, *plugin.Scope) error
}

// QuestionRegistry publishes the local user-questions broker for one scope.
type QuestionRegistry interface {
	RegisterBroker(question.Broker, *plugin.Scope) error
}

// SubagentService lists live delegated agents for presentation.
type SubagentService interface {
	List(string) ([]appSubagent.Info, error)
}

// ImageNormalizer is the local attachment boundary consumed by the TUI.
type ImageNormalizer interface {
	Normalize(context.Context, string) (session.Image, error)
}

type approvalEnvelope struct {
	question approval.Question
	result   chan session.ApprovalOutcome
}

type questionEnvelope struct {
	request question.Request
	result  chan questionResult
}

type questionResult struct {
	answers []question.Answer
	err     error
}

type authEnvelope struct {
	prompt llm.AuthPrompt
	result chan authResult
}

type authResult struct {
	value string
	err   error
}

type transcriptMessage struct{ event session.Event }
type authNoticeMessage struct{ notice llm.AuthNotice }
type operationMessage struct {
	text string
	err  error
}
type turnMessage struct{ result agent.TurnResult }
type attachmentMessage struct {
	image session.Image
	err   error
}
type planMessage struct {
	text    string
	err     error
	message *session.Message
}

// App is both a lifecycle plugin, approval broker, and auth interaction.
type App struct {
	config Config
	agent  agent.Controller

	mu            sync.Mutex
	started       bool
	active        bool
	ran           bool
	events        chan any
	stop          chan struct{}
	uiGone        chan struct{}
	forwardDone   chan struct{}
	runCancel     context.CancelFunc
	runDone       chan struct{}
	interactionMu sync.Mutex
	initial       []session.Event
}

// New validates an assembled TUI without starting terminal I/O.
func New(config Config) (*App, error) {
	if config.Root == nil || config.Registry == nil || config.LLM == nil || config.Settings == nil || config.Approval == nil || config.Questions == nil || config.Images == nil || config.Subagents == nil {
		return nil, ErrInvalidConfig
	}
	return &App{config: config, events: make(chan any, 512), stop: make(chan struct{}), uiGone: make(chan struct{})}, nil
}

// ID returns the stable plugin identity.
func (*App) ID() string { return "tui" }

// Start subscribes to durable events and publishes the approval and
// question brokers.
func (app *App) Start(ctx context.Context, scope *plugin.Scope) error {
	app.mu.Lock()
	if app.started {
		app.mu.Unlock()
		return ErrInvalidConfig
	}
	root, err := app.config.Root.Agent()
	if err != nil {
		app.mu.Unlock()
		return err
	}
	app.agent = root
	initial, err := app.agent.Events(ctx)
	if err != nil {
		app.mu.Unlock()
		return err
	}
	updates, dispose, err := app.agent.Subscribe(256)
	if err != nil {
		app.mu.Unlock()
		return err
	}
	app.initial = initial
	app.started, app.active = true, true
	forwardDone := make(chan struct{})
	app.forwardDone = forwardDone
	app.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		app.mu.Lock()
		if app.active {
			app.active = false
			close(app.stop)
		}
		cancel, done := app.runCancel, app.runDone
		app.mu.Unlock()
		dispose()
		if cancel != nil {
			cancel()
		}
		<-forwardDone
		if done != nil {
			<-done
		}
		return nil
	}); err != nil {
		dispose()
		app.mu.Lock()
		app.started, app.active = false, false
		app.agent, app.initial = nil, nil
		app.mu.Unlock()
		return err
	}
	go func() {
		defer close(forwardDone)
		for {
			select {
			case <-app.stop:
				return
			case event, ok := <-updates:
				if !ok {
					return
				}
				select {
				case app.events <- transcriptMessage{event: event}:
				case <-app.stop:
					return
				}
			}
		}
	}()
	if err := app.config.Approval.RegisterBroker(app, scope); err != nil {
		_ = scope.Close(context.WithoutCancel(ctx))
		return err
	}
	if err := app.config.Questions.RegisterBroker(questionBroker{app: app}, scope); err != nil {
		_ = scope.Close(context.WithoutCancel(ctx))
		return err
	}
	return nil
}

// Run owns one alternate-screen Bubble Tea program until quit or cancellation.
func (app *App) Run(ctx context.Context, input io.Reader, output io.Writer) error {
	if input == nil || output == nil {
		return ErrInvalidConfig
	}
	app.mu.Lock()
	if !app.active || app.ran {
		app.mu.Unlock()
		return ErrNotRunning
	}
	app.ran = true
	runContext, cancel := context.WithCancel(ctx)
	app.runCancel = cancel
	app.runDone = make(chan struct{})
	defer close(app.runDone)
	initial := append([]session.Event(nil), app.initial...)
	app.mu.Unlock()
	model := newModel(runContext, app, initial)
	err := runProgram(runContext, input, output, model)
	cancel()
	app.agent.Interrupt()
	app.mu.Lock()
	select {
	case <-app.uiGone:
	default:
		close(app.uiGone)
	}
	app.mu.Unlock()
	model.commands.close()
	if errors.Is(err, context.Canceled) {
		return runContext.Err()
	}
	return err
}

// Ask presents one serialized local approval question.
func (app *App) Ask(ctx context.Context, question approval.Question) session.ApprovalOutcome {
	app.interactionMu.Lock()
	defer app.interactionMu.Unlock()
	envelope := approvalEnvelope{question: question, result: make(chan session.ApprovalOutcome, 1)}
	select {
	case app.events <- envelope:
	case <-ctx.Done():
		return session.ApprovalCancelled
	case <-app.stop:
		return session.ApprovalUnavailable
	case <-app.uiGone:
		return session.ApprovalUnavailable
	}
	select {
	case outcome := <-envelope.result:
		return outcome
	case <-ctx.Done():
		return session.ApprovalCancelled
	case <-app.stop:
		return session.ApprovalUnavailable
	case <-app.uiGone:
		return session.ApprovalUnavailable
	}
}

// questionBroker presents user questions through the same serialized
// interaction channel as approvals; App already uses Ask for approvals.
type questionBroker struct{ app *App }

// Ask presents one request and returns the user's answers. Ctrl+C in the
// terminal returns question.ErrCancelled; a closed terminal returns
// ErrNotRunning, which the question service treats as unavailable.
func (broker questionBroker) Ask(ctx context.Context, request question.Request) ([]question.Answer, error) {
	app := broker.app
	app.interactionMu.Lock()
	defer app.interactionMu.Unlock()
	envelope := questionEnvelope{request: request, result: make(chan questionResult, 1)}
	select {
	case app.events <- envelope:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-app.stop:
		return nil, ErrNotRunning
	case <-app.uiGone:
		return nil, ErrNotRunning
	}
	select {
	case result := <-envelope.result:
		return result.answers, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-app.stop:
		return nil, ErrNotRunning
	case <-app.uiGone:
		return nil, ErrNotRunning
	}
}

// Prompt presents one serialized provider-owned auth input.
func (app *App) Prompt(ctx context.Context, prompt llm.AuthPrompt) (string, error) {
	app.interactionMu.Lock()
	defer app.interactionMu.Unlock()
	envelope := authEnvelope{prompt: prompt, result: make(chan authResult, 1)}
	select {
	case app.events <- envelope:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-app.stop:
		return "", ErrNotRunning
	case <-app.uiGone:
		return "", ErrNotRunning
	}
	select {
	case result := <-envelope.result:
		return result.value, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-app.stop:
		return "", ErrNotRunning
	case <-app.uiGone:
		return "", ErrNotRunning
	}
}

// Notify queues provider-owned OAuth progress without exposing tokens.
func (app *App) Notify(notice llm.AuthNotice) {
	select {
	case app.events <- authNoticeMessage{notice: notice}:
	case <-app.stop:
	case <-app.uiGone:
	}
}

var _ approval.Broker = (*App)(nil)
var _ question.Broker = questionBroker{}
var _ llm.AuthInteraction = (*App)(nil)
