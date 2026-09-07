package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestModel_StreamKeepsSystemOutputSeparate(t *testing.T) {
	_, current := modelFixture(t)
	current.appendStream("assistant", "first")
	current.addLine("system> no subagents")
	current.appendStream("assistant", "second")
	if current.lines[1] != "system> no subagents" || current.lines[2] != "assistant> second" {
		t.Fatalf("interleaved stream = %#v", current.lines)
	}
}

func TestModel_ReasoningDoesNotHideFinalAnswer(t *testing.T) {
	_, current := modelFixture(t)
	current.appendStream("reasoning", "thinking")
	current.applyEvent(session.Event{Record: session.Record{Type: session.RecordAssistantMessage, Message: messagePointer(session.RoleAssistant, "answer")}}, true)
	if !strings.Contains(strings.Join(current.lines, "\n"), "assistant> answer") {
		t.Fatalf("answer missing: %#v", current.lines)
	}
}

func TestModel_LongLinesWrapAndHistoryStaysVisible(t *testing.T) {
	_, current := modelFixture(t)
	current, _ = update(t, current, tea.WindowSizeMsg{Width: 40, Height: 24})
	text := "you> " + strings.Repeat("中文abc", 15) + " END"
	current.addLine(text)
	if !strings.Contains(current.viewport.View(), "END") || current.viewport.TotalLineCount() < 3 {
		t.Fatalf("long line was clipped: %q", current.viewport.View())
	}
	for line := range strings.SplitSeq(current.View(), "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("view exceeds terminal width: %q", line)
		}
	}
	for range 30 {
		current.addLine("history")
	}
	current.viewport.GotoTop()
	current.addLine("new output")
	if current.viewport.YOffset != 0 {
		t.Fatal("new output moved the history viewport")
	}
	current, _ = update(t, current, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if current.viewport.YOffset != 0 || current.input.Value() != "j" {
		t.Fatal("typing moved the transcript")
	}
	current, _ = update(t, current, tea.KeyMsg{Type: tea.KeyPgDown})
	if current.viewport.YOffset == 0 {
		t.Fatal("page down did not scroll")
	}
	current.viewport.GotoTop()
	current, _ = update(t, current, tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if current.viewport.YOffset == 0 {
		t.Fatal("mouse wheel did not scroll")
	}
	current.viewport.GotoBottom()
	current.addLine("follow at bottom")
	if !current.viewport.AtBottom() {
		t.Fatal("bottom viewport stopped following")
	}
}

func TestModel_ResizeKeepsFollowingTheLatestOutput(t *testing.T) {
	_, current := modelFixture(t)
	for range 30 {
		current.addLine("history")
	}
	current.addLine("latest output")
	current, _ = update(t, current, tea.WindowSizeMsg{Width: 40, Height: 12})
	if !current.viewport.AtBottom() || !strings.Contains(current.viewport.View(), "latest output") {
		t.Fatal("shrinking the terminal hid the latest output")
	}
	current.viewport.GotoTop()
	current, _ = update(t, current, tea.WindowSizeMsg{Width: 50, Height: 15})
	if current.viewport.YOffset != 0 {
		t.Fatal("resizing moved the history viewport")
	}
}
