package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/plan"
	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	"github.com/jinyule/nano-harness/internal/core/session"
	"github.com/jinyule/nano-harness/internal/core/skill"
)

func (model *model) applyEvent(event session.Event, live bool) {
	record := event.Record
	switch record.Type {
	case session.RecordUserMessage:
		text := session.Text(*record.Message)
		_ = model.goal.Apply(record)
		switch source := record.Message.Source; source.Kind {
		case "runtime-context":
			return
		case skill.SourceCatalog:
			model.addLine("skill> catalog updated")
			return
		case skill.SourceInvocation:
			model.addLine("skill> instructions injected")
			return
		case plan.NoticeSource:
			model.addLine("mode> " + text)
			return
		case session.GoalSource:
			model.addLine(fmt.Sprintf("goal> round %d", source.GoalRound))
			return
		case appGoal.WrapUpSource:
			first, _, _ := strings.Cut(text, "\n")
			model.addLine("goal> " + first)
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
		// Completion notices and agent messages are harness input, not
		// something the user typed.
		switch record.Message.Source.Kind {
		case appJob.NoticeSource:
			model.addLine("job> " + text)
		case appSubagent.SourceAgentMessage, appSubagent.SourceSettled:
			model.addLine("agent> " + text)
		default:
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
		line := prefix + "> " + record.Result.Output
		if image := record.Result.Image; image != nil {
			// The image itself stays out of the terminal; its identity lets
			// the user match it to the file and the durable record.
			line += fmt.Sprintf(" [image %s %dx%d %.19s]", image.Name, image.Width, image.Height, image.ID)
		}
		model.addLine(line)
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
	case session.RecordCompactionPrune:
		model.addLine(fmt.Sprintf("compact> pruned tool result #%d to %d bytes", record.Prune.Seq, len(record.Prune.Output)))
	case session.RecordPlanMode:
		model.planActive = record.Plan.Active
		if model.planActive {
			model.addLine("mode> plan mode on")
		} else {
			model.addLine("mode> plan mode off")
		}
	case session.RecordGoalChange:
		// The log was validated when it was committed; the fold only follows it.
		_ = model.goal.Apply(record)
		model.addLine(goalLine(*record.Goal))
	case session.RecordTurnEnd:
		model.streamText = ""
		model.addLine("turn> " + string(record.Outcome))
	case session.RecordTurnStart, session.RecordTodoWrite:
		model.todos = session.StandingTodos(model.todos, record)
		model.layout()
	case session.RecordStepStart, session.RecordApprovalDecided,
		session.RecordApprovalPolicy, session.RecordRetryStarted, session.RecordCompactionSummary,
		session.RecordSubagentDescriptor, session.RecordSubagentCatalog, session.RecordWebSearchRequest, session.RecordSandboxMode, session.RecordStepEnd,
		session.RecordNoticeQueued:
		// These facts affect replay or lifecycle state but have no standalone TUI line;
		// a queued notice is shown when its user/message delivers it.
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
