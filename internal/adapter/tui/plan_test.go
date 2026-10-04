package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func todoEvent(callID string, items ...session.TodoItem) session.Event {
	return session.Event{Record: session.Record{Type: session.RecordTodoWrite, Turn: 1, Step: 1, Todo: &session.TodoWrite{CallID: callID, Items: items}}}
}

func stripped(lines []string) []string {
	plain := make([]string, len(lines))
	for index, line := range lines {
		plain[index] = ansi.Strip(line)
	}
	return plain
}

func TestModel_PlanReplaysLatestTodoWriteAndClearsOnNextTurn(t *testing.T) {
	fixture, config := newAppFixture()
	app, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	app.agent = fixture.controller
	initial := []session.Event{
		{Record: session.Record{Type: session.RecordTurnStart, Turn: 1}},
		todoEvent("first", session.TodoItem{Content: "stale", Status: session.TodoPending}),
		todoEvent("second", session.TodoItem{Content: "write tests", Status: session.TodoCompleted}, session.TodoItem{Content: "implement", Status: session.TodoInProgress}),
		{Record: session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted}},
	}
	current := newModel(context.Background(), app, initial)
	want := []string{"plan> 1 completed · 1 in progress", "  [x] write tests", "  [>] implement"}
	if got := stripped(current.plan); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("replayed plan = %q", got)
	}
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "[>] implement") || strings.Contains(view, "stale") {
		t.Fatalf("view lacks the standing plan:\n%s", view)
	}
	if current.viewport.Height() != current.height-chromeRows-len(want) {
		t.Fatalf("viewport height = %d with %d plan rows", current.viewport.Height(), len(want))
	}

	current, _ = update(t, current, transcriptMessage{event: session.Event{Record: session.Record{Type: session.RecordTurnStart, Turn: 2}}})
	if current.plan != nil || current.todos != nil || strings.Contains(current.View().Content, "plan>") || current.viewport.Height() != current.height-chromeRows {
		t.Fatalf("next turn kept the previous plan: %q", current.plan)
	}
	current, _ = update(t, current, transcriptMessage{event: todoEvent("third", session.TodoItem{Content: "ship", Status: session.TodoPending})})
	if got := stripped(current.plan); len(got) != 2 || got[0] != "plan> 1 pending" || got[1] != "  [ ] ship" {
		t.Fatalf("live plan = %q", got)
	}
	current, _ = update(t, current, transcriptMessage{event: todoEvent("fourth", []session.TodoItem{}...)})
	if current.plan != nil || current.todos == nil {
		t.Fatalf("empty write should clear the panel but keep the written list: %q %#v", current.plan, current.todos)
	}
}

func TestModel_PlanFitsSmallTerminalsAndKeepsTranscriptVisible(t *testing.T) {
	items := make([]session.TodoItem, 12)
	for index := range items {
		items[index] = session.TodoItem{Content: fmt.Sprintf("任务 %d %s", index, strings.Repeat("long words ", 8)), Status: session.TodoCompleted}
	}
	items[7].Status = session.TodoInProgress
	items[8].Status = session.TodoPending
	for _, size := range []tea.WindowSizeMsg{{Width: 18, Height: 8}, {Width: 40, Height: 12}, {Width: 80, Height: 24}, {Width: 30, Height: 6}, {Width: 30, Height: 5}} {
		_, current := modelFixture(t)
		current.applyEvent(todoEvent("call", items...), true)
		current.addLine("latest output")
		current, _ = update(t, current, size)
		view := current.View()
		if lipgloss.Width(view.Content) > size.Width || lipgloss.Height(view.Content) != size.Height {
			t.Errorf("%dx%d: view is %dx%d", size.Width, size.Height, lipgloss.Width(view.Content), lipgloss.Height(view.Content))
		}
		if current.viewport.Height() < 1 || !strings.Contains(current.viewport.View(), "output") {
			t.Errorf("%dx%d: transcript hidden by the plan: %q", size.Width, size.Height, current.viewport.View())
		}
		plain := ansi.Strip(view.Content)
		if size.Height > 5 && !strings.Contains(plain, "plan>") {
			t.Errorf("%dx%d: plan title missing:\n%s", size.Width, size.Height, plain)
		}
		if size.Height >= 12 && !strings.Contains(plain, "[>]") {
			t.Errorf("%dx%d: active item hidden:\n%s", size.Width, size.Height, plain)
		}
	}
}

func TestPlanLines_WindowsOverflowAroundFirstUnfinishedItem(t *testing.T) {
	items := []session.TodoItem{
		{Content: "one", Status: session.TodoCompleted},
		{Content: "two", Status: session.TodoCompleted},
		{Content: "three", Status: session.TodoInProgress},
		{Content: "four", Status: session.TodoInProgress},
		{Content: "five\n\tsix", Status: session.TodoPending},
	}
	done := []session.TodoItem{{Content: "a", Status: session.TodoCompleted}, {Content: "b", Status: session.TodoCompleted}, {Content: "c", Status: session.TodoCompleted}}
	for name, test := range map[string]struct {
		todos []session.TodoItem
		width int
		rows  int
		want  []string
	}{
		"no rows":       {items, 80, 0, nil},
		"title only":    {items, 80, 1, []string{"plan> 2 completed · 2 in progress · 1 pending"}},
		"single item":   {items, 80, 2, []string{"plan> 2 completed · 2 in progress · 1 pending", "  [>] three"}},
		"overflow":      {items, 80, 4, []string{"plan> 2 completed · 2 in progress · 1 pending", "  [>] three", "  [>] four", "  … 3 more"}},
		"tail window":   {items, 80, 5, []string{"plan> 2 completed · 2 in progress · 1 pending", "  [>] three", "  [>] four", "  [ ] five six", "  … 2 more"}},
		"everything":    {items, 80, 6, []string{"plan> 2 completed · 2 in progress · 1 pending", "  [x] one", "  [x] two", "  [>] three", "  [>] four", "  [ ] five six"}},
		"all completed": {done, 80, 3, []string{"plan> 3 completed", "  [x] c", "  … 2 more"}},
		"narrow":        {items[:1], 8, 2, []string{"plan> 1…", "  [x] o…"}},
	} {
		if got := stripped(planLines(test.todos, test.width, test.rows)); strings.Join(got, "\n") != strings.Join(test.want, "\n") || len(got) != len(test.want) {
			t.Errorf("%s: got %q, want %q", name, got, test.want)
		}
	}
}
