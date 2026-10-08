package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/session"
	"github.com/jinyule/nano-harness/internal/core/skill"
)

func (model model) command(value string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(value)
	name := fields[0]
	switch name {
	case "/quit":
		model.quitting = true
		return model, tea.Quit
	case "/help":
		model.addLine("commands> /attach PATH · /accounts · /login PROVIDER METHOD · /logout PROVIDER · /models PROVIDER · /model PROVIDER MODEL · /compact · /permission ask|never · /sandbox read-only|workspace-write|danger-full-access · /plan [off|TEXT] · /goal [OBJECTIVE|edit OBJECTIVE|pause|resume|clear] · /agents · /interrupt · /steer TEXT · /SKILL TEXT · /quit")
		return model, nil
	case "/interrupt":
		model.app.agent.Interrupt()
		model.addLine("system> interrupt requested")
		return model, nil
	case "/attach":
		path := strings.TrimSpace(strings.TrimPrefix(value, name))
		return model, func() tea.Msg {
			image, data, err := model.app.config.Images.PrepareFile(model.ctx, path)
			return attachmentMessage{image: pendingImage{ref: image, data: data}, err: err}
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
	case "/sandbox":
		if len(fields) != 2 || !session.SandboxMode(fields[1]).Valid() {
			return model.withError("usage: /sandbox read-only|workspace-write|danger-full-access")
		}
		return model, func() tea.Msg {
			err := model.app.config.Registry.SetSandboxMode(model.ctx, model.app.agent.Status().SessionID, session.SandboxMode(fields[1]))
			return operationMessage{text: "sandbox mode=" + fields[1], err: err}
		}
	case "/plan":
		return model.planCommand(strings.TrimSpace(strings.TrimPrefix(value, name)))
	case "/goal":
		return model.goalCommand(strings.TrimPrefix(value, name))
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
		// A kebab-case /name that is not a TUI command is a skill gesture: the
		// agent injects the named skill when it is user-invocable, and otherwise
		// the text stays ordinary input.
		if skill.ValidName(strings.TrimPrefix(name, "/")) {
			return model.send(value)
		}
		return model.withError("unknown command; use /help")
	}
}

// planCommand enters plan mode, optionally with a message and the pending
// attachments delivered as the next user input, or leaves it with "off".
func (model model) planCommand(argument string) (tea.Model, tea.Cmd) {
	active := argument != "off"
	if !active && len(model.images) > 0 {
		return model.withError("attachments cannot accompany /plan off")
	}
	var message *session.Message
	var pending []pendingImage
	if active && (argument != "" || len(model.images) > 0) {
		content := imageBlocks(model.images)
		if argument != "" {
			content = append(content, session.ContentBlock{Type: session.ContentText, Text: argument})
		}
		message = &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: content}
		pending, model.images = model.images, nil
	}
	wasActive := model.planActive
	return model, func() tea.Msg {
		change, err := model.app.config.Registry.SetPlanMode(model.ctx, model.app.agent.Status().SessionID, active)
		if err != nil {
			return planMessage{err: err}
		}
		return planMessage{text: planChangeText(change, active, wasActive), message: message, pending: pending}
	}
}

// planChangeText reports a selection; wasActive is the recorded mode the
// terminal had replayed when the command ran.
func planChangeText(change plan.Change, active, wasActive bool) string {
	switch {
	case active && change == plan.Committed:
		return "Plan mode on. Use /plan off to leave."
	case active && change == plan.Cancelled:
		return "Plan mode exit cancelled; plan mode stays on."
	case active && (change == plan.Queued || !wasActive):
		return "Entering plan mode (applies from the next step). Use /plan off to leave."
	case active:
		return "Plan mode is already on."
	case change == plan.Committed:
		return "Plan mode off."
	case change == plan.Cancelled:
		return "Plan mode entry cancelled."
	case change == plan.Queued || wasActive:
		return "Leaving plan mode (applies from the next step)."
	default:
		return "Plan mode is already off."
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
			if candidate.Effort != "" {
				lines[index] += " effort=" + string(candidate.Effort)
			}
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
