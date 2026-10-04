package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	for line := range strings.SplitSeq(current.View().Content, "\n") {
		if lipgloss.Width(line) > 40 {
			t.Fatalf("view exceeds terminal width: %q", line)
		}
	}
	for range 30 {
		current.addLine("history")
	}
	current.viewport.GotoTop()
	current.addLine("new output")
	if current.viewport.YOffset() != 0 {
		t.Fatal("new output moved the history viewport")
	}
	current, _ = update(t, current, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if current.viewport.YOffset() != 0 || current.input.Value() != "j" {
		t.Fatal("typing moved the transcript")
	}
	current, _ = update(t, current, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if current.viewport.YOffset() == 0 {
		t.Fatal("page down did not scroll")
	}
	current.viewport.GotoTop()
	current, _ = update(t, current, tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if current.viewport.YOffset() == 0 {
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
	if current.viewport.YOffset() != 0 {
		t.Fatal("resizing moved the history viewport")
	}
}

func TestModel_ViewFitsSmallTerminalAndUsesV2Modes(t *testing.T) {
	for _, size := range []tea.WindowSizeMsg{{Width: 18, Height: 8}, {Width: 40, Height: 12}} {
		_, current := modelFixture(t)
		current, _ = update(t, current, size)
		view := current.View()
		if !view.AltScreen || view.MouseMode != tea.MouseModeCellMotion {
			t.Fatal("terminal modes missing")
		}
		if lipgloss.Width(view.Content) > size.Width || lipgloss.Height(view.Content) != size.Height {
			t.Errorf("view %dx%d exceeds terminal %dx%d", lipgloss.Width(view.Content), lipgloss.Height(view.Content), size.Width, size.Height)
		}
	}
}

func TestModel_V2PasteReleaseAndSecretInput(t *testing.T) {
	_, current := modelFixture(t)
	current, _ = update(t, current, tea.PasteMsg{Content: "你好 pasted text"})
	current, _ = update(t, current, tea.KeyReleaseMsg{Code: tea.KeyEnter})
	if current.input.Value() != "你好 pasted text" {
		t.Fatal("paste or release changed input")
	}
	current.input.EchoMode = textinput.EchoPassword
	current.input.SetValue("PRIVATE_INPUT")
	view := current.View()
	if strings.Contains(view.Content, "PRIVATE_INPUT") || view.Cursor != nil || !current.input.VirtualCursor() {
		t.Fatal("secret or virtual cursor rendered incorrectly")
	}
}
