package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
	"github.com/jinyule/nano-harness/internal/core/skill"
)

func TestModelSubmit_SendsSkillGesturesThatAreNotCommands(t *testing.T) {
	fixture, current := modelFixture(t)
	current.images = []session.Image{{Name: "one"}}
	current.input.SetValue("/pdf summarize report.pdf")
	next, command := current.submit()
	current = next.(model)
	if command == nil || len(current.images) != 0 {
		t.Fatalf("gesture submission = %#v cmd=%v", current.lines, command)
	}
	_ = command()
	submitted := fixture.controller.submitted
	if len(submitted) != 1 || session.Text(submitted[0]) != "/pdf summarize report.pdf" || len(submitted[0].Content) != 2 || submitted[0].Source.Kind != "user" {
		t.Fatalf("submitted = %#v", submitted)
	}
	for _, value := range []string{"/Pdf summarize", "/pdf/refs", "/-pdf"} {
		current.input.SetValue(value)
		next, command := current.submit()
		current = next.(model)
		if command != nil || current.lines[len(current.lines)-1] != "error> unknown command; use /help" {
			t.Fatalf("%q = %#v cmd=%v", value, current.lines, command)
		}
	}
	current.input.SetValue("/help")
	next, _ = current.submit()
	if help := next.(model).lines; !strings.Contains(help[len(help)-1], "/SKILL TEXT") {
		t.Fatalf("help = %q", help[len(help)-1])
	}
}

func TestApplyEvent_SummarizesInjectedSkillContext(t *testing.T) {
	_, current := modelFixture(t)
	for _, kind := range []string{skill.SourceCatalog, skill.SourceInvocation, "user"} {
		current.applyEvent(session.Event{Record: session.Record{Type: session.RecordUserMessage, Message: &session.Message{
			Role: session.RoleUser, Source: session.MessageSource{Kind: kind}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "<system-reminder>body"}},
		}}}, true)
	}
	if want := []string{"skill> catalog updated", "skill> instructions injected", "you> <system-reminder>body"}; !slices.Equal(current.lines, want) {
		t.Fatalf("lines = %#v", current.lines)
	}
}
