package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

type policyJournal struct {
	events []session.Event
	err    error
}

func (*policyJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}
func (journal *policyJournal) Events(context.Context) ([]session.Event, error) {
	return journal.events, journal.err
}

func TestInvocation_SandboxRequiresReadableHistory(t *testing.T) {
	if _, err := (Invocation{}).SandboxMode(t.Context()); err == nil {
		t.Fatal("missing journal granted policy")
	}
	failure := errors.New("journal failed")
	journal := &policyJournal{err: failure}
	if _, err := (Invocation{Journal: journal}).SandboxMode(t.Context()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	journal.err = nil
	if mode, err := (Invocation{Journal: journal}).SandboxMode(t.Context()); err != nil || mode != session.SandboxWorkspaceWrite {
		t.Fatalf("default=%s %v", mode, err)
	}
	journal.events = []session.Event{{Record: session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: session.SandboxReadOnly}}}}
	if mode, err := (Invocation{Journal: journal}).SandboxMode(t.Context()); err != nil || mode != session.SandboxReadOnly {
		t.Fatalf("override=%s %v", mode, err)
	}
}
