package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type inputMode int

const (
	modeNormal inputMode = iota
	modeApproval
	modeAuth
	modeQuestion
)

type model struct {
	commands   *commandGroup
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
	question   *questionState
	planActive bool
	images     []session.Image
	todos      []session.TodoItem
	goal       session.GoalState
	plan       []string
	stream     string
	streamText string
	quitting   bool
}

func newModel(ctx context.Context, app *App, initial []session.Event) model {
	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = "Ask nano-harness or type /help"
	styles := input.Styles()
	styles.Cursor.Blink = false
	input.SetStyles(styles)
	input.Focus()
	input.SetWidth(72)
	viewport := viewport.New(viewport.WithWidth(76), viewport.WithHeight(20))
	current := model{commands: &commandGroup{}, ctx: ctx, app: app, viewport: viewport, input: input, width: 80, height: 24}
	for _, event := range initial {
		current.applyEvent(event, false)
	}
	current.layout()
	return current
}

func (model model) Init() tea.Cmd {
	return model.commands.wrap(waitUI(model.app.events, model.app.stop, model.ctx.Done()))
}

func waitUI(events <-chan any, stop, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case message := <-events:
			return message
		case <-stop:
			return tea.Quit()
		case <-done:
			return tea.Quit()
		}
	}
}

func (model model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	next, command := model.update(message)
	return next, model.commands.wrap(command)
}

func (model model) update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = max(message.Width, 1), max(message.Height, 1)
		model.layout()
		return model, nil
	case transcriptMessage:
		model.applyEvent(message.event, true)
		model.refresh()
		return model, waitUI(model.app.events, model.app.stop, model.ctx.Done())
	case approvalEnvelope:
		model.mode = modeApproval
		model.approval = &message
		model.input.SetValue("")
		model.input.Placeholder = "y to allow once; n to reject"
		model.input.EchoMode = textinput.EchoNormal
		return model, waitUI(model.app.events, model.app.stop, model.ctx.Done())
	case questionEnvelope:
		model.beginQuestion(message)
		return model, waitUI(model.app.events, model.app.stop, model.ctx.Done())
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
		return model, waitUI(model.app.events, model.app.stop, model.ctx.Done())
	case authNoticeMessage:
		text := message.notice.Message
		if message.notice.URL != "" {
			text += " " + message.notice.URL
		}
		if message.notice.UserCode != "" {
			text += " code=" + message.notice.UserCode
		}
		model.addLine("auth> " + text)
		return model, waitUI(model.app.events, model.app.stop, model.ctx.Done())
	case operationMessage:
		if message.err != nil {
			model.addLine("error> " + message.err.Error())
		} else {
			model.addLine("system> " + message.text)
		}
		return model, nil
	case goalMessage:
		switch {
		case message.err != nil:
			model.addLine("error> " + message.err.Error())
		case message.failed:
			model.addLine("error> " + strings.Join(message.lines, "\n"))
		default:
			for _, line := range message.lines {
				model.addLine("goal> " + line)
			}
		}
		return model, nil
	case planMessage:
		if message.err != nil {
			model.addLine("error> " + message.err.Error())
			return model, nil
		}
		model.addLine("system> " + message.text)
		if message.message == nil {
			return model, nil
		}
		return model, model.deliverCommand(*message.message)
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
	case tea.KeyPressMsg:
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
	case modeQuestion:
		model.cancelQuestion()
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
	case modeQuestion:
		return model.answerQuestion(value)
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
	return model.send(value)
}

// send submits value and every attached image as one user message.
func (model model) send(value string) (tea.Model, tea.Cmd) {
	content := make([]session.ContentBlock, 0, len(model.images)+1)
	content = append(content, session.ContentBlock{Type: session.ContentText, Text: value})
	for index := range model.images {
		image := model.images[index]
		content = append(content, session.ContentBlock{Type: session.ContentImage, Image: &image})
	}
	model.images = nil
	message := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: content}
	return model, func() tea.Msg { return model.submitAndWait(message) }
}

// submitAndWait queues a turn and reports its terminal result.
func (model model) submitAndWait(message session.Message) tea.Msg {
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

// deliverCommand steers a message into the active turn, or starts a turn
// with it when the agent is idle.
func (model model) deliverCommand(message session.Message) tea.Cmd {
	return func() tea.Msg {
		err := model.app.agent.Steer(model.ctx, message)
		if errors.Is(err, agent.ErrAgentIdle) {
			return model.submitAndWait(message)
		}
		return operationMessage{text: "steer queued", err: err}
	}
}

func (model *model) restoreInput() {
	model.mode = modeNormal
	model.input.SetValue("")
	model.input.Placeholder = "Ask nano-harness or type /help"
	model.input.EchoMode = textinput.EchoNormal
}

func (model model) View() tea.View {
	if model.quitting {
		return tea.NewView("")
	}
	document, _, _ := model.app.config.Settings.Snapshot()
	status := model.app.agent.Status()
	mode := ""
	if model.planActive {
		mode = "mode=plan "
	}
	header := headerStyle.MaxWidth(model.width).Render(fmt.Sprintf(" nano-harness  %s/%s  session=%s  busy=%t %s%s", document.Route.Provider, document.Route.Model, status.SessionID, status.Busy, mode, goalStatus(model.goal)))
	prompt := ""
	switch model.mode {
	case modeApproval:
		prompt = warningStyle.Render(fmt.Sprintf("Approval required: %s (%s)", model.approval.question.Reason, model.approval.question.ToolName))
	case modeAuth:
		prompt = warningStyle.Render("Authentication: " + model.auth.prompt.Message)
	case modeQuestion:
		state := model.question
		prompt = warningStyle.Render(fmt.Sprintf("Question %d/%d: %s", state.index+1, len(state.envelope.request.Questions), state.current().Text))
	case modeNormal:
		if len(model.images) > 0 {
			prompt = mutedStyle.Render(fmt.Sprintf("%d image(s) ready", len(model.images)))
		}
	}
	body := lipgloss.NewStyle().Padding(0, 2).Width(model.width).Render(model.viewport.View())
	if len(model.plan) > 0 {
		body += "\n" + strings.Join(model.plan, "\n")
	}
	footer := mutedStyle.Render(" /help · ctrl+c quit/cancel ")
	content := header + "\n" + body + "\n" + ansi.Truncate(prompt, model.width, "…") + "\n" + model.input.View() + "\n" + ansi.Truncate(footer, model.width, "…")
	view := tea.NewView(lipgloss.NewStyle().MaxWidth(model.width).MaxHeight(model.height).Render(content))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	warningStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	mutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)
