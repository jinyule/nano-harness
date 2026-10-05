package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/jinyule/nano-harness/internal/app/question"
)

// recommendedMark is the upstream label convention for a recommended option.
const recommendedMark = "(Recommended)"

// questionState walks one request question by question.
type questionState struct {
	envelope questionEnvelope
	index    int
	answers  []question.Answer
}

func (state *questionState) current() question.Question {
	return state.envelope.request.Questions[state.index]
}

// beginQuestion switches to question input and presents the first question.
func (model *model) beginQuestion(envelope questionEnvelope) {
	model.mode = modeQuestion
	model.question = &questionState{envelope: envelope}
	model.presentQuestion()
}

// presentQuestion writes the current question, its detail, and its numbered
// options to the transcript. A recommended option is prefilled so Enter
// accepts it visibly; the user can clear or replace the input.
func (model *model) presentQuestion() {
	state := model.question
	current := state.current()
	title := current.Text
	if current.Header != "" {
		title = "[" + current.Header + "] " + title
	}
	model.addLine(fmt.Sprintf("question> %s (%d/%d)", title, state.index+1, len(state.envelope.request.Questions)))
	if current.Detail != "" {
		for line := range strings.SplitSeq(current.Detail, "\n") {
			model.addLine("  │ " + line)
		}
	}
	model.input.SetValue("")
	for index, option := range current.Options {
		line := fmt.Sprintf("  %d. %s", index+1, option.Label)
		if option.Description != "" {
			line += " — " + option.Description
		}
		model.addLine(line)
		if model.input.Value() == "" && strings.Contains(option.Label, recommendedMark) {
			model.input.SetValue(strconv.Itoa(index + 1))
		}
	}
	model.input.Placeholder = questionHint(current)
	model.input.CursorEnd()
}

func questionHint(current question.Question) string {
	switch {
	case len(current.Options) == 0:
		return "type an answer; empty skips"
	case current.MultiSelect:
		return "option numbers separated by commas, or type an answer; empty skips"
	default:
		return "an option number, or type an answer; empty skips"
	}
}

// answerQuestion records the input for the current question and either
// presents the next one or returns the completed batch to the broker.
func (model model) answerQuestion(value string) (tea.Model, tea.Cmd) {
	state := model.question
	answer, err := parseAnswer(state.current(), value)
	if err != nil {
		model.addLine("error> " + err.Error())
		return model, nil
	}
	state.answers = append(state.answers, answer)
	model.addLine("answer> " + describeAnswer(answer))
	if state.index+1 < len(state.envelope.request.Questions) {
		state.index++
		model.presentQuestion()
		return model, nil
	}
	state.envelope.result <- questionResult{answers: state.answers}
	model.question = nil
	model.restoreInput()
	return model, nil
}

func (model *model) cancelQuestion() {
	model.question.envelope.result <- questionResult{err: question.ErrCancelled}
	model.question = nil
	model.addLine("answer> cancelled")
	model.restoreInput()
}

// parseAnswer reads option numbers when the whole input is a number list,
// and free-form text otherwise; empty input skips the question.
func parseAnswer(current question.Question, value string) (question.Answer, error) {
	value = strings.TrimSpace(value)
	answer := question.Answer{ID: current.ID, Selected: []string{}}
	if value == "" {
		return answer, nil
	}
	if numbers, ok := choiceNumbers(value); ok && len(current.Options) > 0 {
		if !current.MultiSelect && len(numbers) != 1 {
			return question.Answer{}, errors.New("choose exactly one option number")
		}
		seen := map[int]struct{}{}
		for _, number := range numbers {
			if number < 1 || number > len(current.Options) {
				return question.Answer{}, fmt.Errorf("option numbers range from 1 to %d", len(current.Options))
			}
			if _, duplicate := seen[number]; duplicate {
				return question.Answer{}, fmt.Errorf("option %d is chosen twice", number)
			}
			seen[number] = struct{}{}
			answer.Selected = append(answer.Selected, current.Options[number-1].Label)
		}
		return answer, nil
	}
	if len(value) > question.MaxCustomBytes {
		return question.Answer{}, fmt.Errorf("answers are limited to %d bytes", question.MaxCustomBytes)
	}
	answer.Custom = value
	return answer, nil
}

// choiceNumbers splits on commas and spaces and reports whether every field
// is an unsigned decimal number.
func choiceNumbers(value string) ([]int, bool) {
	fields := strings.FieldsFunc(value, func(char rune) bool { return char == ',' || unicode.IsSpace(char) })
	numbers := make([]int, len(fields))
	for index, field := range fields {
		if strings.TrimLeft(field, "0123456789") != "" {
			return nil, false
		}
		number, err := strconv.Atoi(field)
		if err != nil {
			return nil, false
		}
		numbers[index] = number
	}
	return numbers, len(numbers) > 0
}

func describeAnswer(answer question.Answer) string {
	parts := append([]string(nil), answer.Selected...)
	if answer.Custom != "" {
		parts = append(parts, strconv.Quote(answer.Custom))
	}
	if len(parts) == 0 {
		return answer.ID + ": (skipped)"
	}
	return answer.ID + ": " + strings.Join(parts, ", ")
}
