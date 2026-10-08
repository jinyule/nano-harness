package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestModel_SandboxContextIsNotAUserUtterance(t *testing.T) {
	model := &model{}
	model.applyEvent(session.Event{Record: session.Record{Type: session.RecordUserMessage, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "runtime-context", Plugin: "sandbox:policy"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "Current runtime context."}}}}}, false)
	if len(model.lines) != 0 {
		t.Fatalf("runtime context displayed as user: %+v", model.lines)
	}
}

func TestModel_HumanSandboxCommand(t *testing.T) {
	fixture, current := modelFixture(t)
	for _, mode := range []session.SandboxMode{session.SandboxReadOnly, session.SandboxWorkspaceWrite, session.SandboxDangerFullAccess} {
		_, command := current.command("/sandbox " + string(mode))
		message := command().(operationMessage)
		if message.err != nil || message.text != "sandbox mode="+string(mode) || fixture.registry.sandbox != mode {
			t.Fatalf("switch=%+v registry=%s", message, fixture.registry.sandbox)
		}
	}
	for _, value := range []string{"/sandbox", "/sandbox invalid", "/sandbox read-only extra"} {
		next, command := current.command(value)
		if command != nil || !strings.Contains(next.(model).View().Content, "usage: /sandbox") {
			t.Fatalf("accepted %s", value)
		}
	}
	fixture.registry.err = errors.New("append failed")
	_, command := current.command("/sandbox read-only")
	if message := command().(operationMessage); !errors.Is(message.err, fixture.registry.err) {
		t.Fatal(message)
	}
}
