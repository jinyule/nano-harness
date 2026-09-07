// Package tui implements the full-screen local agent interface.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/llm"
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
		program := tea.NewProgram(initial, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output), tea.WithAltScreen())
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
	Images    ImageNormalizer
	Subagents SubagentService
}

// RootSource publishes the root controller after composition startup.
type RootSource interface {
	Agent() (agent.Controller, error)
}

// PolicyRegistry changes durable root approval policy.
type PolicyRegistry interface {
	SetPolicy(context.Context, string, session.ApprovalPolicy) error
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
	interactionMu sync.Mutex
	initial       []session.Event
}

// New validates an assembled TUI without starting terminal I/O.
func New(config Config) (*App, error) {
	if config.Root == nil || config.Registry == nil || config.LLM == nil || config.Settings == nil || config.Approval == nil || config.Images == nil || config.Subagents == nil {
		return nil, ErrInvalidConfig
	}
	return &App{config: config, events: make(chan any, 512), stop: make(chan struct{}), uiGone: make(chan struct{})}, nil
}

// ID returns the stable plugin identity.
func (*App) ID() string { return "tui" }

// Start subscribes to durable events and publishes the approval broker.
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
		app.mu.Unlock()
		dispose()
		<-forwardDone
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
	initial := append([]session.Event(nil), app.initial...)
	app.mu.Unlock()
	model := newModel(ctx, app, initial)
	err := runProgram(ctx, input, output, model)
	app.agent.Interrupt()
	app.mu.Lock()
	select {
	case <-app.uiGone:
	default:
		close(app.uiGone)
	}
	app.mu.Unlock()
	if errors.Is(err, context.Canceled) {
		return ctx.Err()
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
var _ llm.AuthInteraction = (*App)(nil)

type inputMode int

const (
	modeNormal inputMode = iota
	modeApproval
	modeAuth
)

type model struct {
	ctx        context.Context
	app        *App
	viewport   viewport.Model
	input      textinput.Model
	width      int
	height     int
	lines      []string
	mode       inputMode
	approval   *approvalEnvelope
	auth       *authEnvelope
	images     []session.Image
	stream     string
	streamText string
	quitting   bool
}

func newModel(ctx context.Context, app *App, initial []session.Event) model {
	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = "Ask nano-harness or type /help"
	input.Focus()
	viewport := viewport.New(80, 20)
	current := model{ctx: ctx, app: app, viewport: viewport, input: input, width: 80, height: 24}
	for _, event := range initial {
		current.applyEvent(event, false)
	}
	current.refresh()
	return current
}

func (model model) Init() tea.Cmd { return waitUI(model.app.events, model.app.stop) }

func waitUI(events <-chan any, stop <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case message := <-events:
			return message
		case <-stop:
			return tea.Quit()
		}
	}
}

func (model model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		follow := model.viewport.AtBottom()
		model.width, model.height = message.Width, message.Height
		model.viewport.Width = max(message.Width-4, 20)
		model.viewport.Height = max(message.Height-7, 3)
		model.input.Width = max(message.Width-8, 10)
		model.refresh()
		if follow {
			model.viewport.GotoBottom()
		}
		return model, nil
	case transcriptMessage:
		model.applyEvent(message.event, true)
		model.refresh()
		return model, waitUI(model.app.events, model.app.stop)
	case approvalEnvelope:
		model.mode = modeApproval
		model.approval = &message
		model.input.SetValue("")
		model.input.Placeholder = "y to allow once; n to reject"
		model.input.EchoMode = textinput.EchoNormal
		return model, waitUI(model.app.events, model.app.stop)
	case authEnvelope:
		model.mode = modeAuth
		model.auth = &message
		model.input.SetValue("")
		model.input.Placeholder = message.prompt.Placeholder
		if message.prompt.Type == llm.AuthPromptSecret {
			model.input.EchoMode = textinput.EchoPassword
			model.input.EchoCharacter = '•'
		} else {
			model.input.EchoMode = textinput.EchoNormal
		}
		return model, waitUI(model.app.events, model.app.stop)
	case authNoticeMessage:
		text := message.notice.Message
		if message.notice.URL != "" {
			text += " " + message.notice.URL
		}
		if message.notice.UserCode != "" {
			text += " code=" + message.notice.UserCode
		}
		model.addLine("auth> " + text)
		return model, waitUI(model.app.events, model.app.stop)
	case operationMessage:
		if message.err != nil {
			model.addLine("error> " + message.err.Error())
		} else {
			model.addLine("system> " + message.text)
		}
		return model, nil
	case attachmentMessage:
		if message.err != nil {
			model.addLine("error> " + message.err.Error())
		} else {
			model.images = append(model.images, message.image)
			model.addLine(fmt.Sprintf("system> attached %s (%dx%d)", message.image.Name, message.image.Width, message.image.Height))
		}
		return model, nil
	case turnMessage:
		if message.result.Err != nil {
			model.addLine("turn> " + string(message.result.Outcome) + ": " + message.result.Err.Error())
		}
		return model, nil
	case tea.KeyMsg:
		if message.String() == "ctrl+c" {
			return model.cancelOrQuit()
		}
		if message.String() == "enter" {
			return model.submit()
		}
		switch message.String() {
		case "pgup", "pgdown", "ctrl+u", "ctrl+d", "up", "down":
			model.viewport, _ = model.viewport.Update(message)
			return model, nil
		}
		var command tea.Cmd
		model.input, command = model.input.Update(message)
		return model, command
	}
	var command tea.Cmd
	model.input, command = model.input.Update(message)
	model.viewport, _ = model.viewport.Update(message)
	return model, command
}

func (model model) cancelOrQuit() (tea.Model, tea.Cmd) {
	switch model.mode {
	case modeApproval:
		model.approval.result <- session.ApprovalCancelled
		model.approval = nil
		model.restoreInput()
		return model, nil
	case modeAuth:
		model.auth.result <- authResult{err: context.Canceled}
		model.auth = nil
		model.restoreInput()
		return model, nil
	case modeNormal:
	default:
	}
	model.quitting = true
	return model, tea.Quit
}

func (model model) submit() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(model.input.Value())
	switch model.mode {
	case modeApproval:
		outcome := session.ApprovalRejected
		if strings.EqualFold(value, "y") || strings.EqualFold(value, "yes") {
			outcome = session.ApprovalAllowedOnce
		}
		model.approval.result <- outcome
		model.approval = nil
		model.restoreInput()
		return model, nil
	case modeAuth:
		model.auth.result <- authResult{value: value}
		model.auth = nil
		model.restoreInput()
		return model, nil
	case modeNormal:
		// Normal input continues through command or message submission below.
	}
	model.input.SetValue("")
	if value == "" {
		return model, nil
	}
	if strings.HasPrefix(value, "/") {
		return model.command(value)
	}
	content := make([]session.ContentBlock, 0, len(model.images)+1)
	content = append(content, session.ContentBlock{Type: session.ContentText, Text: value})
	for index := range model.images {
		image := model.images[index]
		content = append(content, session.ContentBlock{Type: session.ContentImage, Image: &image})
	}
	model.images = nil
	message := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: content}
	command := func() tea.Msg {
		results, err := model.app.agent.Submit(model.ctx, message)
		if err != nil {
			return turnMessage{result: agent.TurnResult{Outcome: session.OutcomeError, Err: err}}
		}
		select {
		case result := <-results:
			return turnMessage{result: result}
		case <-model.ctx.Done():
			return turnMessage{result: agent.TurnResult{Outcome: session.OutcomeCanceled, Err: model.ctx.Err()}}
		}
	}
	return model, command
}

func (model model) command(value string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(value)
	name := fields[0]
	switch name {
	case "/quit":
		model.quitting = true
		return model, tea.Quit
	case "/help":
		model.addLine("commands> /attach PATH · /accounts · /login PROVIDER METHOD · /logout PROVIDER · /models PROVIDER · /model PROVIDER MODEL · /compact · /permission ask|never · /agents · /interrupt · /steer TEXT · /quit")
		return model, nil
	case "/interrupt":
		model.app.agent.Interrupt()
		model.addLine("system> interrupt requested")
		return model, nil
	case "/attach":
		path := strings.TrimSpace(strings.TrimPrefix(value, name))
		return model, func() tea.Msg {
			image, err := model.app.config.Images.Normalize(model.ctx, path)
			return attachmentMessage{image: image, err: err}
		}
	case "/accounts":
		return model, model.accountsCommand()
	case "/models":
		if len(fields) != 2 {
			return model.withError("usage: /models PROVIDER")
		}
		return model, model.modelsCommand(fields[1])
	case "/login":
		if len(fields) != 3 {
			return model.withError("usage: /login PROVIDER METHOD")
		}
		return model, func() tea.Msg {
			err := model.app.config.LLM.Login(model.ctx, fields[1], fields[2], model.app)
			return operationMessage{text: "login stored for " + fields[1], err: err}
		}
	case "/logout":
		if len(fields) != 2 {
			return model.withError("usage: /logout PROVIDER")
		}
		return model, func() tea.Msg {
			err := model.app.config.LLM.Logout(model.ctx, fields[1])
			return operationMessage{text: "logged out " + fields[1], err: err}
		}
	case "/model":
		if len(fields) != 3 {
			return model.withError("usage: /model PROVIDER MODEL")
		}
		return model, model.routeCommand(fields[1], fields[2])
	case "/compact":
		return model, func() tea.Msg {
			compacted, err := model.app.agent.Compact(model.ctx)
			return operationMessage{text: fmt.Sprintf("compaction completed=%t", compacted), err: err}
		}
	case "/permission":
		if len(fields) != 2 || fields[1] != "ask" && fields[1] != "never" {
			return model.withError("usage: /permission ask|never")
		}
		return model, func() tea.Msg {
			err := model.app.config.Registry.SetPolicy(model.ctx, model.app.agent.Status().SessionID, session.ApprovalPolicy(fields[1]))
			return operationMessage{text: "permission policy=" + fields[1], err: err}
		}
	case "/agents":
		return model, func() tea.Msg {
			infos, err := model.app.config.Subagents.List("")
			if err != nil {
				return operationMessage{err: err}
			}
			if len(infos) == 0 {
				return operationMessage{text: "no subagents"}
			}
			lines := make([]string, len(infos))
			for index, info := range infos {
				lines[index] = fmt.Sprintf("%s %s mode=%s busy=%t outcome=%s", info.SessionID, info.Label, info.Mode, info.Busy, info.Last.Outcome)
			}
			return operationMessage{text: strings.Join(lines, "\n")}
		}
	case "/steer":
		text := strings.TrimSpace(strings.TrimPrefix(value, name))
		return model, func() tea.Msg {
			err := model.app.agent.Steer(model.ctx, session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
			return operationMessage{text: "steer queued", err: err}
		}
	default:
		return model.withError("unknown command; use /help")
	}
}

func (model model) accountsCommand() tea.Cmd {
	return func() tea.Msg {
		accounts, err := model.app.config.LLM.Accounts(model.ctx)
		if err != nil {
			return operationMessage{err: err}
		}
		if len(accounts) == 0 {
			return operationMessage{text: "no stored accounts"}
		}
		lines := make([]string, len(accounts))
		for index, account := range accounts {
			lines[index] = fmt.Sprintf("%s kind=%s source=%s", account.Provider, account.Kind, account.Source)
		}
		return operationMessage{text: strings.Join(lines, "\n")}
	}
}

func (model model) modelsCommand(provider string) tea.Cmd {
	return func() tea.Msg {
		models, err := model.app.config.LLM.Models(provider)
		if err != nil {
			return operationMessage{err: err}
		}
		lines := make([]string, len(models))
		for index, candidate := range models {
			lines[index] = fmt.Sprintf("%s/%s context=%d vision=%t tools=%t", candidate.Provider, candidate.ID, candidate.ContextWindow, candidate.Vision, candidate.Tools)
		}
		return operationMessage{text: strings.Join(lines, "\n")}
	}
}

func (model model) routeCommand(provider, routeModel string) tea.Cmd {
	return func() tea.Msg {
		_, revision, err := model.app.config.Settings.Snapshot()
		if err == nil {
			err = model.app.config.Settings.Update(model.ctx, revision, func(document *settings.Document) error {
				document.Route = settings.Route{Provider: provider, Model: routeModel}
				return nil
			})
		}
		return operationMessage{text: "route=" + provider + "/" + routeModel, err: err}
	}
}

func (model model) withError(message string) (tea.Model, tea.Cmd) {
	model.addLine("error> " + message)
	return model, nil
}

func (model *model) restoreInput() {
	model.mode = modeNormal
	model.input.SetValue("")
	model.input.Placeholder = "Ask nano-harness or type /help"
	model.input.EchoMode = textinput.EchoNormal
}

func (model *model) applyEvent(event session.Event, live bool) {
	record := event.Record
	switch record.Type {
	case session.RecordUserMessage:
		text := session.Text(*record.Message)
		attachments := 0
		for _, block := range record.Message.Content {
			if block.Type == session.ContentImage {
				attachments++
			}
		}
		if attachments > 0 {
			text += fmt.Sprintf(" [images=%d]", attachments)
		}
		model.addLine("you> " + text)
	case session.RecordRequestHeader:
		model.addLine(fmt.Sprintf("route> %s/%s", record.Header.Provider, record.Header.Model))
	case session.RecordAssistantChunk:
		if live && record.Chunk.Kind == session.ChunkText {
			model.appendStream("assistant", record.Chunk.Text)
		} else if live && record.Chunk.Kind == session.ChunkReasoning {
			model.appendStream("reasoning", record.Chunk.Text)
		}
	case session.RecordAssistantMessage:
		if !live || model.streamText != session.Text(*record.Message) {
			model.addLine("assistant> " + session.Text(*record.Message))
		}
		model.stream = ""
		model.streamText = ""
	case session.RecordToolCall:
		model.addLine(fmt.Sprintf("tool> %s %s", record.Call.Name, record.Call.Arguments))
	case session.RecordApprovalAsked:
		model.addLine("approval> " + record.Approval.Reason)
	case session.RecordToolResult:
		prefix := "result"
		if record.Result.IsError {
			prefix = "tool-error"
		}
		model.addLine(prefix + "> " + record.Result.Output)
	case session.RecordRetry:
		model.addLine(fmt.Sprintf("retry> attempt=%d delay=%dms reason=%s", record.Retry.Attempt, record.Retry.DelayMS, record.Retry.Failure))
	case session.RecordCompactionStart:
		model.addLine("compact> started")
	case session.RecordCompactionEnd:
		if record.Compaction.Error == "" {
			model.addLine("compact> completed")
		} else {
			model.addLine("compact> " + record.Compaction.Error)
		}
	case session.RecordTurnEnd:
		model.streamText = ""
		model.addLine("turn> " + string(record.Outcome))
	case session.RecordTurnStart, session.RecordStepStart, session.RecordApprovalDecided,
		session.RecordApprovalPolicy, session.RecordRetryStarted, session.RecordCompactionSummary,
		session.RecordSubagentDescriptor, session.RecordStepEnd:
		// These facts affect replay or lifecycle state but have no standalone TUI line.
	}
}

func (model *model) appendStream(kind, delta string) {
	if kind == "assistant" {
		model.streamText += delta
	}
	if model.stream != kind || len(model.lines) == 0 {
		model.lines = append(model.lines, kind+"> "+delta)
		model.stream = kind
	} else {
		model.lines[len(model.lines)-1] += delta
	}
	model.refresh()
}

func (model *model) addLine(value string) {
	model.stream = ""
	model.lines = append(model.lines, value)
	if len(model.lines) > 4000 {
		model.lines = append([]string(nil), model.lines[len(model.lines)-4000:]...)
	}
	model.refresh()
}

func (model *model) refresh() {
	follow := model.viewport.AtBottom()
	model.viewport.SetContent(ansi.Hardwrap(strings.Join(model.lines, "\n"), model.viewport.Width, true))
	if follow {
		model.viewport.GotoBottom()
	}
}

func (model model) View() string {
	if model.quitting {
		return ""
	}
	document, _, _ := model.app.config.Settings.Snapshot()
	status := model.app.agent.Status()
	header := headerStyle.MaxWidth(model.width).Render(fmt.Sprintf(" nano-harness  %s/%s  session=%s  busy=%t ", document.Route.Provider, document.Route.Model, status.SessionID, status.Busy))
	prompt := ""
	switch model.mode {
	case modeApproval:
		prompt = warningStyle.Render(fmt.Sprintf("Approval required: %s (%s)", model.approval.question.Reason, model.approval.question.ToolName))
	case modeAuth:
		prompt = warningStyle.Render("Authentication: " + model.auth.prompt.Message)
	case modeNormal:
		if len(model.images) > 0 {
			prompt = mutedStyle.Render(fmt.Sprintf("%d image(s) ready", len(model.images)))
		}
	}
	body := lipgloss.NewStyle().Padding(0, 2).Width(max(model.width, 20)).Render(model.viewport.View())
	footer := mutedStyle.Render(" /help · ctrl+c quit/cancel ")
	return header + "\n" + body + "\n" + prompt + "\n" + model.input.View() + "\n" + footer
}

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	warningStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	mutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)
