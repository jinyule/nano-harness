package tui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// chromeRows counts the header, prompt, input, and footer rows around the transcript.
const chromeRows = 4

var (
	planTitleStyle  = lipgloss.NewStyle().Bold(true)
	planActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// layout sizes the transcript around the pinned plan panel. The panel takes at most half of the
// rows left by the chrome and never the transcript's last row, so small terminals keep output.
func (model *model) layout() {
	follow := model.viewport.AtBottom()
	rows := 0
	if len(model.todos) > 0 {
		rows = max(0, min(len(model.todos)+1, (model.height-chromeRows)/2, model.height-chromeRows-1))
	}
	model.plan = planLines(model.todos, model.width, rows)
	model.viewport.SetWidth(max(model.width-4, 1))
	model.viewport.SetHeight(max(model.height-chromeRows-len(model.plan), 1))
	model.input.SetWidth(max(model.width-8, 1))
	model.refresh()
	if follow {
		model.viewport.GotoBottom()
	}
}

// planLines renders a progress title and as many items as rows allow. When items overflow, the
// window starts at the first unfinished item and the last row counts the hidden items; the title
// keeps the complete per-status counts either way.
func planLines(todos []session.TodoItem, width, rows int) []string {
	if rows < 1 {
		return nil
	}
	counts := map[session.TodoStatus]int{}
	for _, item := range todos {
		counts[item.Status]++
	}
	var segments []string
	for _, status := range []session.TodoStatus{session.TodoCompleted, session.TodoInProgress, session.TodoPending} {
		if counts[status] > 0 {
			segments = append(segments, fmt.Sprintf("%d %s", counts[status], strings.ReplaceAll(string(status), "_", " ")))
		}
	}
	lines := []string{planTitleStyle.Render(ansi.Truncate("plan> "+strings.Join(segments, " · "), width, "…"))}
	visible, start, overflow := rows-1, 0, false
	if visible < len(todos) {
		if visible > 1 {
			visible, overflow = visible-1, true
		}
		start = slices.IndexFunc(todos, func(item session.TodoItem) bool { return item.Status != session.TodoCompleted })
		if start < 0 {
			start = len(todos)
		}
		start = min(start, len(todos)-visible)
	}
	for _, item := range todos[start : start+visible] {
		marker, style := "[ ]", lipgloss.NewStyle()
		switch item.Status {
		case session.TodoCompleted:
			marker, style = "[x]", mutedStyle
		case session.TodoInProgress:
			marker, style = "[>]", planActiveStyle
		case session.TodoPending:
			// Pending items keep the default style.
		}
		lines = append(lines, style.Render(ansi.Truncate("  "+marker+" "+strings.Join(strings.Fields(item.Content), " "), width, "…")))
	}
	if overflow {
		lines = append(lines, mutedStyle.Render(ansi.Truncate(fmt.Sprintf("  … %d more", len(todos)-visible), width, "…")))
	}
	return lines
}
