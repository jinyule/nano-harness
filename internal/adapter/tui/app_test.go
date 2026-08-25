package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/settings"
	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type fakeController struct {
	mu            sync.Mutex
	status        agent.Status
	events        []session.Event
	eventsErr     error
	surface       []session.SurfaceNode
	surfaceErr    error
	updates       chan session.Event
	subscribeErr  error
	disposed      bool
	interrupts    int
	submitted     []session.Message
	submitResult  agent.TurnResult
	submitErr     error
	submitChannel <-chan agent.TurnResult
	steered       []session.Message
	steerErr      error
	compact       bool
	compactErr    error
}

func (controller *fakeController) Submit(_ context.Context, message session.Message) (<-chan agent.TurnResult, error) {
	controller.mu.Lock()
	controller.submitted = append(controller.submitted, message)
	result, err := controller.submitResult, controller.submitErr
	controller.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if controller.submitChannel != nil {
		return controller.submitChannel, nil
	}
	channel := make(chan agent.TurnResult, 1)
	channel <- result
	close(channel)
	return channel, nil
}
func (controller *fakeController) Followup(ctx context.Context, message session.Message) (<-chan agent.TurnResult, error) {
	return controller.Submit(ctx, message)
}
func (controller *fakeController) Steer(_ context.Context, message session.Message) error {
	controller.steered = append(controller.steered, message)
	return controller.steerErr
}
func (controller *fakeController) Interrupt()          { controller.interrupts++ }
func (*fakeController) WhenIdle(context.Context) error { return nil }
func (controller *fakeController) Subscribe(buffer int) (<-chan session.Event, func(), error) {
	if controller.subscribeErr != nil {
		return nil, nil, controller.subscribeErr
	}
	if controller.updates == nil {
		controller.updates = make(chan session.Event, max(buffer, 1))
	}
	return controller.updates, func() {
		controller.mu.Lock()
		if !controller.disposed {
			controller.disposed = true
			close(controller.updates)
		}
		controller.mu.Unlock()
	}, nil
}
func (controller *fakeController) Status() agent.Status { return controller.status }
func (controller *fakeController) Events(context.Context) ([]session.Event, error) {
	return slices.Clone(controller.events), controller.eventsErr
}
func (controller *fakeController) Surface(context.Context) ([]session.SurfaceNode, error) {
	return slices.Clone(controller.surface), controller.surfaceErr
}
func (controller *fakeController) Compact(context.Context) (bool, error) {
	return controller.compact, controller.compactErr
}

type fakeRoot struct {
	controller agent.Controller
	err        error
}

func (root *fakeRoot) Agent() (agent.Controller, error) { return root.controller, root.err }

type fakePolicyRegistry struct {
	session string
	policy  session.ApprovalPolicy
	err     error
}

func (registry *fakePolicyRegistry) SetPolicy(_ context.Context, id string, policy session.ApprovalPolicy) error {
	registry.session, registry.policy = id, policy
	return registry.err
}

type fakeModelService struct {
	accounts    []llm.AccountInfo
	accountsErr error
	models      []llm.ModelInfo
	modelsErr   error
	login       [2]string
	loginErr    error
	logout      string
	logoutErr   error
}

func (service *fakeModelService) Login(_ context.Context, provider, method string, _ llm.AuthInteraction) error {
	service.login = [2]string{provider, method}
	return service.loginErr
}
func (service *fakeModelService) Logout(_ context.Context, provider string) error {
	service.logout = provider
	return service.logoutErr
}
func (service *fakeModelService) Models(string) ([]llm.ModelInfo, error) {
	return slices.Clone(service.models), service.modelsErr
}
func (service *fakeModelService) Accounts(context.Context) ([]llm.AccountInfo, error) {
	return slices.Clone(service.accounts), service.accountsErr
}

type fakeSettings struct {
	document  settings.Document
	revision  uint64
	snapErr   error
	updateErr error
	updated   bool
}

func (configuration *fakeSettings) Snapshot() (settings.Document, uint64, error) {
	return configuration.document, configuration.revision, configuration.snapErr
}
func (configuration *fakeSettings) Update(_ context.Context, revision uint64, mutate func(*settings.Document) error) error {
	if configuration.updateErr != nil {
		return configuration.updateErr
	}
	if err := mutate(&configuration.document); err != nil {
		return err
	}
	configuration.revision = revision + 1
	configuration.updated = true
	return nil
}

type fakeApprovalRegistry struct {
	broker approval.Broker
	err    error
	closed bool
}

func (registry *fakeApprovalRegistry) RegisterBroker(broker approval.Broker, scope *plugin.Scope) error {
	if registry.err != nil {
		return registry.err
	}
	registry.broker = broker
	return scope.Defer(func(context.Context) error {
		registry.broker = nil
		registry.closed = true
		return nil
	})
}

type fakeImages struct {
	image session.Image
	err   error
	path  string
}

func (images *fakeImages) Normalize(_ context.Context, path string) (session.Image, error) {
	images.path = path
	return images.image, images.err
}

type fakeSubagents struct {
	infos []appSubagent.Info
	err   error
}

func (service *fakeSubagents) List(string) ([]appSubagent.Info, error) {
	return slices.Clone(service.infos), service.err
}

type appFixture struct {
	controller *fakeController
	root       *fakeRoot
	registry   *fakePolicyRegistry
	models     *fakeModelService
	settings   *fakeSettings
	approval   *fakeApprovalRegistry
	images     *fakeImages
	subagents  *fakeSubagents
}

func newAppFixture() (*appFixture, Config) {
	fixture := &appFixture{
		controller: &fakeController{status: agent.Status{SessionID: "root"}, submitResult: agent.TurnResult{Outcome: session.OutcomeCompleted, Text: "done"}},
		registry:   &fakePolicyRegistry{}, models: &fakeModelService{},
		settings: &fakeSettings{document: settings.Defaults(), revision: 3},
		approval: &fakeApprovalRegistry{}, images: &fakeImages{}, subagents: &fakeSubagents{},
	}
	fixture.root = &fakeRoot{controller: fixture.controller}
	return fixture, Config{Root: fixture.root, Registry: fixture.registry, LLM: fixture.models, Settings: fixture.settings, Approval: fixture.approval, Images: fixture.images, Subagents: fixture.subagents}
}

func TestNew_RequiresEveryUseCase(t *testing.T) {
	_, valid := newAppFixture()
	if app, err := New(valid); err != nil || app.ID() != "tui" {
		t.Fatalf("New() = %+v, %v", app, err)
	}
	for _, mutate := range []func(*Config){
		func(config *Config) { config.Root = nil }, func(config *Config) { config.Registry = nil },
		func(config *Config) { config.LLM = nil }, func(config *Config) { config.Settings = nil },
		func(config *Config) { config.Approval = nil }, func(config *Config) { config.Images = nil },
		func(config *Config) { config.Subagents = nil },
	} {
		config := valid
		mutate(&config)
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid config error = %v", err)
		}
	}
}

func TestAppStart_ForwardsEventsRegistersBrokerAndCleans(t *testing.T) {
	fixture, config := newAppFixture()
	initial := session.Event{Sequence: 1, Record: session.Record{Type: session.RecordTurnStart, Turn: 1}}
	fixture.controller.events = []session.Event{initial}
	app, _ := New(config)
	scope := &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if app.config.Approval != fixture.approval || fixture.approval.broker != app || len(app.initial) != 1 {
		t.Fatal("start did not publish broker or initial events")
	}
	if err := app.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double start error = %v", err)
	}
	update := session.Event{Sequence: 2, Record: session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted}}
	fixture.controller.updates <- update
	select {
	case message := <-app.events:
		if message.(transcriptMessage).event.Sequence != 2 {
			t.Fatalf("forwarded = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not forwarded")
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fixture.controller.disposed || !fixture.approval.closed || app.active {
		t.Fatalf("cleanup state: disposed=%v approval=%v active=%v", fixture.controller.disposed, fixture.approval.closed, app.active)
	}
}

func TestAppStart_StopsAForwarderBlockedByBackpressure(t *testing.T) {
	fixture, config := newAppFixture()
	app, _ := New(config)
	scope := &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < cap(app.events); index++ {
		app.events <- operationMessage{text: "full"}
	}
	fixture.controller.updates <- session.Event{Record: session.Record{Type: session.RecordTurnStart, Turn: 1}}
	deadline := time.Now().Add(time.Second)
	for len(fixture.controller.updates) != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if len(fixture.controller.updates) != 0 {
		t.Fatal("forwarder did not receive update")
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAppStart_ForwarderExitsWhenSubscriptionCloses(t *testing.T) {
	fixture, config := newAppFixture()
	app, _ := New(config)
	scope := &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	fixture.controller.mu.Lock()
	fixture.controller.disposed = true
	close(fixture.controller.updates)
	fixture.controller.mu.Unlock()
	select {
	case <-app.forwardDone:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not stop on closed subscription")
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAppStart_ContainsRootSnapshotSubscriptionScopeAndBrokerFailures(t *testing.T) {
	failure := errors.New("failure")
	for _, test := range []struct {
		name      string
		configure func(*appFixture)
	}{
		{name: "root", configure: func(fixture *appFixture) { fixture.root.err = failure }},
		{name: "events", configure: func(fixture *appFixture) { fixture.controller.eventsErr = failure }},
		{name: "subscribe", configure: func(fixture *appFixture) { fixture.controller.subscribeErr = failure }},
		{name: "broker", configure: func(fixture *appFixture) { fixture.approval.err = failure }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, config := newAppFixture()
			test.configure(fixture)
			app, _ := New(config)
			scope := &plugin.Scope{}
			if err := app.Start(context.Background(), scope); !errors.Is(err, failure) {
				t.Fatalf("Start() error = %v", err)
			}
			_ = scope.Close(context.Background())
		})
	}
	fixture, config := newAppFixture()
	app, _ := New(config)
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := app.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) || !fixture.controller.disposed || app.active || app.started {
		t.Fatalf("closed scope error = %v state=%+v", err, app)
	}
}

func TestAppRun_ValidatesStateInterruptsAndMapsCancellation(t *testing.T) {
	previousRun := runProgram
	t.Cleanup(func() { runProgram = previousRun })
	fixture, config := newAppFixture()
	app, _ := New(config)
	if err := app.Run(context.Background(), nil, io.Discard); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil input error = %v", err)
	}
	if err := app.Run(context.Background(), bytes.NewReader(nil), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil output error = %v", err)
	}
	if err := app.Run(context.Background(), bytes.NewReader(nil), io.Discard); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive error = %v", err)
	}
	scope := &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	called := false
	runProgram = func(context.Context, io.Reader, io.Writer, tea.Model) error {
		called = true
		return nil
	}
	if err := app.Run(context.Background(), bytes.NewReader(nil), io.Discard); err != nil || !called || fixture.controller.interrupts != 1 {
		t.Fatalf("Run() error = %v called=%v interrupts=%d", err, called, fixture.controller.interrupts)
	}
	if err := app.Run(context.Background(), bytes.NewReader(nil), io.Discard); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second Run() error = %v", err)
	}

	_, config = newAppFixture()
	app, _ = New(config)
	scope = &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runProgram = func(context.Context, io.Reader, io.Writer, tea.Model) error { return context.Canceled }
	if err := app.Run(ctx, bytes.NewReader(nil), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Run() error = %v", err)
	}

	_, config = newAppFixture()
	app, _ = New(config)
	scope = &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	close(app.uiGone)
	runProgram = func(context.Context, io.Reader, io.Writer, tea.Model) error { return nil }
	if err := app.Run(context.Background(), bytes.NewReader(nil), io.Discard); err != nil {
		t.Fatalf("preclosed UI Run() error = %v", err)
	}
}

type immediateQuitModel struct{}

func (immediateQuitModel) Init() tea.Cmd                             { return tea.Quit }
func (model immediateQuitModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return model, nil }
func (immediateQuitModel) View() string                              { return "" }

func TestDefaultRunProgram_ExecutesBubbleTeaProgram(t *testing.T) {
	previousRun := runProgram
	if err := previousRun(context.Background(), bytes.NewReader(nil), io.Discard, immediateQuitModel{}); err != nil {
		t.Fatalf("default program error = %v", err)
	}
}

func TestAppAskPromptAndNotify_AllTerminalStates(t *testing.T) {
	_, config := newAppFixture()
	app, _ := New(config)
	question := approval.Question{ToolName: "shell", Reason: "write"}
	answerDone := make(chan session.ApprovalOutcome, 1)
	go func() { answerDone <- app.Ask(context.Background(), question) }()
	envelope := (<-app.events).(approvalEnvelope)
	envelope.result <- session.ApprovalAllowedOnce
	if outcome := <-answerDone; outcome != session.ApprovalAllowedOnce {
		t.Fatalf("approval outcome = %s", outcome)
	}
	promptDone := make(chan authResult, 1)
	go func() {
		value, err := app.Prompt(context.Background(), llm.AuthPrompt{Message: "token"})
		promptDone <- authResult{value: value, err: err}
	}()
	auth := (<-app.events).(authEnvelope)
	auth.result <- authResult{value: "secret"}
	if result := <-promptDone; result.value != "secret" || result.err != nil {
		t.Fatalf("prompt result = %+v", result)
	}
	app.Notify(llm.AuthNotice{Message: "notice"})
	if message := (<-app.events).(authNoticeMessage); message.notice.Message != "notice" {
		t.Fatalf("notice = %#v", message)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := &App{events: make(chan any), stop: make(chan struct{}), uiGone: make(chan struct{})}
	if outcome := blocked.Ask(ctx, question); outcome != session.ApprovalCancelled {
		t.Fatalf("canceled send outcome = %s", outcome)
	}
	if _, err := blocked.Prompt(ctx, llm.AuthPrompt{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prompt send = %v", err)
	}

	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitApproval := make(chan session.ApprovalOutcome, 1)
	go func() { waitApproval <- app.Ask(waitCtx, question) }()
	<-app.events
	waitCancel()
	if outcome := <-waitApproval; outcome != session.ApprovalCancelled {
		t.Fatalf("canceled wait outcome = %s", outcome)
	}
	waitCtx, waitCancel = context.WithCancel(context.Background())
	waitPrompt := make(chan error, 1)
	go func() { _, err := app.Prompt(waitCtx, llm.AuthPrompt{}); waitPrompt <- err }()
	<-app.events
	waitCancel()
	if err := <-waitPrompt; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prompt wait = %v", err)
	}

	for _, test := range []struct {
		name  string
		close func(*App)
		ask   func(*App) error
	}{
		{name: "approval stop", close: func(app *App) { close(app.stop) }, ask: func(app *App) error {
			if outcome := app.Ask(context.Background(), question); outcome != session.ApprovalUnavailable {
				return fmt.Errorf("outcome %s", outcome)
			}
			return nil
		}},
		{name: "approval UI gone", close: func(app *App) { close(app.uiGone) }, ask: func(app *App) error {
			if outcome := app.Ask(context.Background(), question); outcome != session.ApprovalUnavailable {
				return fmt.Errorf("outcome %s", outcome)
			}
			return nil
		}},
		{name: "prompt stop", close: func(app *App) { close(app.stop) }, ask: func(app *App) error {
			_, err := app.Prompt(context.Background(), llm.AuthPrompt{})
			return err
		}},
		{name: "prompt UI gone", close: func(app *App) { close(app.uiGone) }, ask: func(app *App) error {
			_, err := app.Prompt(context.Background(), llm.AuthPrompt{})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			waiting := &App{events: make(chan any, 1), stop: make(chan struct{}), uiGone: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- test.ask(waiting) }()
			<-waiting.events
			test.close(waiting)
			err := <-done
			if strings.HasPrefix(test.name, "prompt") && !errors.Is(err, ErrNotRunning) {
				t.Fatalf("wait error = %v", err)
			}
			if !strings.HasPrefix(test.name, "prompt") && err != nil {
				t.Fatal(err)
			}
		})
	}

	stopped := &App{events: make(chan any), stop: make(chan struct{}), uiGone: make(chan struct{})}
	close(stopped.stop)
	if outcome := stopped.Ask(context.Background(), question); outcome != session.ApprovalUnavailable {
		t.Fatalf("stopped approval = %s", outcome)
	}
	if _, err := stopped.Prompt(context.Background(), llm.AuthPrompt{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped prompt = %v", err)
	}
	stopped.Notify(llm.AuthNotice{})
	gone := &App{events: make(chan any), stop: make(chan struct{}), uiGone: make(chan struct{})}
	close(gone.uiGone)
	if outcome := gone.Ask(context.Background(), question); outcome != session.ApprovalUnavailable {
		t.Fatalf("gone approval = %s", outcome)
	}
	if _, err := gone.Prompt(context.Background(), llm.AuthPrompt{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("gone prompt = %v", err)
	}
	gone.Notify(llm.AuthNotice{})
}
