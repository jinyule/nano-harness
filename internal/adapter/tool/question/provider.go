// Package question exposes the user-questions seam as the upstream
// ask_user_question tool in its default blocking form.
package question

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	appQuestion "github.com/jinyule/nano-harness/internal/app/question"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// Asker is the user-questions use case consumed by the tool.
type Asker interface {
	Ask(context.Context, appQuestion.Request) ([]appQuestion.Answer, error)
}

// Provider owns the ask_user_question registration.
type Provider struct {
	runtime *appTool.Runtime
	asker   Asker
}

// New constructs the tool provider.
func New(runtime *appTool.Runtime, asker Asker) (*Provider, error) {
	if runtime == nil || asker == nil {
		return nil, errors.New("invalid question tool configuration")
	}
	return &Provider{runtime: runtime, asker: asker}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "question-tools" }

// Start publishes ask_user_question for the caller's scope.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	return provider.runtime.Register(provider.askTool(), scope)
}

type optionArgs struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type questionArgs struct {
	ID          string       `json:"id"`
	Question    string       `json:"question"`
	Header      string       `json:"header"`
	Options     []optionArgs `json:"options"`
	MultiSelect bool         `json:"multi_select"`
}

type askArgs struct {
	Questions []questionArgs `json:"questions"`
}

// answerItem and answerBatch are the upstream result shape: selected is
// always present and custom is omitted when the user typed nothing.
type answerItem struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

type answerBatch struct {
	Answers []answerItem `json:"answers"`
}

func (provider *Provider) askTool() *appTool.Tool {
	option := appTool.Object("", true,
		appTool.Required("label", appTool.String("Short user-facing option label.")),
		appTool.Optional("description", appTool.String("One sentence explaining the tradeoff or impact.")),
	)
	item := appTool.Object("", true,
		appTool.Required("id", appTool.String("Stable id for this question; echoed in the answer.")),
		appTool.Required("question", appTool.String("The specific question to ask the user.")),
		appTool.Optional("header", appTool.String(`Optional short heading for the question, such as "Confirm" or "Choose Mode".`)),
		appTool.Optional("options", appTool.Array(`Optional choices to show the user. If you recommend one, put it first and append "(Recommended)" to that label.`, option)),
		appTool.Optional("multi_select", appTool.Boolean("Whether the user may select more than one option. Defaults to false.")),
	)
	return appTool.Define(appTool.Spec[askArgs]{
		Name:        "ask_user_question",
		Description: "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding.",
		Parameters: appTool.Parameters{
			appTool.Required("questions", appTool.Array("Questions to ask the user before continuing.", item)),
		},
		Execute: func(ctx context.Context, invocation appTool.Invocation, arguments askArgs) (appTool.Result, error) {
			questions := make([]appQuestion.Question, len(arguments.Questions))
			for index, candidate := range arguments.Questions {
				options := make([]appQuestion.Option, len(candidate.Options))
				for position, option := range candidate.Options {
					options[position] = appQuestion.Option{Label: option.Label, Description: option.Description}
				}
				questions[index] = appQuestion.Question{
					ID: candidate.ID, Text: candidate.Question, Header: candidate.Header,
					Options: options, MultiSelect: candidate.MultiSelect,
				}
			}
			answers, err := provider.asker.Ask(ctx, appQuestion.Request{
				SessionID: invocation.SessionID, CallID: invocation.CallID, Delegated: invocation.Delegated, Questions: questions,
			})
			if err != nil {
				return appTool.Result{}, err
			}
			return appTool.Text(render(answers)), nil
		},
	})
}

// render encodes answers like JSON.stringify: compact and without HTML
// escaping.
func render(answers []appQuestion.Answer) string {
	batch := answerBatch{Answers: make([]answerItem, len(answers))}
	for index, answer := range answers {
		batch.Answers[index] = answerItem{ID: answer.ID, Selected: answer.Selected, Custom: answer.Custom}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(batch) // strings and string slices always encode
	return string(bytes.TrimSuffix(buffer.Bytes(), []byte("\n")))
}
