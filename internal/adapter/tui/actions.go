package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/session"
)

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
