package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func (model *model) applyEvent(event session.Event, live bool) {
	record := event.Record
	switch record.Type {
	case session.RecordUserMessage:
		text := session.Text(*record.Message)
		if record.Message.Source.Kind == plan.NoticeSource {
			model.addLine("mode> " + text)
			return
		}
		attachments := 0
		for _, block := range record.Message.Content {
			if block.Type == session.ContentImage {
				attachments++
			}
		}
		if attachments > 0 {
			text += fmt.Sprintf(" [images=%d]", attachments)
		}
		// Completion notices are harness input, not something the user typed.
		if record.Message.Source.Kind == appJob.NoticeSource {
			model.addLine("job> " + text)
		} else {
			model.addLine("you> " + text)
		}
	case session.RecordRequestHeader:
		route := fmt.Sprintf("route> %s/%s", record.Header.Provider, record.Header.Model)
		if record.Header.Effort != "" {
			route += " effort=" + string(record.Header.Effort)
		}
		model.addLine(route)
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
	case session.RecordPlanMode:
		model.planActive = record.Plan.Active
		if model.planActive {
			model.addLine("mode> plan mode on")
		} else {
			model.addLine("mode> plan mode off")
		}
	case session.RecordTurnEnd:
		model.streamText = ""
		model.addLine("turn> " + string(record.Outcome))
	case session.RecordTurnStart, session.RecordTodoWrite:
		model.todos = session.StandingTodos(model.todos, record)
		model.layout()
	case session.RecordStepStart, session.RecordApprovalDecided,
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
	model.viewport.SetContent(ansi.Hardwrap(strings.Join(model.lines, "\n"), model.viewport.Width(), true))
	if follow {
		model.viewport.GotoBottom()
	}
}
